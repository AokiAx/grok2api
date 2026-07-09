#!/usr/bin/env python3
"""Concurrent stress test for grok2api CLI proxy."""

from __future__ import annotations

import argparse
import asyncio
import statistics
import time
from collections import Counter

import httpx


def pct(xs: list[float], p: float) -> float | None:
    if not xs:
        return None
    xs = sorted(xs)
    k = (len(xs) - 1) * p / 100
    f = int(k)
    c = k - f
    if f + 1 < len(xs):
        return xs[f] * (1 - c) + xs[f + 1] * c
    return xs[f]


async def one(
    client: httpx.AsyncClient,
    base: str,
    headers: dict[str, str],
    body: dict,
    i: int,
    sem: asyncio.Semaphore,
    timeout: float,
) -> dict:
    async with sem:
        t0 = time.perf_counter()
        try:
            r = await client.post(
                f"{base}/v1/chat/completions",
                headers=headers,
                json=body,
                timeout=timeout,
            )
            ms = (time.perf_counter() - t0) * 1000
            model = None
            err = None
            content = None
            try:
                data = r.json()
                if r.status_code < 400:
                    model = data.get("model")
                    msg = (data.get("choices") or [{}])[0].get("message") or {}
                    content = msg.get("content")
                else:
                    err = (data.get("error") or {}).get("message") or r.text[:240]
            except Exception:
                err = r.text[:240]
            return {
                "i": i,
                "status": r.status_code,
                "ms": ms,
                "ok": r.status_code == 200,
                "model": model,
                "content": content,
                "err": err,
            }
        except Exception as e:
            ms = (time.perf_counter() - t0) * 1000
            return {
                "i": i,
                "status": 0,
                "ms": ms,
                "ok": False,
                "model": None,
                "content": None,
                "err": f"{type(e).__name__}: {e}",
            }


async def run(args: argparse.Namespace) -> int:
    base = args.base.rstrip("/")
    headers = {
        "Authorization": f"Bearer {args.key}",
        "Content-Type": "application/json",
    }
    body = {
        "model": args.model,
        "messages": [
            {
                "role": "user",
                "content": args.prompt,
            }
        ],
        "max_tokens": args.max_tokens,
        "stream": False,
        "reasoning_effort": args.effort,
    }

    async with httpx.AsyncClient() as c:
        print("=== preflight ===")
        h = await c.get(f"{base}/health", timeout=20)
        health = h.json()
        pool = health.get("cli_pool") or {}
        print(f"health ok={health.get('ok')} version={health.get('version')}")
        print(f"pool usable={pool.get('usable')} total={pool.get('total')}")
        m = await c.get(f"{base}/v1/models", headers=headers, timeout=20)
        ids = [x.get("id") for x in (m.json().get("data") or [])]
        print(f"models status={m.status_code} ids={ids}")

    print(
        f"\n=== stress concurrency={args.concurrency} total={args.total} "
        f"model={args.model} effort={args.effort} ==="
    )
    sem = asyncio.Semaphore(args.concurrency)
    t0 = time.perf_counter()
    limits = httpx.Limits(
        max_connections=args.concurrency + 10,
        max_keepalive_connections=args.concurrency,
    )
    async with httpx.AsyncClient(limits=limits) as client:
        results = await asyncio.gather(
            *[
                one(client, base, headers, body, i, sem, args.timeout)
                for i in range(args.total)
            ]
        )
    wall = time.perf_counter() - t0

    oks = [r for r in results if r["ok"]]
    fails = [r for r in results if not r["ok"]]
    lat = [r["ms"] for r in oks]
    statuses = Counter(r["status"] for r in results)
    models = Counter(r["model"] for r in oks)
    err_kinds = Counter((r["err"] or "")[:100] for r in fails)

    print("\n=== summary ===")
    print(f"wall_time_s={wall:.2f}")
    print(
        f"total={args.total} ok={len(oks)} fail={len(fails)} "
        f"success_rate={len(oks) / args.total * 100:.1f}%"
    )
    print(f"rps={args.total / wall:.2f} ok_rps={len(oks) / wall:.2f}")
    print(f"status_counts={dict(statuses)}")
    if lat:
        print(
            "latency_ms: "
            f"min={min(lat):.0f} p50={pct(lat, 50):.0f} p90={pct(lat, 90):.0f} "
            f"p95={pct(lat, 95):.0f} p99={pct(lat, 99):.0f} max={max(lat):.0f} "
            f"avg={statistics.mean(lat):.0f}"
        )
    print(f"models={dict(models)}")
    if fails:
        print("\n=== fail samples ===")
        for e, n in err_kinds.most_common(10):
            print(f"  [{n}x] {e}")
        for r in fails[:8]:
            print(f"  #{r['i']} status={r['status']} ms={r['ms']:.0f} err={r['err']}")

    async with httpx.AsyncClient() as c:
        acc = await c.get(
            f"{base}/admin/api/cli-accounts", headers=headers, timeout=20
        )
        data = acc.json()
        print("\n=== pool after ===")
        print(f"usable={data.get('usable')} count={data.get('count')}")
        rows = sorted(
            data.get("accounts") or [],
            key=lambda a: -(a.get("request_count") or 0),
        )
        for a in rows:
            print(
                f"  {a.get('email')}: req={a.get('request_count')} "
                f"fail={a.get('fail_count')} en={a.get('enabled')} "
                f"left={a.get('seconds_left')}"
            )
    return 0 if len(fails) == 0 else 1


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--base", default="http://149.88.90.137:8787")
    p.add_argument("--key", default="sk-change-me")
    p.add_argument("--model", default="grok-4.5")
    p.add_argument("--concurrency", type=int, default=20)
    p.add_argument("--total", type=int, default=80)
    p.add_argument("--max-tokens", type=int, default=16)
    p.add_argument("--timeout", type=float, default=90.0)
    p.add_argument("--effort", default="low")
    p.add_argument(
        "--prompt",
        default="Reply with exactly one word: ok",
    )
    args = p.parse_args()
    raise SystemExit(asyncio.run(run(args)))


if __name__ == "__main__":
    main()
