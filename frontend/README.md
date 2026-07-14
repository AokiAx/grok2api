# grok2api 管理前端

**我们自己的前端**，对接本仓库 `/api/admin/v1`。

采用低饱和、紧凑型运维后台设计，使用登录分栏、密集侧栏和 OKLCH 主题 tokens。

## 开发

```bash
# 终端 1
go run ./cmd/grok2api serve --config config.json

# 终端 2
cd frontend
npm install
npm run dev
```

打开 `http://127.0.0.1:5173/login`（代理到 `:8787`）。

## 构建并 embed

```bash
cd frontend && npm run build
# 同步到 Go embed 目录
bash scripts/sync-paneldist.sh
```

生产访问：`http://127.0.0.1:8787/`

## 页面

| 路由 | 说明 |
|------|------|
| `/login` | 管理密钥登录 |
| `/` | 号池总览 |
| `/accounts` | 账号列表 / 恢复 / 删除 |
| `/import` | 导入 preview/commit |
| `/system` | 版本信息 |
