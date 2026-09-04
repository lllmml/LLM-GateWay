import { useMemo } from "react";
import type { UsagePoint } from "../api/analytics";
import { formatCompact } from "../lib/format";

// Minimal dependency-free SVG bar chart for usage timeseries. Renders one
// metric across zero-filled UTC buckets returned by the API.

type MetricKey = "requests_total" | "estimated_cost_nano_usd" | "prompt_tokens";

type Props = {
  points: UsagePoint[];
  metric: MetricKey;
  height?: number;
};

const metricLabel: Record<MetricKey, string> = {
  requests_total: "Requests",
  estimated_cost_nano_usd: "Estimated cost (USD)",
  prompt_tokens: "Prompt tokens",
};

export function UsageBarChart({ points, metric, height = 160 }: Props) {
  const bars = useMemo(() => {
    const values = points.map((point) => point[metric]);
    const max = values.reduce((largest, value) => (value > largest ? value : largest), 0);
    return points.map((point, index) => ({
      key: point.ts,
      value: point[metric],
      ratio: max > 0 ? point[metric] / max : 0,
      label: bucketLabel(points, index, metric === "requests_total"),
    }));
  }, [points, metric]);

  if (points.length === 0) {
    return (
      <div className="flex h-40 items-center justify-center rounded-md border border-dashed border-slate-200 text-sm text-slate-500">
        No data in the selected window.
      </div>
    );
  }

  const barWidth = Math.max(2, Math.min(28, 560 / bars.length - 2));
  return (
    <div>
      <p className="mb-2 text-xs font-medium uppercase tracking-wide text-slate-500">{metricLabel[metric]}</p>
      <svg
        aria-label={metricLabel[metric]}
        className="block w-full"
        height={height}
        preserveAspectRatio="none"
        role="img"
        viewBox={`0 0 560 ${height}`}
      >
        {bars.map((bar, index) => {
          const x = (index * (560 - barWidth)) / Math.max(1, bars.length - 1);
          const barHeight = Math.max(1, bar.ratio * (height - 14));
          return (
            <g key={bar.key}>
              <title>{`${bar.label}: ${formatCompact(bar.value)}`}</title>
              <rect fill={bar.value > 0 ? "currentColor" : "#e2e8f0"} height={barHeight} opacity={0.9} width={barWidth} x={x} y={height - 6 - barHeight} />
            </g>
          );
        })}
      </svg>
      {points.length > 1 ? (
        <div className="mt-1 flex justify-between text-[11px] text-slate-400">
          <span>{formatDay(points[0].ts)}</span>
          <span>{formatDay(points[points.length - 1].ts)}</span>
        </div>
      ) : null}
    </div>
  );
}

function bucketLabel(points: UsagePoint[], index: number, showDate: boolean): string {
  const point = points[index];
  if (!showDate) {
    return new Date(point.ts).toISOString();
  }
  const next = points[index + 1];
  if (!next) {
    return formatDay(point.ts);
  }
  return `${formatDay(point.ts)} → ${formatDay(next.ts)}`;
}

function formatDay(iso: string): string {
  return iso.slice(0, 10);
}
