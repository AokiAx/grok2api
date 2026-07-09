"""Detect local Grok CLI version for matching x-grok-client-version header."""

from __future__ import annotations

import logging
import re
import shutil
import subprocess
from functools import lru_cache
from pathlib import Path

log = logging.getLogger("grok2api.version")

_VERSION_RE = re.compile(r"(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.]+)?)")


def _candidate_bins() -> list[Path]:
    found: list[Path] = []
    which = shutil.which("grok")
    if which:
        found.append(Path(which))
    home = Path.home() / ".grok" / "bin"
    for name in ("grok.exe", "grok", "agent.exe", "agent"):
        p = home / name
        if p.exists():
            found.append(p)
    # de-dupe while preserving order
    seen: set[str] = set()
    out: list[Path] = []
    for p in found:
        key = str(p.resolve()) if p.exists() else str(p)
        if key not in seen:
            seen.add(key)
            out.append(p)
    return out


@lru_cache(maxsize=1)
def detect_grok_cli_version() -> str | None:
    """Return e.g. ``0.2.93`` from ``grok --version``, or None."""
    for bin_path in _candidate_bins():
        try:
            proc = subprocess.run(
                [str(bin_path), "--version"],
                capture_output=True,
                text=True,
                timeout=8,
                check=False,
            )
            text = (proc.stdout or "") + "\n" + (proc.stderr or "")
            m = _VERSION_RE.search(text)
            if m:
                ver = m.group(1)
                log.info("detected grok CLI version %s from %s", ver, bin_path)
                return ver
        except Exception:
            log.debug("version probe failed for %s", bin_path, exc_info=True)
            continue
    return None


def resolve_client_version(configured: str, *, auto: bool = True) -> str:
    """Prefer local CLI version when auto and configured is default-ish."""
    if not auto:
        return configured
    detected = detect_grok_cli_version()
    if detected:
        return detected
    return configured
