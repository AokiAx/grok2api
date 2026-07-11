#!/usr/bin/env python3
"""Probe permission-denied accounts and move usable ones back to ready."""
from __future__ import annotations

import json
import sqlite3
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
from pathlib import Path
from threading import Lock

DB = Path("data/grok2api.db")
BASE = "https://cli-chat-proxy.grok.com/v1"
VER = "0.2.93"
WORKERS = 8
MODEL = "grok-4.5"

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

_print_lock = Lock()


def log(msg: str) -> None:
    with _print_lock:
        print(msg, flush=True)


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f") + "Z"


def probe(token: str) -> tuple[str, str | None, int | None, int | None, str]:
    """Returns (status, rem, actual, limit, detail). status: ok|auth|quota|other|error"""
    headers = {
        "Authorization": f"Bearer {token}",
        "X-XAI-Token-Auth": "xai-grok-cli",
        "User-Agent": f"xai-grok-build/{VER}",
        "x-grok-client-version": VER,
        "x-grok-model-override": MODEL,
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
    }
    payload = json.dumps(
        {
            "model": MODEL,
            "input": [{"role": "user", "content": "ping"}],
            "stream": True,
            "max_output_tokens": 8,
        }
    ).encode()
    req = urllib.request.Request(
        BASE + "/responses", data=payload, headers=headers, method="POST"
    )
    try:
        with urllib.request.urlopen(req, timeout=25) as resp:
            rem_s = resp.headers.get("x-ratelimit-remaining-tokens")
            lim_s = resp.headers.get("x-ratelimit-limit-tokens")
            rem = int(rem_s) if rem_s and rem_s.isdigit() else None
            lim = int(lim_s) if lim_s and lim_s.isdigit() else None
            actual = (lim - rem) if rem is not None and lim is not None else None
            resp.read(64)
            if rem == 0 and lim is not None:
                return "quota", rem_s, actual, lim, "remaining=0"
            return "ok", rem_s, actual, lim, f"HTTP {resp.status}"
    except urllib.error.HTTPError as e:
        body = e.read(400).decode("utf-8", "replace")
        code = ""
        try:
            code = json.loads(body).get("code") or ""
        except Exception:
            pass
        lower = (body + " " + code).lower()
        if "usage-exhausted" in lower or "free-usage" in lower or "spending-limit" in lower:
            return "quota", None, None, None, f"HTTP {e.code} {code or body[:80]}"
        if e.code in (401, 403):
            return "auth", None, None, None, f"HTTP {e.code} {code or body[:80]}"
        if e.code == 429:
            return "cooldown", None, None, None, f"HTTP 429 {code or body[:80]}"
        return "other", None, None, None, f"HTTP {e.code} {code or body[:80]}"
    except Exception as e:
        return "error", None, None, None, str(e)


def refresh_token(row: sqlite3.Row) -> str | None:
    rt = (row["refresh_token"] or "").strip()
    issuer = (row["oidc_issuer"] or "https://auth.x.ai").rstrip("/")
    client_id = (row["oidc_client_id"] or "b1a00492-073a-47ea-816f-4c329264a828").strip()
    if not rt:
        return None
    form = urllib.parse.urlencode(
        {
            "grant_type": "refresh_token",
            "refresh_token": rt,
            "client_id": client_id,
        }
    ).encode()
    req = urllib.request.Request(
        issuer + "/oauth2/token",
        data=form,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            data = json.loads(resp.read())
            return data.get("access_token") or None
    except Exception:
        return None


def process_one(row: sqlite3.Row) -> dict:
    email = row["email"] or row["id"]
    token = row["access_token"]
    status, rem, actual, lim, detail = probe(token)
    new_token = None
    if status in ("auth", "error"):
        refreshed = refresh_token(row)
        if refreshed:
            new_token = refreshed
            status, rem, actual, lim, detail = probe(refreshed)
            detail = "after-refresh: " + detail
    return {
        "id": row["id"],
        "email": email,
        "status": status,
        "rem": rem,
        "actual": actual,
        "limit": lim,
        "detail": detail,
        "new_token": new_token,
    }


def main() -> int:
    if not DB.exists():
        print("db missing:", DB)
        return 1

    con = sqlite3.connect(DB)
    con.row_factory = sqlite3.Row
    rows = con.execute(
        """
        SELECT * FROM accounts
        WHERE unavailable_reason = 'auth'
          AND last_error_code = 'permission-denied'
          AND access_token IS NOT NULL
          AND length(access_token) > 50
        ORDER BY datetime(created_at) DESC
        """
    ).fetchall()
    log(f"candidates: {len(rows)}  workers={WORKERS}")
    if not rows:
        return 0

    results: list[dict] = []
    with ThreadPoolExecutor(max_workers=WORKERS) as ex:
        futs = {ex.submit(process_one, r): r["id"] for r in rows}
        done = 0
        for fut in as_completed(futs):
            done += 1
            try:
                res = fut.result()
            except Exception as e:
                res = {
                    "id": futs[fut],
                    "email": "?",
                    "status": "error",
                    "rem": None,
                    "actual": None,
                    "limit": None,
                    "detail": str(e),
                    "new_token": None,
                }
            results.append(res)
            if done % 10 == 0 or res["status"] != "ok":
                log(
                    f"[{done}/{len(rows)}] {res['status']:8} {str(res['email'])[:40]:40} {res['detail'][:80]}"
                )

    now = utc_now()
    ok = [r for r in results if r["status"] == "ok"]
    quota = [r for r in results if r["status"] == "quota"]
    cooldown = [r for r in results if r["status"] == "cooldown"]
    auth = [r for r in results if r["status"] == "auth"]
    other = [r for r in results if r["status"] not in ("ok", "quota", "cooldown", "auth")]

    recovered = 0
    for r in ok:
        fields = [
            "pool=?",
            "unavailable_reason=?",
            "retry_at=?",
            "last_error_code=?",
            "last_success_at=?",
            "updated_at=?",
            "authentication_fails=0",
        ]
        # retry_at is NOT NULL DEFAULT '' in schema
        params: list = ["ready", "", "", "", now, now]
        if r["actual"] is not None:
            fields.append("quota_actual=?")
            params.append(r["actual"])
        if r["limit"] is not None:
            fields.append("quota_limit=?")
            params.append(r["limit"])
        if r["new_token"]:
            fields.append("access_token=?")
            params.append(r["new_token"])
        params.append(r["id"])
        con.execute(
            f"UPDATE accounts SET {', '.join(fields)} WHERE id=?",
            params,
        )
        recovered += 1

    # quota-exhausted but auth works → keep unavailable(quota)
    for r in quota:
        fields = [
            "pool=?",
            "unavailable_reason=?",
            "last_error_code=?",
            "updated_at=?",
            "authentication_fails=0",
        ]
        params: list = [
            "unavailable",
            "quota",
            "subscription:free-usage-exhausted",
            now,
        ]
        if r["new_token"]:
            fields.append("access_token=?")
            params.append(r["new_token"])
        params.append(r["id"])
        con.execute(
            f"UPDATE accounts SET {', '.join(fields)} WHERE id=?",
            params,
        )

    con.commit()

    # pool stats after
    stats = list(
        con.execute(
            "SELECT pool, unavailable_reason, count(*) c FROM accounts GROUP BY 1,2 ORDER BY c DESC"
        )
    )
    con.close()

    log("")
    log("=" * 60)
    log(f"probed:     {len(results)}")
    log(f"recovered:  {recovered} → ready")
    log(f"quota:      {len(quota)} (reclassified quota)")
    log(f"cooldown:   {len(cooldown)}")
    log(f"still auth: {len(auth)}")
    log(f"other/err:  {len(other)}")
    log("pool after:")
    for s in stats:
        log(f"  {s[0]:12} {s[1] or '-':12} {s[2]}")

    # sample failures
    if auth:
        log("\nstill-auth samples:")
        for r in auth[:8]:
            log(f"  {r['email']}: {r['detail'][:100]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
