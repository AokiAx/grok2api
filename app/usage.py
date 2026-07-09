"""Local request usage stats (JSONL)."""

from __future__ import annotations

import json
import logging
import threading
import time
from collections import deque
from pathlib import Path
from typing import Any

from .config import Settings, settings

log = logging.getLogger("grok2api.usage")


class UsageLogger:
    def __init__(self, cfg: Settings | None = None) -> None:
        self.cfg = cfg or settings
        self._lock = threading.Lock()
        self._recent: deque[dict[str, Any]] = deque(maxlen=200)
        self._totals: dict[str, Any] = {
            "requests": 0,
            "errors": 0,
            "by_model": {},
            "by_mode": {},
        }

    @property
    def path(self) -> Path:
        return self.cfg.resolved_data_dir / "usage.jsonl"

    def log_request(
        self,
        *,
        mode: str,
        model: str,
        stream: bool,
        status: int,
        latency_ms: float,
        error: str | None = None,
    ) -> None:
        if not self.cfg.usage_log_enabled:
            return
        row = {
            "ts": time.time(),
            "mode": mode,
            "model": model,
            "stream": stream,
            "status": status,
            "latency_ms": round(latency_ms, 1),
            "error": error,
        }
        with self._lock:
            self._recent.appendleft(row)
            self._totals["requests"] += 1
            if status >= 400 or error:
                self._totals["errors"] += 1
            bm = self._totals["by_model"]
            bm[model] = int(bm.get(model) or 0) + 1
            bmode = self._totals["by_mode"]
            bmode[mode] = int(bmode.get(mode) or 0) + 1
            try:
                self.path.parent.mkdir(parents=True, exist_ok=True)
                with self.path.open("a", encoding="utf-8") as f:
                    f.write(json.dumps(row, ensure_ascii=False) + "\n")
            except Exception:
                log.debug("usage write failed", exc_info=True)

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            return {
                "totals": dict(self._totals),
                "recent": list(self._recent)[:50],
                "path": str(self.path),
            }


usage_logger = UsageLogger()
