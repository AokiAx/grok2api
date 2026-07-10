from __future__ import annotations

from typing import Any

import httpx
import pytest

import app.cli_pool as cli_pool_module
from app.config import Settings
from app.upstream import UpstreamClient


class FakePool:
    def __init__(self) -> None:
        self.failures: list[str] = []
        self.successes: list[str] = []
        self.releases: list[str] = []

    def count(self, *, enabled_only: bool = True) -> int:
        return 2

    def report_failure(self, account_id: str, **_: Any) -> None:
        self.failures.append(account_id)

    def report_success(self, account_id: str) -> None:
        self.successes.append(account_id)

    def release(self, account_id: str) -> None:
        self.releases.append(account_id)


class FakeStore:
    def __init__(self) -> None:
        self.calls: list[set[str]] = []

    def get_access_token_with_account(
        self,
        force_refresh: bool = False,
        exclude_ids: set[str] | None = None,
    ) -> tuple[str, str]:
        excluded = set(exclude_ids or set())
        self.calls.append(excluded)
        if "account-a" not in excluded:
            return "token-a", "account-a"
        return "token-b", "account-b"


@pytest.mark.asyncio
async def test_retry_excludes_account_that_just_failed(monkeypatch):
    pool = FakePool()
    store = FakeStore()
    monkeypatch.setattr(cli_pool_module, "cli_pool", pool)
    client = UpstreamClient(Settings(timeout_secs=1), store=store)  # type: ignore[arg-type]
    statuses = iter([429, 200])

    async def send(request: httpx.Request, *, stream: bool = False) -> httpx.Response:
        return httpx.Response(next(statuses), request=request, json={"ok": True})

    monkeypatch.setattr(client._client, "send", send)
    try:
        response, account_id = await client._request_with_auth_retry(
            "GET", "/models", model="grok-4.5"
        )
        assert response.status_code == 200
        assert account_id == "account-b"
        assert store.calls == [set(), {"account-a"}]
        assert pool.failures == ["account-a"]
    finally:
        await response.aclose()
        pool.release(account_id)
        await client.aclose()


@pytest.mark.asyncio
async def test_nonstream_success_reports_health_recovery(monkeypatch):
    pool = FakePool()
    monkeypatch.setattr(cli_pool_module, "cli_pool", pool)
    client = UpstreamClient(Settings(timeout_secs=1), store=FakeStore())  # type: ignore[arg-type]
    request = httpx.Request("POST", "https://example.test/chat/completions")
    response = httpx.Response(
        200,
        request=request,
        json={"choices": [{"message": {"content": "ok"}}]},
    )

    async def request_with_retry(*args: Any, **kwargs: Any):
        return response, "account-a"

    monkeypatch.setattr(client, "_request_with_auth_retry", request_with_retry)
    try:
        result = await client._chat_nonstream({"stream": False}, "grok-4.5")
        assert result["choices"][0]["message"]["content"] == "ok"
        assert pool.successes == ["account-a"]
        assert pool.releases == ["account-a"]
    finally:
        await client.aclose()
