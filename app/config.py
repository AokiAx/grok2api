from __future__ import annotations

from pathlib import Path
from typing import Any

from pydantic_settings import BaseSettings, SettingsConfigDict

from .unified_config import load_unified_config


def _file_defaults() -> dict[str, Any]:
    """Map unified config.json keys → Settings field names."""
    raw = load_unified_config()
    if not raw:
        return {}
    mapping = {
        "host": "host",
        "port": "port",
        "api_key": "api_key",
        "app_key": "app_key",
        "panel_password": "panel_password",
        "proxy_base_url": "proxy_base_url",
        "client_version": "client_version",
        "auto_client_version": "auto_client_version",
        "default_model": "default_model",
        "data_dir": "data_dir",
        "cli_pool_sync_auth_json": "cli_pool_sync_auth_json",
        "cli_pool_rotate": "cli_pool_rotate",
        "ensure_auth_on_start": "ensure_auth_on_start",
        "stream_fallback": "stream_fallback",
        "fold_reasoning": "fold_reasoning",
        "cors_origins": "cors_origins",
        "timeout_secs": "timeout_secs",
        "refresh_skew_secs": "refresh_skew_secs",
        "normalize_content": "normalize_content",
        "usage_log_enabled": "usage_log_enabled",
    }
    out: dict[str, Any] = {}
    for src, dst in mapping.items():
        if src in raw and raw[src] is not None:
            out[dst] = raw[src]
    return out


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="GROK2API_",
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    host: str = "127.0.0.1"
    port: int = 8787
    api_key: str = ""
    app_key: str = ""
    # Web panel password (falls back to app_key / api_key if empty)
    panel_password: str = ""
    mode: str = "cli"

    proxy_base_url: str = "https://cli-chat-proxy.grok.com/v1"
    client_version: str = "0.2.93"
    auto_client_version: bool = True
    user_agent: str = "xai-grok-build/0.2.93"
    token_auth: str = "xai-grok-cli"
    grok_home: Path = Path.home() / ".grok"
    auth_file: Path | None = None
    refresh_skew_secs: int = 300
    default_model: str = "grok-4.5"
    timeout_secs: float = 600.0
    ensure_auth_on_start: bool = True

    data_dir: Path = Path("data")
    cli_pool_sync_auth_json: bool = True
    cli_pool_rotate: bool = True
    cli_pool_import_auth_on_start: bool = True
    sso_max_fails: int = 5

    force_upstream_stream: bool = False
    stream_fallback: bool = True
    fold_reasoning: bool = False
    cors_origins: str = ""
    normalize_content: bool = True
    usage_log_enabled: bool = True

    def __init__(self, **kwargs: Any) -> None:
        # file defaults < explicit kwargs < env (pydantic env applied after)
        merged = {**_file_defaults(), **kwargs}
        super().__init__(**merged)

    @property
    def resolved_auth_file(self) -> Path:
        if self.auth_file is not None:
            return self.auth_file
        return self.grok_home / "auth.json"

    @property
    def resolved_data_dir(self) -> Path:
        p = self.data_dir
        if not p.is_absolute():
            p = Path.cwd() / p
        return p

    @property
    def resolved_admin_key(self) -> str:
        return (self.app_key or self.api_key or "").strip()

    @property
    def resolved_panel_password(self) -> str:
        return (
            self.panel_password or self.app_key or self.api_key or ""
        ).strip()

    @property
    def resolved_client_version(self) -> str:
        from .version_detect import resolve_client_version

        return resolve_client_version(
            self.client_version, auto=self.auto_client_version
        )

    @property
    def resolved_user_agent(self) -> str:
        ver = self.resolved_client_version
        if self.user_agent.startswith("xai-grok-build/"):
            return f"xai-grok-build/{ver}"
        return self.user_agent

    def cors_origin_list(self) -> list[str]:
        raw = (self.cors_origins or "").strip()
        if not raw:
            return []
        return [o.strip() for o in raw.split(",") if o.strip()]


settings = Settings()
