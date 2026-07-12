# 外部注册机（grok-register）

自本版本起，**批量注册 / mint 不再内嵌于 grok2api 进程**。

## 仓库

- 路径（本机）：`E:\AokiCore\grok-register`（与 `grok2api` 同级）
- 模块：`github.com/AokiAx/grok-register`

## 对接方式

1. **自动推送**（推荐）  
   在 `grok-register/config.json` 设置：
   - `grok2api_base_url`: `http://host:8787`
   - `grok2api_api_key`: 与 grok2api 的 `api_key` / 管理密钥一致  

   注册成功后会 `POST /admin/api/accounts/import`。

2. **手工导入**  
   读 `grok-register/output/import/*.json`，在面板导入，或调同一 import API。

## 本仓仍保留

- `internal/register/*` 源码暂存（可后续删除）
- 面板「注册」相关路由：未接线时不注册；请用外部 CLI  
- 管理端 **账号导入** API 与面板导入

## 删除内嵌后的 CLI

```text
grok2api register  → 提示改用 grok-register
grok2api mint      → 提示改用 grok-register
```
