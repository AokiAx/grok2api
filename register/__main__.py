"""Legacy entry.

Production registration is Go: `grok2api register` or the panel.
This module no longer runs the full register+mint pipeline (mint deps removed).

Use:
  python -m register          → print guidance
  python -m register solver   → hint how to start tools/turnstile
"""
from __future__ import annotations

import sys


def main() -> int:
    args = sys.argv[1:]
    if args and args[0] in {"solver", "turnstile"}:
        print(
            "Start the local Turnstile solver with:\n"
            "  cd register/tools/turnstile\n"
            "  xvfb-run -a .venv/bin/python api_solver.py "
            "--browser_type camoufox --thread 2 --host 0.0.0.0 --port 5072\n"
            "Then set turnstile_solver=local and turnstile_solver_url "
            "in data/register_settings.json."
        )
        return 0
    print(
        "DEPRECATED: python -m register is not the production registrar.\n"
        "Use Go instead:\n"
        "  grok2api register --config config.json --count N --workers W\n"
        "  or open /panel → 账号注册\n"
        "For the local captcha solver only: python -m register solver\n"
        "See register/README.md."
    )
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
