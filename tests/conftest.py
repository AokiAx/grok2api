from __future__ import annotations

import atexit
import os
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

_RUNTIME = tempfile.TemporaryDirectory(prefix="grok2api-tests-")
atexit.register(_RUNTIME.cleanup)
os.environ.setdefault("GROK2API_DATA_DIR", _RUNTIME.name)
os.environ.setdefault("GROK2API_AUTH_FILE", str(Path(_RUNTIME.name) / "auth.json"))
os.environ.setdefault("GROK2API_ENSURE_AUTH_ON_START", "false")
