from __future__ import annotations

from app.auth import _parse_expires_at


def test_parse_expires_z():
    ts = _parse_expires_at("2026-07-09T20:49:45.994129900Z")
    assert ts is not None
    assert ts > 1_700_000_000


def test_parse_expires_offset():
    ts = _parse_expires_at("2026-07-09T20:49:45+00:00")
    assert ts is not None


def test_parse_expires_bad():
    assert _parse_expires_at("not-a-date") is None
    assert _parse_expires_at(None) is None
