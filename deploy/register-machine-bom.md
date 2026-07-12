# 注册机整理与采购清单

## 架构（已有代码）

```text
面板/CLI  register
    │
    ├─ 邮箱：cfmail（自有域名）或 mail.tm
    ├─ Turnstile：本机 camoufox solver :5072  或 CapMonster
    ├─ 出口：WARP → Privoxy（http://privoxy:8118）
    ├─ 可选：FlareSolverr
    └─ 成功 → mint CLI OIDC → 写入 grok2api.db ready 池
```

| 组件 | 路径 |
|------|------|
| Go 注册核心 | `internal/register/` |
| Python 兼容管线 | `register/` |
| Turnstile solver | `register/tools/turnstile/` |
| 运行时配置 | `data/register_settings.json`（面板为准） |
| 启动种子 | `config.json` |

## 当前生产盘点

| 项 | 状态 |
|----|------|
| 主机 | 4 核 / ~4G 内存 / 50G 盘（偏紧） |
| Turnstile | `:5072` camoufox + xvfb，threads=2 |
| 邮箱 | cfmail `aokix.top` |
| 代理 | privoxy 侧车 |
| FlareSolverr | compose 侧车 |
| 小机限制 | 不适合高并发浏览器打码；批量应用 CapMonster 或独立注册机 |

## 采购清单（按优先级）

### A. 最小可用（沿用本机，几乎 0 元）

- 磁盘清理、settings 对齐 cfmail、solver xvfb、workers=2、每批 ≤20

### B. 稳产量（推荐采购）

| 项 | 规格建议 | 用途 |
|----|----------|------|
| 独立注册机 VPS | ≥8 核 / 16G / 100G SSD | 专跑 solver + 注册，与 API 分离 |
| 住宅/ISP 代理池 | 按量 | 注册出口与 API WARP 隔离 |
| CapMonster（或同类） | 充值 $20–50 起 | 替代本机浏览器 Turnstile |
| CF Mail / 备用域名 | 现有 aokix.top + 1～2 备用域 | 收验证码 |

### C. 推荐小机参数

```json
{
  "email_provider": "cfmail",
  "turnstile_solver": "local",
  "turnstile_solver_url": "http://172.23.0.1:5072",
  "proxy": "http://privoxy:8118",
  "max_workers": 2,
  "total_accounts": 20
}
```

CapMonster 大流量：

```json
{
  "turnstile_solver": "capmonster",
  "capmonster_api_key": "<采购后填入>",
  "max_workers": 10,
  "total_accounts": 100
}
```

## 启动

- 面板：`/panel` → 账号注册  
- CLI：`grok2api register --config config.json --count 10 --workers 2`

## 验收

1. `5072` 监听且无 DISPLAY 报错  
2. 面板 mail / solver / proxy 健康  
3. dry-run → 实跑 1 个进 ready  

## 不要买

- 在 4G 小机上开 20 线程 camoufox  
- 只买 FlareSolverr 当 Turnstile（不能替代）  
