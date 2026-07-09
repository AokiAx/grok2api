"""Multi-account CLI OIDC credential pool (cli-chat-proxy tokens)."""

from __future__ import annotations

import json
import logging
import threading
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .config import Settings, settings

log = logging.getLogger("grok2api.cli_pool")


def _iso_z(ts: float | None = None) -> str:
    if ts is None:
        ts = time.time()
    return (
        datetime.fromtimestamp(ts, tz=timezone.utc)
        .isoformat()
        .replace("+00:00", "Z")
    )


def _parse_expires(value: Any) -> float | None:
    if value is None:
        return None
    if isinstance(value, (int, float)):
        return float(value)
    if not isinstance(value, str) or not value.strip():
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
            raw = f"{head}.{(''.join(frac)+'000000')[:6]}{tz}"
        dt = datetime.fromisoformat(raw)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except ValueError:
        return None


@dataclass
class CliAccount:
    """One CLI OIDC session (same fields family as ~/.grok/auth.json entry)."""

    id: str
    key: str
    refresh_token: str | None = None
    expires_at: str | None = None
    oidc_issuer: str = "https://auth.x.ai"
    oidc_client_id: str = "b1a00492-073a-47ea-816f-4c329264a828"
    email: str = ""
    password: str = ""  # optional, for re-mint if refresh dies
    user_id: str = ""
    team_id: str = ""
    enabled: bool = True
    request_count: int = 0
    fail_count: int = 0
    last_used_at: float | None = None
    cooldown_until: float | None = None
    created_at: float = field(default_factory=time.time)
    note: str = ""

    def expires_ts(self) -> float | None:
        return _parse_expires(self.expires_at)

    def seconds_left(self) -> int | None:
        exp = self.expires_ts()
        if exp is None:
            return None
        return int(exp - time.time())

    def to_public(self) -> dict[str, Any]:
        key = self.key or ""
        masked = (key[:10] + "…" + key[-6:]) if len(key) > 20 else "***"
        return {
            "id": self.id,
            "email": self.email,
            "key_preview": masked,
            "has_refresh_token": bool(self.refresh_token),
            "expires_at": self.expires_at,
            "seconds_left": self.seconds_left(),
            "enabled": self.enabled,
            "request_count": self.request_count,
            "fail_count": self.fail_count,
            "cooldown_until": self.cooldown_until,
            "user_id": self.user_id,
            "note": self.note,
        }

    def to_auth_json_entry(self) -> dict[str, Any]:
        entry: dict[str, Any] = {
            "key": self.key,
            "auth_mode": "oidc",
            "oidc_issuer": self.oidc_issuer.rstrip("/"),
            "oidc_client_id": self.oidc_client_id,
            "expires_at": self.expires_at,
        }
        if self.refresh_token:
            entry["refresh_token"] = self.refresh_token
        if self.email:
            entry["email"] = self.email
        if self.user_id:
            entry["user_id"] = self.user_id
            entry["principal_id"] = self.user_id
            entry["principal_type"] = "User"
        if self.team_id:
            entry["team_id"] = self.team_id
        return entry


class CliAccountPool:
    """Thread-safe multi-account CLI OIDC pool + optional sync to auth.json."""

    def __init__(self, cfg: Settings | None = None) -> None:
        self.cfg = cfg or settings
        self._lock = threading.RLock()
        self._accounts: dict[str, CliAccount] = {}
        self._rr = 0
        self._load()

    @property
    def path(self) -> Path:
        return self.cfg.resolved_data_dir / "cli_accounts.json"

    def _load(self) -> None:
        path = self.path
        if not path.exists():
            return
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except Exception as e:
            log.warning("cli pool load failed: %s", e)
            return
        items = data.get("accounts") if isinstance(data, dict) else data
        if not isinstance(items, list):
            return
        for item in items:
            if not isinstance(item, dict) or not item.get("key"):
                continue
            aid = str(item.get("id") or item.get("email") or item["key"][:16])
            self._accounts[aid] = CliAccount(
                id=aid,
                key=str(item["key"]),
                refresh_token=item.get("refresh_token"),
                expires_at=item.get("expires_at"),
                oidc_issuer=str(item.get("oidc_issuer") or "https://auth.x.ai"),
                oidc_client_id=str(
                    item.get("oidc_client_id")
                    or "b1a00492-073a-47ea-816f-4c329264a828"
                ),
                email=str(item.get("email") or ""),
                password=str(item.get("password") or ""),
                user_id=str(item.get("user_id") or ""),
                team_id=str(item.get("team_id") or ""),
                enabled=bool(item.get("enabled", True)),
                request_count=int(item.get("request_count") or 0),
                fail_count=int(item.get("fail_count") or 0),
                last_used_at=item.get("last_used_at"),
                cooldown_until=item.get("cooldown_until"),
                created_at=float(item.get("created_at") or time.time()),
                note=str(item.get("note") or ""),
            )
        log.info("loaded %d CLI accounts", len(self._accounts))

    def reload(self) -> int:
        """Re-read pool file from disk (e.g. after external register process)."""
        with self._lock:
            self._accounts.clear()
            self._load()
            return len(self._accounts)

    def _save(self) -> None:
        path = self.path
        path.parent.mkdir(parents=True, exist_ok=True)
        payload = {
            "updated_at": time.time(),
            "accounts": [asdict(a) for a in self._accounts.values()],
        }
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(
            json.dumps(payload, indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
        tmp.replace(path)

    def count(self, *, enabled_only: bool = True) -> int:
        with self._lock:
            if not enabled_only:
                return len(self._accounts)
            now = time.time()
            return sum(
                1
                for a in self._accounts.values()
                if a.enabled
                and a.key
                and (a.cooldown_until is None or a.cooldown_until <= now)
            )

    def list_public(self) -> list[dict[str, Any]]:
        with self._lock:
            return [a.to_public() for a in self._accounts.values()]

    def upsert_from_tokens(
        self,
        *,
        access_token: str,
        refresh_token: str | None,
        expires_in: int | None,
        email: str = "",
        password: str = "",
        user_id: str = "",
        team_id: str = "",
        oidc_issuer: str = "https://auth.x.ai",
        oidc_client_id: str = "b1a00492-073a-47ea-816f-4c329264a828",
        note: str = "",
        account_id: str | None = None,
    ) -> CliAccount:
        aid = account_id or email or f"cli-{access_token[:12]}"
        exp_at = None
        if expires_in is not None:
            exp_at = _iso_z(time.time() + int(expires_in))
        with self._lock:
            prev = self._accounts.get(aid)
            acc = CliAccount(
                id=aid,
                key=access_token,
                refresh_token=refresh_token
                or (prev.refresh_token if prev else None),
                expires_at=exp_at or (prev.expires_at if prev else None),
                oidc_issuer=oidc_issuer,
                oidc_client_id=oidc_client_id,
                email=email or (prev.email if prev else ""),
                password=password or (prev.password if prev else ""),
                user_id=user_id or (prev.user_id if prev else ""),
                team_id=team_id or (prev.team_id if prev else ""),
                enabled=True,
                request_count=prev.request_count if prev else 0,
                fail_count=0,
                last_used_at=prev.last_used_at if prev else None,
                cooldown_until=None,
                created_at=prev.created_at if prev else time.time(),
                note=note or (prev.note if prev else ""),
            )
            self._accounts[aid] = acc
            self._save()
            if self.cfg.cli_pool_sync_auth_json:
                self._sync_auth_json_unlocked()
            log.info("upserted CLI account id=%s email=%s", aid, acc.email)
            return acc

    def _sync_auth_json_unlocked(self) -> None:
        """Write best active account (+ all as multi-scope) into auth.json."""
        path = self.cfg.resolved_auth_file
        path.parent.mkdir(parents=True, exist_ok=True)
        data: dict[str, Any] = {}
        if path.exists():
            try:
                raw = json.loads(path.read_text(encoding="utf-8"))
                if isinstance(raw, dict):
                    data = raw
            except Exception:
                data = {}
        # Keep non-oidc keys; replace our managed scopes
        for acc in self._accounts.values():
            if not acc.enabled or not acc.key:
                continue
            scope = f"{acc.oidc_issuer.rstrip('/')}::{acc.oidc_client_id}"
            # multi-account: use email suffix in scope when multiple
            if acc.email:
                scope = f"{scope}::{acc.email}"
            data[scope] = acc.to_auth_json_entry()
        # Also write canonical single scope for stock grok CLI (pick best)
        best = self._pick_best_unlocked()
        if best:
            canon = f"{best.oidc_issuer.rstrip('/')}::{best.oidc_client_id}"
            data[canon] = best.to_auth_json_entry()
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(
            json.dumps(data, indent=2, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
        tmp.replace(path)
        log.info("synced CLI pool → %s (%d keys)", path, len(data))

    def _pick_best_unlocked(self) -> CliAccount | None:
        now = time.time()
        usable = [
            a
            for a in self._accounts.values()
            if a.enabled
            and a.key
            and (a.cooldown_until is None or a.cooldown_until <= now)
        ]
        if not usable:
            return None
        usable.sort(
            key=lambda a: (
                a.seconds_left() is not None,
                a.seconds_left() or 0,
                -(a.fail_count),
                -(a.last_used_at or 0),
            ),
            reverse=True,
        )
        return usable[0]

    def acquire(self) -> CliAccount | None:
        with self._lock:
            now = time.time()
            usable = [
                a
                for a in self._accounts.values()
                if a.enabled
                and a.key
                and (a.cooldown_until is None or a.cooldown_until <= now)
            ]
            if not usable:
                return None
            usable.sort(
                key=lambda a: (a.last_used_at or 0.0, a.request_count, a.fail_count)
            )
            pick = usable[0]
            pick.request_count += 1
            pick.last_used_at = now
            self._save()
            return pick

    def report_success(self, account_id: str) -> None:
        with self._lock:
            a = self._accounts.get(account_id)
            if not a:
                return
            a.fail_count = max(0, a.fail_count - 1)
            self._save()

    def report_failure(
        self,
        account_id: str,
        *,
        cooldown_secs: float = 300,
        disable: bool = False,
    ) -> None:
        with self._lock:
            a = self._accounts.get(account_id)
            if not a:
                return
            a.fail_count += 1
            a.cooldown_until = time.time() + cooldown_secs
            if disable or a.fail_count >= self.cfg.sso_max_fails:
                a.enabled = False
            self._save()

    def update_tokens(
        self,
        account_id: str,
        *,
        key: str,
        refresh_token: str | None,
        expires_in: int | None,
    ) -> None:
        with self._lock:
            a = self._accounts.get(account_id)
            if not a:
                return
            a.key = key
            if refresh_token:
                a.refresh_token = refresh_token
            if expires_in is not None:
                a.expires_at = _iso_z(time.time() + int(expires_in))
            a.cooldown_until = None
            a.enabled = True
            self._save()
            if self.cfg.cli_pool_sync_auth_json:
                self._sync_auth_json_unlocked()

    def get(self, account_id: str) -> CliAccount | None:
        with self._lock:
            return self._accounts.get(account_id)

    def delete(self, account_id: str) -> bool:
        with self._lock:
            if account_id not in self._accounts:
                return False
            del self._accounts[account_id]
            self._save()
            return True

    def import_from_auth_json(self) -> int:
        """Import existing ~/.grok/auth.json entries into pool."""
        path = self.cfg.resolved_auth_file
        if not path.exists():
            return 0
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except Exception:
            return 0
        if not isinstance(data, dict):
            return 0
        n = 0
        for scope, entry in data.items():
            if not isinstance(entry, dict) or not entry.get("key"):
                continue
            email = str(entry.get("email") or "")
            aid = email or scope
            self.upsert_from_tokens(
                access_token=str(entry["key"]),
                refresh_token=entry.get("refresh_token"),
                expires_in=None,
                email=email,
                user_id=str(entry.get("user_id") or ""),
                team_id=str(entry.get("team_id") or ""),
                oidc_issuer=str(entry.get("oidc_issuer") or "https://auth.x.ai"),
                oidc_client_id=str(
                    entry.get("oidc_client_id")
                    or "b1a00492-073a-47ea-816f-4c329264a828"
                ),
                note=f"imported from auth.json scope={scope[:40]}",
                account_id=aid,
            )
            # preserve expires_at string
            with self._lock:
                acc = self._accounts.get(aid)
                if acc and entry.get("expires_at"):
                    acc.expires_at = str(entry["expires_at"])
                    self._save()
            n += 1
        return n


cli_pool = CliAccountPool()
