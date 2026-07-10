from __future__ import annotations

from pathlib import Path


def test_panel_exposes_bulk_import_preview_and_apply_controls():
    panel = (
        Path(__file__).resolve().parents[1] / "app" / "static" / "panel.html"
    ).read_text(encoding="utf-8")

    assert 'id="accountImportInput"' in panel
    assert 'id="btnPreviewImport"' in panel
    assert 'id="btnApplyImport"' in panel
    assert "/admin/api/accounts/import/preview" in panel
    assert "/admin/api/accounts/import" in panel
