import { useMemo } from "react";
import { cn } from "@/lib/cn";

export type TrendPoint = {
  bucketStart?: string;
  requests?: number;
  failures?: number;
  tokens?: number;
};

type SeriesKey = "requests" | "failures" | "tokens";

const SERIES: Array<{ key: SeriesKey; label: string; color: string }> = [
  { key: "requests", label: "请求", color: "var(--color-foreground)" },
  { key: "failures", label: "失败", color: "var(--color-destructive)" },
  { key: "tokens", label: "Tokens", color: "var(--quota-product-1)" },
];

function valueOf(point: TrendPoint, key: SeriesKey): number {
  const raw = point[key];
  return typeof raw === "number" && Number.isFinite(raw) ? Math.max(0, raw) : 0;
}

function formatAxis(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 1_000) return `${(value / 1_000).toFixed(value >= 10_000 ? 0 : 1)}k`;
  return String(Math.round(value));
}

function formatBucketLabel(iso?: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return `${String(date.getMonth() + 1).padStart(2, "0")}/${String(date.getDate()).padStart(2, "0")} ${String(date.getHours()).padStart(2, "0")}:00`;
}

function buildPath(values: number[], width: number, height: number, max: number): string {
  if (!values.length || max <= 0) return "";
  return values
    .map((value, index) => {
      const x = values.length === 1 ? width / 2 : (index / (values.length - 1)) * width;
      const y = height - (value / max) * height;
      return `${index === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(" ");
}

function buildArea(values: number[], width: number, height: number, max: number): string {
  const line = buildPath(values, width, height, max);
  if (!line || !values.length) return "";
  const lastX = values.length === 1 ? width / 2 : width;
  return `${line} L${lastX.toFixed(2)},${height.toFixed(2)} L0,${height.toFixed(2)} Z`;
}

export function UsageTrendChart({
  series,
  className,
}: {
  series: TrendPoint[];
  className?: string;
}) {
  const chart = useMemo(() => {
    const points = (series || []).filter((item) => item && item.bucketStart);
    const width = 720;
    const height = 180;
    const padX = 36;
    const padY = 10;
    const innerW = width - padX * 2;
    const innerH = height - padY * 2;

    const maxRequests = Math.max(
      1,
      ...points.map((p) => Math.max(valueOf(p, "requests"), valueOf(p, "failures"))),
    );
    const maxTokens = Math.max(1, ...points.map((p) => valueOf(p, "tokens")));
    const hasAny =
      points.some((p) => valueOf(p, "requests") > 0 || valueOf(p, "failures") > 0 || valueOf(p, "tokens") > 0);

    const paths = SERIES.map((spec) => {
      const values = points.map((p) => valueOf(p, spec.key));
      const max = spec.key === "tokens" ? maxTokens : maxRequests;
      return {
        ...spec,
        values,
        path: buildPath(values, innerW, innerH, max),
        area: buildArea(values, innerW, innerH, max),
        max,
        total: values.reduce((sum, v) => sum + v, 0),
      };
    });

    const labels = points.map((p) => formatBucketLabel(p.bucketStart));
    const ticks = [0, 0.25, 0.5, 0.75, 1].map((ratio) => ({
      ratio,
      y: padY + innerH * (1 - ratio),
      left: formatAxis(maxRequests * ratio),
      right: formatAxis(maxTokens * ratio),
    }));

    return {
      points,
      paths,
      labels,
      ticks,
      width,
      height,
      padX,
      padY,
      innerW,
      innerH,
      hasAny,
    };
  }, [series]);

  if (!chart.points.length) {
    return (
      <div className={cn("flex h-56 items-center justify-center text-xs text-muted-foreground", className)}>
        暂无趋势数据（需要先有网关请求审计）
      </div>
    );
  }

  const first = chart.labels[0];
  const mid = chart.labels[Math.floor(chart.labels.length / 2)];
  const last = chart.labels[chart.labels.length - 1];

  return (
    <div className={cn("space-y-3", className)}>
      <div className="flex flex-wrap items-center gap-3 text-[11px] text-muted-foreground">
        {chart.paths.map((item) => (
          <span key={item.key} className="inline-flex items-center gap-1.5">
            <span className="inline-block size-2 rounded-full" style={{ background: item.color }} />
            {item.label}
            <span className="tabular-nums text-foreground/80">{formatAxis(item.total)}</span>
          </span>
        ))}
      </div>

      {!chart.hasAny ? (
        <div className="rounded-lg border border-dashed border-border/70 px-3 py-6 text-center text-xs text-muted-foreground">
          窗口内暂无请求；有流量后会自动填充曲线
        </div>
      ) : null}

      <div className="relative">
        <svg
          viewBox={`0 0 ${chart.width} ${chart.height}`}
          className="h-56 w-full overflow-visible"
          role="img"
          aria-label="用量趋势：请求、失败与 tokens"
        >
          {chart.ticks.map((tick) => (
            <g key={tick.ratio}>
              <line
                x1={chart.padX}
                x2={chart.padX + chart.innerW}
                y1={tick.y}
                y2={tick.y}
                stroke="currentColor"
                strokeOpacity={0.08}
              />
              <text x={2} y={tick.y + 3} className="fill-muted-foreground text-[10px]">
                {tick.left}
              </text>
              <text
                x={chart.width - 2}
                y={tick.y + 3}
                textAnchor="end"
                className="fill-muted-foreground text-[10px]"
              >
                {tick.right}
              </text>
            </g>
          ))}

          <g transform={`translate(${chart.padX}, ${chart.padY})`}>
            {chart.paths.map((item) =>
              item.area && item.key === "requests" ? (
                <path key={`${item.key}-area`} d={item.area} fill="currentColor" opacity={0.06} />
              ) : null,
            )}
            {chart.paths.map((item) =>
              item.path ? (
                <path
                  key={item.key}
                  d={item.path}
                  fill="none"
                  stroke={item.color}
                  strokeWidth={item.key === "tokens" ? 2.2 : 1.8}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  strokeDasharray={item.key === "failures" ? "4 3" : undefined}
                  opacity={item.key === "tokens" ? 0.95 : 1}
                />
              ) : null,
            )}
          </g>
        </svg>

        <div className="mt-1 flex justify-between px-1 text-[10px] text-muted-foreground tabular-nums">
          <span>{first}</span>
          <span>{mid && mid !== first && mid !== last ? mid : ""}</span>
          <span>{last}</span>
        </div>
        <div className="mt-1 flex justify-between px-1 text-[10px] text-muted-foreground">
          <span>左轴：请求 / 失败</span>
          <span>右轴：Tokens</span>
        </div>
      </div>
    </div>
  );
}
