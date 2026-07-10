from __future__ import annotations

from pathlib import Path

import pytest

from app.config import Settings
from app.infrastructure.account_repository import SQLiteAccountRepository
from app.services.account_pool import CliAccountPool


def _pool(tmp_path: Path, *, max_concurrent: int = 1) -> CliAccountPool:
    cfg = Settings(
        data_dir=tmp_path,
        auth_file=tmp_path / "auth.json",
        cli_pool_max_concurrent=max_concurrent,
        cli_pool_acquire_timeout=0.05,
    )
    repository = SQLiteAccountRepository(tmp_path / "grok2api.db")
    return CliAccountPool(cfg, repository=repository)


def _add(pool: CliAccountPool, email: str) -> None:
    pool.upsert_from_tokens(
        access_token=f"token-{email}-xxxxxxxx",
        refresh_token=f"refresh-{email}",
        expires_in=3600,
        email=email,
    )


@pytest.mark.asyncio
async def test_async_lease_releases_slot_after_exception(tmp_path: Path):
    pool = _pool(tmp_path)
    _add(pool, "a@example.com")

    with pytest.raises(RuntimeError, match="boom"):
        async with pool.lease(wait=False) as lease:
            assert lease.account.email == "a@example.com"
            assert pool.list_public()[0]["inflight"] == 1
            raise RuntimeError("boom")

    assert pool.list_public()[0]["inflight"] == 0


def test_selection_excludes_accounts_already_attempted(tmp_path: Path):
    pool = _pool(tmp_path)
    _add(pool, "a@example.com")
    _add(pool, "b@example.com")

    first = pool.acquire(wait=False)
    assert first is not None
    pool.release(first.id)

    second = pool.acquire(wait=False, exclude_ids={first.id})
    assert second is not None
    assert second.id != first.id
    pool.release(second.id)


def test_success_recovers_consecutive_failures(tmp_path: Path):
    pool = _pool(tmp_path)
    _add(pool, "a@example.com")

    pool.report_failure("a@example.com", cooldown_secs=0)
    failed = pool.get("a@example.com")
    assert failed is not None
    assert failed.consecutive_failures == 1

    pool.report_success("a@example.com")
    recovered = pool.get("a@example.com")
    assert recovered is not None
    assert recovered.consecutive_failures == 0
    assert recovered.fail_count == 0


def test_acquire_does_not_persist_every_request(tmp_path: Path):
    pool = _pool(tmp_path)
    _add(pool, "a@example.com")
    database = tmp_path / "grok2api.db"
    before = database.stat().st_mtime_ns

    account = pool.acquire(wait=False)
    assert account is not None
    pool.release(account.id)

    assert database.stat().st_mtime_ns == before
