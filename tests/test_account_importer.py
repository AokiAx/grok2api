from __future__ import annotations

from pathlib import Path

from app.infrastructure.account_repository import SQLiteAccountRepository
from app.services.account_importer import AccountImporter


def test_import_preview_is_non_mutating_and_reports_invalid_rows(tmp_path: Path):
    repository = SQLiteAccountRepository(tmp_path / "grok2api.db")
    importer = AccountImporter(repository)

    result = importer.import_accounts(
        [
            {
                "key": "access-valid-1",
                "refresh_token": "refresh-valid-1",
                "email": "User@Example.com",
            },
            {"key": "", "email": "invalid@example.com"},
        ],
        dry_run=True,
    )

    assert result.added == 1
    assert result.invalid == 1
    assert result.applied is False
    assert repository.list_accounts() == []


def test_import_deduplicates_by_issuer_and_email(tmp_path: Path):
    repository = SQLiteAccountRepository(tmp_path / "grok2api.db")
    importer = AccountImporter(repository)

    first = importer.import_accounts(
        [
            {
                "key": "access-first",
                "refresh_token": "refresh-first",
                "email": "User@Example.com",
                "oidc_issuer": "https://auth.x.ai/",
            }
        ]
    )
    second = importer.import_accounts(
        [
            {
                "key": "access-second",
                "refresh_token": "refresh-second",
                "email": "user@example.com",
                "oidc_issuer": "https://auth.x.ai",
            }
        ]
    )

    assert first.added == 1
    assert second.updated == 1
    accounts = repository.list_accounts()
    assert len(accounts) == 1
    assert accounts[0].key == "access-second"


def test_import_skip_conflict_policy_preserves_existing_account(tmp_path: Path):
    repository = SQLiteAccountRepository(tmp_path / "grok2api.db")
    importer = AccountImporter(repository)
    importer.import_accounts(
        [{"key": "first", "email": "user@example.com"}]
    )

    result = importer.import_accounts(
        [{"key": "second", "email": "user@example.com"}],
        conflict_policy="skip",
    )

    assert result.skipped == 1
    assert repository.list_accounts()[0].key == "first"


def test_import_accepts_access_token_alias(tmp_path: Path):
    repository = SQLiteAccountRepository(tmp_path / "grok2api.db")
    importer = AccountImporter(repository)

    result = importer.import_accounts(
        [
            {
                "access_token": "legacy-access",
                "refresh_token": "legacy-refresh",
                "email": "alias@example.com",
            }
        ]
    )

    assert result.added == 1
    assert repository.list_accounts()[0].key == "legacy-access"
