from __future__ import annotations

from pathlib import Path

from app.cli_pool import CliAccountPool
from app.config import Settings


def test_cli_pool_upsert(tmp_path: Path, monkeypatch):
    cfg = Settings(
        data_dir=tmp_path,
        cli_pool_sync_auth_json=False,
        auth_file=tmp_path / "auth.json",
    )
    pool = CliAccountPool(cfg)
    acc = pool.upsert_from_tokens(
        access_token="access-token-value-1234567890",
        refresh_token="refresh-xyz",
        expires_in=3600,
        email="a@test.com",
        password="Secret1!",
        note="unit",
    )
    assert acc.email == "a@test.com"
    assert pool.count() == 1
    got = pool.acquire()
    assert got is not None
    assert got.key.startswith("access-token")
    pub = pool.list_public()[0]
    assert "key_preview" in pub
    assert "password" not in pub


def test_cli_pool_sync_auth_json(tmp_path: Path):
    auth = tmp_path / "auth.json"
    cfg = Settings(
        data_dir=tmp_path / "data",
        cli_pool_sync_auth_json=True,
        auth_file=auth,
    )
    pool = CliAccountPool(cfg)
    pool.upsert_from_tokens(
        access_token="tok-aaa-bbbbbbbbbbbb",
        refresh_token="ref",
        expires_in=100,
        email="b@test.com",
        oidc_issuer="https://auth.x.ai",
        oidc_client_id="b1a00492-073a-47ea-816f-4c329264a828",
    )
    assert auth.exists()
    text = auth.read_text(encoding="utf-8")
    assert "tok-aaa" in text
    assert "refresh_token" in text
