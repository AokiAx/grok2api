# grok2api

用 Go 将 xAI Grok CLI 凭证转换为 OpenAI / Anthropic 兼容 HTTP API，并以两个逻辑号池管理账号：

- `ready`：可用；默认按 round-robin 选号，也支持 fill-first、热集合和会话 sticky；不按额度评分。
- `unavailable`：暂不可用，原因见 `unavailable_reason`：`quota` / `auth` / `cooldown` / `validating` / `disabled`。

上游是 `cli-chat-proxy.grok.com`（CLI 凭证链路）
本服务负责 **导入凭证、号池调度、兼容 API**；
演进计划见 [docs/ROADMAP.md](docs/ROADMAP.md)（CLI 指纹对齐、管理面重写、可选凭据加密）。

批量注册已拆到独立项目 [`grok-register`](../grok-register)
请仅使用你有权操作的账号资源，并遵守上游服务条款

## 当前架构

```text
HTTP API / 管理面板
        │
        ▼
协议桥（Chat / Responses / Anthropic → Grok Responses）
        │
        ▼
统一 Gateway（账号重试 / 状态回写 / 熔断）
        │
        ▼
Ready 环形轮询 ── 账号租约 ── Grok CLI 上游
        │                         │
        │   free-usage / 429 / auth
        └──────────┬──────────────┘
                   ▼
          Unavailable + RetryAt
                   │
     cooldown / quota 到期回池，auth refresh + 校验
                   │
                   └──────────────► Ready
```

账号状态与状态事件存放在 SQLite schema v3：`data/grok2api.db`。

## 快速开始

要求 Go 1.25+。SQLite 使用纯 Go 驱动，本地运行不需要额外安装 SQLite、CGO 或 Python。

```powershell
git clone https://github.com/AokiAx/grok2api.git
cd grok2api
Copy-Item config.example.json config.json

# 打开数据库时会自动幂等迁移；输出 Ready/Unavailable 数量
go run ./cmd/grok2api migrate --config config.json

# 启动服务
go run ./cmd/grok2api serve --config config.json
```

`serve` 是默认命令，因此也可以直接运行 `go run ./cmd/grok2api --config config.json`。本地运行时即使 `config.json` 不存在也会使用内置默认值，但建议从示例复制一份以明确保存密钥和运行参数；Docker Compose 则必须存在 `config.json`。

默认监听 `127.0.0.1:8787`，面板：

```text
http://127.0.0.1:8787/
```

也可先构建：

```powershell
go build -trimpath -o grok2api.exe ./cmd/grok2api
./grok2api.exe status --config config.json
./grok2api.exe serve --config config.json
```

### CLI 命令

<!-- AUTO-GENERATED: source=cmd/grok2api/main.go -->
| 命令 | 说明 |
|------|------|
| `serve` | 启动 HTTP 服务；省略命令时的默认行为 |
| `status` | 打开数据库、执行必要迁移并输出版本、两池数量和原因统计 |
| `migrate` | `status` 的兼容别名；迁移本身在数据库打开时自动执行 |
| `export` | 导出可重新导入的账号 JSON；默认写入 `data/export_accounts.json` |
<!-- /AUTO-GENERATED -->

导出示例：

```powershell
# 导出全部账号
go run ./cmd/grok2api export --config config.json

# 只导出 ready 池并指定文件
go run ./cmd/grok2api export --config config.json --pool ready --out data/ready_accounts.json
```

`--pool` 可取 `ready` / `unavailable`，留空表示全部。旧的 `register` / `mint` 命令已迁移到外部 `grok-register`，在本程序中调用会直接返回迁移提示。

## 管理面板

路径：`/`。生产二进制直接在服务根路径挂载内嵌 SPA。

新管理 API 契约：[`/api/admin/v1/*`](docs/ADMIN_API_V1.md)（`{ok,data,error}`）；旧 `/admin/api/*` 仍供内嵌面板使用。

管理前端位于 [`frontend/`](frontend/)：`cd frontend && npm install && npm run dev`（默认 `http://127.0.0.1:5173`，代理到 API `:8787`）。

### 登录

- 使用配置中的管理密钥：`panel_password` → `app_key` → `api_key`
- 默认勾选 **记住登录**：凭证写入浏览器 `localStorage`，关闭浏览器后仍可自动进入
- 取消勾选则仅用 `sessionStorage`（关闭标签失效）
- 点「退出」会清除本机保存的登录态
- 仅当管理 API 返回 **401** 才会强制重新登录；网络抖动不会踢会话

### 功能分区

| 分区 | 说明 |
|------|------|
| 总览 | 号池汇总、额度、并发、到期/待恢复、错误码 Top |
| 账号 | 分页列表、搜索、筛选、删除 / 恢复验证 |
| 导入 | JSON / auth.json 预览与验证写入 |
| 注册 | 指向外部 `grok-register` 的说明（本服务不再内嵌注册机） |

### 总览统计

列表接口的 `summary` 来自 SQLite 聚合 + 调度器实时租约，主要字段：

| 指标 | 含义 |
|------|------|
| Ready / Unavailable | 两池数量与占比 |
| 累计请求 | 全账号 `request_count` 之和 |
| 在途并发 | 内存租约 Active / MaxActive 槽位 |
| 可自动刷新 | 有 `refresh_token` 的账号数 |
| Free Token 剩余 | 已观测账号的 token 额度粗加总 |
| 到期 / 将到期 | access token 已过期 / 1 小时内将过期 |
| 待恢复 | `unavailable` 且 `retry_at` 已到期 |
| 认证失败号 | `authentication_fails > 0` |
| 无 refresh | 无法 OIDC 自动续期 |
| 不可用原因 / 错误码 Top | `unavailable_reason` 与 `last_error_code` 分布 |

面板默认约 **15 秒** 自动刷新；切回标签页时会再拉一次。

## 号池与恢复

### 两个池

| 池 | 含义 |
|----|------|
| `ready` | 可被轮询使用 |
| `unavailable` | 暂不可用，带原因与 `retry_at` |

### unavailable 原因

| 原因 | 含义 | 恢复方式 |
|------|------|----------|
| `quota` | free 额度耗尽 | 默认按 **24 小时滚动窗口**设置 `retry_at`，到期直接回 ready 并清空本地窗口计数 |
| `cooldown` | 普通限流 | 默认约 **45 秒**后直接回 ready |
| `auth` | 认证失效 | 有 `refresh_token + oidc_*` 时自动 OIDC refresh + 校验；否则人工恢复/重导 |
| `validating` | 校验中/不确定 | 后续验证结果决定 |
| `disabled` | 人工禁用 | 人工处理 |

说明：

- **不是**所有隔离都“到点直接回去”。
- `cooldown`：到点直回。
- `quota`：到达 `retry_at` 后按时间回池，不发 chat 探测；若新窗口仍耗尽，会由后续真实请求再次隔离。
- `auth`：有完整 OIDC refresh 信息时先续期并校验；失败会退避，明确撤销的 refresh token 会转为 `disabled`。
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
4. 顶部汇总：对各账号 `quota_actual` / `quota_limit` **粗加总**（仅统计已观测账号）
5. `remaining=0` 或 `subscription:free-usage-exhausted`：账号进 `unavailable(quota)`  
   - 成功但 remaining=0：本次结果仍返回客户端，号隔离  
   - 429 free 耗尽：换下一个 ready 号重试

注意：

- 未成功回写过头的号，额度常显示 `—`
- 汇总不是独立总账本，也不是 USD
- 服务端在请求成功后立即写库；面板定时拉取展示

### 并发显示

账号 `Active` 是调度器**内存租约计数**，不落 SQLite。  
管理面板列表会合并 scheduler 实时 `Active`，因此“在途并发”会随负载变化，而不是永远 `0 / N`。

## 额度耗尽后的流程

1. 当前账号返回 free usage 耗尽，或成功响应头 remaining=0。
2. 账号离开 Ready，保存额度字段、错误码、`retry_at`。
3. 若是失败型耗尽，当前请求继续尝试下一个 Ready 账号。
4. 本轮全部因额度失败时，开启全池 quota 熔断，返回 `429 + Retry-After`。
5. 号池变化（导入、人工恢复、后台恢复回池）会让旧熔断失效。
6. `quota` / `cooldown` 到期后按时间回 Ready；`auth` 优先 refresh + 校验。

已落库且 `quota_actual >= quota_limit` 的旧 Ready 账号不会继续占用请求尝试：调度器会立即跳过并隔离，后台恢复任务也会将其移入 `unavailable(quota)`。

不会回退读取单账号 `~/.grok/auth.json`。空池返回结构化 429，而不是因缺文件 503。

## 账号注册（外部项目）

本仓库 **已移除** 内嵌注册机。批注册请使用独立项目 **`grok-register`**（与本仓同级：`../grok-register`）。

```powershell
cd ../grok-register
go run ./cmd/grok-register register --config config.json --count 10 --workers 2

# 可选：配置 grok2api_base_url + grok2api_api_key
# 注册成功后自动 POST /admin/api/accounts/import
# 或将 output/import/*.json 粘贴到本面板「导入」
```

对接说明见 [docs/EXTERNAL_REGISTER.md](docs/EXTERNAL_REGISTER.md) 与 `../grok-register/README.md`。

### Docker 侧车（WARP + Privoxy + FlareSolverr）

仓库 `docker-compose.yml` 可编排代理侧车。不过当前 Go 服务虽然会读取 `proxy` / `GROK2API_PROXY`，尚未把它接到 `http.Client.Transport`，因此 **app 的 Grok 上游请求目前不会实际经过 Privoxy/WARP**。FlareSolverr 也不被 app 直接使用，主要供外部注册项目使用。

```text
app ── 当前直连 ──► Grok CLI 上游

计划中的代理链：app → privoxy → WARP → Grok CLI 上游

grok-register → FlareSolverr（可选；app 不直接使用）
```

示例：

```powershell
Copy-Item config.example.json config.json
# GROK2API_PROXY 会进入配置，但当前 app 尚未接线使用
docker compose up -d
```

在 Bash 环境中也可用（需要 Docker Compose 与 `curl`）：

```bash
bash ./deploy/deploy-stack.sh
```

Windows 需使用 WSL 或 Git Bash。

说明：

- 代理配置字段是 URL（`proxy` / `GROK2API_PROXY`），但当前 Go app 尚未消费该字段
- 外部注册机在容器网络内连接侧车时应写服务名（`privoxy` / `flaresolverr`），不要写宿主机 `127.0.0.1`
- FlareSolverr / 邮箱 / Turnstile 等注册相关配置在 **`grok-register`** 中设置，不在本服务面板内
- Compose 的 `depends_on` 只保证启动顺序，不表示代理侧车已经通过健康检查

## 导入账号

在管理面板根路径 `/` 的“导入”页面粘贴 JSON，或「从文件加载」`auth.json` / `auth_from_*.json`，先预览再导入。

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

面板支持删除、人工「恢复验证」（先验证，不强制标可用）。

## API

<!-- AUTO-GENERATED: source=internal/api/server.go -->
| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 两池数量、原因统计、quota 熔断 |
| GET | `/` | 管理面板 |
| GET | `/admin/api/panel-meta` | 面板元数据（是否需要密码等） |
| GET | `/v1/models` | 模型列表；补充 `api_backend` / context_window / reasoning 等元数据；上游不可达时静态兼容列表 |
| GET | `/v1/billing` | 上游 billing 透传（free 号通常不是额度真值来源） |
| POST | `/v1/chat/completions` | OpenAI Chat Completions |
| POST | `/chat/completions` | 别名 |
| POST | `/v1/responses` | OpenAI Responses 兼容桥接：规范化字段/工具，支持 SSE 或服务端聚合 |
| POST | `/v1/messages` | Anthropic Messages 兼容 |
| GET | `/admin/api/cli-accounts` | 账号分页列表（无 token）+ `summary` 全局统计 + 实时 Active |
| POST | `/admin/api/accounts/import/preview` | 导入预览 |
| POST | `/admin/api/accounts/import` | 验证并导入 |
| DELETE | `/admin/api/cli-accounts/{id}` | 删除 |
| POST | `/admin/api/cli-accounts/{id}/recover` | 验证并尝试恢复 |
<!-- /AUTO-GENERATED -->

`GET /admin/api/cli-accounts` 查询参数：

| 参数 | 说明 |
|------|------|
| `page` | 页码，从 1 起 |
| `page_size` | 每页条数，默认 50，最大 200 |
| `pool` | `ready` / `unavailable` / 空=全部 |
| `q` | 搜索 id / email / reason / error code |

配置管理密钥后，管理端需 Bearer 或 `x-api-key`。三个密钥都为空时管理 API 和面板无认证，只适合回环监听或受信网络。

### 协议兼容范围

- Chat Completions、Responses、Anthropic Messages 最终统一进入 Grok CLI `/responses` 后端；流式请求使用 SSE，非流式请求由服务端聚合。
- 支持常用工具调用、多轮 tool history、`reasoning_effort`、图片内容、`web_search` / `x_search` 等字段转换。
- Anthropic 直转路径保留 thinking 内容与 signature、`tool_use` / `tool_result`、图片块和服务端 web search；模型 ID 原样透传，不做 Claude → Grok 别名改写。
- Chat / Messages 入口可识别误投的 Responses 形状请求并走 Responses 处理链。
- 单个 Chat / Responses / Messages 请求体上限为 32 MiB。
- `/v1/messages` 的本地认证、参数和网关错误目前仍使用 OpenAI 风格 `error` envelope；正常响应为 Anthropic Messages 格式。

默认静态模型目录（上游 `/models` 不可用时作为 fallback）：

| 模型 | 后端 | 上下文 | 说明 |
|------|------|--------|------|
| `grok-4.5` | Responses | 500,000 | reasoning `high` / `medium` / `low`，支持 backend search |
| `grok-composer-2.5-fast` | Responses | 200,000 | 非 reasoning Composer 模型 |

未知模型 ID 也会原样传给上游，并默认按 Responses 后端处理；最终可用性仍以上游账号和模型权限为准。

示例：

```powershell
curl http://127.0.0.1:8787/v1/chat/completions `
  -H "Authorization: Bearer YOUR_API_KEY" `
  -H "Content-Type: application/json" `
  -d '{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 配置

复制 [config.example.json](config.example.json) 为 `config.json`。 CLI 指纹相关字段：`client_version` / `client_identifier` / `client_user_agent` / `token_auth`；可选 `credential_key`（AES-GCM 加密落库）；会话粘滞优先请求体 `prompt_cache_key`。配置优先级是：**内置默认值 < JSON 配置文件 < 源码明确支持的进程环境变量**。

程序不会自动加载 `.env`；本地运行需在当前 shell / 服务管理器中设置环境变量。仓库根目录 `.env` 主要用于 Docker Compose 的变量替换。配置文件路径只能通过 `--config` 指定，当前源码不支持 `GROK2API_CONFIG` 或 `GROK2API_AUTO_CLIENT_VERSION`。

管理密钥优先级：`panel_password` → `app_key` → `api_key`。  
`api_key` 非空时，`/v1` 请求需 Bearer 或 `x-api-key`。

关键参数：

| 键 | 含义 | 默认 |
|----|------|------|
| `quota_retry_minutes` | free 额度耗尽后的本地滚动窗口隔离时长 | `1440`（24h） |
| `rate_retry_seconds` | 普通 429 冷却 | `45` |
| `timeout_secs` | 单次上游超时 | `600` |
| `cli_pool_max_concurrent` | 单号最大并发租约 | `4` |
| `cli_pool_max_attempts` | 单请求最多尝试账号数 | `3` |
| `cli_pool_strategy` | 选号策略 | `round-robin` |
| `cli_pool_active_size` | 热集合大小；`0` 表示整个 Ready 池 | `0` |
| `cli_pool_acquire_timeout` | 等待可用账号/并发槽的秒数 | `60` |
| `cli_pool_sticky` | 会话粘账号（利于 cache） | `true` |
| `cli_pool_sticky_ttl_minutes` | 粘账号映射有效期 | `30` |
| `proxy` | 预留的出口代理 URL；当前 Go HTTP client 尚未接线 | 空 |
| `debug_trace` | 请求链路 JSONL 调试 | `false` |
| `debug_trace_dir` | 调试文件目录；空时使用 `{data_dir}/traces` | 空 |
| `debug_trace_errors_only` | 只落失败请求 trace | `true` |

`cli_pool_strategy` 支持 `round-robin` 和 `fill-first`。启用 sticky 后，服务会综合 `X-Grok2API-Sticky`、常见用户/请求标识、API key 以及请求 payload 特征生成粘性键；Anthropic 客户端的会话标识还会用于维持上游 conversation ID。

源码支持的环境变量如下。`cli_pool_max_attempts`、`cli_pool_strategy`、`cli_pool_active_size` 当前只能通过 JSON 配置：

<!-- AUTO-GENERATED: source=internal/config/config.go -->
| 类型 | 环境变量 |
|------|----------|
| 字符串 | `GROK2API_HOST`、`GROK2API_API_KEY`、`GROK2API_APP_KEY`、`GROK2API_PANEL_PASSWORD`、`GROK2API_PROXY_BASE_URL`、`GROK2API_CLIENT_VERSION`、`GROK2API_DEFAULT_MODEL`、`GROK2API_DATA_DIR`、`GROK2API_PROXY`、`PROXY_URL`、`GROK2API_DEBUG_TRACE_DIR` |
| 整数 | `GROK2API_PORT`、`GROK2API_CLI_POOL_MAX_CONCURRENT`、`GROK2API_CLI_POOL_ACQUIRE_TIMEOUT`、`GROK2API_CLI_POOL_STICKY_TTL_MINUTES`、`GROK2API_QUOTA_RETRY_MINUTES`、`GROK2API_RATE_RETRY_SECONDS`、`GROK2API_TIMEOUT_SECS` |
| 布尔 | `GROK2API_DEBUG_TRACE`、`GROK2API_DEBUG_TRACE_ERRORS_ONLY`、`GROK2API_CLI_POOL_STICKY` |
<!-- /AUTO-GENERATED -->

布尔值接受 `1/true/yes/on` 或 `0/false/no/off`（不区分大小写）。

账号注册相关配置（邮箱、Turnstile、FlareSolverr 等）请写在 **`grok-register`** 的配置中。

## 数据迁移

任意命令首次打开数据库时都会幂等迁移；无需先单独执行 `migrate`：

- Python SQLite v1 `cli_accounts` → Go `accounts`（当前 schema v3）
- 库为空时兼容导入 `data/cli_accounts.json`
- 旧 `usage-exhausted` → `unavailable(quota)`
- token/auth/401/403/refresh → `unavailable(auth)`
- 过期凭证先入 `auth`，由后台 refresh 处理
- quota 恢复时间会错峰，避免同时打穿上游

迁移前请备份 `config.json` 和整个 `data/`。`migrate` / `status` 不启 HTTP，但会打开并迁移数据库，空库时还可能导入旧 `data/cli_accounts.json`；`export` 会写出包含访问凭证的文件，请按敏感数据保护。

## Docker / GHCR

> 安全提示：`config.example.json` 的 `api_key`、`app_key`、`panel_password` 默认都为空，而 Compose 默认把宿主 8787 端口发布到所有接口。暴露到局域网或公网前，请至少设置 `api_key` 和独立的 `panel_password`；仅本机使用时可把端口映射改为 `127.0.0.1:8787:8787`。

镜像：

```text
ghcr.io/aokiax/grok2api
```

`main` 构建 `latest`（另有 branch / sha 标签）。支持 `linux/amd64`、`linux/arm64`。

```powershell
Copy-Item config.example.json config.json
docker compose up -d
```

Compose 持久化 `./data` 到容器 `/app/data`，并以非 root UID/GID `65532` 运行。Linux bind mount 必须允许该用户写入，否则 SQLite 无法创建或更新数据库：

```bash
mkdir -p data
sudo chown -R 65532:65532 data
docker compose up -d
```

常用 Compose 变量：`GROK2API_IMAGE`、`GROK2API_PORT`（宿主端口）、`GROK2API_PROXY`、`WARP_SOCKS_PORT`、`PRIVOXY_PORT`、`FLARESOLVERR_PORT`、`FLARESOLVERR_LOG_LEVEL`、`TZ`。

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

### 健康检查语义

- `GET /health` 始终返回 HTTP 200；JSON 中 `ok: true` 才表示当前至少有一个 Ready 账号，`ok: false` 只表示服务存活但业务池未就绪。
- Compose 的 app healthcheck 执行 `/grok2api status`，验证配置、数据目录和 SQLite 可打开，但不验证 HTTP 监听或 Ready 账号。
- 灰度/生产验收应同时检查 `/health` 的 JSON `ok`、`/v1/models`，并发起一次真实的流式或非流式模型请求。

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
