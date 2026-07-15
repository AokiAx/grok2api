import { useMemo } from "react";
import { Area, Bar, CartesianGrid, ComposedChart, Line, XAxis, YAxis } from "recharts";
import {
  ChartContainer,
  ChartLegend,
  ChartLegendContent,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart";
import { cn } from "@/lib/cn";

export type TrendPoint = {
  bucketStart?: string;
  start?: string;
  end?: string;
  requests?: number;
  failures?: number;
  tokens?: number;
  models?: Array<{ model?: string; tokens?: number }>;
};

type UsageTrendChartProps = {
  series: TrendPoint[];
  topModels?: Array<{ name?: string; model?: string; tokens?: number; count?: number }>;
  period?: "24h" | "7d" | "30d";
  loading?: boolean;
  className?: string;
};

const MODEL_CHART_COLORS = [
  { light: "oklch(0.76 0.1 205)", dark: "oklch(0.72 0.1 205)" },
  { light: "oklch(0.77 0.1 160)", dark: "oklch(0.73 0.1 160)" },
  { light: "oklch(0.8 0.11 85)", dark: "oklch(0.76 0.11 85)" },
  { light: "oklch(0.77 0.11 30)", dark: "oklch(0.73 0.11 30)" },
  { light: "oklch(0.77 0.1 300)", dark: "oklch(0.73 0.1 300)" },
  { light: "oklch(0.74 0.09 185)", dark: "oklch(0.7 0.09 185)" },
  { light: "oklch(0.8 0.1 125)", dark: "oklch(0.76 0.1 125)" },
  { light: "oklch(0.78 0.1 345)", dark: "oklch(0.74 0.1 345)" },
];

function formatCompact(value: number): string {
  if (!Number.isFinite(value)) return "0";
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(value >= 10_000 ? 0 : 1)}k`;
  return new Intl.NumberFormat("zh-CN").format(Math.round(value));
}

function formatTick(iso: string | undefined, period: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const mm = String(date.getMonth() + 1).padStart(2, "0");
  const dd = String(date.getDate()).padStart(2, "0");
  const hh = String(date.getHours()).padStart(2, "0");
  if (period === "24h") return `${hh}:00`;
  if (period === "7d") return `${mm}/${dd} ${hh}:00`;
  return `${mm}/${dd}`;
}

function formatTooltipLabel(start?: string, end?: string, period = "24h"): string {
  if (!start) return "";
  const s = new Date(start);
  if (Number.isNaN(s.getTime())) return "";
  if (period === "30d" || !end) {
    return s.toLocaleDateString("zh-CN", { month: "2-digit", day: "2-digit" });
  }
  const e = new Date(end);
  return `${s.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  })} – ${e.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })}`;
}

function shouldShowTick(index: number, total: number, period: string): boolean {
  if (total <= 8) return true;
  if (period === "24h") return index % 3 === 0 || index === total - 1;
  if (period === "7d") return index % 2 === 0 || index === total - 1;
  return index % Math.ceil(total / 10) === 0 || index === total - 1;
}

export function UsageTrendChart({
  series,
  topModels = [],
  period = "24h",
  loading = false,
  className,
}: UsageTrendChartProps) {
  const modelSeries = useMemo(() => {
    const names = (topModels || [])
      .map((item) => String(item.model || item.name || "").trim())
      .filter(Boolean)
      .slice(0, 8);
    // Fallback: derive from series if topModels empty but buckets have models.
    if (!names.length) {
      const counts = new Map<string, number>();
      for (const bucket of series || []) {
        for (const m of bucket.models || []) {
          const name = String(m.model || "").trim();
          if (!name) continue;
          counts.set(name, (counts.get(name) || 0) + (Number(m.tokens) || 0));
        }
      }
      return Array.from(counts.entries())
        .sort((a, b) => b[1] - a[1])
        .slice(0, 8)
        .map(([model], index) => ({
          key: `model_${index}`,
          model,
          color: MODEL_CHART_COLORS[index % MODEL_CHART_COLORS.length],
        }));
    }
    return names.map((model, index) => ({
      key: `model_${index}`,
      model,
      color: MODEL_CHART_COLORS[index % MODEL_CHART_COLORS.length],
    }));
  }, [series, topModels]);

  const chartData = useMemo(() => {
    const points = series || [];
    return points.map((bucket, index, all) => {
      const row: Record<string, string | number> = {
        requests: Number(bucket.requests) || 0,
        failures: Number(bucket.failures) || 0,
        tokens: Number(bucket.tokens) || 0,
        tick: shouldShowTick(index, all.length, period)
          ? formatTick(bucket.bucketStart || bucket.start, period)
          : "",
        tooltipLabel: formatTooltipLabel(bucket.bucketStart || bucket.start, bucket.end, period),
      };
      let assigned = 0;
      for (const item of modelSeries) {
        const usage = (bucket.models || []).find((candidate) => candidate.model === item.model);
        const value = Number(usage?.tokens) || 0;
        row[item.key] = value;
        assigned += value;
      }
      row.other = Math.max(0, (Number(bucket.tokens) || 0) - assigned);
      return row;
    });
  }, [series, period, modelSeries]);

  const chartConfig = useMemo(() => {
    const config: ChartConfig = {
      requests: {
        label: "请求",
        theme: { light: "oklch(0.68 0.15 245)", dark: "oklch(0.74 0.13 245)" },
      },
      failures: {
        label: "失败",
        theme: { light: "oklch(0.63 0.2 25)", dark: "oklch(0.7 0.17 25)" },
      },
      other: {
        label: "其他模型",
        theme: { light: "oklch(0.82 0.04 245)", dark: "oklch(0.64 0.05 245)" },
      },
    };
    for (const item of modelSeries) {
      config[item.key] = { label: item.model, theme: item.color };
    }
    return config;
  }, [modelSeries]);

  const totals = useMemo(() => {
    let requests = 0;
    let failures = 0;
    let tokens = 0;
    for (const row of chartData) {
      requests += Number(row.requests) || 0;
      failures += Number(row.failures) || 0;
      tokens += Number(row.tokens) || 0;
    }
    return { requests, failures, tokens };
  }, [chartData]);

  if (!chartData.length) {
    return (
      <div className={cn("flex h-[280px] items-center justify-center text-xs text-muted-foreground", className)}>
        暂无趋势数据（需要先有网关请求审计）
      </div>
    );
  }

  return (
    <div className={cn("space-y-3", className)}>
      <div className="flex flex-wrap items-center gap-3 text-[11px] text-muted-foreground">
        <span className="inline-flex items-center gap-1.5">
          请求 <span className="tabular-nums text-foreground/80">{formatCompact(totals.requests)}</span>
        </span>
        <span className="inline-flex items-center gap-1.5">
          失败 <span className="tabular-nums text-foreground/80">{formatCompact(totals.failures)}</span>
        </span>
        <span className="inline-flex items-center gap-1.5">
          Tokens <span className="tabular-nums text-foreground/80">{formatCompact(totals.tokens)}</span>
        </span>
      </div>

      <ChartContainer config={chartConfig} className={cn("h-[280px] w-full aspect-auto", loading && "opacity-40")}>
        <ComposedChart accessibilityLayer data={chartData} margin={{ left: 4, right: 8, top: 8, bottom: 0 }}>
          <defs>
            <linearGradient id="dashboard-requests-fill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="5%" stopColor="var(--color-requests)" stopOpacity={0.24} />
              <stop offset="95%" stopColor="var(--color-requests)" stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid vertical={false} strokeDasharray="3 3" />
          <XAxis dataKey="tick" tickLine={false} axisLine={false} tickMargin={10} minTickGap={12} />
          <YAxis
            yAxisId="usage"
            tickLine={false}
            axisLine={false}
            tickMargin={8}
            width={48}
            allowDecimals={false}
            tickFormatter={(value) => formatCompact(Number(value))}
          />
          <YAxis
            yAxisId="requests"
            orientation="right"
            tickLine={false}
            axisLine={false}
            tickMargin={8}
            width={40}
            allowDecimals={false}
            tickFormatter={(value) => formatCompact(Number(value))}
          />
          <ChartTooltip
            cursor={false}
            content={
              <ChartTooltipContent
                className="w-80 max-w-[calc(100vw-2rem)]"
                indicator="dot"
                labelFormatter={(_label, payload) => payload?.[0]?.payload?.tooltipLabel ?? ""}
                formatter={(value, name) => (
                  <div className="flex w-full items-center justify-between gap-4">
                    <span className="min-w-0 truncate text-xs font-normal text-muted-foreground">
                      {chartConfig[String(name)]?.label ?? name}
                    </span>
                    <span className="shrink-0 font-mono text-xs font-normal tabular-nums text-muted-foreground">
                      {formatCompact(Number(value))}
                    </span>
                  </div>
                )}
              />
            }
          />
          <Area
            yAxisId="requests"
            dataKey="requests"
            type="monotone"
            stroke="none"
            fill="url(#dashboard-requests-fill)"
            dot={false}
            activeDot={false}
            legendType="none"
            tooltipType="none"
          />
          {modelSeries.map((item) => (
            <Bar
              key={item.key}
              yAxisId="usage"
              dataKey={item.key}
              stackId="models"
              fill={`var(--color-${item.key})`}
              maxBarSize={36}
            />
          ))}
          <Bar
            yAxisId="usage"
            dataKey="other"
            stackId="models"
            fill="var(--color-other)"
            maxBarSize={36}
            radius={[3, 3, 0, 0]}
          />
          <Line
            yAxisId="requests"
            dataKey="requests"
            type="monotone"
            stroke="var(--color-requests)"
            strokeWidth={2}
            dot={false}
            activeDot={{ r: 3 }}
          />
          <Line
            yAxisId="requests"
            dataKey="failures"
            type="monotone"
            stroke="var(--color-failures)"
            strokeWidth={1.6}
            strokeDasharray="4 3"
            dot={false}
            activeDot={{ r: 3 }}
          />
          <ChartLegend content={<ChartLegendContent />} />
        </ComposedChart>
      </ChartContainer>
    </div>
  );
}
