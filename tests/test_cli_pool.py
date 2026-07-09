from __future__ import annotations

from pathlib import Path

from app.cli_pool import CliAccountPool
from app.config import Settings


def test_cli_pool_upsert(tmp_path: Path, monkeypatch):
    cfg = Settings(
        data_dir=tmp_path,
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


def test_cli_pool_acquire_round_robin(tmp_path: Path):
    cfg = Settings(
        data_dir=tmp_path,
        auth_file=tmp_path / "auth.json",
        cli_pool_max_concurrent=1,
        cli_pool_acquire_timeout=0.2,
    )
    pool = CliAccountPool(cfg)
    for i, email in enumerate(["a@t.com", "b@t.com", "c@t.com"]):
        pool.upsert_from_tokens(
            access_token=f"token-{email}-xxxxxxxx",
            refresh_token="r",
            expires_in=3600,
            email=email,
            note=f"n{i}",
        )
    # with max_concurrent=1, must release after each acquire to cycle
    order = []
    for _ in range(6):
        acc = pool.acquire(wait=False)
        assert acc is not None
        order.append(acc.email)
        pool.release(acc.id)
    assert order[:3] == ["a@t.com", "b@t.com", "c@t.com"]
    assert set(order[3:]) == {"a@t.com", "b@t.com", "c@t.com"}

    # hold all 3 slots → no free account
    held = [pool.acquire(wait=False) for _ in range(3)]
    assert all(h is not None for h in held)
    assert pool.acquire(wait=False) is None
    for h in held:
        pool.release(h.id)  # type: ignore[union-attr]

    # 429-style cooldown removes account from rotation temporarily
    pool.report_failure("a@t.com", cooldown_secs=600)
    next_ids = set()
    for _ in range(4):
        acc = pool.acquire(wait=False)
        assert acc is not None
        next_ids.add(acc.email)
        pool.release(acc.id)
    assert "a@t.com" not in next_ids


def test_cli_pool_max_concurrent(tmp_path: Path):
    cfg = Settings(
        data_dir=tmp_path,
        auth_file=tmp_path / "auth.json",
        cli_pool_max_concurrent=2,
        cli_pool_acquire_timeout=0.1,
    )
    pool = CliAccountPool(cfg)
    pool.upsert_from_tokens(
        access_token="token-only-account-xxxxxx",
        refresh_token="r",
        expires_in=3600,
        email="solo@t.com",
    )
    a1 = pool.acquire(wait=False)
    a2 = pool.acquire(wait=False)
    a3 = pool.acquire(wait=False)
    assert a1 is not None and a2 is not None
    assert a3 is None  # third exceeds max_concurrent=2
    pool.release(a1.id)
    a4 = pool.acquire(wait=False)
    assert a4 is not None
    pool.release(a2.id)
    pool.release(a4.id)


def test_cli_pool_does_not_write_home_auth_json(tmp_path: Path):
    """Pool must only persist under data_dir, never touch auth_file home path."""
    auth = tmp_path / "home" / "auth.json"
    auth.parent.mkdir(parents=True, exist_ok=True)
    data_dir = tmp_path / "data"
    cfg = Settings(
        data_dir=data_dir,
        auth_file=auth,
    )
    pool = CliAccountPool(cfg)
    pool.upsert_from_tokens(
        access_token="tok-aaa-bbbbbbbbbbbb",
        refresh_token="ref",
        expires_in=100,
        email="b@test.com",
    )
    assert (data_dir / "cli_accounts.json").exists()
    assert not auth.exists()
