import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { fetchUsageBreakdown, fetchUsageSummary, fetchUsageTimeseries } from "../api/analytics";
import { formatCount, formatNanoUSD, formatTime } from "../lib/format";
import { CostNotice } from "../components/CostNotice";
import { SummaryTiles, TileGridLoading, SectionCard } from "../components/AnalyticsShared";
import { UsageBarChart } from "../components/UsageBarChart";

type RangePreset = "24h" | "7d" | "30d";

const rangeDays: Record<RangePreset, number> = { "24h": 1, "7d": 7, "30d": 30 };

function windowFor(preset: RangePreset): { from: string; to: string } {
  const to = new Date();
  const from = new Date(to.getTime() - rangeDays[preset] * 24 * 60 * 60 * 1000);
  return { from: from.toISOString(), to: to.toISOString() };
}

export function UsageCostPage() {
  const [preset, setPreset] = useState<RangePreset>("30d");
  const [bucket, setBucket] = useState<"day" | "hour">("day");
  const window = useMemo(() => windowFor(preset), [preset]);

  const summary = useQuery({ queryKey: ["usage-summary", preset], queryFn: () => fetchUsageSummary(window) });
  const timeseries = useQuery({ queryKey: ["usage-timeseries", preset, bucket], queryFn: () => fetchUsageTimeseries(bucket, window) });
  const byProvider = useQuery({ queryKey: ["usage-breakdown", preset, "provider"], queryFn: () => fetchUsageBreakdown("provider", window) });
  const byModel = useQuery({ queryKey: ["usage-breakdown", preset, "model"], queryFn: () => fetchUsageBreakdown("model", window) });
  const byKey = useQuery({ queryKey: ["usage-breakdown", preset, "key"], queryFn: () => fetchUsageBreakdown("key", window) });

  const failed = summary.isError || timeseries.isError || byProvider.isError || byModel.isError || byKey.isError;

  return (
    <main className="min-w-0 space-y-6">
      <header className="rounded-lg border border-slate-200 bg-white p-6 shadow-sm">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <p className="text-sm font-medium uppercase tracking-wide text-slate-500">Usage &amp; Cost</p>
            <h1 className="mt-1 text-2xl font-semibold text-slate-950">Where LLM traffic and spend come from</h1>
            <p className="mt-2 max-w-3xl text-sm leading-6 text-slate-600">
              Aggregates are server-side and time windows are half-open UTC ranges. Cost figures are base-rate estimates.
            </p>
          </div>
          <div className="flex items-center gap-1 rounded-md border border-slate-200 p-1">
            {(Object.keys(rangeDays) as RangePreset[]).map((option) => (
              <button
                className={`rounded px-3 py-1.5 text-sm font-medium ${preset === option ? "bg-slate-900 text-white" : "text-slate-600 hover:bg-slate-100"}`}
                key={option}
                onClick={() => setPreset(option)}
                type="button"
              >
                {option}
              </button>
            ))}
          </div>
        </div>
        {summary.data ? (
          <p className="mt-3 font-mono text-xs text-slate-500">
            {formatTime(summary.data.from)} → {formatTime(summary.data.to)} UTC
          </p>
        ) : null}
      </header>

      {failed ? <p className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">Usage data could not be loaded.</p> : null}
      {summary.data ? (
        <SummaryTiles summary={summary.data} />
      ) : (
        <TileGridLoading />
      )}
      <CostNotice />

      <SectionCard
        actions={
          <div className="flex items-center gap-1 rounded-md border border-slate-200 p-1">
            {(["day", "hour"] as const).map((option) => (
              <button
                className={`rounded px-3 py-1.5 text-sm font-medium ${bucket === option ? "bg-slate-900 text-white" : "text-slate-600 hover:bg-slate-100"}`}
                key={option}
                onClick={() => setBucket(option)}
                type="button"
              >
                {option === "day" ? "Daily" : "Hourly"}
              </button>
            ))}
          </div>
        }
        subtitle="Empty buckets are zero-filled by the server and aligned to UTC bucket starts."
        title="Trends"
      >
        <div className="space-y-6 p-5">
          {timeseries.data ? (
            <>
              <UsageBarChart metric="requests_total" points={timeseries.data.items} />
              <UsageBarChart metric="estimated_cost_nano_usd" points={timeseries.data.items} />
            </>
          ) : null}
        </div>
      </SectionCard>

      <div className="grid gap-6 xl:grid-cols-2">
        <BreakdownCard items={byProvider.data?.items ?? []} title="By provider" />
        <BreakdownCard items={byModel.data?.items ?? []} title="By model" />
        <div className="xl:col-span-2">
          <BreakdownCard items={byKey.data?.items ?? []} title="By virtual key" />
        </div>
      </div>
    </main>
  );
}

function BreakdownCard({ items, title }: { items: { key: string; key_prefix?: string; requests_total: number; requests_failed: number; priced_requests: number; unpriced_requests: number; prompt_tokens: number; completion_tokens: number; estimated_cost_nano_usd: number }[]; title: string }) {
  return (
    <SectionCard subtitle="Sorted by estimated cost descending; an entirely unpriced group shows 'unpriced' rather than $0.00." title={title}>
      {items.length === 0 ? (
        <p className="px-5 py-6 text-sm text-slate-500">No breakdown data in this window.</p>
      ) : (
        <table className="w-full text-left text-sm">
          <thead className="border-b border-slate-100 text-xs uppercase tracking-wide text-slate-500">
            <tr>
              <th className="px-5 py-2 font-medium">Group</th>
              <th className="px-3 py-2 text-right font-medium">Requests</th>
              <th className="px-3 py-2 text-right font-medium">Priced / unpriced</th>
              <th className="px-3 py-2 text-right font-medium">Tokens</th>
              <th className="px-5 py-2 text-right font-medium">Est. cost</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-100">
            {items.map((group) => {
              const entirelyUnpriced = group.priced_requests === 0;
              const partial = !entirelyUnpriced && group.unpriced_requests > 0;
              return (
                <tr className="text-slate-700" key={group.key}>
                  <td className="max-w-56 truncate px-5 py-2 font-medium text-slate-800" title={group.key}>
                    {title === "By virtual key" && group.key_prefix ? group.key_prefix : group.key}
                    {partial ? (
                      <span className="ml-2 rounded bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700" title="Some requests in this group have no attributed cost.">
                        partial cost
                      </span>
                    ) : null}
                  </td>
                  <td className="px-3 py-2 text-right">{formatCount(group.requests_total)}</td>
                  <td className="px-3 py-2 text-right font-mono text-xs">
                    {group.priced_requests} / {group.unpriced_requests}
                  </td>
                  <td className="px-3 py-2 text-right">{formatCount(group.prompt_tokens + group.completion_tokens)}</td>
                  <td className="px-5 py-2 text-right">
                    {entirelyUnpriced ? <span className="font-medium text-amber-700">unpriced</span> : formatNanoUSD(group.estimated_cost_nano_usd)}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </SectionCard>
  );
}
