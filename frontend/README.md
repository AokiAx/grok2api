# Grok2API 管理前端

本目录是对接 `/api/admin/v1` 的 Vite + React 前端源码。正式交付由 Docker 多阶段构建完成，`dist/` 不提交，也不同步到 Go 包。

## 本地开发

```bash
# 终端 1：后端开发服务（默认只提供 API）
go -C backend run ./cmd/grok2api serve --config ../config.json

# 终端 2：前端热更新服务
cd frontend
npm ci
npm run dev
```

打开 `http://127.0.0.1:5173/login`。Vite 会将 API 请求代理到 `http://127.0.0.1:8787`。

## 构建

```bash
cd frontend
npm ci
npm run build
```

产物写入 `frontend/dist/`。正式镜像构建会将它复制到 `/app/frontend/dist`，并设置 `GROK2API_FRONTEND_STATIC_PATH=/app/frontend/dist`。无需手工复制生成文件。

## 页面

| 路由 | 说明 |
|------|------|
| `/login` | 管理员 session 登录；access token 仅保存在内存，refresh 使用 HttpOnly cookie |
| `/` | 号池总览与请求审计 |
| `/accounts` | 账号列表 / 恢复 / 删除；页内「导入」含 JSON 与 Device OAuth |
| `/client-keys` | 客户端密钥列表、创建、权限编辑与撤销；secret 仅创建后展示一次 |
| `/models` | 模型注册表 |
| `/settings` | 运行配置与版本回滚；页内「系统」含版本信息与 OpenAPI 入口 |
| `/import` | 兼容跳转 → `/accounts` |
| `/system` | 兼容跳转 → `/settings` |
