import { useCallback, useEffect, useState } from "react";
import { CircleAlert, RefreshCw } from "lucide-react";
import { adminApi, AdminApiError, type Dashboard } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { UsageTrendChart } from "@/components/UsageTrendChart";
import { formatAdaptiveNumber, formatAdaptiveRatio } from "@/lib/formatNumber";
import { statusCodeLabel } from "@/lib/statusLabels";

function n(v?: number) {
  if (v == null || Number.isNaN(v)) return "—";
  return new Intl.NumberFormat("zh-CN").format(v);
}

function compact(v?: number) {
  return formatAdaptiveNumber(v);
}

type Metric = {
  label: string;
  value: string | number;
  hint?: string;
  title?: string;
};

function MetricCell({ label, value, hint, title }: Metric) {
  return (
    <div className="min-w-0 space-y-1.5">
      <p className="text-[11px] text-muted-foreground">{label}</p>
      <p className="truncate text-xl font-semibold tracking-tight tabular-nums" title={title}>
        {value}
      </p>
      {hint ? <p className="truncate text-[11px] text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

function MetricGroup({
  title,
  description,
  metrics,
}: {
  title: string;
  description?: string;
  metrics: Metric[];
}) {
  return (
    <Card className="min-w-0">
      <CardHeader className="space-y-0 pb-3">
        <CardTitle>{title}</CardTitle>
        {description ? <CardDescription className="mt-0.5">{description}</CardDescription> : null}
      </CardHeader>
      <CardContent className="grid grid-cols-2 gap-x-4 gap-y-4 pt-0">
        {metrics.map((metric) => (
          <MetricCell key={metric.label} {...metric} />
        ))}
      </CardContent>
    </Card>
  );
}

export function DashboardPage() {
  const [period, setPeriod] = useState<"24h" | "7d" | "30d">("24h");
  const [data, setData] = useState<Dashboard | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setData(await adminApi.dashboard(period));
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, [period]);

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
  const usage = data?.usage;
  const trendSeries = data?.series || [];
  const topModels = data?.topModels || [];
  const topAccounts = data?.topAccounts || [];
  const recentFailures = data?.recentFailures || [];
  const periodLabel =
    period === "24h" ? "近 24 小时" : period === "7d" ? "近 7 天" : "近 30 天";
  const inputTokens = usage?.inputTokens ?? 0;
  const cachedInputTokens = usage?.cachedInputTokens ?? 0;
  const cacheHitRate =
    inputTokens > 0 ? (cachedInputTokens / inputTokens) * 100 : 0;
  const cacheHitDisplay =
    inputTokens > 0 ? `${cacheHitRate.toFixed(1)}%` : "—";

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
        <div className="flex flex-wrap items-center gap-2">
          <div className="flex shrink-0 rounded-full bg-secondary/60 p-0.5" aria-label="统计窗口">
            {([
              ["24h", "24h"],
              ["7d", "7天"],
              ["30d", "30天"],
            ] as const).map(([value, label]) => (
              <button
                key={value}
                type="button"
                className={`h-7 rounded-full px-3 text-[11px] font-medium transition-colors ${
                  period === value
                    ? "bg-primary text-primary-foreground"
                    : "text-muted-foreground hover:text-foreground"
                }`}
                onClick={() => setPeriod(value)}
              >
                {label}
              </button>
            ))}
          </div>
          <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
            <RefreshCw className={`h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} />
            {loading ? "刷新中" : "刷新"}
          </Button>
        </div>
      </header>

      {error ? (
        <div className="flex items-center gap-2 rounded-lg bg-destructive/8 px-3 py-2 text-xs text-destructive" role="alert">
          <CircleAlert className="h-4 w-4 shrink-0" />
          {error}
        </div>
      ) : null}

      <section aria-label="核心指标" className="grid gap-3 xl:grid-cols-3">
        <MetricGroup
          title="号池"
          description="账号可用性与并发"
          metrics={[
            {
              label: "可用账号",
              value: n(ready),
              hint: total != null ? `总数 ${n(total)}` : "当前可用池",
            },
            {
              label: "不可用",
              value: n(unavailable),
              hint: "当前不可用池",
            },
            {
              label: "在途并发",
              value: `${n(s?.active_leases)} / ${n(s?.max_active)}`,
              hint: "活动租约 / 上限",
            },
            {
              label: "累计请求",
              value: n(requests),
              hint: "全部账号总计",
            },
          ]}
        />
        <MetricGroup
          title="运行"
          description="额度、恢复与认证"
          metrics={[
            {
              label: "Free 剩余",
              value: quotaRemaining.display,
              title: quotaRemaining.exact,
              hint: s?.quota_limit ? `${quotaUsage.display} 已使用` : "暂无额度观测",
            },
            {
              label: "可自动刷新",
              value: n(s?.refreshable_accounts),
              hint: "持有 refresh token",
            },
            {
              label: "待恢复",
              value: n(s?.retry_due),
              hint: "已到重试时间",
            },
            {
              label: "认证失败",
              value: n(s?.auth_fail_accounts),
              hint: s?.total_auth_fails != null ? `累计 ${n(s.total_auth_fails)} 次` : undefined,
            },
          ]}
        />
        <MetricGroup
          title="窗口用量"
          description={periodLabel}
          metrics={[
            {
              label: "请求",
              value: n(usage?.requests),
              hint:
                usage?.successRate != null
                  ? `成功率 ${Number(usage.successRate).toFixed(1)}%`
                  : "成功率 —",
            },
            {
              label: "失败",
              value: n(usage?.failedRequests),
              hint: periodLabel,
            },
            {
              label: "P95 延迟",
              value: usage?.p95DurationMs != null ? `${n(usage.p95DurationMs)} ms` : "—",
              hint: "请求耗时",
            },
            {
              label: "Tokens",
              value: n(usage?.tokens),
              hint:
                inputTokens > 0 || (usage?.outputTokens ?? 0) > 0
                  ? `入 ${n(usage?.inputTokens)} / 出 ${n(usage?.outputTokens)}`
                  : "累计 token",
            },
          ]}
        />
      </section>

      <section aria-label="Token 明细" className="grid gap-3 xl:grid-cols-3">
        <MetricGroup
          title="Token 明细"
          description={`${periodLabel} · 来自上游 usage`}
          metrics={[
            {
              label: "输入",
              value: n(usage?.inputTokens),
              hint: "input / prompt tokens",
            },
            {
              label: "缓存命中",
              value: n(usage?.cachedInputTokens),
              hint: `命中率 ${cacheHitDisplay}`,
              title:
                inputTokens > 0
                  ? `${cachedInputTokens} / ${inputTokens} cached input tokens`
                  : undefined,
            },
            {
              label: "输出",
              value: n(usage?.outputTokens),
              hint: "output / completion tokens",
            },
            {
              label: "命中率",
              value: cacheHitDisplay,
              hint: "cached / input",
            },
          ]}
        />
      </section>

      {(usage || recentFailures.length || trendSeries.length) ? (
        <section aria-label="请求审计" className="space-y-3">
          <Card>
            <CardHeader className="flex-row items-start justify-between space-y-0">
              <div>
                <CardTitle>用量趋势</CardTitle>
              </div>
            </CardHeader>
            <CardContent>
              <UsageTrendChart
                series={trendSeries}
                topModels={topModels}
                period={period}
                loading={loading}
              />
            </CardContent>
          </Card>

          {(topModels.length > 0 || topAccounts.length > 0) ? (
            <div className="grid gap-3 lg:grid-cols-2">
              <Card>
                <CardHeader>
                  <CardTitle>热门模型</CardTitle>
                  <CardDescription>{periodLabel} 请求量 Top</CardDescription>
                </CardHeader>
                <CardContent>
                  <TopList items={topModels} empty="暂无模型用量" />
                </CardContent>
              </Card>
              <Card>
                <CardHeader>
                  <CardTitle>热门账号</CardTitle>
                  <CardDescription>{periodLabel} 请求量 Top</CardDescription>
                </CardHeader>
                <CardContent>
                  <TopList items={topAccounts} empty="暂无账号用量" />
                </CardContent>
              </Card>
            </div>
          ) : null}
        </section>
      ) : null}

      {recentFailures.length ? (
        <Card>
          <CardHeader>
            <CardTitle>最近失败</CardTitle>
            <CardDescription>审计窗口内最近失败请求（不含 prompt/响应正文）</CardDescription>
          </CardHeader>
          <CardContent>
            <ul className="divide-y divide-border/70">
              {recentFailures.slice(0, 8).map((item, idx) => (
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

function TopList({
  items,
  empty,
}: {
  items: Array<{ name?: string; model?: string; count?: number; requests?: number; tokens?: number }>;
  empty: string;
}) {
  if (!items.length) {
    return <p className="py-5 text-center text-xs text-muted-foreground">{empty}</p>;
  }
  const max = Math.max(1, ...items.map((item) => Number(item.requests ?? item.count) || 0));
  return (
    <ul className="space-y-2.5">
      {items.slice(0, 8).map((item, index) => {
        const count = Number(item.requests ?? item.count) || 0;
        const tokens = Number(item.tokens) || 0;
        const width = `${Math.max(6, (count / max) * 100)}%`;
        const label = item.model || item.name || "—";
        return (
          <li key={`${label}-${index}`} className="space-y-1">
            <div className="flex items-center justify-between gap-3 text-xs">
              <span className="min-w-0 truncate font-mono text-[11px]">{label}</span>
              <span className="shrink-0 tabular-nums text-muted-foreground">
                {n(count)}
                {tokens > 0 ? ` · ${n(tokens)} tok` : ""}
              </span>
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-secondary">
              <div className="h-full rounded-full bg-foreground/70" style={{ width }} />
            </div>
          </li>
        );
      })}
    </ul>
  );
}
