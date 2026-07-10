from __future__ import annotations

import json
import sqlite3
from pathlib import Path

from app.infrastructure.account_repository import SQLiteAccountRepository


def _write_legacy_pool(path: Path) -> None:
    path.write_text(
        json.dumps(
            {
                "accounts": [
                    {
                        "id": "legacy@example.com",
                        "key": "access-token-legacy",
                        "refresh_token": "refresh-token-legacy",
                        "expires_at": "2030-01-01T00:00:00Z",
                        "email": "Legacy@Example.com",
                        "password": "must-not-enter-sqlite",
                        "enabled": True,
                        "request_count": 7,
                    }
                ]
            }
        ),
        encoding="utf-8",
    )


def test_repository_migrates_legacy_json_once_without_password(tmp_path: Path):
    legacy = tmp_path / "cli_accounts.json"
    database = tmp_path / "grok2api.db"
    _write_legacy_pool(legacy)

    repo = SQLiteAccountRepository(database, legacy_json_path=legacy)
    accounts = repo.list_accounts()

    assert len(accounts) == 1
    assert accounts[0].email == "legacy@example.com"
    assert accounts[0].request_count == 7
    assert list(tmp_path.glob("cli_accounts.migration-backup-*.json"))

    with sqlite3.connect(database) as connection:
        columns = {
            row[1]
            for row in connection.execute("PRAGMA table_info(cli_accounts)").fetchall()
        }
    assert "password" not in columns

    repo.close()
    repo = SQLiteAccountRepository(database, legacy_json_path=legacy)
    assert len(repo.list_accounts()) == 1


def test_repository_uses_wal_and_schema_version(tmp_path: Path):
    database = tmp_path / "grok2api.db"

    repo = SQLiteAccountRepository(database)

    assert repo.schema_version == 1
    with sqlite3.connect(database) as connection:
        mode = connection.execute("PRAGMA journal_mode").fetchone()[0]
    assert mode.lower() == "wal"


def test_repository_can_resync_accounts_written_by_legacy_process(tmp_path: Path):
    legacy = tmp_path / "cli_accounts.json"
    database = tmp_path / "grok2api.db"
    _write_legacy_pool(legacy)
    repo = SQLiteAccountRepository(database, legacy_json_path=legacy)
    assert len(repo.list_accounts()) == 1

    raw = json.loads(legacy.read_text(encoding="utf-8"))
    raw["accounts"].append(
        {
            "id": "external@example.com",
            "key": "access-token-external",
            "refresh_token": "refresh-token-external",
            "email": "external@example.com",
        }
    )
    legacy.write_text(json.dumps(raw), encoding="utf-8")

    assert repo.sync_legacy_json() == 2
    assert len(repo.list_accounts()) == 2


def test_repository_closes_short_lived_connections(tmp_path: Path, monkeypatch):
    database = tmp_path / "grok2api.db"
    repo = SQLiteAccountRepository(database)
    connections: list[TrackingConnection] = []

    class TrackingConnection(sqlite3.Connection):
        was_closed = False

        def close(self) -> None:
            self.was_closed = True
            super().close()

    def connect() -> TrackingConnection:
        connection = sqlite3.connect(database, factory=TrackingConnection)
        connection.row_factory = sqlite3.Row
        connections.append(connection)
        return connection

    monkeypatch.setattr(repo, "_connect", connect)

    repo.list_accounts()

    assert connections[-1].was_closed is True
