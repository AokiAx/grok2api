# grok2api

用 Go 将 xAI Grok CLI 凭证转换为 OpenAI / Anthropic 兼容 HTTP API，并以两个逻辑号池管理账号：

- `ready`：可用，简单环形轮询选号，不评分、不排序。
- `unavailable`：暂不可用，原因见 `unavailable_reason`：`quota` / `auth` / `cooldown` / `validating` / `disabled`。

上游是 `cli-chat-proxy.grok.com`（CLI 凭证链路），不是 grok.com 网页 SSO。支持导入已有 CLI 凭证，也支持服务内/CLI 批量自动注册并直写号池（需邮箱与 Turnstile 求解器）。请仅使用你有权操作的账号资源，并遵守上游服务条款。

## 当前架构

```text
HTTP API / 管理面板
        │
        ▼
统一 Gateway（Chat / Models / Billing / Responses / Messages）
        │
        ▼
Ready 环形轮询 ── 账号租约 ── Grok CLI 上游
        │                         │
        │   free-usage / 429 / auth
        └──────────┬──────────────┘
                   ▼
          Unavailable + RetryAt
                   │
     cooldown 到期直回 / quota 探测 / auth refresh
                   │
                   └──────────────► Ready
```

账号状态与状态事件存放在 SQLite schema v2：`data/grok2api.db`。

## 快速开始

要求 Go 1.25+。

```powershell
git clone https://github.com/AokiAx/grok2api.git
cd grok2api
Copy-Item config.example.json config.json

# 查看并执行数据迁移；输出 Ready/Unavailable 数量
go run ./cmd/grok2api migrate --config config.json

# 启动服务
go run ./cmd/grok2api serve --config config.json
```

默认监听 `127.0.0.1:8787`，面板：

```text
http://127.0.0.1:8787/panel
```

也可先构建：

```powershell
go build -trimpath -o grok2api.exe ./cmd/grok2api
./grok2api.exe status --config config.json
./grok2api.exe serve --config config.json
```

## 号池与恢复

### 两个池

| 池 | 含义 |
|----|------|
| `ready` | 可被轮询使用 |
| `unavailable` | 暂不可用，带原因与 `retry_at` |

### unavailable 原因

| 原因 | 含义 | 恢复方式 |
|------|------|----------|
| `quota` | free 额度耗尽 | 默认约 **24 小时**后做探测；成功才回 ready |
| `cooldown` | 普通限流 | 默认约 **45 秒**后直接回 ready |
| `auth` | 认证失效 | 有 `refresh_token + oidc_*` 时自动 OIDC refresh + 校验；否则人工恢复/重导 |
| `validating` | 校验中/不确定 | 后续验证结果决定 |
| `disabled` | 人工禁用 | 人工处理 |

说明：

- **不是**所有隔离都“到点直接回去”。
- `cooldown`：到点直回。
- `quota` / `auth`：会先验再回；探测/refresh 失败会继续退避。
- “隔多久再试”主要是本地配置（如 `quota_retry_minutes=1440`），不是上游下发的固定恢复时刻表。上游通过响应头/错误告诉你**当前**是否还有额度。

### Free 额度怎么统计

Free 账号没有可靠独立 billing 真值查询，统计来自**真实请求响应**：

1. **成功请求**：解析成对响应头  
   - `x-ratelimit-limit-tokens` / `x-ratelimit-remaining-tokens`（优先）  
   - 或 `x-ratelimit-limit-requests` / `x-ratelimit-remaining-requests`
2. 落库：  
   - `quota_limit = limit`  
   - `quota_actual = limit - remaining`（已用）
3. 面板单号显示：`剩余 tokens · 已用/上限`
4. 顶部汇总：对各账号 `quota_actual` / `quota_limit` **粗加总**
5. `remaining=0` 或 `subscription:free-usage-exhausted`：账号进 `unavailable(quota)`  
   - 成功但 remaining=0：本次结果仍返回客户端，号隔离  
   - 429 free 耗尽：换下一个 ready 号重试

注意：

- 未成功回写过头的号，额度常显示 `—`
- 汇总不是独立总账本，也不是 USD
- 面板默认约 **15 秒**刷新一次；服务端在请求成功后立即写库

### 并发显示

账号 `Active` 是调度器**内存租约计数**，不落 SQLite。  
管理面板列表会合并 scheduler 实时 `Active`，因此“在途并发”会随负载变化，而不是永远 `0 / N`。

## 额度耗尽后的流程

1. 当前账号返回 free usage 耗尽，或成功响应头 remaining=0。
2. 账号离开 Ready，保存额度字段、错误码、`retry_at`。
3. 若是失败型耗尽，当前请求继续尝试下一个 Ready 账号。
4. 本轮全部因额度失败时，开启全池 quota 熔断，返回 `429 + Retry-After`。
5. 号池变化（导入、人工恢复、探测成功）会让旧熔断失效。
6. `quota` 到期后 **先探测** 再决定是否回 Ready；`cooldown` 到期直回；`auth` 优先 refresh。

不会回退读取单账号 `~/.grok/auth.json`。空池返回结构化 429，而不是因缺文件 503。

## 自动注册（服务内 + CLI）

注册机已融入 Go 服务。成功后经 `admin.Import` 校验写入 `data/grok2api.db`，运行中 Ready 池热更新。

### 配置双源（重要）

| 文件 | 作用 |
|------|------|
| `config.json`（+ `GROK2API_*` 环境变量） | 启动种子配置 |
| `data/register_settings.json` | 面板设置页/注册机运行时配置 |

面板读写的是 `register_settings.json`。  
若其中 `proxy` / FlareSolverr 为空，启动时会尽量继承 `config.json` 里的对应值，避免“compose 侧车已部署、面板却显示未配置”。

### 面板

打开 `/panel` → **账号注册 / 设置**：

1. 邮箱、Turnstile、代理、FlareSolverr 等在设置页配置并保存
2. 注册页填写数量/并发，可 Dry-run
3. 开始/停止，查看日志与健康摘要（solver / mail / proxy）

依赖：

- `cfmail_accounts` 或 `email_provider=mailtm`
- Turnstile：`auto|local|capmonster`（本地默认 `http://127.0.0.1:5072`）
- 可选 `proxy` / `proxy_pool`（可指向 Privoxy）
- FlareSolverr **可选**，不能替代 Turnstile token

### CLI

```powershell
go run ./cmd/grok2api register --config config.json --count 3 --workers 1
go run ./cmd/grok2api mint --config config.json --sso-cookie "..." --email user@example.com
```

### Docker 一键栈（WARP + Privoxy + FlareSolverr）

仓库 `docker-compose.yml` 可直接编排：

```text
warp → privoxy → app
flaresolverr ──↗
```

示例：

```powershell
Copy-Item config.example.json config.json
# 建议在 config.json / 环境里设置：
# proxy=http://privoxy:8118
# flaresolverr_url=http://flaresolverr:8191
# flaresolverr_enabled=true
docker compose up -d
```

也可用：

```powershell
./deploy/deploy-stack.sh
```

说明：

- 应用只认代理 URL 与 FlareSolverr URL
- 容器网络内应写服务名（`privoxy` / `flaresolverr`），不要写宿主机 `127.0.0.1` 去连侧车
- 面板设置保存后以 `data/register_settings.json` 为准

## 导入账号

在 `/panel` 粘贴 JSON，或“从文件加载” `auth.json` / `auth_from_*.json`，先预览再导入。

兼容：

- 数组：`[{"key":"...","refresh_token":"..."}]`
- 旧字段：`access_token` 可替代 `key`
- `{"accounts":[...]}`
- `~/.grok/auth.json` map 格式

推荐：

```json
[
  {
    "key": "...",
    "refresh_token": "...",
    "email": "user@example.com",
    "expires_in": 3600,
    "oidc_issuer": "https://auth.x.ai",
    "oidc_client_id": "b1a00492-073a-47ea-816f-4c329264a828"
  }
]
```

导入时会验证账号：

- 成功 → `ready`
- 额度耗尽 → `unavailable(quota)` + 恢复时间
- 401/403 → `unavailable(auth)`
- 普通限流 → `unavailable(cooldown)`
- 验证基础设施异常 → 停止导入并报错，不把未知状态塞进 Ready

面板支持删除、人工“恢复验证”（先验证，不强制标可用）。

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 两池数量、原因统计、quota 熔断 |
| GET | `/panel`、`/manager` | 管理面板 |
| GET | `/v1/models` | 模型列表；补充 `api_backend` / context_window / reasoning 等元数据；上游不可达时静态兼容列表 |
| GET | `/v1/billing` | 上游 billing 透传（free 号通常不是额度真值来源） |
| POST | `/v1/chat/completions` | OpenAI Chat Completions |
| POST | `/chat/completions` | 别名 |
| POST | `/v1/responses` | OpenAI Responses 透传，支持 SSE |
| POST | `/v1/messages` | Anthropic Messages 兼容 |
| GET | `/admin/api/cli-accounts` | 账号列表（无 token）；含额度与实时 Active |
| POST | `/admin/api/accounts/import/preview` | 导入预览 |
| POST | `/admin/api/accounts/import` | 验证并导入 |
| DELETE | `/admin/api/cli-accounts/{id}` | 删除 |
| POST | `/admin/api/cli-accounts/{id}/recover` | 验证并尝试恢复 |
| GET/PUT | `/admin/api/register/settings` | 注册设置（落盘 `register_settings.json`） |
| GET | `/admin/api/register/status` | 注册任务状态 |
| POST | `/admin/api/register/start` | 启动批注册 |
| POST | `/admin/api/register/stop` | 停止批注册 |
| GET | `/admin/api/register/health` | 注册依赖健康 |

示例：

```powershell
curl http://127.0.0.1:8787/v1/chat/completions `
  -H "Authorization: Bearer YOUR_API_KEY" `
  -H "Content-Type: application/json" `
  -d '{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 配置

复制 [config.example.json](config.example.json) 为 `config.json`。环境变量 `GROK2API_*` 覆盖文件值。

管理密钥优先级：`panel_password` → `app_key` → `api_key`。  
`api_key` 非空时，`/v1` 请求需 Bearer 或 `x-api-key`。

关键参数：

| 键 | 含义 | 默认 |
|----|------|------|
| `quota_retry_minutes` | free 额度耗尽后再次探测间隔 | `1440`（24h） |
| `rate_retry_seconds` | 普通 429 冷却 | `45` |
| `timeout_secs` | 单次上游超时 | `600` |
| `proxy` | 注册/出口代理 | 空；compose 栈常用 `http://privoxy:8118` |
| `flaresolverr_url` | FlareSolverr | 空；compose 常用 `http://flaresolverr:8191` |
| `flaresolverr_enabled` | 是否启用 Flare | `false` |

## 数据迁移

首次打开数据库时幂等迁移：

- Python SQLite v1 `cli_accounts` → Go v2 `accounts`
- v2 为空时兼容导入 `data/cli_accounts.json`
- 旧 `usage-exhausted` → `unavailable(quota)`
- token/auth/401/403/refresh → `unavailable(auth)`
- 过期凭证先入 `auth`，由后台 refresh 处理
- quota 恢复时间会错峰，避免同时打穿上游

迁移前请备份整个 `data/`。`migrate` / `status` 只输出状态，不启 HTTP。

## Docker / GHCR

镜像：

```text
ghcr.io/aokiax/grok2api
```

`main` 构建 `latest`（另有 branch / sha 标签）。支持 `linux/amd64`、`linux/arm64`。

```powershell
Copy-Item config.example.json config.json
docker compose up -d
```

本地构建：

```powershell
docker build -f Dockerfile.golang -t grok2api:go .
```

## GitHub 灰度部署

`Deploy Go canary` workflow 默认部署到回环 `8788`（生产数据副本）；勾选 `promote` 才切 `8787`。

`production` environment 需要：

- `PRODUCTION_SSH_KEY`
- `PRODUCTION_HOST`
- `PRODUCTION_USER`
- `GHCR_READ_TOKEN`

推荐顺序：

1. 推送后等 CI / `Build Go image` 成功
2. 部署 `latest` 或 `sha-*` 到 8788
3. 检查 `/health`、`/v1/models`、面板号池与额度
4. 验证轮询、429、SSE、恢复
5. 再 promote 到 8787

## 验证

```powershell
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/grok2api
```

CI 对核心包有覆盖率门禁。运行时以 Go 服务为准。

## 说明

- free 与订阅额度策略由 xAI 上游决定；本服务对 free 以响应头/耗尽错误为准做号池治理。
- 不按额度高低选号；Ready 中简单轮询。
- 请仅使用自己有权使用的凭证，并遵守上游服务条款。
