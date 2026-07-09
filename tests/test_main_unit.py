from __future__ import annotations

from app.main import _normalize_model
from app.config import settings


def test_normalize_model_alias():
    assert _normalize_model(None) == settings.default_model
    assert _normalize_model("gpt-4o") == settings.default_model
    assert _normalize_model("grok-4.5") == "grok-4.5"
    assert _normalize_model("grok-composer-2.5-fast") == "grok-composer-2.5-fast"
