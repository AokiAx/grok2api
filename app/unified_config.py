"""Single project-root config.json loader (server + register + email)."""

from __future__ import annotations

import json
import os
from functools import lru_cache
from pathlib import Path
from typing import Any


def project_root() -> Path:
    return Path(__file__).resolve().parents[1]


def config_path() -> Path:
    # Allow override: GROK2API_CONFIG=/path/to.json
    env = os.environ.get("GROK2API_CONFIG", "").strip()
    if env:
        return Path(env)
    root = project_root()
    for name in ("config.json", "config.example.json"):
        p = root / name
        if p.exists():
            return p
    return root / "config.json"


@lru_cache(maxsize=1)
def load_unified_config() -> dict[str, Any]:
    path = config_path()
    if not path.exists():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        return {}
    if not isinstance(data, dict):
        return {}
    return {k: v for k, v in data.items() if not str(k).startswith("_")}


def clear_config_cache() -> None:
    load_unified_config.cache_clear()
