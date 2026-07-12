# 注册机说明

## 生产路径（唯一）

**Go** 服务内注册是唯一完整链路：

```text
面板/CLI
  → JobManager / Pipeline
  → mail (cfmail|mailtm)
  → Registrar (accounts.x.ai)
  → Turnstile (local solver 或 CapMonster)
  → mint Device OIDC（与注册同一代理）
  → admin.Import → SQLite ready 池
```

| 入口 | 命令/接口 |
|------|-----------|
| 面板 | `/panel` → 账号注册 |
| CLI | `grok2api register --count N --workers W` |
| Mint | `grok2api mint --sso-cookie ...` |
| 配置 | `data/register_settings.json`（面板为准） |

实现目录：`internal/register/`。

## 本目录 Python 角色

| 路径 | 状态 |
|------|------|
| `tools/turnstile/` | **保留** — 本地 Camoufox Turnstile solver（Go `local` 模式会调它） |
| `pipeline.py` / `registrar.py` / `providers/` 等 | **遗留** — 与 Go 重复；mint 依赖已缺失，**不要**再当生产注册入口 |

```bash
# 仅启动本地 solver（生产常用）
cd register/tools/turnstile
xvfb-run -a .venv/bin/python api_solver.py --browser_type camoufox --thread 2 --host 0.0.0.0 --port 5072
```

## 健康检查

`GET /admin/api/register/health` 会探测：

- Turnstile solver 可达 / CapMonster key
- 邮箱配置是否可用
- 代理是否配置
- FlareSolverr（可选侧车）

## 代理约定

注册、邮箱 API、OIDC mint **共用同一 proxy**，避免「注册过了 mint 直连失败」。
