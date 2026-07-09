"""Admin routes for CLI OIDC account pool (cli-chat-proxy credentials only)."""

from __future__ import annotations

from typing import Any

from fastapi import APIRouter, Depends, Header, HTTPException
from pydantic import BaseModel, Field

from .cli_pool import cli_pool
from .config import settings
from .usage import usage_logger

router = APIRouter(tags=["admin"])


def require_admin(
    authorization: str | None = Header(default=None),
    x_api_key: str | None = Header(default=None, alias="x-api-key"),
) -> None:
    # Prefer panel_password, then app_key / api_key
    expected = settings.resolved_panel_password or settings.resolved_admin_key
    if not expected:
        return
    token = None
    if authorization and authorization.lower().startswith("bearer "):
        token = authorization[7:].strip()
    elif x_api_key:
        token = x_api_key.strip()
    if token != expected:
        raise HTTPException(status_code=401, detail="Invalid password")


@router.get("/admin/api/panel-meta")
async def panel_meta() -> Any:
    """Public: whether panel needs a password (no secrets returned)."""
    need = bool(settings.resolved_panel_password or settings.resolved_admin_key)
    return {
        "auth_required": need,
        "version": __import__("app", fromlist=["__version__"]).__version__,
        "default_model": settings.default_model,
    }


class CliImportBody(BaseModel):
    """Import access/refresh tokens into CLI pool."""

    access_token: str
    refresh_token: str | None = None
    expires_in: int | None = None
    email: str = ""
    password: str = ""
    note: str = "admin-import"


class CliDeleteBody(BaseModel):
    id: str = Field(..., description="CLI account id or email")


@router.get("/admin/api/cli-accounts")
@router.get("/admin/api/tokens")
async def list_cli_accounts(_: None = Depends(require_admin)) -> Any:
    return {
        "mode": "cli",
        "count": cli_pool.count(enabled_only=False),
        "usable": cli_pool.count(enabled_only=True),
        "accounts": cli_pool.list_public(),
    }


@router.post("/admin/api/cli-accounts/add")
@router.post("/admin/api/tokens/add")
async def add_cli_account(
    body: CliImportBody, _: None = Depends(require_admin)
) -> Any:
    if not body.access_token.strip():
        raise HTTPException(status_code=400, detail="access_token required")
    acc = cli_pool.upsert_from_tokens(
        access_token=body.access_token.strip(),
        refresh_token=body.refresh_token,
        expires_in=body.expires_in,
        email=body.email,
        password=body.password,
        note=body.note,
        account_id=body.email or None,
    )
    return {
        "ok": True,
        "account": acc.to_public(),
        "usable": cli_pool.count(enabled_only=True),
    }


@router.post("/admin/api/cli-accounts/delete")
@router.post("/admin/api/tokens/delete")
async def delete_cli_account(
    body: CliDeleteBody, _: None = Depends(require_admin)
) -> Any:
    ok = cli_pool.delete(body.id)
    return {"ok": ok, "message": "deleted" if ok else "not found"}


@router.post("/admin/api/cli-accounts/reload")
async def reload_cli_accounts(_: None = Depends(require_admin)) -> Any:
    """Reload pool from data/cli_accounts.json (after external register)."""
    total = cli_pool.reload()
    return {
        "ok": True,
        "count": total,
        "usable": cli_pool.count(enabled_only=True),
        "accounts": cli_pool.list_public(),
    }


@router.get("/admin/api/usage")
async def get_usage(_: None = Depends(require_admin)) -> Any:
    return usage_logger.snapshot()


@router.get("/admin/api/health")
async def admin_health(_: None = Depends(require_admin)) -> Any:
    return {
        "mode": "cli",
        "cli_pool_usable": cli_pool.count(enabled_only=True),
        "cli_pool_total": cli_pool.count(enabled_only=False),
    }
