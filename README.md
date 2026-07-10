# grok2api

用 Go 将 xAI Grok CLI 凭证转换为 OpenAI / Anthropic 兼容 HTTP API，并以两个逻辑号池管理账号：

- `ready`：已验证可用，按简单环形轮询选号，不评分、不排序。
- `unavailable`：暂不可用，并记录 `quota`、`auth`、`cooldown`、`validating` 或 `disabled` 原因。

上游是 `cli-chat-proxy.grok.com`，不是 grok.com 网页 SSO。项目只支持导入和验证用户已授权的 CLI 凭证，不包含批量自动注册逻辑。

## 当前架构

```text
HTTP API / 管理面板
        │
        ▼
统一 Gateway（Chat / Models / Billing / Responses）
        │
        ▼
Ready 环形轮询 ── 账号租约 ── Grok CLI 上游
        │                         │
        │      quota/auth/429     │
        └──────────┬──────────────┘
                   ▼
          Unavailable + RetryAt
                   │
          到期恢复 / 人工恢复验证
                   │
                   └──────────────► Ready
```

账号状态和状态变更事件存放在 SQLite schema v2：`data/grok2api.db`。

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

服务默认监听 `127.0.0.1:8787`，管理面板为：

```text
http://127.0.0.1:8787/panel
```

也可先构建静态二进制：

```powershell
go build -trimpath -o grok2api.exe ./cmd/grok2api
./grok2api.exe status --config config.json
./grok2api.exe serve --config config.json
```

## 导入账号

在 `/panel` 中粘贴已授权凭证，先“预览”，再“导入”：

```json
[
  {
    "access_token": "...",
    "refresh_token": "...",
    "email": "user@example.com",
    "expires_in": 3600
  }
]
```

导入时会调用上游 `/models` 验证账号：

- 验证成功：进入 `ready`。
- 额度耗尽：进入 `unavailable(quota)` 并设置恢复时间。
- 401/403：进入 `unavailable(auth)`。
- 普通限流：进入 `unavailable(cooldown)`。
- 验证基础设施异常：停止导入并返回错误，不把未知状态账号放入 Ready。

管理面板支持删除账号和人工“恢复验证”。人工恢复同样先验证凭证，不会直接强制标记可用。

## 额度耗尽后的流程

1. 当前账号返回 rolling quota / `subscription:free-usage-exhausted`。
2. 账号立即从 Ready 轮询中移除，保存 `quota_actual`、`quota_limit`、错误码和 `retry_at`。
3. 当前请求继续尝试下一个 Ready 账号。
4. 如果本轮所有账号都因额度耗尽失败，开启全池 quota 熔断并返回 `429 + Retry-After`。
5. 号池发生变化（新账号导入、人工恢复、到期恢复）时，旧熔断自动失效，允许新一轮探测。
6. `quota` / `cooldown` 到期后由恢复 worker 放回 Ready；`auth` 不会自动恢复，必须更新凭证或人工验证。

不会回退读取单账号 `~/.grok/auth.json`，因此空池会返回结构化 429，而不是因缺失文件返回 503。

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 两池数量、原因统计和 quota 熔断状态 |
| GET | `/panel`、`/manager` | Go 管理面板 |
| GET | `/v1/models` | 模型列表；上游不可达时返回静态兼容列表 |
| GET | `/v1/billing` | 上游额度信息 |
| POST | `/v1/chat/completions` | OpenAI Chat Completions，支持 SSE |
| POST | `/chat/completions` | Chat Completions 别名 |
| POST | `/v1/responses` | OpenAI Responses 透传，支持 SSE |
| POST | `/v1/messages` | Anthropic Messages 转换，支持工具和 SSE |
| GET | `/admin/api/cli-accounts` | 账号列表，不返回 token |
| POST | `/admin/api/accounts/import/preview` | 导入预览 |
| POST | `/admin/api/accounts/import` | 验证并导入 |
| DELETE | `/admin/api/cli-accounts/{id}` | 删除账号 |
| POST | `/admin/api/cli-accounts/{id}/recover` | 验证并尝试恢复账号 |

调用示例：

```powershell
curl http://127.0.0.1:8787/v1/chat/completions `
  -H "Authorization: Bearer YOUR_API_KEY" `
  -H "Content-Type: application/json" `
  -d '{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 配置

复制 [config.example.json](config.example.json) 为 `config.json`。环境变量 `GROK2API_*` 覆盖文件值。

管理密钥优先级：`panel_password` → `app_key` → `api_key`。`api_key` 非空时，所有 `/v1` 请求必须携带 Bearer 或 `x-api-key`。

关键恢复参数：

- `quota_retry_minutes`：额度耗尽后的首次重试间隔，默认 30 分钟。
- `rate_retry_seconds`：普通 429 冷却时间，默认 45 秒。
- `timeout_secs`：单次上游请求超时，默认 600 秒。

## 数据迁移

Go 服务首次打开数据库时会执行幂等迁移：

- Python SQLite v1 `cli_accounts` → Go SQLite v2 `accounts`。
- 当 v2 数据库为空时，兼容导入 `data/cli_accounts.json`。
- Python 中已禁用且错误含 `usage-exhausted` 的账号迁移为 `unavailable(quota)`。
- token/auth/401/403/refresh 错误迁移为 `unavailable(auth)`。
- quota 账号会设置错峰恢复时间，避免同时打穿上游。

迁移前仍应备份整个 `data/` 目录。`migrate` 和 `status` 命令只输出状态，不启动 HTTP 服务。

## Docker / GHCR

GitHub Actions 构建并发布多架构镜像：

```text
ghcr.io/aokiax/grok2api
```

支持 `linux/amd64`、`linux/arm64`，并包含 provenance、Cosign keyless 签名和 Trivy 高危扫描。

```powershell
Copy-Item config.example.json config.json
docker compose up -d
```

或本地构建 Go 镜像：

```powershell
docker build -f Dockerfile.golang -t grok2api:go .
```

## GitHub 灰度部署

`Deploy Go canary` workflow 默认把镜像部署到服务器回环端口 `8788`，使用生产数据副本；只有手动勾选 `promote` 才会切换 `8787`。

GitHub `production` environment 需要：

- `PRODUCTION_SSH_KEY`
- `PRODUCTION_HOST`
- `PRODUCTION_USER`
- `GHCR_READ_TOKEN`

推荐顺序：

1. 推送分支，等待 CI 和 `Build Go image` 全部成功。
2. 部署分支标签或 `sha-*` 到 8788。
3. 检查 `/health`、`/v1/models`、面板账号数量及 Ready/Unavailable 原因。
4. 使用测试请求验证轮询、429、SSE 和人工恢复。
5. 确认数据副本无异常后，重新运行 workflow 并勾选 `promote`。

## 验证

```powershell
go test ./...
go test -race ./...
go vet ./...
pytest -q
```

CI 对 `internal/...` 执行 80% 覆盖率门禁，并保留 Python 旧实现回归测试，直到迁移完全结束。

## 说明

- 免费或订阅额度由 xAI 上游策略决定。
- 本项目不对账号做评分；只区分可用与不可用，并在 Ready 中简单轮询。
- 请仅使用自己有权使用的凭证，并遵守上游服务条款。
