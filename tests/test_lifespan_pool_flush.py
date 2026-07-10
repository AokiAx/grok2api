from __future__ import annotations

import pytest

import app.cli_pool as cli_pool_module
import app.main as main_module


class FakePool:
    def __init__(self) -> None:
        self.closed = False

    def count(self, *, enabled_only: bool = True) -> int:
        return 0

    def close(self) -> None:
        self.closed = True


class FakeUpstream:
    async def aclose(self) -> None:
        return None


@pytest.mark.asyncio
async def test_lifespan_flushes_account_pool_on_shutdown(monkeypatch):
    pool = FakePool()
    monkeypatch.setattr(cli_pool_module, "cli_pool", pool)
    monkeypatch.setattr(main_module, "upstream", FakeUpstream())

    async with main_module.lifespan(None):
        pass

    assert pool.closed is True
