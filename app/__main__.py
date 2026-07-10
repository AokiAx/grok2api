"""python -m app login | status | serve | register | cli-pool | mint-cli"""

from __future__ import annotations

import argparse
import json
import logging
import sys


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m app",
        description="Grok2API: OpenAI-compatible proxy over xAI CLI OIDC",
    )
    sub = parser.add_subparsers(dest="cmd", required=True)

    p_login = sub.add_parser(
        "login",
        help="Ensure CLI auth: silent refresh, else browser/device/cli login",
    )
    p_login.add_argument(
        "--method",
        choices=("auto", "refresh", "silent", "browser", "device", "cli"),
        default="auto",
    )
    p_login.add_argument("--force", action="store_true")

    sub.add_parser("status", help="Show CLI auth + CLI account pool")

    p_serve = sub.add_parser("serve", help="Start OpenAI-compatible proxy (CLI only)")
    p_serve.add_argument("--host", default=None)
    p_serve.add_argument("--port", type=int, default=None)

    p_reg = sub.add_parser(
        "register",
        help="(Optional private module) auto-register → CLI OIDC",
    )
    p_reg.add_argument("--auto", action="store_true", help="Non-interactive")
    p_reg.add_argument("--count", type=int, default=None)
    p_reg.add_argument("--workers", type=int, default=None)
    sub.add_parser("auto-register", help="Alias of register --auto")

    p_cli = sub.add_parser("cli-pool", help="Show multi-account CLI OIDC pool")
    p_cli.add_argument("--delete", default=None)

    p_mint = sub.add_parser(
        "mint-cli",
        help="Email+password+turnstile → CLI OIDC tokens",
    )
    p_mint.add_argument("--email", required=True)
    p_mint.add_argument("--password", required=True)
    p_mint.add_argument("--turnstile", default=None)
    p_mint.add_argument("--proxy", default=None)

    args = parser.parse_args(argv)
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    if args.cmd == "status":
        from .auth import auth_store
        from .cli_pool import cli_pool
        from .config import settings

        out = {
            "mode": "cli",
            "cli_auth": auth_store.status(),
            "cli_pool": {
                "usable": cli_pool.count(enabled_only=True),
                "total": cli_pool.count(enabled_only=False),
                "accounts": cli_pool.list_public(),
            },
            "proxy_base_url": settings.proxy_base_url,
        }
        print(json.dumps(out, indent=2, ensure_ascii=False))
        return 0

    if args.cmd == "login":
        from .oauth_login import ensure_login

        try:
            result = ensure_login(
                force_browser=args.force,
                method=args.method
                if not args.force
                else (
                    "browser"
                    if args.method in ("auto", "refresh", "browser")
                    else args.method
                ),
            )
        except Exception as e:
            print(f"[grok2api] login failed: {e}", file=sys.stderr)
            return 1
        safe = {
            k: v for k, v in result.items() if k not in ("key", "refresh_token")
        }
        print(json.dumps(safe, indent=2, ensure_ascii=False))
        return 0

    if args.cmd == "serve":
        import uvicorn

        from .config import settings

        host = args.host or settings.host
        port = args.port or settings.port
        uvicorn.run("app.main:app", host=host, port=port, reload=False)
        return 0

    if args.cmd in ("register", "auto-register"):
        # Register stack is intentionally private / not shipped in public repo.
        try:
            from register.pipeline import run_batch
            from register.settings import CFG
        except Exception as e:
            print(
                "[grok2api] register module not available in this build.\n"
                "  Public release is CLI proxy + panel only.\n"
                "  Use: python -m app login  |  import tokens into the panel.\n"
                f"  detail: {e}",
                file=sys.stderr,
            )
            return 1

        print("[grok2api] register → CLI OIDC → SQLite account repository")
        run_batch(
            total_accounts=getattr(args, "count", None)
            or int(CFG.get("total_accounts") or 1),
            max_workers=getattr(args, "workers", None)
            or int(CFG.get("max_workers") or 1),
            proxy=CFG.get("proxy") or None,
            capmonster_key=str(CFG.get("capmonster_api_key") or ""),
            turnstile_timeout=int(CFG.get("turnstile_timeout") or 120),
            email_code_timeout=int(CFG.get("email_code_timeout") or 120),
        )
        try:
            from .cli_pool import cli_pool

            cli_pool.reload()

            print(
                json.dumps(
                    {
                        "cli_pool_usable": cli_pool.count(enabled_only=True),
                        "accounts": cli_pool.list_public(),
                    },
                    indent=2,
                    ensure_ascii=False,
                )
            )
        except Exception:
            pass
        return 0

    if args.cmd == "cli-pool":
        from .cli_pool import cli_pool

        if args.delete:
            ok = cli_pool.delete(args.delete)
            print(json.dumps({"ok": ok}, indent=2))
            return 0 if ok else 1
        print(
            json.dumps(
                {
                    "usable": cli_pool.count(enabled_only=True),
                    "total": cli_pool.count(enabled_only=False),
                    "accounts": cli_pool.list_public(),
                },
                indent=2,
                ensure_ascii=False,
            )
        )
        return 0

    if args.cmd == "mint-cli":
        from .oidc_mint import mint_cli_from_password

        turnstile = args.turnstile
        if not turnstile:
            print(
                "[mint-cli] --turnstile required (or use: python -m app register)",
                file=sys.stderr,
            )
            return 2
        try:
            result = mint_cli_from_password(
                args.email,
                args.password,
                turnstile,
                proxy=args.proxy,
                save_to_pool=True,
            )
        except Exception as e:
            print(f"[mint-cli] failed: {e}", file=sys.stderr)
            return 1
        safe = {
            k: v for k, v in result.items() if k not in ("access_token", "refresh_token")
        }
        print(json.dumps(safe, indent=2, ensure_ascii=False))
        return 0

    return 2


if __name__ == "__main__":
    raise SystemExit(main())
