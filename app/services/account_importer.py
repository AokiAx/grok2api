from __future__ import annotations

import time
from collections.abc import Iterable
from dataclasses import dataclass, field
from typing import Any, Literal

from app.domain.accounts import CliAccount, account_identity, iso_z, token_fingerprint
from app.infrastructure.account_repository import AccountRepository

ConflictPolicy = Literal["merge", "replace", "skip"]


@dataclass(slots=True)
class ImportItemResult:
    index: int
    status: str
    account_id: str | None = None
    message: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "status": self.status,
            "account_id": self.account_id,
            "message": self.message,
        }


@dataclass(slots=True)
class ImportBatchResult:
    added: int = 0
    updated: int = 0
    skipped: int = 0
    invalid: int = 0
    applied: bool = True
    items: list[ImportItemResult] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "added": self.added,
            "updated": self.updated,
            "skipped": self.skipped,
            "invalid": self.invalid,
            "applied": self.applied,
            "items": [item.to_dict() for item in self.items],
        }


class AccountImporter:
    def __init__(self, repository: AccountRepository) -> None:
        self.repository = repository

    def import_accounts(
        self,
        rows: Iterable[dict[str, Any]],
        *,
        dry_run: bool = False,
        conflict_policy: ConflictPolicy = "merge",
    ) -> ImportBatchResult:
        if conflict_policy not in {"merge", "replace", "skip"}:
            raise ValueError(f"unsupported conflict policy: {conflict_policy}")
        result = ImportBatchResult(applied=not dry_run)
        pending: list[CliAccount] = []

        for index, row in enumerate(rows):
            if not isinstance(row, dict):
                result.invalid += 1
                result.items.append(
                    ImportItemResult(index, "invalid", message="account must be an object")
                )
                continue
            token = str(row.get("access_token") or row.get("key") or "").strip()
            if not token:
                result.invalid += 1
                result.items.append(
                    ImportItemResult(index, "invalid", message="access_token required")
                )
                continue

            issuer = str(row.get("oidc_issuer") or "https://auth.x.ai").rstrip("/")
            email = str(row.get("email") or "").strip().lower()
            user_id = str(row.get("user_id") or "").strip()
            identity = account_identity(
                access_token=token,
                issuer=issuer,
                email=email,
                user_id=user_id,
            )
            existing = self.repository.find_by_identity(identity)
            if existing and conflict_policy == "skip":
                result.skipped += 1
                result.items.append(
                    ImportItemResult(index, "skipped", existing.id, "identity already exists")
                )
                continue

            expires_at = row.get("expires_at")
            if expires_at is None and row.get("expires_in") is not None:
                expires_at = iso_z(time.time() + int(row["expires_in"]))
            account_id = str(
                row.get("id")
                or (existing.id if existing else "")
                or email
                or f"cli-{token_fingerprint(token)[:16]}"
            )

            if existing and conflict_policy == "merge":
                account = existing.copy(
                    key=token,
                    refresh_token=row.get("refresh_token") or existing.refresh_token,
                    expires_at=expires_at or existing.expires_at,
                    oidc_issuer=issuer,
                    oidc_client_id=str(
                        row.get("oidc_client_id") or existing.oidc_client_id
                    ),
                    email=email or existing.email,
                    user_id=user_id or existing.user_id,
                    team_id=str(row.get("team_id") or existing.team_id),
                    enabled=bool(row.get("enabled", True)),
                    cooldown_until=None,
                    disabled_reason="",
                    note=str(row.get("note") or existing.note),
                    identity_key=identity,
                )
                status = "updated"
                result.updated += 1
            else:
                account = CliAccount(
                    id=account_id,
                    key=token,
                    refresh_token=row.get("refresh_token"),
                    expires_at=expires_at,
                    oidc_issuer=issuer,
                    oidc_client_id=str(
                        row.get("oidc_client_id")
                        or "b1a00492-073a-47ea-816f-4c329264a828"
                    ),
                    email=email,
                    user_id=user_id,
                    team_id=str(row.get("team_id") or ""),
                    enabled=bool(row.get("enabled", True)),
                    note=str(row.get("note") or "admin-import"),
                    priority=int(row.get("priority") or 0),
                    identity_key=identity,
                )
                status = "updated" if existing else "added"
                if existing:
                    result.updated += 1
                else:
                    result.added += 1
            pending.append(account)
            result.items.append(ImportItemResult(index, status, account.id))

        if not dry_run and pending:
            self.repository.upsert_many(pending)
        return result
