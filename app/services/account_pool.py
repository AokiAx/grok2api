from __future__ import annotations

import asyncio
import logging
import threading
import time
from pathlib import Path
from typing import Any

from app.config import Settings, settings
from app.domain.accounts import CliAccount, iso_z, token_fingerprint
from app.infrastructure.account_repository import (
    AccountRepository,
    SQLiteAccountRepository,
)

log = logging.getLogger("grok2api.accounts.pool")


class AccountLease:
    def __init__(
        self,
        pool: CliAccountPool,
        *,
        wait: bool,
        exclude_ids: set[str] | None,
    ) -> None:
        self.pool = pool
        self.wait = wait
        self.exclude_ids = exclude_ids or set()
        self.account: CliAccount | None = None

    async def __aenter__(self) -> AccountLease:
        account = await asyncio.to_thread(
            self.pool.acquire,
            wait=self.wait,
            exclude_ids=self.exclude_ids,
        )
        if account is None:
            raise RuntimeError(
                "CLI account pool empty or all accounts at concurrency limit / cooldown"
            )
        self.account = account
        return self

    async def __aexit__(self, exc_type: Any, exc: Any, traceback: Any) -> bool:
        if self.account is not None:
            self.pool.release(self.account.id)
            self.account = None
        return False

    def report_success(self) -> None:
        if self.account is not None:
            self.pool.report_success(self.account.id)

    def report_failure(self, *, cooldown_secs: float, disable: bool = False) -> None:
        if self.account is not None:
            self.pool.report_failure(
                self.account.id,
                cooldown_secs=cooldown_secs,
                disable=disable,
            )


class CliAccountPool:
    """Thread-safe scheduler backed by durable account repository storage."""

    def __init__(
        self,
        cfg: Settings | None = None,
        *,
        repository: AccountRepository | None = None,
    ) -> None:
        self.cfg = cfg or settings
        self.repository = repository or SQLiteAccountRepository(
            self.cfg.resolved_data_dir / "grok2api.db",
            legacy_json_path=self.cfg.resolved_data_dir / "cli_accounts.json",
        )
        self._lock = threading.RLock()
        self._slot_cv = threading.Condition(self._lock)
        self._accounts = {account.id: account for account in self.repository.list_accounts()}
        self._inflight: dict[str, int] = {}
        self._round_robin_index = 0
        self._dirty_runtime: set[str] = set()
        self._last_runtime_flush = time.monotonic()

    @property
    def path(self) -> Path:
        return Path(getattr(self.repository, "path", self.cfg.resolved_data_dir / "grok2api.db"))

    @property
    def max_concurrent(self) -> int:
        return max(1, int(self.cfg.cli_pool_max_concurrent or 1))

    def _inflight_of(self, account_id: str) -> int:
        return int(self._inflight.get(account_id) or 0)

    def _is_usable_unlocked(
        self,
        account: CliAccount,
        *,
        now: float,
        need_slot: bool,
        exclude_ids: set[str] | None = None,
    ) -> bool:
        if exclude_ids and account.id in exclude_ids:
            return False
        if not account.enabled or not account.key:
            return False
        if account.cooldown_until is not None and account.cooldown_until > now:
            return False
        if need_slot and self._inflight_of(account.id) >= self.max_concurrent:
            return False
        return True

    def _select_unlocked(self, candidates: list[CliAccount]) -> CliAccount:
        strategy = str(
            getattr(self.cfg, "cli_pool_selection_strategy", "balanced") or "balanced"
        ).lower()
        if strategy == "round_robin":
            ordered = sorted(candidates, key=lambda account: (account.created_at, account.id))
            pick = ordered[self._round_robin_index % len(ordered)]
            self._round_robin_index += 1
            return pick
        if strategy == "least_used":
            return min(
                candidates,
                key=lambda account: (
                    account.request_count,
                    account.last_used_at or 0.0,
                    account.id,
                ),
            )
        if strategy == "priority":
            return min(
                candidates,
                key=lambda account: (
                    -account.priority,
                    self._inflight_of(account.id),
                    account.last_used_at or 0.0,
                    account.id,
                ),
            )
        return min(
            candidates,
            key=lambda account: (
                self._inflight_of(account.id) / self.max_concurrent,
                account.consecutive_failures,
                -account.priority,
                account.last_used_at or 0.0,
                account.request_count,
                account.id,
            ),
        )

    def count(self, *, enabled_only: bool = True) -> int:
        with self._lock:
            if not enabled_only:
                return len(self._accounts)
            now = time.time()
            return sum(
                self._is_usable_unlocked(account, now=now, need_slot=False)
                for account in self._accounts.values()
            )

    def count_available_slots(self) -> int:
        with self._lock:
            now = time.time()
            return sum(
                self._is_usable_unlocked(account, now=now, need_slot=True)
                for account in self._accounts.values()
            )

    def list_public(self) -> list[dict[str, Any]]:
        with self._lock:
            return [
                account.to_public(
                    inflight=self._inflight_of(account.id),
                    max_concurrent=self.max_concurrent,
                )
                for account in self._accounts.values()
            ]

    def upsert_from_tokens(
        self,
        *,
        access_token: str,
        refresh_token: str | None,
        expires_in: int | None,
        email: str = "",
        password: str = "",
        user_id: str = "",
        team_id: str = "",
        oidc_issuer: str = "https://auth.x.ai",
        oidc_client_id: str = "b1a00492-073a-47ea-816f-4c329264a828",
        note: str = "",
        account_id: str | None = None,
    ) -> CliAccount:
        del password  # Passwords are intentionally not persisted in the new store.
        token = access_token.strip()
        normalized_email = email.strip().lower()
        fallback_id = f"cli-{token_fingerprint(token)[:16]}"
        requested_id = account_id or normalized_email or fallback_id
        expires_at = iso_z(time.time() + int(expires_in)) if expires_in is not None else None
        with self._lock:
            previous = self._accounts.get(requested_id)
            account = CliAccount(
                id=requested_id,
                key=token,
                refresh_token=refresh_token or (previous.refresh_token if previous else None),
                expires_at=expires_at or (previous.expires_at if previous else None),
                oidc_issuer=oidc_issuer,
                oidc_client_id=oidc_client_id,
                email=normalized_email or (previous.email if previous else ""),
                user_id=user_id or (previous.user_id if previous else ""),
                team_id=team_id or (previous.team_id if previous else ""),
                enabled=True,
                request_count=previous.request_count if previous else 0,
                fail_count=0,
                consecutive_failures=0,
                last_used_at=previous.last_used_at if previous else None,
                cooldown_until=None,
                created_at=previous.created_at if previous else time.time(),
                note=note or (previous.note if previous else ""),
                priority=previous.priority if previous else 0,
            )
            saved = self.repository.upsert(account)
            if saved.id != requested_id:
                self._accounts.pop(requested_id, None)
            self._accounts[saved.id] = saved
            self._slot_cv.notify_all()
            return saved

    def acquire(
        self,
        *,
        wait: bool = True,
        exclude_ids: set[str] | None = None,
    ) -> CliAccount | None:
        timeout = float(self.cfg.cli_pool_acquire_timeout or 0.0)
        deadline = time.time() + max(0.0, timeout) if wait else time.time()
        excluded = exclude_ids or set()
        with self._slot_cv:
            while True:
                now = time.time()
                candidates = [
                    account
                    for account in self._accounts.values()
                    if self._is_usable_unlocked(
                        account,
                        now=now,
                        need_slot=True,
                        exclude_ids=excluded,
                    )
                ]
                if candidates:
                    selected = self._select_unlocked(candidates)
                    self._inflight[selected.id] = self._inflight_of(selected.id) + 1
                    selected.request_count += 1
                    selected.last_used_at = now
                    self._dirty_runtime.add(selected.id)
                    self._maybe_flush_runtime_unlocked()
                    return selected
                any_enabled = any(
                    self._is_usable_unlocked(
                        account,
                        now=now,
                        need_slot=False,
                        exclude_ids=excluded,
                    )
                    for account in self._accounts.values()
                )
                if not any_enabled or not wait or time.time() >= deadline:
                    return None
                remaining = deadline - time.time()
                self._slot_cv.wait(timeout=min(0.5, max(0.05, remaining)))

    def lease(
        self,
        *,
        wait: bool = True,
        exclude_ids: set[str] | None = None,
    ) -> AccountLease:
        return AccountLease(self, wait=wait, exclude_ids=exclude_ids)

    def release(self, account_id: str) -> None:
        if not account_id:
            return
        with self._slot_cv:
            current = self._inflight_of(account_id)
            if current <= 1:
                self._inflight.pop(account_id, None)
            else:
                self._inflight[account_id] = current - 1
            self._slot_cv.notify_all()

    def report_success(self, account_id: str) -> None:
        with self._slot_cv:
            account = self._accounts.get(account_id)
            if account is None:
                return
            account.fail_count = 0
            account.consecutive_failures = 0
            account.cooldown_until = None
            if account.disabled_reason == "consecutive_failures":
                account.enabled = True
                account.disabled_reason = ""
            self.repository.upsert(account)
            self._dirty_runtime.discard(account_id)
            self._slot_cv.notify_all()

    def report_failure(
        self,
        account_id: str,
        *,
        cooldown_secs: float = 300,
        disable: bool = False,
    ) -> None:
        with self._slot_cv:
            account = self._accounts.get(account_id)
            if account is None:
                return
            account.fail_count += 1
            account.consecutive_failures += 1
            account.cooldown_until = time.time() + max(0.0, cooldown_secs)
            if disable or account.consecutive_failures >= self.cfg.sso_max_fails:
                account.enabled = False
                account.disabled_reason = (
                    "manual_failure" if disable else "consecutive_failures"
                )
            self.repository.upsert(account)
            self._dirty_runtime.discard(account_id)
            self._slot_cv.notify_all()

    def update_tokens(
        self,
        account_id: str,
        *,
        key: str,
        refresh_token: str | None,
        expires_in: int | None,
    ) -> None:
        with self._slot_cv:
            account = self._accounts.get(account_id)
            if account is None:
                return
            account.key = key
            if refresh_token:
                account.refresh_token = refresh_token
            if expires_in is not None:
                account.expires_at = iso_z(time.time() + int(expires_in))
            account.cooldown_until = None
            account.enabled = True
            account.disabled_reason = ""
            self.repository.upsert(account)
            self._dirty_runtime.discard(account_id)
            self._slot_cv.notify_all()

    def get(self, account_id: str) -> CliAccount | None:
        with self._lock:
            return self._accounts.get(account_id)

    def delete(self, account_id: str) -> bool:
        with self._slot_cv:
            if account_id not in self._accounts:
                return False
            if not self.repository.delete(account_id):
                return False
            del self._accounts[account_id]
            self._inflight.pop(account_id, None)
            self._dirty_runtime.discard(account_id)
            self._slot_cv.notify_all()
            return True

    def reload(self) -> int:
        with self._slot_cv:
            sync_legacy = getattr(self.repository, "sync_legacy_json", None)
            if callable(sync_legacy):
                sync_legacy()
            self._accounts = {
                account.id: account for account in self.repository.list_accounts()
            }
            self._inflight = {
                account_id: count
                for account_id, count in self._inflight.items()
                if account_id in self._accounts
            }
            self._slot_cv.notify_all()
            return len(self._accounts)

    def _maybe_flush_runtime_unlocked(self) -> None:
        interval = float(getattr(self.cfg, "cli_pool_flush_interval", 30.0) or 0.0)
        if interval > 0 and time.monotonic() - self._last_runtime_flush < interval:
            return
        self._flush_runtime_unlocked()

    def _flush_runtime_unlocked(self) -> None:
        accounts = [
            self._accounts[account_id]
            for account_id in self._dirty_runtime
            if account_id in self._accounts
        ]
        if accounts:
            self.repository.upsert_many(accounts)
        self._dirty_runtime.clear()
        self._last_runtime_flush = time.monotonic()

    def close(self) -> None:
        with self._lock:
            self._flush_runtime_unlocked()
        close = getattr(self.repository, "close", None)
        if callable(close):
            close()
