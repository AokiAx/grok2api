"""Compatibility exports for the SQLite-backed CLI account pool."""

from __future__ import annotations

import threading
from typing import Any

from .services.account_pool import AccountLease, CliAccountPool


class _LazyCliAccountPool:
    """Delay database initialization until the pool is actually used."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._instance: CliAccountPool | None = None

    def _get(self) -> CliAccountPool:
        if self._instance is not None:
            return self._instance
        with self._lock:
            if self._instance is None:
                self._instance = CliAccountPool()
        return self._instance

    def __getattr__(self, name: str) -> Any:
        return getattr(self._get(), name)


cli_pool = _LazyCliAccountPool()

__all__ = ["AccountLease", "CliAccountPool", "cli_pool"]
