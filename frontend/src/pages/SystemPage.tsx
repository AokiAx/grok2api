import { useEffect, useState } from "react";
import { ExternalLink } from "lucide-react";
import { adminApi, AdminApiError } from "@/api/client";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

const linkClass =
  "inline-flex h-8 items-center gap-1.5 rounded-full border border-input bg-background px-3 text-xs font-medium transition-colors hover:bg-secondary";

export function SystemPage() {
  const [info, setInfo] = useState<Record<string, string> | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    adminApi
      .system()
      .then((s) =>
        setInfo({
          version: s.version,
          api_version: s.api_version,
          default_model: s.default_model,
          auth_required: String(s.auth_required),
        }),
      )
      .catch((e) => setError(e instanceof AdminApiError ? e.message : "加载失败"));
  }, []);

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-xl font-medium">系统</h1>
        <p className="mt-1.5 text-xs text-muted-foreground">当前服务版本、公开配置与 API 文档入口。</p>
      </div>
      {error ? <p className="text-sm text-destructive">{error}</p> : null}

      <div className="grid gap-4 lg:grid-cols-2">
        <Card className="p-4 sm:p-5">
          <CardHeader>
            <CardTitle>运行信息</CardTitle>
            <CardDescription>来自 /api/admin/v1/system</CardDescription>
          </CardHeader>
          <CardContent className="mt-2 px-0 pb-0 text-xs">
            {Object.entries(info || {}).map(([k, v]) => (
              <div
                key={k}
                className="grid grid-cols-[120px_1fr] gap-4 border-b border-border/70 py-3 first:pt-0 last:border-0 last:pb-0"
              >
                <span className="text-muted-foreground">{k}</span>
                <span className="mono">{v}</span>
              </div>
            ))}
          </CardContent>
        </Card>

        <Card className="p-4 sm:p-5">
          <CardHeader>
            <CardTitle>API 文档</CardTitle>
            <CardDescription>OpenAPI 契约与 Swagger UI（同域）</CardDescription>
          </CardHeader>
          <CardContent className="mt-3 flex flex-wrap gap-2">
            <a className={linkClass} href="/docs" target="_blank" rel="noreferrer">
              打开 /docs
              <ExternalLink className="h-3.5 w-3.5" />
            </a>
            <a className={linkClass} href="/openapi.json" target="_blank" rel="noreferrer">
              openapi.json
            </a>
            <a className={linkClass} href="/openapi.yaml" target="_blank" rel="noreferrer">
              openapi.yaml
            </a>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
