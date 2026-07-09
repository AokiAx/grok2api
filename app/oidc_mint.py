"""
Mint CLI OIDC tokens for cli-chat-proxy (NOT web sso).

Verified protocol:
  grant_types = authorization_code | refresh_token | device_code
  (no password grant)

Automated post-register path:
  A) Reuse registrar curl session (already has accounts/auth cookies after SSO chain)
  B) Else createSession(email, password, turnstile) → cookie chain → same session
  C) Manual redirect walk on /oauth2/authorize + PKCE
     → extract code from Location: http://127.0.0.1/.../callback?code=
     → POST /oauth2/token
  D) Save access_token + refresh_token into CLI pool (data/cli_accounts.json)
"""

from __future__ import annotations

import base64
import hashlib
import json
import logging
import secrets
import time
import urllib.parse
from typing import Any, Callable

import httpx

from .cli_pool import cli_pool
from .config import Settings, settings
from .oauth_login import (
    DEFAULT_CLIENT_ID,
    DEFAULT_ISSUER,
    DEFAULT_SCOPES,
    discover,
)

log = logging.getLogger("grok2api.oidc_mint")

ACCOUNTS_BASE = "https://accounts.x.ai"
# Public CLI native client — loopback redirect is accepted (same as grok login)
CLI_CLIENT_ID = DEFAULT_CLIENT_ID


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def _pkce_pair() -> tuple[str, str]:
    verifier = _b64url(secrets.token_bytes(32))
    challenge = _b64url(hashlib.sha256(verifier.encode("ascii")).digest())
    return verifier, challenge


def _common_headers() -> dict[str, str]:
    return {
        "user-agent": (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
        ),
        "accept-language": "en-US,en;q=0.9",
        "sec-ch-ua": '"Chromium";v="136", "Google Chrome";v="136", "Not.A/Brand";v="99"',
        "sec-ch-ua-mobile": "?0",
        "sec-ch-ua-platform": '"Windows"',
    }


def _cookie_names(session: Any) -> list[str]:
    try:
        return list(session.cookies.keys())
    except Exception:
        return []


def _abs_url(base: str, loc: str) -> str:
    if not loc:
        return ""
    if loc.startswith("http://") or loc.startswith("https://"):
        return loc
    return urllib.parse.urljoin(base, loc)


def create_session_login(
    email: str,
    password: str,
    turnstile_token: str,
    *,
    proxy: str | None = None,
    impersonate: str = "chrome136",
    session: Any | None = None,
) -> Any:
    """
    Password login via accounts.x.ai createSession.
    Returns curl_cffi Session with auth cookies set (same session object).
    """
    from curl_cffi import requests as curl_requests

    if session is None:
        session = curl_requests.Session(impersonate=impersonate)
        if proxy:
            session.proxies = {"http": proxy, "https": proxy}
        # warm CF
        try:
            session.get(
                f"{ACCOUNTS_BASE}/sign-in",
                headers={**_common_headers(), "accept": "text/html,*/*;q=0.8"},
                timeout=30,
            )
        except Exception:
            pass

    payload = {
        "rpc": "createSession",
        "req": {
            "createSessionRequest": {
                "credentials": {
                    "case": "emailAndPassword",
                    "value": {
                        "email": email,
                        "clearTextPassword": password,
                    },
                },
            },
            "turnstileToken": turnstile_token,
        },
    }
    headers = {
        **_common_headers(),
        "accept": "application/json",
        "content-type": "application/json",
        "origin": ACCOUNTS_BASE,
        "referer": f"{ACCOUNTS_BASE}/sign-in",
    }
    resp = session.post(
        f"{ACCOUNTS_BASE}/api/rpc",
        headers=headers,
        json=payload,
        timeout=45,
    )
    if resp.status_code >= 400:
        raise RuntimeError(
            f"createSession HTTP {resp.status_code}: {(resp.text or '')[:500]}"
        )
    try:
        data = resp.json()
    except Exception as e:
        raise RuntimeError(
            f"createSession non-JSON ({resp.status_code}): {(resp.text or '')[:300]}"
        ) from e
    if not isinstance(data, dict):
        raise RuntimeError("createSession returned non-object")
    if data.get("error"):
        raise RuntimeError(f"createSession error: {data.get('error')}")

    cookie_url = str(data.get("cookieSetterUrl") or "").strip()
    if not cookie_url.startswith("https://auth."):
        raise RuntimeError(
            f"createSession missing cookieSetterUrl: "
            f"{json.dumps(data, ensure_ascii=False)[:500]}"
        )

    log.info("createSession OK → follow cookie chain")
    nav = {
        **_common_headers(),
        "accept": "text/html,application/xhtml+xml,*/*;q=0.8",
        "upgrade-insecure-requests": "1",
    }
    try:
        session.get(cookie_url, headers=nav, allow_redirects=True, timeout=45)
    except Exception as e:
        log.warning("cookie chain: %s", e)
    try:
        session.get("https://auth.x.ai/", headers=nav, allow_redirects=True, timeout=20)
    except Exception:
        pass
    log.info("session cookies after login: %s", _cookie_names(session))
    return session


def _extract_oauth_code_from_location(loc: str) -> tuple[str | None, str | None, str | None]:
    """Return (code, error, state) if Location is loopback callback."""
    if not loc:
        return None, None, None
    parsed = urllib.parse.urlparse(loc)
    host = (parsed.hostname or "").lower()
    if host not in ("127.0.0.1", "localhost"):
        return None, None, None
    qs = urllib.parse.parse_qs(parsed.query)
    err = qs.get("error", [None])[0]
    if err:
        desc = qs.get("error_description", [""])[0]
        return None, f"{err}: {desc}".strip(": "), qs.get("state", [None])[0]
    return qs.get("code", [None])[0], None, qs.get("state", [None])[0]


def mint_cli_tokens_with_session(
    session: Any,
    *,
    issuer: str = DEFAULT_ISSUER,
    client_id: str = CLI_CLIENT_ID,
    scopes: str = DEFAULT_SCOPES,
    timeout_secs: float = 45.0,
    prompt: str | None = None,
) -> dict[str, Any]:
    """
    PKCE authorize using authenticated session cookies.

    Critical: do NOT rely on following redirect to 127.0.0.1 with curl
    (often fails). Manually walk redirects and read ?code= from Location.
    """
    conf = discover(issuer)
    authorize_endpoint = conf["authorization_endpoint"]
    token_url = conf["token_endpoint"]
    verifier, challenge = _pkce_pair()
    state = secrets.token_urlsafe(24)
    # fixed high port — public native clients allow any loopback port
    redirect_uri = f"http://127.0.0.1:19787/callback"

    params: dict[str, str] = {
        "response_type": "code",
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "scope": scopes,
        "state": state,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
    }
    if prompt is not None:
        params["prompt"] = prompt

    url = authorize_endpoint + "?" + urllib.parse.urlencode(params)
    headers = {
        **_common_headers(),
        "accept": "text/html,application/xhtml+xml,*/*;q=0.8",
        "upgrade-insecure-requests": "1",
    }
    log.info(
        "OIDC authorize walk prompt=%r cookies=%s",
        prompt,
        _cookie_names(session),
    )

    code: str | None = None
    last_url = url
    for hop in range(20):
        try:
            resp = session.get(
                last_url,
                headers=headers,
                allow_redirects=False,
                timeout=timeout_secs,
            )
        except Exception as e:
            # Some stacks throw when Location is loopback
            msg = str(e)
            for part in (msg, getattr(e, "args", (None,))[0] or ""):
                s = str(part)
                if "127.0.0.1" in s or "localhost" in s:
                    # try parse URL from error text
                    for token in s.replace("'", " ").replace('"', " ").split():
                        if "127.0.0.1" in token or "localhost" in token:
                            c, err, st = _extract_oauth_code_from_location(token)
                            if err:
                                raise RuntimeError(f"OIDC error: {err}")
                            if c:
                                code = c
                                break
                if code:
                    break
            if code:
                break
            raise RuntimeError(f"authorize hop failed: {e}") from e

        status = int(getattr(resp, "status_code", 0) or 0)
        loc = resp.headers.get("location") or resp.headers.get("Location") or ""
        loc = _abs_url(last_url, loc)

        log.debug("authorize hop %s status=%s loc=%s", hop, status, (loc or "")[:160])

        if loc:
            c, err, st = _extract_oauth_code_from_location(loc)
            if err:
                raise RuntimeError(f"OIDC authorize error: {err}")
            if c:
                if st and st != state:
                    raise RuntimeError("OIDC state mismatch")
                code = c
                break

        if status in (301, 302, 303, 307, 308) and loc:
            last_url = loc
            continue

        # non-redirect terminal page (login / consent HTML)
        body = ""
        try:
            body = (resp.text or "")[:300]
        except Exception:
            pass
        raise RuntimeError(
            f"OIDC authorize stuck HTTP {status} (need login/consent?). "
            f"body={body!r}"
        )

    if not code:
        raise RuntimeError(
            "OIDC authorize produced no code "
            "(session not logged in on auth.x.ai or consent required)"
        )

    with httpx.Client(timeout=30.0) as client:
        tr = client.post(
            token_url,
            data={
                "grant_type": "authorization_code",
                "code": code,
                "redirect_uri": redirect_uri,
                "client_id": client_id,
                "code_verifier": verifier,
            },
        )
        if tr.status_code >= 400:
            raise RuntimeError(
                f"token exchange failed ({tr.status_code}): {tr.text[:500]}"
            )
        tok = tr.json()

    access = tok.get("access_token")
    if not access:
        raise RuntimeError("token response missing access_token")
    return {
        "access_token": access,
        "refresh_token": tok.get("refresh_token"),
        "expires_in": int(tok.get("expires_in") or 21600),
        "id_token": tok.get("id_token"),
        "token_type": tok.get("token_type"),
        "oidc_issuer": issuer.rstrip("/"),
        "oidc_client_id": client_id,
        "scope": tok.get("scope"),
    }


def _save_tokens(
    tokens: dict[str, Any],
    *,
    email: str,
    password: str = "",
    note: str = "",
    cfg: Settings | None = None,
) -> dict[str, Any]:
    cfg = cfg or settings
    user_id = ""
    try:
        conf = discover(DEFAULT_ISSUER)
        ui = conf.get("userinfo_endpoint")
        if ui:
            with httpx.Client(timeout=15.0) as client:
                ur = client.get(
                    ui,
                    headers={"Authorization": f"Bearer {tokens['access_token']}"},
                )
                if ur.status_code < 400:
                    info = ur.json()
                    user_id = str(info.get("sub") or "")
                    if not email and info.get("email"):
                        email = str(info["email"])
    except Exception:
        log.debug("userinfo skipped", exc_info=True)

    acc = cli_pool.upsert_from_tokens(
        access_token=tokens["access_token"],
        refresh_token=tokens.get("refresh_token"),
        expires_in=tokens.get("expires_in"),
        email=email,
        password=password,
        user_id=user_id,
        oidc_issuer=tokens["oidc_issuer"],
        oidc_client_id=tokens["oidc_client_id"],
        note=note,
        account_id=email or user_id or None,
    )
    return {
        "ok": True,
        "email": email,
        "user_id": user_id,
        "account_id": acc.id,
        "expires_in": tokens.get("expires_in"),
        "has_refresh_token": bool(tokens.get("refresh_token")),
        "pool_usable": cli_pool.count(enabled_only=True),
        "oidc_issuer": tokens["oidc_issuer"],
        "oidc_client_id": tokens["oidc_client_id"],
        "scope": tokens.get("scope"),
    }


def _cookies_from_session(session: Any) -> list[dict[str, Any]]:
    """Export curl_cffi cookies into Playwright cookie dicts."""
    out: list[dict[str, Any]] = []
    jar = getattr(session.cookies, "jar", None) or session.cookies
    try:
        iterable = list(jar)
    except Exception:
        # mapping-like
        for name in _cookie_names(session):
            try:
                val = session.cookies.get(name)
            except Exception:
                continue
            if not val:
                continue
            for domain in (".x.ai", "auth.x.ai", ".grok.com", "accounts.x.ai"):
                out.append(
                    {
                        "name": name,
                        "value": str(val),
                        "domain": domain,
                        "path": "/",
                    }
                )
        return out

    for c in iterable:
        name = getattr(c, "name", None)
        value = getattr(c, "value", None)
        if not name or value is None:
            continue
        domain = (getattr(c, "domain", None) or "").lstrip(".") or "x.ai"
        # expand to parent for SSO cookies
        domains = {domain, f".{domain.lstrip('.')}"}
        if "x.ai" in domain or domain.endswith("x.ai"):
            domains.update({".x.ai", "auth.x.ai", "accounts.x.ai", "x.ai"})
        if "grok" in domain:
            domains.update({".grok.com", "grok.com", "auth.grok.com"})
        for d in domains:
            out.append(
                {
                    "name": str(name),
                    "value": str(value),
                    "domain": d if d.startswith(".") or d.count(".") >= 1 else f".{d}",
                    "path": getattr(c, "path", None) or "/",
                }
            )
    # always seed sso/sso-rw on .x.ai if present in mapping
    try:
        for name in ("sso", "sso-rw", "sso_rw"):
            val = session.cookies.get(name) or session.cookies.get(name.replace("_", "-"))
            if val:
                for d in (".x.ai", "auth.x.ai", "accounts.x.ai", ".grok.com"):
                    out.append(
                        {
                            "name": "sso-rw" if name == "sso_rw" else name,
                            "value": str(val),
                            "domain": d,
                            "path": "/",
                        }
                    )
    except Exception:
        pass
    # de-dupe
    seen: set[tuple[str, str, str]] = set()
    uniq: list[dict[str, Any]] = []
    for item in out:
        key = (item["name"], item["value"], item["domain"])
        if key in seen:
            continue
        seen.add(key)
        uniq.append(item)
    return uniq


def mint_cli_tokens_with_browser(
    *,
    cookies: list[dict[str, Any]] | None = None,
    session: Any | None = None,
    issuer: str = DEFAULT_ISSUER,
    client_id: str = CLI_CLIENT_ID,
    scopes: str = DEFAULT_SCOPES,
    timeout_secs: float = 90.0,
    headless: bool = True,
) -> dict[str, Any]:
    """
    Camoufox-driven PKCE: IdP authorize is a JS SPA, curl alone stays on HTML 200.
    Inject cookies → open authorize → catch loopback ?code= → token exchange.
    """
    from camoufox.sync_api import Camoufox

    conf = discover(issuer)
    authorize_endpoint = conf["authorization_endpoint"]
    token_url = conf["token_endpoint"]
    verifier, challenge = _pkce_pair()
    state = secrets.token_urlsafe(24)
    redirect_uri = "http://127.0.0.1:19787/callback"

    params = {
        "response_type": "code",
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "scope": scopes,
        "state": state,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
    }
    auth_url = authorize_endpoint + "?" + urllib.parse.urlencode(params)

    cookie_list = list(cookies or [])
    if session is not None:
        cookie_list.extend(_cookies_from_session(session))

    captured: dict[str, str | None] = {"code": None, "error": None}

    log.info("OIDC browser PKCE cookies=%d headless=%s", len(cookie_list), headless)

    with Camoufox(headless=headless) as browser:
        context = browser.new_context()
        if cookie_list:
            try:
                context.add_cookies(cookie_list)
            except Exception as e:
                log.warning("add_cookies partial fail: %s", e)
                # try one-by-one
                for c in cookie_list:
                    try:
                        context.add_cookies([c])
                    except Exception:
                        pass
        page = context.new_page()

        def on_frame_nav(frame: Any) -> None:
            try:
                u = frame.url
            except Exception:
                return
            if "127.0.0.1" in u or "localhost" in u:
                c, err, st = _extract_oauth_code_from_location(u)
                if err:
                    captured["error"] = err
                if c:
                    captured["code"] = c

        page.on("framenavigated", on_frame_nav)

        # also intercept requests to loopback
        def on_request(req: Any) -> None:
            u = req.url
            if "127.0.0.1" in u or "localhost" in u:
                c, err, st = _extract_oauth_code_from_location(u)
                if err:
                    captured["error"] = err
                if c:
                    captured["code"] = c

        page.on("request", on_request)

        try:
            page.goto(auth_url, wait_until="domcontentloaded", timeout=int(timeout_secs * 1000))
        except Exception as e:
            # navigation to 127.0.0.1 often throws ERR_CONNECTION_REFUSED — OK if code captured
            log.info("goto note: %s", e)

        deadline = time.time() + timeout_secs
        while time.time() < deadline and not captured["code"] and not captured["error"]:
            # auto-click common Allow / Continue buttons if consent UI shows
            for sel in (
                'button:has-text("Allow")',
                'button:has-text("Authorize")',
                'button:has-text("Continue")',
                'button:has-text("Accept")',
                'button:has-text("同意")',
                'button:has-text("允许")',
                '[type="submit"]',
            ):
                try:
                    loc = page.locator(sel)
                    if loc.count() > 0 and loc.first.is_visible():
                        loc.first.click(timeout=2000)
                        log.info("clicked consent control %s", sel)
                        time.sleep(1.0)
                        break
                except Exception:
                    pass
            try:
                u = page.url
                c, err, st = _extract_oauth_code_from_location(u)
                if err:
                    captured["error"] = err
                if c:
                    captured["code"] = c
            except Exception:
                pass
            time.sleep(0.5)

        try:
            context.close()
        except Exception:
            pass

    if captured["error"]:
        raise RuntimeError(f"OIDC browser error: {captured['error']}")
    if not captured["code"]:
        raise RuntimeError(
            "OIDC browser PKCE got no code "
            "(login/consent still required or cookies not accepted)"
        )

    with httpx.Client(timeout=30.0) as client:
        tr = client.post(
            token_url,
            data={
                "grant_type": "authorization_code",
                "code": captured["code"],
                "redirect_uri": redirect_uri,
                "client_id": client_id,
                "code_verifier": verifier,
            },
        )
        if tr.status_code >= 400:
            raise RuntimeError(
                f"token exchange failed ({tr.status_code}): {tr.text[:500]}"
            )
        tok = tr.json()
    access = tok.get("access_token")
    if not access:
        raise RuntimeError("token response missing access_token")
    return {
        "access_token": access,
        "refresh_token": tok.get("refresh_token"),
        "expires_in": int(tok.get("expires_in") or 21600),
        "id_token": tok.get("id_token"),
        "token_type": tok.get("token_type"),
        "oidc_issuer": issuer.rstrip("/"),
        "oidc_client_id": client_id,
        "scope": tok.get("scope"),
    }


def mint_cli_from_session(
    session: Any,
    *,
    email: str = "",
    password: str = "",
    note: str = "session+PKCE",
    use_browser: bool = True,
    prefer_browser: bool = True,
) -> dict[str, Any]:
    """Mint CLI OIDC. Prefer Camoufox PKCE (IdP is JS SPA); curl walk is fallback."""
    last_err: Exception | None = None
    scope_sets = [
        DEFAULT_SCOPES,
        "openid offline_access grok-cli:access api:access",
        "openid offline_access grok-cli:access",
    ]

    def try_browser() -> dict[str, Any] | None:
        nonlocal last_err
        if not use_browser:
            return None
        for scopes in scope_sets:
            try:
                log.info("browser PKCE scopes=%r", scopes[:48])
                tokens = mint_cli_tokens_with_browser(
                    session=session, scopes=scopes, headless=True
                )
                return _save_tokens(
                    tokens,
                    email=email,
                    password=password,
                    note=f"{note} browser scopes={scopes[:40]}",
                )
            except Exception as e:
                last_err = e
                log.info("browser mint failed: %s", e)
        return None

    def try_curl() -> dict[str, Any] | None:
        nonlocal last_err
        prompts: list[str | None] = ["none", None]
        for scopes in scope_sets[:1]:  # one scope set for speed
            for prompt in prompts:
                try:
                    tokens = mint_cli_tokens_with_session(
                        session, prompt=prompt, scopes=scopes
                    )
                    return _save_tokens(
                        tokens,
                        email=email,
                        password=password,
                        note=f"{note} curl prompt={prompt!r}",
                    )
                except Exception as e:
                    last_err = e
                    log.info("curl mint prompt=%r failed: %s", prompt, e)
        return None

    order = (try_browser, try_curl) if prefer_browser else (try_curl, try_browser)
    for fn in order:
        out = fn()
        if out:
            return out
    raise RuntimeError(f"CLI OIDC mint failed: {last_err}")


def mint_cli_from_password(
    email: str,
    password: str,
    turnstile_token: str,
    *,
    proxy: str | None = None,
    impersonate: str = "chrome136",
    session: Any | None = None,
    cfg: Settings | None = None,
    save_to_pool: bool = True,
    note: str = "createSession+PKCE",
) -> dict[str, Any]:
    session = create_session_login(
        email,
        password,
        turnstile_token,
        proxy=proxy,
        impersonate=impersonate,
        session=session,
    )
    result = mint_cli_from_session(
        session, email=email, password=password, note=note
    )
    if not save_to_pool:
        # already saved; for API parity strip if needed — keep saved
        pass
    return result


def mint_cli_after_register(
    *,
    email: str,
    password: str,
    registrar_session: Any | None = None,
    turnstile_solver: Callable[[], str] | None = None,
    turnstile_token: str | None = None,
    proxy: str | None = None,
    impersonate: str = "chrome136",
) -> dict[str, Any]:
    """
    Full post-register mint. Prefer existing registrar session first
    (already completed SSO cookie chain).
    """
    errors: list[str] = []

    # Path A: reuse register session (best)
    if registrar_session is not None:
        try:
            log.info("mint path A: registrar session PKCE")
            return mint_cli_from_session(
                registrar_session,
                email=email,
                password=password,
                note="register-session+PKCE",
            )
        except Exception as e:
            errors.append(f"pathA: {e}")
            log.warning("path A failed: %s", e)

    # Path B: createSession + PKCE (needs turnstile)
    ts = (turnstile_token or "").strip()
    if not ts and turnstile_solver:
        try:
            ts = turnstile_solver()
        except Exception as e:
            errors.append(f"turnstile: {e}")
            ts = ""
    if not ts:
        raise RuntimeError(
            "CLI mint failed after register. "
            + " | ".join(errors)
            + " | need turnstile for createSession fallback"
        )

    try:
        log.info("mint path B: createSession + PKCE")
        return mint_cli_from_password(
            email,
            password,
            ts,
            proxy=proxy,
            impersonate=impersonate,
            session=registrar_session,
            note="register-createSession+PKCE",
        )
    except Exception as e:
        errors.append(f"pathB: {e}")
        raise RuntimeError("CLI mint failed: " + " | ".join(errors)) from e


