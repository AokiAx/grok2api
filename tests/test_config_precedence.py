from __future__ import annotations

from app.config import Settings


def test_environment_overrides_unified_config(monkeypatch):
    monkeypatch.setenv("GROK2API_PORT", "9999")

    assert Settings().port == 9999


def test_explicit_settings_override_environment(monkeypatch):
    monkeypatch.setenv("GROK2API_PORT", "9999")

    assert Settings(port=7777).port == 7777
