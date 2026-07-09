#!/usr/bin/env python3
"""Hammer a single free CLI account until quota/rate limit errors appear."""

from __future__ import annotations

import argparse
import json
import time
from collections import Counter
from pathlib import Path

import httpx


def load_account(pool_path: Path, email: str | None) -> dict:
    data = json.loads(pool_path.read_text(encoding="utf-8"))
    accounts = data.get("accounts") or []
    if not accounts:
        raise SystemExit(f"no accounts in {pool_path}")
    if email:
        for a in accounts:
            if a.get("email") == email or a.get("id") == email:
                return a
        raise SystemExit(f"account not found: {email}")
    # pick newest by expires_at / created
    return accounts[-1]


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument(
        "--pool",
        default="data/cli_accounts.json",
        help="path to cli_accounts.json (local or scp'd)",
    )
    p.add_argument("--email", default=None, help="pin this account email/id")
    p.add_argument(
        "--proxy",
        default="https://cli-chat-proxy.grok.com/v1",
    )
    p.add_argument("--model", default="grok-4.5")
    p.add_argument("--client-version", default="0.2.93")
    p.add_argument("--max-requests", type=int, default=200)
    p.add_argument("--sleep", type=float, default=0.0, help="delay between requests")
    p.add_argument("--max-tokens", type=int, default=8)
    p.add_argument(
        "--stop-on",
        default="402,429",
        help="comma statuses that stop the burn (empty=never stop on status)",
    )
    args = p.parse_args()

    acc = load_account(Path(args.pool), args.email)
    token = acc.get("key") or ""
    if not token:
        raise SystemExit("account has no access token")
    email = acc.get("email") or acc.get("id")
    print(f"=== burn one account ===")
    print(f"email={email}")
    print(f"user_id={acc.get('user_id')}")
    print(f"token_preview={token[:12]}…{token[-6:]}")
    print(f"max_requests={args.max_requests} sleep={args.sleep}")

    stop_on = {
        int(x.strip())
        for x in (args.stop_on or "").split(",")
        if x.strip().isdigit()
    }

    headers = {
        "Authorization": f"Bearer {token}",
        "X-XAI-Token-Auth": "xai-grok-cli",
        "x-grok-client-version": args.client_version,
        "x-grok-model-override": args.model,
        "User-Agent": f"xai-grok-build/{args.client_version}",
        "Content-Type": "application/json",
        "Accept": "application/json",
    }
    body = {
        "model": args.model,
        "messages": [
            {
                "role": "user",
                "content": f"Reply with exactly: ok-{int(time.time())}",
            }
        ],
        "max_tokens": args.max_tokens,
        "stream": False,
        "reasoning_effort": "low",
    }

    statuses: Counter[int] = Counter()
    errors: Counter[str] = Counter()
    ok = 0
    t0 = time.perf_counter()
    first_fail: dict | None = None

    with httpx.Client(timeout=90.0) as client:
        # billing snapshot
        try:
            br = client.get(f"{args.proxy.rstrip('/')}/billing", headers=headers)
            print(f"billing_status={br.status_code} body={br.text[:300]}")
        except Exception as e:
            print(f"billing_error={e}")

        for i in range(1, args.max_requests + 1):
            body["messages"][0]["content"] = f"Reply with exactly: ok-{i}"
            tr = time.perf_counter()
            try:
                r = client.post(
                    f"{args.proxy.rstrip('/')}/chat/completions",
                    headers=headers,
                    json=body,
                )
                ms = (time.perf_counter() - tr) * 1000
                statuses[r.status_code] += 1
                if r.status_code == 200:
                    ok += 1
                    model = None
                    try:
                        model = r.json().get("model")
                    except Exception:
                        pass
                    if i == 1 or i % 10 == 0:
                        print(f"  [{i}] 200 {ms:.0f}ms model={model}")
                else:
                    err = r.text[:400]
                    errors[f"{r.status_code}:{err[:120]}"] += 1
                    print(f"  [{i}] FAIL {r.status_code} {ms:.0f}ms {err[:200]}")
                    if first_fail is None:
                        first_fail = {
                            "n": i,
                            "status": r.status_code,
                            "body": err,
                            "ms": ms,
                        }
                    if r.status_code in stop_on:
                        print(f"stop: status {r.status_code} in stop-on")
                        break
            except Exception as e:
                statuses[0] += 1
                errors[f"EXC:{type(e).__name__}:{e}"] += 1
                print(f"  [{i}] EXC {e}")
                if first_fail is None:
                    first_fail = {"n": i, "status": 0, "body": str(e), "ms": 0}
                break
            if args.sleep > 0:
                time.sleep(args.sleep)

    wall = time.perf_counter() - t0
    print("\n=== result ===")
    print(f"account={email}")
    print(f"ok={ok} wall_s={wall:.1f} rps={ok / wall if wall else 0:.2f}")
    print(f"status_counts={dict(statuses)}")
    if first_fail:
        print(
            f"first_fail at request#{first_fail['n']} "
            f"status={first_fail['status']}\n  {first_fail['body'][:500]}"
        )
    print("error_kinds:")
    for k, n in errors.most_common(10):
        print(f"  [{n}x] {k}")


if __name__ == "__main__":
    main()
