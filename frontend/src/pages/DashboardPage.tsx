import { useCallback, useEffect, useState, type ReactNode } from "react";
import {
  Activity,
  CircleAlert,
  Gauge,
  RefreshCw,
  RotateCcw,
  ShieldCheck,
  Users,
  Zap,
} from "lucide-react";
import { adminApi, AdminApiError, type Dashboard } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { formatAdaptiveNumber, formatAdaptiveRatio } from "@/lib/formatNumber";
import { statusCodeLabel } from "@/lib/statusLabels";

function n(v?: number) {
  if (v == null || Number.isNaN(v)) return "—";
  return new Intl.NumberFormat("zh-CN").format(v);
}

function compact(v?: number) {
  return formatAdaptiveNumber(v);
}

function Stat({
  label,
  value,
  hint,
  icon,
  title,
}: {
  label: string;
  value: string | number;
  hint?: string;
  icon: ReactNode;
  title?: string;
}) {
  return (
    <Card className="min-w-0">
      <CardContent className="flex items-start justify-between gap-4 p-4">
        <div className="min-w-0">
          <p className="text-xs text-muted-foreground">{label}</p>
          <p
            className="mt-2 max-w-full truncate text-2xl font-semibold tracking-tight tabular-nums"
            title={title}
          >
            {value}
          </p>
          {hint ? <p className="mt-1 truncate text-[11px] text-muted-foreground">{hint}</p> : null}
        </div>
        <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-background text-muted-foreground">
          {icon}
        </div>
      </CardContent>
    </Card>
  );
}

export function DashboardPage() {
  const [data, setData] = useState<Dashboard | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setData(await adminApi.dashboard());
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
    const t = window.setInterval(() => void load(), 15000);
    return () => window.clearInterval(t);
  }, [load]);

  const s = data?.summary;
  const pool = data?.account_pool;
  // The backend may return the compact resources shape too.
  const resources = (data as any)?.resources as
    | { activeAccounts?: number; totalAccounts?: number; allTimeRequests?: number }
    | undefined;
  const ready = s?.ready_accounts ?? resources?.activeAccounts ?? pool?.ready;
  const total = resources?.totalAccounts;
  const unavailable = s?.unavailable_accounts ?? pool?.unavailable;
  const requests = s?.total_requests ?? resources?.allTimeRequests;
  const circuit = data?.quota_circuit as { open?: boolean; retry_at?: string } | null | undefined;
  const quotaRemaining = compact(s?.quota_remaining);
  const quotaUsage = formatAdaptiveRatio(s?.quota_actual, s?.quota_limit);

  return (
    <div className="space-y-6">
      <header className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <h1 className="text-xl font-medium tracking-tight">总览</h1>
          <p className="mt-1 text-xs text-muted-foreground">
            号池运行快照
            {data?.generated_at ? ` · 更新于 ${new Date(data.generated_at).toLocaleString()}` : ""}
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
          <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} />
          {loading ? "刷新中" : "刷新"}
        </Button>
      </header>

      {error ? (
        <div className="flex items-center gap-2 rounded-lg bg-destructive/8 px-3 py-2 text-xs text-destructive" role="alert">
          <CircleAlert className="h-4 w-4 shrink-0" />
          {error}
        </div>
      ) : null}

      <section aria-label="核心指标" className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <Stat
          label="可用账号"
          value={n(ready)}
          hint={total != null ? `账号总数 ${n(total)}` : "当前可用池"}
          icon={<Users className="h-4 w-4" />}
        />
        <Stat
          label="不可用账号"
          value={n(unavailable)}
          hint="当前不可用池"
          icon={<CircleAlert className="h-4 w-4" />}
        />
        <Stat
          label="在途并发"
          value={`${n(s?.active_leases)} / ${n(s?.max_active)}`}
          hint="活动租约 / 最大并发"
          icon={<Activity className="h-4 w-4" />}
        />
        <Stat
          label="累计请求"
          value={n(requests)}
          hint="全部账号请求总计"
          icon={<Zap className="h-4 w-4" />}
        />
      </section>

      <section aria-labelledby="operations-heading">
        <div className="mb-3 flex items-center justify-between">
          <div>
            <h2 id="operations-heading" className="text-sm font-medium">运行指标</h2>
            <p className="mt-0.5 text-xs text-muted-foreground">额度、恢复能力与认证状态</p>
          </div>
        </div>
        <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <Stat
            label="Free 剩余"
            value={quotaRemaining.display}
            title={quotaRemaining.exact}
            hint={s?.quota_limit ? `${quotaUsage.display} 已使用` : "暂无额度观测"}
            icon={<Gauge className="h-4 w-4" />}
          />
          <Stat
            label="可自动刷新"
            value={n(s?.refreshable_accounts)}
            hint="持有 refresh token"
            icon={<RotateCcw className="h-4 w-4" />}
          />
          <Stat
            label="待恢复"
            value={n(s?.retry_due)}
            hint="已到重试时间"
            icon={<Activity className="h-4 w-4" />}
          />
          <Stat
            label="认证失败"
            value={n(s?.auth_fail_accounts)}
            hint={s?.total_auth_fails != null ? `累计 ${n(s.total_auth_fails)} 次` : undefined}
            icon={<ShieldCheck className="h-4 w-4" />}
          />
        </div>
      </section>

      {(data as any)?.usage || (data as any)?.recentFailures?.length ? (
        <section aria-label="请求审计" className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <Stat
            label="窗口请求"
            value={n((data as any)?.usage?.requests)}
            hint={`成功率 ${((data as any)?.usage?.successRate ?? 0).toFixed?.(1) ?? (data as any)?.usage?.successRate ?? "—"}%`}
            icon={<Zap className="h-4 w-4" />}
          />
          <Stat
            label="失败请求"
            value={n((data as any)?.usage?.failedRequests)}
            hint="审计窗口内"
            icon={<CircleAlert className="h-4 w-4" />}
          />
          <Stat
            label="P95 延迟"
            value={(data as any)?.usage?.p95DurationMs != null ? `${n((data as any).usage.p95DurationMs)} ms` : "—"}
            hint="请求耗时"
            icon={<Gauge className="h-4 w-4" />}
          />
          <Stat
            label="窗口 Tokens"
            value={n((data as any)?.usage?.tokens)}
            hint="累计 token（若可观测）"
            icon={<Activity className="h-4 w-4" />}
          />
        </section>
      ) : null}

      {(data as any)?.recentFailures?.length ? (
        <Card>
          <CardHeader>
            <CardTitle>最近失败</CardTitle>
            <CardDescription>审计窗口内最近失败请求（不含 prompt/响应正文）</CardDescription>
          </CardHeader>
          <CardContent>
            <ul className="divide-y divide-border/70">
              {((data as any).recentFailures as Array<any>).slice(0, 8).map((item, idx) => (
                <li key={`${item.requestId || idx}`} className="flex items-center justify-between gap-3 py-2.5 first:pt-0 last:pb-0">
                  <div className="min-w-0">
                    <p className="truncate text-xs font-medium">{item.model || "(no model)"} · {item.errorCode || item.errorType || item.statusCode}</p>
                    <p className="truncate text-[11px] text-muted-foreground">{item.path || ""} {item.accountId ? `· ${item.accountId}` : ""}</p>
                  </div>
                  <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">
                    {item.startedAt ? new Date(item.startedAt).toLocaleString() : ""}
                  </span>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>
      ) : null}

      <section aria-label="异常分布" className="grid gap-3 lg:grid-cols-[1fr_1fr_0.75fr]">
        <Card>
          <CardHeader>
            <CardTitle>不可用原因</CardTitle>
            <CardDescription>按账号当前状态汇总</CardDescription>
          </CardHeader>
          <CardContent>
            <ReasonMap map={s?.reasons || pool?.reasons || {}} />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>错误码</CardTitle>
            <CardDescription>最近一次错误分布</CardDescription>
          </CardHeader>
          <CardContent>
            <ReasonMap map={s?.error_codes || {}} empty="暂无错误码" />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>额度熔断</CardTitle>
            <CardDescription>全局额度保护状态</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex items-center justify-between gap-3 rounded-lg bg-background px-3 py-2.5">
              <span className="text-xs text-muted-foreground">状态</span>
              <Badge tone={circuit?.open ? "danger" : "success"}>{circuit?.open ? "熔断中" : "正常"}</Badge>
            </div>
            {circuit?.retry_at ? (
              <div className="flex items-start justify-between gap-3 px-3 text-xs">
                <span className="shrink-0 text-muted-foreground">恢复时间</span>
                <span className="text-right tabular-nums">{new Date(circuit.retry_at).toLocaleString()}</span>
              </div>
            ) : null}
          </CardContent>
        </Card>
      </section>
    </div>
  );
}

function ReasonMap({ map, empty = "无" }: { map: Record<string, number>; empty?: string }) {
  const entries = Object.entries(map).sort((a, b) => b[1] - a[1]);
  if (!entries.length) return <p className="py-5 text-center text-xs text-muted-foreground">{empty}</p>;
  return (
    <ul className="divide-y divide-border/70">
      {entries.map(([k, v]) => {
        const label = statusCodeLabel(k);
        return (
          <li key={k} className="flex items-center justify-between gap-4 py-2.5 first:pt-0 last:pb-0">
            <span className="min-w-0 truncate text-xs text-muted-foreground" title={k || "(empty)"}>
              {label === "—" ? "(空)" : label}
            </span>
            <strong className="shrink-0 text-xs font-medium tabular-nums">{n(v)}</strong>
          </li>
        );
      })}
    </ul>
  );
}
