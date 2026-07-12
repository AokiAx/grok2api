# 外部注册机（grok-register）

本仓库 **已完全移除** 内嵌注册机代码。

## 仓库位置

- 本机：`E:\AokiCore\grok-register`（与 `grok2api` 同级）
- 模块：`github.com/AokiAx/grok-register`

## 对接

1. 在 `grok-register/config.json` 配置邮箱、Turnstile、代理  
2. 可选：`grok2api_base_url` + `grok2api_api_key` → 自动 `POST /admin/api/accounts/import`  
3. 或将 `output/import/*.json` 在面板导入  

## 本服务仍提供

- 管理端账号列表 / 恢复 / 删除  
- `POST /admin/api/accounts/import`（及 preview）  
- 号池调度与 API 代理  
