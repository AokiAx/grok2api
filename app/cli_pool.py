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
    """One CLI OIDC session for cli-chat-proxy."""

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

    def to_public(self, *, inflight: int = 0, max_concurrent: int = 1) -> dict[str, Any]:
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
            "inflight": inflight,
            "max_concurrent": max_concurrent,
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
    """Thread-safe multi-account CLI OIDC pool (data/cli_accounts.json only)."""

    def __init__(self, cfg: Settings | None = None) -> None:
        self.cfg = cfg or settings
        self._lock = threading.RLock()
        self._accounts: dict[str, CliAccount] = {}
        self._rr = 0
        # in-flight requests per account (runtime only, not persisted)
        self._inflight: dict[str, int] = {}
        self._slot_cv = threading.Condition(self._lock)
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

    @property
    def max_concurrent(self) -> int:
        return max(1, int(getattr(self.cfg, "cli_pool_max_concurrent", 1) or 1))

    def _inflight_of(self, account_id: str) -> int:
        return int(self._inflight.get(account_id) or 0)

    def _is_usable_unlocked(self, a: CliAccount, *, now: float, need_slot: bool) -> bool:
        if not a.enabled or not a.key:
            return False
        if a.cooldown_until is not None and a.cooldown_until > now:
            return False
        if need_slot and self._inflight_of(a.id) >= self.max_concurrent:
            return False
        return True

    def count(self, *, enabled_only: bool = True) -> int:
        with self._lock:
            if not enabled_only:
                return len(self._accounts)
            now = time.time()
            return sum(
                1
                for a in self._accounts.values()
                if self._is_usable_unlocked(a, now=now, need_slot=False)
            )

    def count_available_slots(self) -> int:
        """Accounts that can accept another concurrent request right now."""
        with self._lock:
            now = time.time()
            return sum(
                1
                for a in self._accounts.values()
                if self._is_usable_unlocked(a, now=now, need_slot=True)
            )

    def list_public(self) -> list[dict[str, Any]]:
        with self._lock:
            mc = self.max_concurrent
            return [
                a.to_public(
                    inflight=self._inflight_of(a.id),
                    max_concurrent=mc,
                )
                for a in self._accounts.values()
            ]

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
            log.info("upserted CLI account id=%s email=%s", aid, acc.email)
            return acc

    def acquire(self, *, wait: bool = True) -> CliAccount | None:
        """Pick an account with free concurrency capacity.

        Preference order among free-slot accounts:
        lowest inflight → least recently used → lowest request_count → lowest fails.

        If all accounts are at ``cli_pool_max_concurrent``, waits up to
        ``cli_pool_acquire_timeout`` seconds for a slot (when wait=True).
        """
        timeout = float(getattr(self.cfg, "cli_pool_acquire_timeout", 60.0) or 0.0)
        deadline = time.time() + max(0.0, timeout) if wait else time.time()

        with self._slot_cv:
            while True:
                now = time.time()
                free = [
                    a
                    for a in self._accounts.values()
                    if self._is_usable_unlocked(a, now=now, need_slot=True)
                ]
                if free:
                    free.sort(
                        key=lambda a: (
                            self._inflight_of(a.id),
                            a.last_used_at or 0.0,
                            a.request_count,
                            a.fail_count,
                        )
                    )
                    pick = free[0]
                    self._inflight[pick.id] = self._inflight_of(pick.id) + 1
                    pick.request_count += 1
                    pick.last_used_at = now
                    # persist counts occasionally — every request is ok for small pools
                    self._save()
                    return pick

                # no free slot: any enabled account at all?
                any_enabled = any(
                    self._is_usable_unlocked(a, now=now, need_slot=False)
                    for a in self._accounts.values()
                )
                if not any_enabled:
                    return None
                if not wait or time.time() >= deadline:
                    return None
                remaining = deadline - time.time()
                self._slot_cv.wait(timeout=min(0.5, max(0.05, remaining)))

    def release(self, account_id: str) -> None:
        """Release one in-flight slot for the account (must pair with acquire)."""
        if not account_id:
            return
        with self._slot_cv:
            cur = self._inflight_of(account_id)
            if cur <= 1:
                self._inflight.pop(account_id, None)
            else:
                self._inflight[account_id] = cur - 1
            self._slot_cv.notify_all()

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
        with self._slot_cv:
            a = self._accounts.get(account_id)
            if not a:
                return
            a.fail_count += 1
            a.cooldown_until = time.time() + cooldown_secs
            if disable or a.fail_count >= self.cfg.sso_max_fails:
                a.enabled = False
            self._save()
            self._slot_cv.notify_all()

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

cli_pool = CliAccountPool()
