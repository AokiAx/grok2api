"""LEGACY batch registration pipeline (deprecated).

Production path is Go `internal/register`. This file is kept only as a
reference implementation of the accounts.x.ai signup protocol. Mint via
`app.oidc_mint` is no longer in-tree — do not run for production.
"""
from __future__ import annotations

import json
import logging
import os
import secrets
import sys
import time
import traceback
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from pathlib import Path
from typing import Optional, Tuple

from .providers import create_email_provider, get_provider_label
from .providers.base import generate_random_password
from .registrar import GrokRegistrar
from .settings import (
    CAPMONSTER_DEFAULT_API_KEY,
    CFMAIL_ACCOUNTS_DEFAULT,
    CFMAIL_PROFILE_MODE_DEFAULT,
    CFG,
    EMAIL_PROVIDER,
    FIRST_NAME_POOL,
    IMPERSONATE_BROWSER,
    LAST_NAME_POOL,
    MAILTM_API_BASE_DEFAULT,
    MAILTM_DOMAIN_DEFAULT,
    PROXY_DEFAULT,
    TOKEN_JSON_DIR,
    TURNSTILE_SOLVER_MODE,
    TURNSTILE_SOLVER_URL,
    _file_lock,
    _print_lock,
    project_root,
)

log = logging.getLogger("register.pipeline")


def generate_random_name() -> Tuple[str, str]:
    return secrets.choice(FIRST_NAME_POOL), secrets.choice(LAST_NAME_POOL)


def _save_grok_token(email: str, password: str, session_data: dict) -> str:
    """保存注册结果到 grok_tokens/{email}.json，返回文件路径。"""
    from datetime import datetime, timedelta, timezone

    now = datetime.now(tz=timezone(timedelta(hours=8)))

    token_data = {
        "type": "cli_register",
        "email": email,
        "password": password,
        "sso": session_data.get("sso", ""),
        "sso_rw": session_data.get("sso_rw", ""),
        "registered_at": now.strftime("%Y-%m-%dT%H:%M:%S+08:00"),
        "note": "intermediate; CLI tokens in data/cli_accounts.json",
    }

    if os.path.isabs(TOKEN_JSON_DIR):
        token_dir = TOKEN_JSON_DIR
    else:
        # paths in config.json are relative to project root
        token_dir = str(project_root() / TOKEN_JSON_DIR)
    os.makedirs(token_dir, exist_ok=True)
    token_path = os.path.join(token_dir, f"{email}.json")
    with _file_lock:
        with open(token_path, "w", encoding="utf-8") as f:
            json.dump(token_data, f, ensure_ascii=False, indent=2)
    return token_path




# ══════════════════════════════════════════════════════════════
#  注册后铸造 CLI OIDC 凭证（cli-chat-proxy）
# ══════════════════════════════════════════════════════════════


def _mint_cli_credentials_after_register(
    *,
    email: str,
    password: str,
    registrar: "GrokRegistrar",
    proxy: Optional[str] = None,
) -> bool:
    """
    注册成功后铸造 CLI OIDC 凭证（cli-chat-proxy 用）:
      1) 优先复用 registrar.session（已有 SSO/auth cookie 链）
      2) 失败则 createSession + 新 Turnstile 再 PKCE
    写入 data/cli_accounts.json，并同步 ~/.grok/auth.json。
    """
    root = str(Path(__file__).resolve().parents[1])
    if root not in sys.path:
        sys.path.insert(0, root)

    try:
        from app.oidc_mint import mint_cli_after_register
    except Exception as e:
        with _print_lock:
            print(f"  [cli-oidc] 无法加载 app.oidc_mint: {e}")
        return False

    def _solve_ts() -> str:
        return registrar._resolve_turnstile_token(
            capmonster_key=CAPMONSTER_DEFAULT_API_KEY or None,
            turnstile_timeout=int(CFG.get("turnstile_timeout") or 120),
        )

    try:
        registrar._print(
            "[cli-oidc] 铸造 CLI 凭证: session PKCE → 或 createSession+PKCE…"
        )
        result = mint_cli_after_register(
            email=email,
            password=password,
            registrar_session=getattr(registrar, "session", None),
            turnstile_solver=_solve_ts,
            proxy=proxy or PROXY_DEFAULT or None,
            impersonate=IMPERSONATE_BROWSER,
        )
        with _print_lock:
            print(
                f"  [cli-oidc] OK account={result.get('account_id')} "
                f"refresh={result.get('has_refresh_token')} "
                f"pool={result.get('pool_usable')} "
                f"expires_in={result.get('expires_in')} "
                f"scope={result.get('scope')}"
            )
        return bool(result.get("ok"))
    except Exception as e:
        with _print_lock:
            print(f"  [cli-oidc] 失败: {e}")
        log.exception("CLI OIDC mint after register failed")
        return False


# ══════════════════════════════════════════════════════════════
#  并发批量注册
# ══════════════════════════════════════════════════════════════


@dataclass
class BatchConfig:
    count: int
    workers: int
    capmonster_api_key: str
    turnstile_timeout: int
    email_code_timeout: int
    proxy: Optional[str]


def _register_one(
    idx: int, total: int, cfg: BatchConfig
) -> Tuple[bool, str, Optional[str]]:
    """单个注册任务，返回 (ok, email, error_msg)"""
    registrar = None
    try:
        registrar = GrokRegistrar(proxy=cfg.proxy, tag=str(idx))

        provider_label = registrar.get_mail_provider_label()

        # 创建邮箱
        registrar._print(f"[{provider_label}] 创建邮箱...")
        email, mail_token = registrar.create_mailbox()

        # 生成注册信息
        password = generate_random_password()
        given_name, family_name = generate_random_name()

        registrar.tag = email.split("@")[0]

        with _print_lock:
            print(f"\n{'=' * 60}")
            print(f"  [{idx}/{total}] 注册: {email}")
            print(f"  密码: {password}")
            print(f"  姓名: {given_name} {family_name}")
            print(f"{'=' * 60}")

        # 执行注册
        result = registrar.register(
            email=email,
            password=password,
            given_name=given_name,
            family_name=family_name,
            mail_token=mail_token,
            capmonster_api_key=cfg.capmonster_api_key,
            turnstile_timeout=cfg.turnstile_timeout,
            email_code_timeout=cfg.email_code_timeout,
        )

        status_code = result.get("status_code")
        session_data = result.get("session") or {}
        sso_value = session_data.get("sso", "")
        ok = status_code == 200 and bool(sso_value)

        if not ok:
            action_err = result.get("action_error", "")
            detail = f", action_error={action_err}" if action_err else ""
            raise RuntimeError(
                f"注册返回 HTTP {status_code}, SSO {'present' if sso_value else 'missing'}{detail}"
            )

        token_path = _save_grok_token(email, password, session_data)
        registrar._print(f"[save] {os.path.basename(token_path)}")

        cli_ok = _mint_cli_credentials_after_register(
            email=email,
            password=password,
            registrar=registrar,
            proxy=cfg.proxy,
        )
        if not cli_ok:
            raise RuntimeError(
                "注册成功但 CLI OIDC 凭证铸造失败 "
                "(createSession+PKCE)。可稍后: python -m app mint-cli --email ... --password ..."
            )

        with _print_lock:
            print(f"\n[OK] [{registrar.tag}] {email} 注册+CLI凭证 成功!")
        return True, email, None

    except Exception as e:
        error_msg = str(e)
        email_str = (
            registrar.tag if registrar and registrar.tag else f"task-{idx}"
        )
        # provider 失败标记
        if registrar:
            registrar.email_provider.record_failure(reason=error_msg[:200])
        with _print_lock:
            print(f"\n[FAIL] [{email_str}] 注册失败: {error_msg}")
            traceback.print_exc()
        return False, email_str, error_msg


def _create_default_provider():
    return create_email_provider(
        EMAIL_PROVIDER,
        cfmail_profile=CFMAIL_PROFILE_MODE_DEFAULT,
        cfmail_accounts=list(CFMAIL_ACCOUNTS_DEFAULT)
        if CFMAIL_ACCOUNTS_DEFAULT
        else None,
        mailtm_api_base=MAILTM_API_BASE_DEFAULT,
        mailtm_domain=MAILTM_DOMAIN_DEFAULT,
    )


def run_batch(
    total_accounts: int = 3,
    max_workers: int = 3,
    proxy: Optional[str] = None,
    capmonster_key: str = "",
    turnstile_timeout: int = 120,
    email_code_timeout: int = 120,
):
    """并发批量注册"""
    provider_label = get_provider_label(EMAIL_PROVIDER)

    # 检查邮箱配置
    _tmp_provider = _create_default_provider()
    if not _tmp_provider.has_accounts:
        print(f"❌ 错误: 未找到可用的 {provider_label} 配置")
        if _tmp_provider.config_path:
            print(f"   请检查配置文件: {_tmp_provider.config_path}")
        return

    actual_workers = min(max_workers, total_accounts)

    cfg = BatchConfig(
        count=total_accounts,
        workers=actual_workers,
        capmonster_api_key=capmonster_key,
        turnstile_timeout=turnstile_timeout,
        email_code_timeout=email_code_timeout,
        proxy=proxy,
    )

    names = _tmp_provider.get_account_names()
    print(f"\n{'#' * 60}")
    print(f"  Grok 批量自动注册")
    print(f"  注册数量: {total_accounts} | 并发数: {actual_workers}")
    print(f"  邮箱 provider: {provider_label} (模式: {_tmp_provider.profile_mode})")
    print(f"  {provider_label} 配置: {names or '-'}")
    if TURNSTILE_SOLVER_MODE == "local":
        print(f"  过盾方式: 本地过盾机 ({TURNSTILE_SOLVER_URL})")
    elif TURNSTILE_SOLVER_MODE == "capmonster":
        print("  过盾方式: CapMonster")
    else:
        print(
            f"  过盾方式: auto (本地过盾机优先: {TURNSTILE_SOLVER_URL}, "
            "失败后回退 CapMonster)"
        )
    print(f"  备份: {TOKEN_JSON_DIR}/ | CLI: data/cli_accounts.json")
    print(f"{'#' * 60}\n")

    success_count = 0
    fail_count = 0
    start_time = time.time()

    with ThreadPoolExecutor(max_workers=actual_workers) as executor:
        futures = {}
        for idx in range(1, total_accounts + 1):
            future = executor.submit(
                _register_one, idx, total_accounts, cfg
            )
            futures[future] = idx

        for future in as_completed(futures):
            idx = futures[future]
            try:
                ok, email, err = future.result()
                if ok:
                    success_count += 1
                else:
                    fail_count += 1
                    print(f"  [账号 {idx}] 失败: {err}")
            except Exception as e:
                fail_count += 1
                with _print_lock:
                    print(f"[FAIL] 账号 {idx} 线程异常: {e}")

    elapsed = time.time() - start_time
    avg = elapsed / total_accounts if total_accounts else 0
    print(f"\n{'#' * 60}")
    print(f"  注册完成! 耗时 {elapsed:.1f} 秒")
    print(f"  总数: {total_accounts} | 成功: {success_count} | 失败: {fail_count}")
    print(f"  平均速度: {avg:.1f} 秒/个")
    if success_count > 0:
        print(f"  备份: {TOKEN_JSON_DIR}/ | CLI: data/cli_accounts.json")
    print(f"{'#' * 60}")


# ══════════════════════════════════════════════════════════════
#  CLI 入口
# ══════════════════════════════════════════════════════════════



def main() -> None:
    """python -m register — auto from config, mint CLI OIDC."""
    print("=" * 60)
    print("  Grok register -> CLI OIDC")
    print("=" * 60)
    run_batch(
        total_accounts=int(CFG.get("total_accounts") or 1),
        max_workers=int(CFG.get("max_workers") or 1),
        proxy=PROXY_DEFAULT or None,
        capmonster_key=str(CAPMONSTER_DEFAULT_API_KEY or ""),
        turnstile_timeout=int(CFG.get("turnstile_timeout") or 120),
        email_code_timeout=int(CFG.get("email_code_timeout") or 120),
    )



