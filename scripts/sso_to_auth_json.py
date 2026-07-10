#!/usr/bin/env python3
"""
SSO cookie → ~/.grok/auth.json 格式（纯 HTTP Device Flow）

用法:
  # 从 tokens-all-all-all.json 批量转换
  python scripts/sso_to_auth_json.py --tokens-json tokens-all-all-all.json --out auth_from_tokens.json --merge

  # 单个 / 批量 SSO 列表
  python scripts/sso_to_auth_json.py --sso sso_list.txt --out-dir ./auth_out

  # 单行 sso
  python scripts/sso_to_auth_json.py --sso-cookie 'eyJ...' --out ~/.grok/auth.json
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
from pathlib import Path
from threading import Lock

from curl_cffi import requests

try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    sys.stderr.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass

CLIENT_ID = "b1a00492-073a-47ea-816f-4c329264a828"
OIDC_ISSUER = "https://auth.x.ai"
AUTH_KEY = f"{OIDC_ISSUER}::{CLIENT_ID}"
SCOPES = (
    "openid profile email offline_access grok-cli:access "
    "api:access conversations:read conversations:write"
)

_print_lock = Lock()


def b64url_decode(seg: str) -> bytes:
    seg += "=" * (-len(seg) % 4)
    return base64.urlsafe_b64decode(seg)


def decode_jwt_payload(token: str) -> dict:
    try:
        return json.loads(b64url_decode(token.split(".")[1]))
    except Exception:
        return {}


def rfc3339_ns(ts: float | None = None) -> str:
    if ts is None:
        ts = time.time()
    dt = datetime.fromtimestamp(ts, tz=timezone.utc)
    return dt.strftime("%Y-%m-%dT%H:%M:%S") + ".000000000Z"


def request_device_code() -> dict | None:
    data = urllib.parse.urlencode({"client_id": CLIENT_ID, "scope": SCOPES}).encode()
    req = urllib.request.Request(
        f"{OIDC_ISSUER}/oauth2/device/code",
        data=data,
        method="POST",
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        print(f"  ❌ device/code HTTP {e.code}: {e.read().decode()[:200]}")
        return None
    except Exception as e:
        print(f"  ❌ device/code: {e}")
        return None


def poll_token(
    device_code: str, interval: int, expires_in: int, timeout: int = 60
) -> dict | None:
    deadline = time.time() + min(expires_in, timeout)
    while time.time() < deadline:
        time.sleep(interval)
        data = urllib.parse.urlencode(
            {
                "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
                "client_id": CLIENT_ID,
                "device_code": device_code,
            }
        ).encode()
        req = urllib.request.Request(
            f"{OIDC_ISSUER}/oauth2/token",
            data=data,
            method="POST",
            headers={"Content-Type": "application/x-www-form-urlencoded"},
        )
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            try:
                err = json.loads(e.read())
            except Exception:
                print(f"  ❌ token HTTP {e.code}")
                return None
            error = err.get("error", "")
            if error == "authorization_pending":
                continue
            if error == "slow_down":
                interval += 5
                continue
            print(f"  ❌ token: {error}")
            return None
        except Exception as e:
            print(f"  ❌ token poll: {e}")
            return None
    print("  ❌ 轮询超时")
    return None


def sso_to_token(sso_cookie: str) -> dict | None:
    s = requests.Session()
    s.cookies.set("sso", sso_cookie, domain=".x.ai")

    try:
        r = s.get("https://accounts.x.ai/", impersonate="chrome", timeout=15)
    except Exception as e:
        print(f"  ❌ 网络错误: {e}")
        return None
    if "sign-in" in r.url or "sign-up" in r.url:
        print("  ❌ sso 无效")
        return None
    print("  ✅ sso 有效")

    print("  🔑 Device Flow...")
    dc = request_device_code()
    if not dc:
        return None
    print(f"  📋 user_code: {dc.get('user_code')}")

    try:
        s.get(dc["verification_uri_complete"], impersonate="chrome", timeout=15)
        r = s.post(
            f"{OIDC_ISSUER}/oauth2/device/verify",
            data={"user_code": dc["user_code"]},
            headers={"Content-Type": "application/x-www-form-urlencoded"},
            impersonate="chrome",
            timeout=15,
            allow_redirects=True,
        )
        if "consent" not in r.url:
            print(f"  ❌ verify 失败: {r.url}")
            return None
    except Exception as e:
        print(f"  ❌ verify 异常: {e}")
        return None

    try:
        r = s.post(
            f"{OIDC_ISSUER}/oauth2/device/approve",
            data={
                "user_code": dc["user_code"],
                "action": "allow",
                "principal_type": "User",
                "principal_id": "",
            },
            headers={"Content-Type": "application/x-www-form-urlencoded"},
            impersonate="chrome",
            timeout=15,
            allow_redirects=True,
        )
        if "done" not in r.url:
            print(f"  ❌ approve 失败: {r.url}")
            return None
        print("  ✅ 授权确认")
    except Exception as e:
        print(f"  ❌ approve 异常: {e}")
        return None

    token = poll_token(
        dc["device_code"],
        dc.get("interval", 5),
        dc.get("expires_in", 1800),
    )
    if not token:
        return None
    print(
        f"  ✅ access_token (expires_in={token.get('expires_in')}s)"
        + (" + refresh_token" if token.get("refresh_token") else "")
    )
    return token


def token_to_auth_entry(token: dict, email: str = "") -> tuple[str, dict]:
    access = token.get("access_token") or token.get("key") or ""
    refresh = token.get("refresh_token") or ""
    payload = decode_jwt_payload(access)

    user_id = payload.get("sub") or payload.get("principal_id") or ""
    principal_id = payload.get("principal_id") or user_id
    principal_type = payload.get("principal_type") or "User"

    expires_in = int(token.get("expires_in") or 21600)
    if "exp" in payload:
        expires_at = rfc3339_ns(float(payload["exp"]))
    else:
        expires_at = rfc3339_ns(time.time() + expires_in)

    iat = payload.get("iat")
    create_time = rfc3339_ns(float(iat) if iat else time.time())

    entry = {
        "key": access,
        "auth_mode": "oidc",
        "create_time": create_time,
        "user_id": user_id,
        "email": email or "",
        "principal_type": principal_type,
        "principal_id": principal_id,
        "refresh_token": refresh,
        "expires_at": expires_at,
        "oidc_issuer": OIDC_ISSUER,
        "oidc_client_id": CLIENT_ID,
    }
    return AUTH_KEY, entry


def write_auth_json(path: Path, auth_key: str, entry: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = {auth_key: entry}
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(
        json.dumps(data, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    os.replace(tmp, path)


def merge_auth_json(
    path: Path, auth_key: str, entry: dict, unique: bool = True
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    existing: dict = {}
    if path.exists():
        try:
            existing = json.loads(path.read_text(encoding="utf-8"))
        except Exception:
            existing = {}
    key = auth_key
    if unique and entry.get("user_id"):
        key = f"{auth_key}::{entry['user_id']}"
    existing[key] = entry
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(
        json.dumps(existing, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    os.replace(tmp, path)


def load_sso_list(path: str | None, single: str | None) -> list[str]:
    if single:
        return [single.strip()]
    if not path:
        return []
    out = []
    for line in Path(path).read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if "----" in line:
            parts = line.split("----")
            line = parts[-1].strip()
        out.append(line)
    return out


def load_tokens_json(path: str) -> list[str]:
    """Load SSO cookies from tokens-all-all-all.json style files."""
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    items: list[dict] = []
    if isinstance(data, dict):
        for value in data.values():
            if isinstance(value, list):
                items.extend(x for x in value if isinstance(x, dict))
            elif isinstance(value, dict) and "token" in value:
                items.append(value)
    elif isinstance(data, list):
        items = [x for x in data if isinstance(x, dict)]
    else:
        raise SystemExit(f"unsupported tokens json shape: {type(data).__name__}")

    out: list[str] = []
    seen: set[str] = set()
    for item in items:
        token = str(item.get("token") or item.get("sso") or "").strip()
        if not token or token in seen:
            continue
        seen.add(token)
        out.append(token)
    return out


def convert_one(
    index: int,
    total: int,
    sso: str,
    email: str,
) -> tuple[bool, str, dict | None, str]:
    with _print_lock:
        print(f"\n{'=' * 60}\n[{index}/{total}]\n{'=' * 60}")
    try:
        token = sso_to_token(sso)
        if not token:
            return False, "", None, "convert failed"
        key, entry = token_to_auth_entry(token, email=email)
        uid = entry.get("user_id") or secrets.token_hex(4)
        with _print_lock:
            print(f"  ✅ [{index}] 完成 user_id={uid[:12]}...")
        return True, key, entry, uid
    except Exception as e:
        with _print_lock:
            print(f"  ❌ [{index}] 异常: {e}")
        return False, "", None, str(e)


def main() -> int:
    ap = argparse.ArgumentParser(description="SSO cookie → grok auth.json (纯 HTTP)")
    ap.add_argument(
        "--tokens-json",
        metavar="FILE",
        help="tokens-all-all-all.json 风格文件（{group:[{token,tags}]}）",
    )
    ap.add_argument(
        "--sso", metavar="FILE", help="sso 列表文件（一行一个 JWT，或 邮箱----密码----sso）"
    )
    ap.add_argument("--sso-cookie", metavar="JWT", help="单个 sso cookie")
    ap.add_argument("--out", default=None, help="输出 auth.json 路径（单账号或 --merge）")
    ap.add_argument(
        "--out-dir",
        default=None,
        help="批量时每个账号写一个 {user_id}.json",
    )
    ap.add_argument(
        "--merge",
        action="store_true",
        help="合并到 --out，key 用 issuer::client_id::user_id",
    )
    ap.add_argument("--delay", type=int, default=0, help="每个间隔秒数（串行时）")
    ap.add_argument("--workers", type=int, default=1, help="并发数，默认 1")
    ap.add_argument("--limit", type=int, default=0, help="只处理前 N 个，0=全部")
    ap.add_argument("--email", default="", help="写入 entry.email（可选）")
    args = ap.parse_args()

    if args.tokens_json:
        cookies = load_tokens_json(args.tokens_json)
    else:
        cookies = load_sso_list(args.sso, args.sso_cookie)
    if not cookies:
        ap.error("需要 --tokens-json / --sso / --sso-cookie")
    if args.limit and args.limit > 0:
        cookies = cookies[: args.limit]

    if len(cookies) > 1 and not args.out_dir and not args.merge and not args.out:
        args.out_dir = "./auth_out"
        print(f"批量模式默认 --out-dir {args.out_dir}")

    if args.out is None and args.out_dir is None and len(cookies) == 1:
        args.out = str(Path.home() / ".grok" / "auth.json")

    if args.merge and not args.out:
        args.out = "auth_from_tokens.json"

    print(f"🚀 SSO → auth.json: {len(cookies)} 个, workers={args.workers}, delay={args.delay}s")
    ok = 0
    fail = 0
    merge_lock = Lock()
    workers = max(1, int(args.workers or 1))

    def handle_result(success: bool, key: str, entry: dict | None, uid: str) -> None:
        nonlocal ok, fail
        if not success or not entry:
            fail += 1
            return
        if args.out_dir:
            p = Path(args.out_dir) / f"{uid}.json"
            write_auth_json(p, key, entry)
            with _print_lock:
                print(f"  💾 {p}")
        if args.out:
            with merge_lock:
                if args.merge or len(cookies) > 1:
                    merge_auth_json(Path(args.out), key, entry, unique=True)
                    with _print_lock:
                        print(f"  💾 merge → {args.out}")
                else:
                    write_auth_json(Path(args.out), key, entry)
                    with _print_lock:
                        print(f"  💾 {args.out}")
        ok += 1

    if workers == 1:
        for i, sso in enumerate(cookies, 1):
            success, key, entry, uid = convert_one(i, len(cookies), sso, args.email)
            handle_result(success, key, entry, uid)
            if args.delay > 0 and i < len(cookies):
                time.sleep(args.delay)
    else:
        with ThreadPoolExecutor(max_workers=workers) as executor:
            futures = {
                executor.submit(convert_one, i, len(cookies), sso, args.email): i
                for i, sso in enumerate(cookies, 1)
            }
            for future in as_completed(futures):
                success, key, entry, uid = future.result()
                handle_result(success, key, entry, uid)

    print(f"\n{'=' * 60}\n📊 完成: {ok}/{len(cookies)} 成功, {fail} 失败")
    if args.out and Path(args.out).exists():
        try:
            merged = json.loads(Path(args.out).read_text(encoding="utf-8"))
            print(f"📦 {args.out}: {len(merged)} entries")
        except Exception:
            pass
    return 0 if fail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
