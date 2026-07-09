# grok2api

把 **xAI Grok CLI 会话** 变成 **OpenAI / Anthropic 兼容 HTTP API** 的本地服务。

- 上游：`cli-chat-proxy.grok.com`（**不是** grok.com 网页 SSO 聊天）
- 凭证：OIDC `access_token` + `refresh_token`（`data/cli_accounts.json`）
- 面板：密码保护的 Web 管理台 `/panel`

## 快速开始

```powershell
git clone https://github.com/AokiAx/grok2api.git
cd grok2api
pip install -r requirements.txt

# 配置
copy config.example.json config.json
# 建议设置 api_key / panel_password

# 登录 CLI 凭证（需本机已安装 grok CLI，或浏览器 OIDC）
python -m app login

# 启动
python run.py
# → http://127.0.0.1:8787
# → 面板 http://127.0.0.1:8787/panel
```

或：

```powershell
.\scripts\start.ps1
```

### 调用示例

```powershell
curl http://127.0.0.1:8787/health
curl http://127.0.0.1:8787/v1/models -H "Authorization: Bearer YOUR_API_KEY"
curl http://127.0.0.1:8787/v1/billing -H "Authorization: Bearer YOUR_API_KEY"

curl http://127.0.0.1:8787/v1/chat/completions `
  -H "Authorization: Bearer YOUR_API_KEY" `
  -H "Content-Type: application/json" `
  -d "{\"model\":\"grok-4.5\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":false}"
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="YOUR_API_KEY")
r = client.chat.completions.create(
    model="grok-4.5",
    messages=[{"role": "user", "content": "hi"}],
)
print(r.choices[0].message.content)
```

未设置 `api_key` 时，客户端可用任意非空 Bearer。

## 管理面板

```text
http://127.0.0.1:8787/panel
```

别名：`/manager`。

| 功能 | 说明 |
|------|------|
| 服务状态 | health / 版本 / 默认模型 |
| CLI 号池 | 列表、删除、手动导入 tokens、热重载 |
| 额度 | 调用 `/v1/billing` |
| 最近请求 | 本地 usage 摘要 |
| 网页对话 | 直连本机 `/v1/chat/completions`（支持流式） |

**面板密码**（`config.json`）：

```json
"panel_password": "your-password",
"api_key": "your-password",
"app_key": "your-password"
```

优先级：`panel_password` → `app_key` → `api_key`；都空则面板不设密码。

## 配置

| 文件 | 说明 |
|------|------|
| **`config.json`** | 本地主配置（**勿提交**） |
| `config.example.json` | 模板，含 `_help_*` 说明 |
| `.env` | 可选，`GROK2API_*` 覆盖同名字段 |

```powershell
copy config.example.json config.json
```

## 凭证从哪来

| 方式 | 命令 / 操作 |
|------|-------------|
| 已有 access/refresh | 面板导入，或写入 `data/cli_accounts.json` |
| 注册机 / mint | 写入 `data/cli_accounts.json` |

号池文件：`data/cli_accounts.json`（git 忽略）。与 `~/.grok` 无关。

## 项目结构

```text
app/                 # 2api 服务（CLI only）
  main.py            # 路由 / 面板
  admin.py           # 号池管理 API
  auth.py            # 单会话 auth.json
  cli_pool.py        # 多账号轮询
  oauth_login.py     # login / refresh
  oidc_mint.py       # 可选：密码/session → CLI OIDC
  upstream.py        # cli-chat-proxy 客户端
  static/panel.html  # 管理面板
config.example.json
run.py
scripts/start.ps1
tests/
data/                # 运行时（本地，不入库）
```

## 命令

```text
python -m app login [--method auto|refresh|browser|device|cli]
python -m app status
python -m app serve
python -m app cli-pool [--delete id]
python -m app mint-cli --email ... --password ... --turnstile ...
```

## 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 状态 / 号池摘要 |
| GET | `/panel` | 管理面板 |
| GET | `/v1/models` | 模型列表 |
| GET | `/v1/billing` | 上游额度 |
| POST | `/v1/chat/completions` | OpenAI 对话 |
| POST | `/v1/messages` | Anthropic 兼容 |
| POST | `/v1/responses` | Responses 透传 |
| GET/POST | `/admin/api/cli-accounts*` | 号池管理（需面板密码） |

## Docker

```powershell
copy config.example.json config.json
docker compose up -d --build
```

需自行挂载或注入凭证到 `data/`。

## 说明

- 本仓库只发布 **CLI 代理 + 面板**，不包含任何自动批量注册账号相关代码。
- 免费 / 订阅额度以 xAI 上游策略为准；`/v1/billing` 在免费档可能 limit/used 均为 0，以实际对话是否 429 为准。
- 请遵守 xAI 服务条款；仅限个人自用与学习。

## License

个人工具，按需自用。
