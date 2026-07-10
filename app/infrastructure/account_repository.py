from __future__ import annotations

import json
import logging
import shutil
import sqlite3
import threading
import time
from collections.abc import Iterable, Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import Protocol

from app.domain.accounts import CliAccount

log = logging.getLogger("grok2api.accounts.repository")

SCHEMA_VERSION = 1


class AccountRepository(Protocol):
    def list_accounts(self) -> list[CliAccount]: ...

    def get(self, account_id: str) -> CliAccount | None: ...

    def find_by_identity(self, identity_key: str) -> CliAccount | None: ...

    def upsert(self, account: CliAccount) -> CliAccount: ...

    def upsert_many(self, accounts: Iterable[CliAccount]) -> None: ...

    def delete(self, account_id: str) -> bool: ...


class SQLiteAccountRepository:
    """Versioned SQLite persistence for CLI accounts.

    Connections are intentionally short-lived. Runtime scheduling state remains in
    memory, while durable mutations use transactions and WAL mode.
    """

    def __init__(
        self,
        path: Path,
        *,
        legacy_json_path: Path | None = None,
    ) -> None:
        self.path = Path(path)
        self.legacy_json_path = legacy_json_path
        self._lock = threading.RLock()
        self._initialize()

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, timeout=30.0)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys=ON")
        connection.execute("PRAGMA busy_timeout=30000")
        return connection

    @contextmanager
    def _connection(self) -> Iterator[sqlite3.Connection]:
        connection = self._connect()
        try:
            with connection:
                yield connection
        finally:
            connection.close()

    def _initialize(self) -> None:
        with self._lock:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            with self._connection() as connection:
                connection.execute("PRAGMA journal_mode=WAL")
                connection.execute("PRAGMA synchronous=NORMAL")
                connection.executescript(
                    """
                    CREATE TABLE IF NOT EXISTS app_meta (
                        key TEXT PRIMARY KEY,
                        value TEXT NOT NULL
                    );

                    CREATE TABLE IF NOT EXISTS cli_accounts (
                        id TEXT PRIMARY KEY,
                        identity_key TEXT NOT NULL UNIQUE,
                        key TEXT NOT NULL,
                        refresh_token TEXT,
                        expires_at TEXT,
                        oidc_issuer TEXT NOT NULL,
                        oidc_client_id TEXT NOT NULL,
                        email TEXT NOT NULL DEFAULT '',
                        user_id TEXT NOT NULL DEFAULT '',
                        team_id TEXT NOT NULL DEFAULT '',
                        enabled INTEGER NOT NULL DEFAULT 1,
                        request_count INTEGER NOT NULL DEFAULT 0,
                        fail_count INTEGER NOT NULL DEFAULT 0,
                        consecutive_failures INTEGER NOT NULL DEFAULT 0,
                        last_used_at REAL,
                        cooldown_until REAL,
                        created_at REAL NOT NULL,
                        updated_at REAL NOT NULL,
                        note TEXT NOT NULL DEFAULT '',
                        priority INTEGER NOT NULL DEFAULT 0,
                        disabled_reason TEXT NOT NULL DEFAULT ''
                    );

                    CREATE INDEX IF NOT EXISTS idx_cli_accounts_email
                        ON cli_accounts(email);
                    CREATE INDEX IF NOT EXISTS idx_cli_accounts_state
                        ON cli_accounts(enabled, cooldown_until);
                    """
                )
                connection.execute(
                    "INSERT INTO app_meta(key, value) VALUES('schema_version', ?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                    (str(SCHEMA_VERSION),),
                )
            self._migrate_legacy_json()

    @property
    def schema_version(self) -> int:
        with self._connection() as connection:
            row = connection.execute(
                "SELECT value FROM app_meta WHERE key='schema_version'"
            ).fetchone()
        return int(row[0]) if row else 0

    def _migration_done(self, connection: sqlite3.Connection) -> bool:
        row = connection.execute(
            "SELECT value FROM app_meta WHERE key='legacy_json_migrated'"
        ).fetchone()
        return bool(row)

    def _migrate_legacy_json(self) -> None:
        legacy = self.legacy_json_path
        if legacy is None or not legacy.exists():
            return
        with self._lock, self._connection() as connection:
            if self._migration_done(connection):
                return
            count = int(connection.execute("SELECT COUNT(*) FROM cli_accounts").fetchone()[0])
            if count:
                connection.execute(
                    "INSERT INTO app_meta(key, value) VALUES('legacy_json_migrated', ?) "
                    "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                    ("skipped-nonempty",),
                )
                return
            try:
                items = self._read_legacy_items(legacy)
            except (OSError, ValueError, json.JSONDecodeError) as error:
                log.warning("legacy CLI account migration skipped: %s", error)
                return

            timestamp = time.strftime("%Y%m%d-%H%M%S")
            backup = legacy.with_name(
                f"{legacy.stem}.migration-backup-{timestamp}{legacy.suffix}"
            )
            shutil.copy2(legacy, backup)

            migrated = 0
            for item in items:
                account = self._legacy_account(item)
                if account is None:
                    continue
                self._upsert_connection(connection, account)
                migrated += 1
            connection.execute(
                "INSERT INTO app_meta(key, value) VALUES('legacy_json_migrated', ?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                (str(migrated),),
            )
            log.info("migrated %d CLI accounts to %s; backup=%s", migrated, self.path, backup)

    @staticmethod
    def _read_legacy_items(path: Path) -> list[dict[str, object]]:
        raw = json.loads(path.read_text(encoding="utf-8"))
        items = raw.get("accounts") if isinstance(raw, dict) else raw
        if not isinstance(items, list):
            raise ValueError("legacy account file must contain an accounts list")
        return [item for item in items if isinstance(item, dict)]

    @staticmethod
    def _legacy_account(item: dict[str, object]) -> CliAccount | None:
        token = str(item.get("key") or item.get("access_token") or "").strip()
        if not token:
            return None
        email = str(item.get("email") or "").strip().lower()
        account_id = str(item.get("id") or email or f"cli-{token[:12]}")
        return CliAccount(
            id=account_id,
            key=token,
            refresh_token=item.get("refresh_token"),  # type: ignore[arg-type]
            expires_at=item.get("expires_at"),  # type: ignore[arg-type]
            oidc_issuer=str(item.get("oidc_issuer") or "https://auth.x.ai"),
            oidc_client_id=str(
                item.get("oidc_client_id")
                or "b1a00492-073a-47ea-816f-4c329264a828"
            ),
            email=email,
            user_id=str(item.get("user_id") or ""),
            team_id=str(item.get("team_id") or ""),
            enabled=bool(item.get("enabled", True)),
            request_count=int(item.get("request_count") or 0),
            fail_count=int(item.get("fail_count") or 0),
            consecutive_failures=int(
                item.get("consecutive_failures") or item.get("fail_count") or 0
            ),
            last_used_at=item.get("last_used_at"),  # type: ignore[arg-type]
            cooldown_until=item.get("cooldown_until"),  # type: ignore[arg-type]
            created_at=float(item.get("created_at") or time.time()),
            updated_at=float(item.get("updated_at") or time.time()),
            note=str(item.get("note") or ""),
            priority=int(item.get("priority") or 0),
            disabled_reason=str(item.get("disabled_reason") or ""),
        )

    def sync_legacy_json(self) -> int:
        """Merge accounts produced by legacy/external writers into SQLite."""
        legacy = self.legacy_json_path
        if legacy is None or not legacy.exists():
            return 0
        try:
            items = self._read_legacy_items(legacy)
        except (OSError, ValueError, json.JSONDecodeError) as error:
            log.warning("legacy CLI account sync skipped: %s", error)
            return 0
        incoming_accounts = [
            account
            for item in items
            if (account := self._legacy_account(item)) is not None
        ]
        with self._lock, self._connection() as connection:
            for incoming in incoming_accounts:
                row = connection.execute(
                    "SELECT * FROM cli_accounts WHERE identity_key=?",
                    (incoming.identity_key,),
                ).fetchone()
                if row:
                    existing = self._from_row(row)
                    incoming = incoming.copy(
                        id=existing.id,
                        enabled=existing.enabled,
                        request_count=existing.request_count,
                        fail_count=existing.fail_count,
                        consecutive_failures=existing.consecutive_failures,
                        last_used_at=existing.last_used_at,
                        cooldown_until=existing.cooldown_until,
                        created_at=existing.created_at,
                        priority=existing.priority,
                        disabled_reason=existing.disabled_reason,
                    )
                self._upsert_connection(connection, incoming)
        return len(incoming_accounts)

    @staticmethod
    def _from_row(row: sqlite3.Row) -> CliAccount:
        return CliAccount(
            id=row["id"],
            identity_key=row["identity_key"],
            key=row["key"],
            refresh_token=row["refresh_token"],
            expires_at=row["expires_at"],
            oidc_issuer=row["oidc_issuer"],
            oidc_client_id=row["oidc_client_id"],
            email=row["email"],
            user_id=row["user_id"],
            team_id=row["team_id"],
            enabled=bool(row["enabled"]),
            request_count=row["request_count"],
            fail_count=row["fail_count"],
            consecutive_failures=row["consecutive_failures"],
            last_used_at=row["last_used_at"],
            cooldown_until=row["cooldown_until"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
            note=row["note"],
            priority=row["priority"],
            disabled_reason=row["disabled_reason"],
        )

    def list_accounts(self) -> list[CliAccount]:
        with self._lock, self._connection() as connection:
            rows = connection.execute(
                "SELECT * FROM cli_accounts ORDER BY created_at, id"
            ).fetchall()
        return [self._from_row(row) for row in rows]

    def get(self, account_id: str) -> CliAccount | None:
        with self._lock, self._connection() as connection:
            row = connection.execute(
                "SELECT * FROM cli_accounts WHERE id=?", (account_id,)
            ).fetchone()
        return self._from_row(row) if row else None

    def find_by_identity(self, identity_key: str) -> CliAccount | None:
        with self._lock, self._connection() as connection:
            row = connection.execute(
                "SELECT * FROM cli_accounts WHERE identity_key=?", (identity_key,)
            ).fetchone()
        return self._from_row(row) if row else None

    def _upsert_connection(
        self, connection: sqlite3.Connection, account: CliAccount
    ) -> CliAccount:
        identity_row = connection.execute(
            "SELECT id FROM cli_accounts WHERE identity_key=?", (account.identity_key,)
        ).fetchone()
        if identity_row and identity_row["id"] != account.id:
            account = account.copy(id=identity_row["id"])
        account.updated_at = time.time()
        connection.execute(
            """
            INSERT INTO cli_accounts (
                id, identity_key, key, refresh_token, expires_at,
                oidc_issuer, oidc_client_id, email, user_id, team_id,
                enabled, request_count, fail_count, consecutive_failures,
                last_used_at, cooldown_until, created_at, updated_at,
                note, priority, disabled_reason
            ) VALUES (
                :id, :identity_key, :key, :refresh_token, :expires_at,
                :oidc_issuer, :oidc_client_id, :email, :user_id, :team_id,
                :enabled, :request_count, :fail_count, :consecutive_failures,
                :last_used_at, :cooldown_until, :created_at, :updated_at,
                :note, :priority, :disabled_reason
            )
            ON CONFLICT(id) DO UPDATE SET
                identity_key=excluded.identity_key,
                key=excluded.key,
                refresh_token=excluded.refresh_token,
                expires_at=excluded.expires_at,
                oidc_issuer=excluded.oidc_issuer,
                oidc_client_id=excluded.oidc_client_id,
                email=excluded.email,
                user_id=excluded.user_id,
                team_id=excluded.team_id,
                enabled=excluded.enabled,
                request_count=excluded.request_count,
                fail_count=excluded.fail_count,
                consecutive_failures=excluded.consecutive_failures,
                last_used_at=excluded.last_used_at,
                cooldown_until=excluded.cooldown_until,
                updated_at=excluded.updated_at,
                note=excluded.note,
                priority=excluded.priority,
                disabled_reason=excluded.disabled_reason
            """,
            {
                "id": account.id,
                "identity_key": account.identity_key,
                "key": account.key,
                "refresh_token": account.refresh_token,
                "expires_at": account.expires_at,
                "oidc_issuer": account.oidc_issuer,
                "oidc_client_id": account.oidc_client_id,
                "email": account.email,
                "user_id": account.user_id,
                "team_id": account.team_id,
                "enabled": int(account.enabled),
                "request_count": account.request_count,
                "fail_count": account.fail_count,
                "consecutive_failures": account.consecutive_failures,
                "last_used_at": account.last_used_at,
                "cooldown_until": account.cooldown_until,
                "created_at": account.created_at,
                "updated_at": account.updated_at,
                "note": account.note,
                "priority": account.priority,
                "disabled_reason": account.disabled_reason,
            },
        )
        return account

    def upsert(self, account: CliAccount) -> CliAccount:
        with self._lock, self._connection() as connection:
            return self._upsert_connection(connection, account)

    def upsert_many(self, accounts: Iterable[CliAccount]) -> None:
        with self._lock, self._connection() as connection:
            for account in accounts:
                self._upsert_connection(connection, account)

    def delete(self, account_id: str) -> bool:
        with self._lock, self._connection() as connection:
            cursor = connection.execute(
                "DELETE FROM cli_accounts WHERE id=?", (account_id,)
            )
            return cursor.rowcount > 0

    def close(self) -> None:
        """Kept for repository lifecycle symmetry; connections are short-lived."""
