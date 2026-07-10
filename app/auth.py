from __future__ import annotations

import json
import logging
import threading
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import httpx

from .config import Settings, settings

log = logging.getLogger("grok2api.auth")


def _parse_expires_at(value: str | None) -> float | None:
    if not value:
        return None
    raw = value.strip()
    # Handle trailing fractional seconds / Z
    if raw.endswith("Z"):
        raw = raw[:-1] + "+00:00"
    try:
        # Python fromisoformat does not always like >6 digit fractions
        if "." in raw:
            head, rest = raw.split(".", 1)
            frac = ""
            tz = ""
            for i, ch in enumerate(rest):
                if ch.isdigit():
                    frac += ch
                else:
                    tz = rest[i:]
                    break
            frac = (frac + "000000")[:6]
            raw = f"{head}.{frac}{tz}"
        dt = datetime.fromisoformat(raw)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except ValueError:
        log.warning("unparseable expires_at: %s", value)
        return None


@dataclass
class Credential:
    scope: str
    key: str
    refresh_token: str | None
    expires_at: float | None
    oidc_issuer: str | None
    oidc_client_id: str | None
    raw: dict[str, Any]


class AuthStore:
    """Load/refresh session tokens from ~/.grok/auth.json."""

    def __init__(self, cfg: Settings | None = None) -> None:
        self.cfg = cfg or settings
        self._lock = threading.RLock()
        self._cred: Credential | None = None
        self._loaded_mtime: float | None = None

    @property
    def path(self) -> Path:
        return self.cfg.resolved_auth_file

    def _read_file(self) -> dict[str, Any]:
        path = self.path
        if not path.exists():
            raise FileNotFoundError(
                f"auth file not found: {path}. Run `grok login` first."
            )
        with path.open("r", encoding="utf-8") as f:
            data = json.load(f)
        if not isinstance(data, dict) or not data:
            raise ValueError(f"invalid auth.json: {path}")
        return data

    def _pick_entry(self, data: dict[str, Any]) -> Credential:
        # Prefer entries that look like CLI/OIDC session tokens
        candidates: list[Credential] = []
        for scope, entry in data.items():
            if not isinstance(entry, dict):
                continue
            key = entry.get("key") or entry.get("access_token")
            if not key or not isinstance(key, str):
                continue
            candidates.append(
                Credential(
                    scope=scope,
                    key=key,
                    refresh_token=entry.get("refresh_token"),
                    expires_at=_parse_expires_at(entry.get("expires_at")),
                    oidc_issuer=entry.get("oidc_issuer"),
                    oidc_client_id=entry.get("oidc_client_id"),
                    raw=entry,
                )
            )
        if not candidates:
            raise ValueError(
                f"no usable credentials in {self.path}. Run `grok login`."
            )
        # Prefer non-expired, then longest expiry
        now = time.time()

        def score(c: Credential) -> tuple[int, float]:
            alive = 1 if (c.expires_at is None or c.expires_at > now) else 0
            return (alive, c.expires_at or 0.0)

        candidates.sort(key=score, reverse=True)
        return candidates[0]

    def _load(self, force: bool = False) -> Credential:
        path = self.path
        mtime = path.stat().st_mtime
        if (
            not force
            and self._cred is not None
            and self._loaded_mtime == mtime
        ):
            return self._cred
        data = self._read_file()
        self._cred = self._pick_entry(data)
        self._loaded_mtime = mtime
        return self._cred

    def _persist_update(
        self,
        scope: str,
        *,
        key: str,
        refresh_token: str | None,
        expires_in: int | None,
    ) -> None:
        path = self.path
        data = self._read_file()
        entry = data.get(scope)
        if not isinstance(entry, dict):
            entry = {}
            data[scope] = entry
        entry["key"] = key
        if refresh_token:
            entry["refresh_token"] = refresh_token
        if expires_in is not None:
            exp = datetime.now(timezone.utc).timestamp() + int(expires_in)
            entry["expires_at"] = (
                datetime.fromtimestamp(exp, tz=timezone.utc)
                .isoformat()
                .replace("+00:00", "Z")
            )
        # Atomic-ish write
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(
            json.dumps(data, indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
        tmp.replace(path)

    def _discover_token_endpoint(self, issuer: str) -> str:
        url = issuer.rstrip("/") + "/.well-known/openid-configuration"
        with httpx.Client(timeout=20.0) as client:
            r = client.get(url)
            r.raise_for_status()
            conf = r.json()
        endpoint = conf.get("token_endpoint")
        if not endpoint:
            raise RuntimeError(f"no token_endpoint for issuer {issuer}")
        return endpoint

    def _refresh(self, cred: Credential) -> Credential:
        if not cred.refresh_token or not cred.oidc_issuer or not cred.oidc_client_id:
            raise RuntimeError(
                "credential has no OIDC refresh fields; run `grok login`"
            )
        token_url = self._discover_token_endpoint(cred.oidc_issuer)
        form = {
            "grant_type": "refresh_token",
            "refresh_token": cred.refresh_token,
            "client_id": cred.oidc_client_id,
        }
        with httpx.Client(timeout=30.0) as client:
            r = client.post(token_url, data=form)
            if r.status_code >= 400:
                raise RuntimeError(
                    f"OIDC refresh failed ({r.status_code}): {r.text[:400]}"
                )
            payload = r.json()
        access = payload.get("access_token")
        if not access:
            raise RuntimeError("OIDC refresh returned no access_token")
        new_refresh = payload.get("refresh_token") or cred.refresh_token
        expires_in = payload.get("expires_in")
        self._persist_update(
            cred.scope,
            key=access,
            refresh_token=new_refresh,
            expires_in=int(expires_in) if expires_in is not None else None,
        )
        log.info("refreshed session token for scope %s", cred.scope)
        return self._load(force=True)

    def get_access_token(self, force_refresh: bool = False) -> str:
        token, _account_id = self.get_access_token_with_account(
            force_refresh=force_refresh
        )
        return token

    def get_access_token_with_account(
        self,
        force_refresh: bool = False,
        exclude_ids: set[str] | None = None,
    ) -> tuple[str, str | None]:
        """Return ``(access_token, cli_account_id|None)`` for pool-aware retries."""
        # Prefer multi-account CLI pool when enabled and non-empty
        if getattr(self.cfg, "cli_pool_rotate", True):
            from .cli_pool import cli_pool

            if cli_pool.count(enabled_only=False) > 0:
                return self._token_from_cli_pool(
                    force_refresh=force_refresh,
                    exclude_ids=exclude_ids,
                )

        with self._lock:
            cred = self._load(force=True)
            now = time.time()
            need = force_refresh
            if not need and cred.expires_at is not None:
                need = cred.expires_at <= now + self.cfg.refresh_skew_secs
            if need and cred.refresh_token:
                try:
                    cred = self._refresh(cred)
                except Exception:
                    # If still valid, keep going; else re-raise
                    if cred.expires_at is not None and cred.expires_at <= now:
                        raise
                    log.exception("token refresh failed; using existing token")
            return cred.key, None

    def _token_from_cli_pool(
        self,
        force_refresh: bool = False,
        exclude_ids: set[str] | None = None,
    ) -> tuple[str, str]:
        from .cli_pool import cli_pool

        acc = cli_pool.acquire(wait=True, exclude_ids=exclude_ids)
        if acc is None:
            raise RuntimeError(
                "CLI account pool empty or all accounts at concurrency limit / cooldown"
            )
        now = time.time()
        exp = acc.expires_ts()
        need = force_refresh
        if not need and exp is not None:
            need = exp <= now + self.cfg.refresh_skew_secs
        if need and acc.refresh_token:
            try:
                tmp = Credential(
                    scope=acc.id,
                    key=acc.key,
                    refresh_token=acc.refresh_token,
                    expires_at=exp,
                    oidc_issuer=acc.oidc_issuer,
                    oidc_client_id=acc.oidc_client_id,
                    raw=acc.to_auth_json_entry(),
                )
                # inline refresh without rewriting single-file pick
                if not tmp.refresh_token or not tmp.oidc_issuer or not tmp.oidc_client_id:
                    raise RuntimeError("no refresh fields")
                token_url = self._discover_token_endpoint(tmp.oidc_issuer)
                form = {
                    "grant_type": "refresh_token",
                    "refresh_token": tmp.refresh_token,
                    "client_id": tmp.oidc_client_id,
                }
                with httpx.Client(timeout=30.0) as client:
                    r = client.post(token_url, data=form)
                    if r.status_code >= 400:
                        raise RuntimeError(
                            f"OIDC refresh failed ({r.status_code}): {r.text[:400]}"
                        )
                    payload = r.json()
                access = payload.get("access_token")
                if not access:
                    raise RuntimeError("refresh returned no access_token")
                new_refresh = payload.get("refresh_token") or acc.refresh_token
                expires_in = payload.get("expires_in")
                cli_pool.update_tokens(
                    acc.id,
                    key=access,
                    refresh_token=new_refresh,
                    expires_in=int(expires_in) if expires_in is not None else None,
                )
                cli_pool.report_success(acc.id)
                return access, acc.id
            except Exception:
                log.exception("cli pool refresh failed for %s", acc.id)
                if exp is not None and exp <= now:
                    # release slot: caller won't get a usable token
                    cli_pool.report_failure(acc.id, cooldown_secs=60)
                    cli_pool.release(acc.id)
                    raise
                # keep using existing access token; slot still held for caller
        return acc.key, acc.id

    def status(self) -> dict[str, Any]:
        with self._lock:
            try:
                cred = self._load()
            except Exception as e:
                return {"ok": False, "error": str(e), "path": str(self.path)}
            now = time.time()
            return {
                "ok": True,
                "path": str(self.path),
                "scope": cred.scope,
                "expires_at": cred.raw.get("expires_at"),
                "seconds_left": (
                    None
                    if cred.expires_at is None
                    else int(cred.expires_at - now)
                ),
                "has_refresh_token": bool(cred.refresh_token),
                "oidc_issuer": cred.oidc_issuer,
                "user_id": cred.raw.get("user_id"),
                "team_id": cred.raw.get("team_id"),
            }


auth_store = AuthStore()
