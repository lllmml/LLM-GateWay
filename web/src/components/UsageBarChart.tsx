import { useMemo } from "react";
import type { UsagePoint } from "../api/analytics";
import { costCoverageState, type CostCoverageState } from "../lib/costCoverage";
import { formatCompact, formatNanoUSD } from "../lib/format";

// Minimal dependency-free SVG bar chart for usage timeseries. Renders one
// metric across the zero-filled UTC buckets returned by the API.
//
// Cost is stored/transmitted in nano-USD; the formatter always matches the
// metric (cost -> formatNanoUSD, requests/tokens -> compact/count). Cost bars
// never imply a bill: buckets whose traffic is unpriced or not estimated
// (failed-only) are labeled as such instead of presenting $0.00 as a known
// zero. Only buckets with zero traffic may read as zero cost.

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
  const isCost = metric === "estimated_cost_nano_usd";
  const showDateLabel = metric === "requests_total";
  const bars = useMemo(() => {
    const values = points.map((point) => point[metric]);
    const max = values.reduce((largest, value) => (value > largest ? value : largest), 0);
    return points.map((point, index) => {
      const value = point[metric];
      const state = isCost ? costCoverageState(point.requests_total, point.priced_requests, point.unpriced_requests) : undefined;
      const title = `${bucketLabel(points, index, showDateLabel)}: ${state ? costTitle(state, value, point.priced_requests, point.unpriced_requests) : formatCompact(value)}`;
      return {
        key: point.ts,
        value,
        ratio: max > 0 ? value / max : 0,
        title,
        state,
        unknownCost: state === "unpriced" || state === "not_estimated",
      };
    });
  }, [points, metric, isCost, showDateLabel]);

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
              <title>{bar.title}</title>
              <rect fill={bar.value > 0 ? "currentColor" : bar.unknownCost ? "#fde68a" : "#e2e8f0"} height={barHeight} opacity={0.9} width={barWidth} x={x} y={height - 6 - barHeight} />
            </g>
          );
        })}
      </svg>
      {metric === "estimated_cost_nano_usd" ? (
        <p className="mt-1 text-[11px] leading-4 text-slate-400">
          Amber bars mark buckets with traffic but no cost attribution (succeeded-but-unpriced) or no estimate (failed-only); they are not billed as
          $0.00. Values are base-rate estimates, not invoices.
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

// costTitle maps the shared coverage semantics to a tooltip string:
//   empty traffic   -> $0.00 (a real zero)
//   attributed      -> the base-rate estimate
//   partial         -> estimate + priced/unpriced split
//   unpriced        -> explanatory text, never $0.00
//   not_estimated   -> failed-only requests, never $0.00
function costTitle(state: CostCoverageState, value: number, priced: number, unpriced: number): string {
  switch (state) {
    case "empty":
      return "no requests — $0.00";
    case "attributed":
      return formatNanoUSD(value);
    case "partial":
      return `${formatNanoUSD(value)} · partial — ${priced} priced · ${unpriced} unpriced`;
    case "unpriced":
      return "unpriced — no attributed base-rate cost";
    case "not_estimated":
      return "not estimated — failed-only requests are not priced";
  }
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
