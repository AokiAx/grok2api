"""
xAI Grok CLI-compatible OAuth login.

Flow (same family as `grok login --oauth`):
  1) Prefer silent refresh_token if auth.json is present
  2) Else Authorization Code + PKCE on loopback http://127.0.0.1:<port>/callback
  3) Browser already signed in to xAI → often just redirect, no password
  4) Write ~/.grok/auth.json in CLI-compatible shape

This is for *your* account on *your* machine — not multi-account farming.
"""

from __future__ import annotations

import base64
import hashlib
import json
import logging
import secrets
import threading
import time
import urllib.parse
import webbrowser
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

import httpx

from .config import Settings, settings

log = logging.getLogger("grok2api.oauth")

DEFAULT_ISSUER = "https://auth.x.ai"
# Public CLI client id observed from local grok login sessions
DEFAULT_CLIENT_ID = "b1a00492-073a-47ea-816f-4c329264a828"
DEFAULT_SCOPES = (
    "openid profile email offline_access "
    "grok-cli:access api:access conversations:read conversations:write"
)


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def _pkce_pair() -> tuple[str, str]:
    verifier = _b64url(secrets.token_bytes(32))
    challenge = _b64url(hashlib.sha256(verifier.encode("ascii")).digest())
    return verifier, challenge


def _utcnow() -> datetime:
    return datetime.now(timezone.utc)


def _iso_z(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def _parse_expires_ts(value: Any) -> float | None:
    if not value or not isinstance(value, str):
        return None
    raw = value.strip()
    if raw.endswith("Z"):
        raw = raw[:-1] + "+00:00"
    try:
        if "." in raw:
            head, rest = raw.split(".", 1)
            frac = []
            tz = ""
            for i, ch in enumerate(rest):
                if ch.isdigit():
                    frac.append(ch)
                else:
                    tz = rest[i:]
                    break
            frac_s = ("".join(frac) + "000000")[:6]
            raw = f"{head}.{frac_s}{tz}"
        dt = datetime.fromisoformat(raw)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except ValueError:
        return None


def _auth_path(cfg: Settings) -> Path:
    return cfg.resolved_auth_file


def _scope_key(issuer: str, client_id: str) -> str:
    return f"{issuer.rstrip('/')}::{client_id}"


def discover(issuer: str) -> dict[str, Any]:
    url = issuer.rstrip("/") + "/.well-known/openid-configuration"
    with httpx.Client(timeout=20.0) as client:
        r = client.get(url)
        r.raise_for_status()
        return r.json()


def _load_auth_file(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except json.JSONDecodeError:
        return {}


def _write_auth_file(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(
        json.dumps(data, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    tmp.replace(path)


def _merge_entry(path: Path, scope: str, entry: dict[str, Any]) -> None:
    data = _load_auth_file(path)
    prev = data.get(scope) if isinstance(data.get(scope), dict) else {}
    merged = {**prev, **entry}
    data[scope] = merged
    _write_auth_file(path, data)


def try_refresh(cfg: Settings | None = None) -> dict[str, Any] | None:
    """Return status dict if refresh succeeded or token still fresh; else None."""
    cfg = cfg or settings
    path = _auth_path(cfg)
    data = _load_auth_file(path)
    if not data:
        return None

    now = time.time()
    best: tuple[float, str, dict[str, Any]] | None = None
    for scope, entry in data.items():
        if not isinstance(entry, dict) or not entry.get("key"):
            continue
        exp_ts = _parse_expires_ts(entry.get("expires_at"))
        score = exp_ts if exp_ts is not None else 0.0
        if best is None or score > best[0]:
            best = (score, scope, entry)

    if best is None:
        return None

    _, scope, entry = best
    exp_ts = _parse_expires_ts(entry.get("expires_at"))
    skew = cfg.refresh_skew_secs

    if exp_ts is not None and exp_ts > now + skew:
        return {
            "ok": True,
            "method": "cached",
            "scope": scope,
            "seconds_left": int(exp_ts - now),
            "path": str(path),
        }

    refresh = entry.get("refresh_token")
    issuer = entry.get("oidc_issuer") or DEFAULT_ISSUER
    client_id = entry.get("oidc_client_id") or DEFAULT_CLIENT_ID
    if not refresh:
        return None

    conf = discover(issuer)
    token_url = conf["token_endpoint"]
    with httpx.Client(timeout=30.0) as client:
        r = client.post(
            token_url,
            data={
                "grant_type": "refresh_token",
                "refresh_token": refresh,
                "client_id": client_id,
            },
        )
        if r.status_code >= 400:
            log.warning("refresh failed: %s %s", r.status_code, r.text[:200])
            return None
        tok = r.json()

    access = tok.get("access_token")
    if not access:
        return None
    new_refresh = tok.get("refresh_token") or refresh
    expires_in = int(tok.get("expires_in") or 21600)
    exp_at = _iso_z(
        datetime.fromtimestamp(time.time() + expires_in, tz=timezone.utc)
    )

    _merge_entry(
        path,
        scope,
        {
            "key": access,
            "refresh_token": new_refresh,
            "expires_at": exp_at,
            "auth_mode": "oidc",
            "oidc_issuer": issuer,
            "oidc_client_id": client_id,
        },
    )
    return {
        "ok": True,
        "method": "refresh",
        "scope": scope,
        "seconds_left": expires_in,
        "path": str(path),
    }


def _success_html() -> bytes:
    return b"""<!doctype html>
<html><head><meta charset="utf-8"><title>Grok login OK</title>
<style>
body{font-family:system-ui,sans-serif;display:flex;min-height:100vh;
align-items:center;justify-content:center;background:#0b0f14;color:#e7ecf3}
.card{padding:2rem 2.5rem;border-radius:12px;background:#151b24;border:1px solid #2a3340}
h1{margin:0 0 .5rem;font-size:1.25rem}p{margin:0;opacity:.8}
</style></head>
<body><div class="card">
<h1>Login successful</h1>
<p>You can close this window and return to the terminal.</p>
</div></body></html>"""


def _save_token_response(
    *,
    path: Path,
    conf: dict[str, Any],
    tok: dict[str, Any],
    issuer: str,
    client_id: str,
    method: str,
    redirect_uri: str | None = None,
) -> dict[str, Any]:
    access = tok.get("access_token")
    if not access:
        raise RuntimeError("token response missing access_token")
    refresh = tok.get("refresh_token")
    expires_in = int(tok.get("expires_in") or 21600)
    now = _utcnow()
    entry: dict[str, Any] = {
        "key": access,
        "auth_mode": "oidc",
        "oidc_issuer": issuer.rstrip("/"),
        "oidc_client_id": client_id,
        "expires_at": _iso_z(
            datetime.fromtimestamp(time.time() + expires_in, tz=timezone.utc)
        ),
        "create_time": _iso_z(now),
        "coding_data_retention_opt_out": False,
    }
    if refresh:
        entry["refresh_token"] = refresh

    try:
        userinfo_url = conf.get("userinfo_endpoint")
        if userinfo_url:
            with httpx.Client(timeout=15.0) as client:
                ur = client.get(
                    userinfo_url,
                    headers={"Authorization": f"Bearer {access}"},
                )
                if ur.status_code < 400:
                    info = ur.json()
                    if info.get("sub"):
                        entry["user_id"] = info["sub"]
                        entry["principal_id"] = info["sub"]
                        entry["principal_type"] = "User"
                    if info.get("email"):
                        entry["email"] = info["email"]
                    if info.get("name"):
                        entry["first_name"] = str(info["name"]).split(" ")[0]
    except Exception:
        log.debug("userinfo skipped", exc_info=True)

    scope = _scope_key(issuer, client_id)
    _merge_entry(path, scope, entry)
    out: dict[str, Any] = {
        "ok": True,
        "method": method,
        "scope": scope,
        "path": str(path),
        "seconds_left": expires_in,
    }
    if redirect_uri:
        out["redirect_uri"] = redirect_uri
    return out


def _run_pkce_flow(
    cfg: Settings,
    *,
    prompt: str | None,
    open_browser: bool,
    timeout_secs: float,
    client_id: str,
    issuer: str,
    scopes: str,
    method_label: str,
    soft_fail: bool,
) -> dict[str, Any] | None:
    """
    Loopback Authorization Code + PKCE.

    prompt=None  → normal (may show consent if IdP requires)
    prompt=none  → silent SSO: no UI if browser session + prior consent exist;
                   otherwise IdP redirects error=login_required / consent_required
    """
    path = _auth_path(cfg)
    conf = discover(issuer)
    authorize_url = conf["authorization_endpoint"]
    token_url = conf["token_endpoint"]

    verifier, challenge = _pkce_pair()
    state = secrets.token_urlsafe(24)
    result: dict[str, Any] = {"error": None, "code": None}

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, fmt: str, *args: Any) -> None:
            return

        def do_GET(self) -> None:  # noqa: N802
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path.rstrip("/") not in ("/callback", ""):
                self.send_response(404)
                self.end_headers()
                return
            qs = urllib.parse.parse_qs(parsed.query)
            if qs.get("error"):
                result["error"] = qs.get("error", ["unknown"])[0]
                if qs.get("error_description"):
                    result["error"] += ": " + qs["error_description"][0]
            else:
                if qs.get("state", [None])[0] != state:
                    result["error"] = "state mismatch"
                else:
                    result["code"] = qs.get("code", [None])[0]
            ok = not result["error"] and bool(result["code"])
            body = _success_html() if ok else (
                f"<h1>Login failed</h1><p>{result.get('error') or 'no code'}</p>".encode()
            )
            self.send_response(200 if ok else 400)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    httpd = HTTPServer(("127.0.0.1", 0), Handler)
    port = httpd.server_address[1]
    redirect_uri = f"http://127.0.0.1:{port}/callback"

    params: dict[str, str] = {
        "response_type": "code",
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "scope": scopes,
        "state": state,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
    }
    if prompt:
        params["prompt"] = prompt
    url = authorize_url + "?" + urllib.parse.urlencode(params)

    thread = threading.Thread(target=httpd.handle_request, daemon=True)
    thread.start()

    log.info("OAuth callback %s prompt=%s", redirect_uri, prompt)
    if prompt == "none":
        print("[grok2api] Trying silent authorize (prompt=none)…")
    else:
        print(f"[grok2api] Open this URL if the browser does not open:\n{url}\n")
    if open_browser:
        webbrowser.open(url)

    thread.join(timeout=timeout_secs)
    try:
        httpd.server_close()
    except Exception:
        pass

    if result["error"] or not result["code"]:
        err = result.get("error") or "timeout / no code"
        if soft_fail:
            log.info("PKCE soft-fail (%s): %s", method_label, err)
            return None
        raise RuntimeError(f"OAuth error: {err}")

    with httpx.Client(timeout=30.0) as client:
        r = client.post(
            token_url,
            data={
                "grant_type": "authorization_code",
                "code": result["code"],
                "redirect_uri": redirect_uri,
                "client_id": client_id,
                "code_verifier": verifier,
            },
        )
        if r.status_code >= 400:
            if soft_fail:
                log.info("token exchange soft-fail: %s", r.text[:200])
                return None
            raise RuntimeError(
                f"token exchange failed ({r.status_code}): {r.text[:400]}"
            )
        tok = r.json()

    return _save_token_response(
        path=path,
        conf=conf,
        tok=tok,
        issuer=issuer,
        client_id=client_id,
        method=method_label,
        redirect_uri=redirect_uri,
    )


def browser_login(
    cfg: Settings | None = None,
    *,
    open_browser: bool = True,
    timeout_secs: float = 300.0,
    client_id: str = DEFAULT_CLIENT_ID,
    issuer: str = DEFAULT_ISSUER,
    scopes: str = DEFAULT_SCOPES,
    silent: bool = False,
) -> dict[str, Any]:
    """
    Run loopback OAuth (PKCE).

    silent=True uses prompt=none: if the browser already has an xAI session
    *and* you previously approved the CLI client, no consent click is needed.
    First-ever Approve still requires a normal interactive run once.
    """
    cfg = cfg or settings
    if silent:
        out = _run_pkce_flow(
            cfg,
            prompt="none",
            open_browser=open_browser,
            timeout_secs=min(timeout_secs, 45.0),
            client_id=client_id,
            issuer=issuer,
            scopes=scopes,
            method_label="silent_sso",
            soft_fail=True,
        )
        if out is None:
            raise RuntimeError(
                "silent authorize failed "
                "(no browser SSO session or consent not granted yet)"
            )
        return out

    out = _run_pkce_flow(
        cfg,
        prompt=None,
        open_browser=open_browser,
        timeout_secs=timeout_secs,
        client_id=client_id,
        issuer=issuer,
        scopes=scopes,
        method_label="browser_pkce",
        soft_fail=False,
    )
    assert out is not None
    return out


def device_login(
    cfg: Settings | None = None,
    *,
    client_id: str = DEFAULT_CLIENT_ID,
    issuer: str = DEFAULT_ISSUER,
    scopes: str = DEFAULT_SCOPES,
    poll_timeout_secs: float = 300.0,
) -> dict[str, Any]:
    """Device-code flow (like `grok login --device-auth`)."""
    cfg = cfg or settings
    path = _auth_path(cfg)
    conf = discover(issuer)
    device_url = conf.get("device_authorization_endpoint")
    token_url = conf["token_endpoint"]
    if not device_url:
        raise RuntimeError("issuer has no device_authorization_endpoint")

    with httpx.Client(timeout=30.0) as client:
        r = client.post(
            device_url,
            data={"client_id": client_id, "scope": scopes},
        )
        r.raise_for_status()
        dev = r.json()

    device_code = dev["device_code"]
    user_code = dev["user_code"]
    verify_uri = dev.get("verification_uri_complete") or dev.get("verification_uri")
    interval = int(dev.get("interval") or 5)
    expires_in_dev = int(dev.get("expires_in") or 600)

    print(f"[grok2api] Device login")
    print(f"  Open: {verify_uri}")
    print(f"  Code: {user_code}")
    if open_url := dev.get("verification_uri_complete"):
        try:
            webbrowser.open(open_url)
        except Exception:
            pass

    deadline = time.time() + min(poll_timeout_secs, expires_in_dev)
    tok: dict[str, Any] | None = None
    with httpx.Client(timeout=30.0) as client:
        while time.time() < deadline:
            time.sleep(interval)
            tr = client.post(
                token_url,
                data={
                    "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
                    "device_code": device_code,
                    "client_id": client_id,
                },
            )
            if tr.status_code < 400:
                tok = tr.json()
                break
            err = {}
            try:
                err = tr.json()
            except Exception:
                pass
            et = err.get("error")
            if et in ("authorization_pending", "slow_down"):
                if et == "slow_down":
                    interval += 5
                continue
            raise RuntimeError(f"device login failed: {tr.text[:300]}")

    if not tok or not tok.get("access_token"):
        raise TimeoutError("device login timed out")

    access = tok["access_token"]
    refresh = tok.get("refresh_token")
    expires_in = int(tok.get("expires_in") or 21600)
    now = _utcnow()
    entry: dict[str, Any] = {
        "key": access,
        "auth_mode": "oidc",
        "oidc_issuer": issuer.rstrip("/"),
        "oidc_client_id": client_id,
        "expires_at": _iso_z(
            datetime.fromtimestamp(time.time() + expires_in, tz=timezone.utc)
        ),
        "create_time": _iso_z(now),
    }
    if refresh:
        entry["refresh_token"] = refresh

    scope = _scope_key(issuer, client_id)
    _merge_entry(path, scope, entry)
    return {
        "ok": True,
        "method": "device_code",
        "scope": scope,
        "path": str(path),
        "seconds_left": expires_in,
    }


def login_via_cli(extra_args: list[str] | None = None) -> dict[str, Any]:
    """Delegate to installed `grok login` (most compatible)."""
    import shutil
    import subprocess

    grok = shutil.which("grok")
    if not grok:
        # common install path on this machine
        candidate = Path.home() / ".grok" / "bin" / "grok.exe"
        if candidate.exists():
            grok = str(candidate)
    if not grok:
        raise FileNotFoundError("grok CLI not found on PATH")

    args = [grok, "login", "--oauth"]
    if extra_args:
        args.extend(extra_args)
    print(f"[grok2api] running: {' '.join(args)}")
    proc = subprocess.run(args, check=False)
    if proc.returncode != 0:
        raise RuntimeError(f"grok login exited {proc.returncode}")
    return {"ok": True, "method": "grok_cli", "path": str(settings.resolved_auth_file)}


def ensure_login(
    cfg: Settings | None = None,
    *,
    force_browser: bool = False,
    method: str = "auto",
    # auto | refresh | silent | browser | device | cli
) -> dict[str, Any]:
    """
    Ensure a usable auth.json exists.

    auto pipeline (no manual "Agree" when possible):
      1) valid cached access token
      2) refresh_token  → fully silent, no browser
      3) prompt=none SSO → browser may flash open; no Agree if already consented
      4) interactive browser → only if 1–3 fail (first install / new scopes)

    The first-ever "Allow Grok CLI" consent cannot be skipped by design of OAuth;
    after that + offline_access, day-to-day use stays on step 1–2.
    """
    cfg = cfg or settings
    method = method.lower().strip()

    if method in ("auto", "refresh", "silent") and not force_browser:
        refreshed = try_refresh(cfg)
        if refreshed and refreshed.get("ok"):
            print(
                f"[grok2api] auth ok via {refreshed['method']} "
                f"(~{refreshed.get('seconds_left')}s left) — no consent needed"
            )
            return refreshed
        if method == "refresh":
            raise RuntimeError("refresh failed and method=refresh only")

    if method == "device":
        return device_login(cfg)

    if method == "cli":
        return login_via_cli()

    # Silent SSO (automates "agree" when prior consent + browser session exist)
    if method in ("auto", "silent") and not force_browser:
        try:
            silent = browser_login(cfg, open_browser=True, silent=True)
            print(
                "[grok2api] auth ok via silent SSO "
                f"(~{silent.get('seconds_left')}s) — no consent click"
            )
            return silent
        except Exception as e:
            log.info("silent SSO unavailable: %s", e)
            if method == "silent":
                raise
            print(
                "[grok2api] Silent authorize not possible "
                "(need one interactive login if never approved CLI, "
                "or refresh_token is gone)."
            )

    print(
        "[grok2api] Interactive browser OAuth. "
        "If you already approved Grok CLI once, IdP often skips the Agree page "
        "and only redirects. First-time users must click Allow once."
    )
    return browser_login(cfg, open_browser=True, silent=False)
