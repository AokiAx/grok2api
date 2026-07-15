# Grok2API 演进路线

完善 **Grok Build / CLI** 兼容行为，同时规划管理面重写与可选加固。  
**不引入** Web / Console 多 Provider。

---

## 已落地

| 项 | 说明 |
|----|------|
| CLI 指纹头 | `upstream.Client` 对齐 Build adapter：agent/session 按账号稳定、`x-grok-*` 全套、可配 identifier/UA/token_auth |
| 流式 Accept-Encoding | SSE 使用 `identity`，避免 gzip 破坏流 |
| `prompt_cache_key` sticky | body 字段优先；写入 pool sticky + `x-grok-conv-id` / `conversation-id` |
| 配置 | `client_identifier` / `client_user_agent` / `token_auth` + 环境变量 |
| 选号失败 reason | `scheduler.SelectionError` → `service.PoolUnavailableError.Reason`；HTTP `code` + `X-Grok2API-Pool-Reason` + `Retry-After` |
| `response_format` → `text.format` | Chat/Responses 路径提升；`json_schema` 嵌套解包；Finalize 白名单保留 `text` |
| Admin API v1 | `/api/admin/v1/*` envelope；旧 `/admin/api/*` 兼容；契约见 [ADMIN_API_V1.md](ADMIN_API_V1.md) |
| 前端 SPA C1 | `frontend/` Vite+React：登录/总览/账号/系统；dev proxy → :8787 |
| 前端 C2 | 导入 preview/commit + 账号详情侧栏 |
| 前端 C3 容器交付 | Docker 构建 `frontend/dist` → `/app/frontend/dist` → `/` |

---

## 路线图

### Phase A — CLI 网关继续对齐（短周期）

1. ~~**选号失败 reason 细分**（P1）~~ ✅  
   - 枚举：`no_ready` / `cooling` / `quota` / `saturated` / `auth` / `validating` / `quota_circuit`  
   - HTTP：quota/circuit → 429，其余容量类 → 503 + `Retry-After`  
   - 落点：`scheduler` → `gateway` → `api.writeGatewayError`

2. **compat 缺口只补不换**（P1）  
   - 现有 `compat/*` 已覆盖 namespace 展开、shell/apply_patch 映射、search 去重  
   - ~~`response_format` → `text.format`（json_schema 解包）~~ ✅  
   - 待补：  
     - 兼容警告头 `X-Grok2API-Compatibility-Warnings`（可选）  
     - 明确拒绝且不可等价的参数继续 400，不静默丢  
   - **禁止**整包替换 conversation 层

3. **gzip 非流式响应**（P2）  
   - 非 stream 可 `Accept-Encoding: gzip` + 解压归一

### Phase B — 管理面 API 契约（前端重写前置）

目标：用稳定 JSON API 替换「HTML 内嵌 + 临时 admin 路径」，**不强制引入 Gin**。  
~~建议契约（v1）~~ **已落地**：见 [ADMIN_API_V1.md](ADMIN_API_V1.md)。

- `/api/admin/v1/*`：`{ok,data,error}` envelope  
- 旧 `/admin/api/*`：flat JSON，供 `panel.html`  
- `/healthz` `/readyz` 别名  
- 管理认证已切换为短期 opaque access token + HttpOnly refresh cookie rotation；旧配置密钥仅用于一次性迁移

### Phase C — 前端重写

C1 已脚手架：`frontend/`（Vite + React）。开发见 [frontend/README.md](../frontend/README.md)。

#### 后续（next）

现状：`frontend/` 已作为唯一前端源码，Docker 是唯一正式交付物。

#### 目标形态

```text
frontend/                 # 独立源码包，dist 仅在 Docker/本地构建中生成
  src/
    pages/  dashboard | accounts | import | settings
    api/    typed client → /api/admin/v1
    auth/   login + remember
```

#### 技术建议（可调整）

| 选择 | 建议 | 理由 |
|------|------|------|
| 框架 | Vite + React 或 Vue（二选一） | 生态与表格/表单成熟 |
| UI | 自研 tokens + 少量 headless（勿默认 shadcn 套皮） | 产品向运营台，不是 SaaS 模板 |
| 状态 | 服务端状态用 fetch/SWR；本地 UI 状态轻量 | 避免再造全局 store |
| 部署 | Docker 复制 `dist` 到 `/app/frontend/dist` | 源码、生成物和运行边界清晰 |
| 开发 | Vite dev proxy → `:8787` | 热更前端 |

#### 页面信息架构

1. **总览** — Ready/Unavailable、并发租约、额度粗加总、错误码 Top、circuit  
2. **账号** — 筛选/搜索/分页、recover、delete、sticky/热集标识  
3. **导入** — preview / commit（auth.json / 数组）  
4. **系统** — 版本、脱敏配置、外链 grok-register  
5. **（可选）调试** — 近期失败 reason、trace 开关状态  

#### 里程碑

| M | 交付 |
|---|------|
| M0 | 冻结 admin JSON 契约 + OpenAPI 草图 |
| M1 | 新前端壳 + 登录 + 总览读现有 summary |
| M2 | 账号 CRUD 流完整 |
| M3 | 导入 + 系统页；Docker 静态目录交付；下线旧 panel 依赖 |
| M4 | 删除 `panel.html` 或降为 redirect |

### Phase D — 凭据加密（可选加固） ✅

已落地：AES-256-GCM、Base64 32 字节密钥、Raw Base64 密文；兼容旧 `enc:v1:` 读路径；空 key=明文。

**原触发条件**：DB 可能离开本机、多租户、或仓库/备份会扩散。  
单机本机 SQLite 且磁盘已加密时，优先级低于 Phase A/C。

#### 设计要点

```text
config.credential_key   # 或 env GROK2API_CREDENTIAL_KEY（32-byte / passphrase→KDF）
repository:
  access_token  / refresh_token  列存 ciphertext（前缀 enc:v1:）
读路径: Decrypt → account.Account 明文（仅内存）
写路径: Encrypt 后落库
迁移: schema v4 打开时扫描无前缀行 → 加密写回（事务）
导出: export 命令输出明文 JSON（需 admin），或可选密文导出
```

算法建议：AES-GCM + 随机 nonce；密钥用 scrypt/argon2id 从 passphrase 派生。  
**没有 key 时**：保持现网明文行为（或 fail-closed 由配置 `credential_encryption=required` 决定）。

### Phase E — 明确不做（除非产品变向）

- Grok Web / Console Provider  
- 完整 `/responses/{id}` store ownership（free 池收益低）  
- Build Device OAuth 单账号自助接入（已进入后续产品计划；不恢复旧批量注册机）
- 为管理面强行引入 Gin 全家桶  

---

## 实施顺序建议

```text
A1 选号 reason          ✅
A2 response_format normalize   ✅
C0 admin API 契约 + 旧路径 alias   ✅  → docs/ADMIN_API_V1.md
C1 前端脚手架     ✅ frontend/
C2 导入页 + 打磨   ✅
C3 Docker 交付 dist   ✅
D  加密 ✅
兼容警告头 ✅
```
