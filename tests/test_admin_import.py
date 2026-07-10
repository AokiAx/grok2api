from __future__ import annotations

from pathlib import Path

from fastapi.testclient import TestClient

import app.admin as admin_module
from app.config import Settings
from app.infrastructure.account_repository import SQLiteAccountRepository
from app.main import app
from app.services.account_pool import CliAccountPool


def _temporary_pool(tmp_path: Path) -> CliAccountPool:
    cfg = Settings(
        data_dir=tmp_path,
        auth_file=tmp_path / "auth.json",
        cli_pool_acquire_timeout=0.05,
    )
    return CliAccountPool(
        cfg,
        repository=SQLiteAccountRepository(tmp_path / "grok2api.db"),
    )


def test_bulk_import_preview_and_apply(tmp_path: Path, monkeypatch):
    pool = _temporary_pool(tmp_path)
    monkeypatch.setattr(admin_module, "cli_pool", pool)
    app.dependency_overrides[admin_module.require_admin] = lambda: None
    client = TestClient(app)
    payload = {
        "accounts": [
            {
                "access_token": "bulk-access-token",
                "refresh_token": "bulk-refresh-token",
                "email": "Bulk@Example.com",
                "password": "must-be-ignored",
            }
        ],
        "conflict_policy": "merge",
    }

    try:
        preview = client.post("/admin/api/accounts/import/preview", json=payload)
        assert preview.status_code == 200
        assert preview.json()["added"] == 1
        assert preview.json()["applied"] is False
        assert pool.count(enabled_only=False) == 0

        applied = client.post("/admin/api/accounts/import", json=payload)
        assert applied.status_code == 200
        assert applied.json()["added"] == 1
        assert applied.json()["applied"] is True
        assert pool.count(enabled_only=False) == 1
    finally:
        app.dependency_overrides.clear()
        client.close()
