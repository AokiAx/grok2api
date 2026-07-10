from __future__ import annotations

import hashlib
import time
from dataclasses import dataclass, field, replace
from datetime import UTC, datetime
from typing import Any


def parse_expires_at(value: Any) -> float | None:
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
            fraction: list[str] = []
            timezone_suffix = ""
            for index, character in enumerate(rest):
                if character.isdigit():
                    fraction.append(character)
                else:
                    timezone_suffix = rest[index:]
                    break
            raw = f"{head}.{(''.join(fraction) + '000000')[:6]}{timezone_suffix}"
        parsed = datetime.fromisoformat(raw)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=UTC)
    return parsed.timestamp()


def iso_z(timestamp: float | None = None) -> str:
    value = time.time() if timestamp is None else timestamp
    return (
        datetime.fromtimestamp(value, tz=UTC)
        .isoformat()
        .replace("+00:00", "Z")
    )


def token_fingerprint(token: str) -> str:
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


def account_identity(
    *,
    access_token: str,
    issuer: str,
    email: str = "",
    user_id: str = "",
) -> str:
    normalized_issuer = issuer.rstrip("/").lower()
    if user_id.strip():
        return f"sub:{normalized_issuer}:{user_id.strip()}"
    if email.strip():
        return f"email:{normalized_issuer}:{email.strip().lower()}"
    return f"token:{token_fingerprint(access_token)}"


@dataclass(slots=True)
class CliAccount:
    id: str
    key: str
    refresh_token: str | None = None
    expires_at: str | None = None
    oidc_issuer: str = "https://auth.x.ai"
    oidc_client_id: str = "b1a00492-073a-47ea-816f-4c329264a828"
    email: str = ""
    user_id: str = ""
    team_id: str = ""
    enabled: bool = True
    request_count: int = 0
    fail_count: int = 0
    consecutive_failures: int = 0
    last_used_at: float | None = None
    cooldown_until: float | None = None
    created_at: float = field(default_factory=time.time)
    updated_at: float = field(default_factory=time.time)
    note: str = ""
    priority: int = 0
    disabled_reason: str = ""
    identity_key: str = ""

    def __post_init__(self) -> None:
        self.email = self.email.strip().lower()
        self.oidc_issuer = self.oidc_issuer.rstrip("/")
        if not self.identity_key:
            self.identity_key = account_identity(
                access_token=self.key,
                issuer=self.oidc_issuer,
                email=self.email,
                user_id=self.user_id,
            )

    def expires_ts(self) -> float | None:
        return parse_expires_at(self.expires_at)

    def seconds_left(self) -> int | None:
        expires = self.expires_ts()
        return None if expires is None else int(expires - time.time())

    def copy(self, **changes: Any) -> CliAccount:
        return replace(self, **changes)

    def to_public(self, *, inflight: int = 0, max_concurrent: int = 1) -> dict[str, Any]:
        key = self.key or ""
        masked = (key[:10] + "…" + key[-6:]) if len(key) > 20 else "***"
        now = time.time()
        if not self.enabled:
            status = "disabled"
        elif self.cooldown_until is not None and self.cooldown_until > now:
            status = "cooldown"
        else:
            status = "ready"
        return {
            "id": self.id,
            "email": self.email,
            "key_preview": masked,
            "has_refresh_token": bool(self.refresh_token),
            "expires_at": self.expires_at,
            "seconds_left": self.seconds_left(),
            "enabled": self.enabled,
            "status": status,
            "request_count": self.request_count,
            "fail_count": self.fail_count,
            "consecutive_failures": self.consecutive_failures,
            "cooldown_until": self.cooldown_until,
            "inflight": inflight,
            "max_concurrent": max_concurrent,
            "user_id": self.user_id,
            "note": self.note,
            "priority": self.priority,
            "disabled_reason": self.disabled_reason,
        }

    def to_auth_json_entry(self) -> dict[str, Any]:
        entry: dict[str, Any] = {
            "key": self.key,
            "auth_mode": "oidc",
            "oidc_issuer": self.oidc_issuer,
            "oidc_client_id": self.oidc_client_id,
            "expires_at": self.expires_at,
        }
        if self.refresh_token:
            entry["refresh_token"] = self.refresh_token
        if self.email:
            entry["email"] = self.email
        if self.user_id:
            entry.update(
                user_id=self.user_id,
                principal_id=self.user_id,
                principal_type="User",
            )
        if self.team_id:
            entry["team_id"] = self.team_id
        return entry
