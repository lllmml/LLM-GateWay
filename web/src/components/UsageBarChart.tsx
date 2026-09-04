import { useMemo } from "react";
import type { UsagePoint } from "../api/analytics";
import { formatCompact, formatNanoUSD } from "../lib/format";

// Minimal dependency-free SVG bar chart for usage timeseries. Renders one
// metric across the zero-filled UTC buckets returned by the API.
//
// Cost is stored/transmitted in nano-USD; bars are unit-correct only because
// the formatter matches the metric. Cost bars never imply a bill: buckets
// whose requests were all unpriced carry no attributed cost and are labeled
// as such instead of presenting $0.00 as a known zero.

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
      label: metric === "estimated_cost_nano_usd" ? formatNanoUSD(point[metric]) : formatCompact(point[metric]),
      coverage:
        metric === "estimated_cost_nano_usd" && point.priced_requests === 0 && point.unpriced_requests > 0
          ? "unpriced — no attributed cost (Week 7 has no DeepSeek price versions)"
          : metric === "estimated_cost_nano_usd" && point.priced_requests > 0 && point.unpriced_requests > 0
            ? `partial — ${point.priced_requests} priced · ${point.unpriced_requests} unpriced`
            : undefined,
      bucketLabel: bucketLabel(points, index, metric === "requests_total"),
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
          const isUnpriced = bar.coverage?.startsWith("unpriced") ?? false;
          return (
            <g key={bar.key}>
              <title>{bar.coverage ? `${bar.bucketLabel}: ${bar.coverage}` : `${bar.bucketLabel}: ${bar.label}`}</title>
              <rect fill={bar.value > 0 ? "currentColor" : isUnpriced ? "#fde68a" : "#e2e8f0"} height={barHeight} opacity={0.9} width={barWidth} x={x} y={height - 6 - barHeight} />
            </g>
          );
        })}
      </svg>
      {metric === "estimated_cost_nano_usd" ? (
        <p className="mt-1 text-[11px] leading-4 text-slate-400">
          Amber bars mark buckets with requests but no attributed cost (unpriced); they are not billed as $0.00. Values are base-rate
          estimates, not invoices.
        </p>
      ) : null}
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
