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
  requests?: number;
  failures?: number;
  tokens?: number;
};

type UsageTrendChartProps = {
  series: TrendPoint[];
  period?: "24h" | "7d" | "30d";
  loading?: boolean;
  className?: string;
};

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

function formatTooltipLabel(iso: string | undefined, period: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  if (period === "30d") {
    return date.toLocaleDateString("zh-CN", { month: "2-digit", day: "2-digit" });
  }
  return date.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function shouldShowTick(index: number, total: number, period: string): boolean {
  if (total <= 8) return true;
  if (period === "24h") return index % 3 === 0 || index === total - 1;
  if (period === "7d") return index % 2 === 0 || index === total - 1;
  return index % Math.ceil(total / 10) === 0 || index === total - 1;
}

const chartConfig = {
  requests: {
    label: "请求",
    theme: { light: "oklch(0.68 0.15 245)", dark: "oklch(0.74 0.13 245)" },
  },
  failures: {
    label: "失败",
    theme: { light: "oklch(0.63 0.2 25)", dark: "oklch(0.7 0.17 25)" },
  },
  tokens: {
    label: "Tokens",
    theme: { light: "oklch(0.76 0.1 160)", dark: "oklch(0.73 0.1 160)" },
  },
} satisfies ChartConfig;

export function UsageTrendChart({
  series,
  period = "24h",
  loading = false,
  className,
}: UsageTrendChartProps) {
  const chartData = useMemo(() => {
    const points = series || [];
    return points.map((bucket, index, all) => ({
      requests: Number(bucket.requests) || 0,
      failures: Number(bucket.failures) || 0,
      tokens: Number(bucket.tokens) || 0,
      tick: shouldShowTick(index, all.length, period)
        ? formatTick(bucket.bucketStart, period)
        : "",
      tooltipLabel: formatTooltipLabel(bucket.bucketStart, period),
    }));
  }, [series, period]);

  const totals = useMemo(() => {
    return chartData.reduce(
      (acc, row) => {
        acc.requests += row.requests;
        acc.failures += row.failures;
        acc.tokens += row.tokens;
        return acc;
      },
      { requests: 0, failures: 0, tokens: 0 },
    );
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
          <span className="size-2 rounded-full bg-[var(--color-requests,#4b8df8)]" />
          请求
          <span className="tabular-nums text-foreground/80">{formatCompact(totals.requests)}</span>
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="size-2 rounded-full bg-[var(--color-failures,#e35d5d)]" />
          失败
          <span className="tabular-nums text-foreground/80">{formatCompact(totals.failures)}</span>
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="size-2 rounded-full bg-[var(--color-tokens,#4db68a)]" />
          Tokens
          <span className="tabular-nums text-foreground/80">{formatCompact(totals.tokens)}</span>
        </span>
      </div>

      <ChartContainer
        config={chartConfig}
        className={cn("h-[280px] w-full aspect-auto", loading && "opacity-40")}
      >
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
            yAxisId="tokens"
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
                className="w-72 max-w-[calc(100vw-2rem)]"
                indicator="dot"
                labelFormatter={(_label, payload) => payload?.[0]?.payload?.tooltipLabel ?? ""}
                formatter={(value, name) => (
                  <div className="flex w-full items-center justify-between gap-4">
                    <span className="min-w-0 truncate text-xs font-normal text-muted-foreground">
                      {chartConfig[String(name) as keyof typeof chartConfig]?.label ?? name}
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
          <Bar yAxisId="tokens" dataKey="tokens" fill="var(--color-tokens)" maxBarSize={36} radius={[3, 3, 0, 0]} />
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
            strokeWidth={1.8}
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
