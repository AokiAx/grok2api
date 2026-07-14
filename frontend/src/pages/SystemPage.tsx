import { useEffect, useState } from "react";
import { adminApi, AdminApiError } from "@/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

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
        <p className="mt-1.5 text-xs text-muted-foreground">当前服务版本与公开运行配置。</p>
      </div>
      {error ? <p className="text-sm text-destructive">{error}</p> : null}
      <Card className="max-w-3xl p-4 sm:p-5">
        <CardHeader>
          <CardTitle>运行信息</CardTitle>
        </CardHeader>
        <CardContent className="mt-2 px-0 pb-0 text-xs">
          {Object.entries(info || {}).map(([k, v]) => (
            <div key={k} className="grid grid-cols-[120px_1fr] gap-4 border-b border-border/70 py-3 first:pt-0 last:border-0 last:pb-0">
              <span className="text-muted-foreground">{k}</span>
              <span className="mono">{v}</span>
            </div>
          ))}
        </CardContent>
      </Card>
    </div>
  );
}
