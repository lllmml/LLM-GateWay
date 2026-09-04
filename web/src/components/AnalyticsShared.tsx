import type { ReactNode } from "react";
import { formatCount, formatErrorRate, formatNanoUSD, formatTime } from "../lib/format";
import type { GatewayRequest } from "../api/analytics";
import { StatusBadge } from "./StatusBadge";

export function Tile({ label, value, detail }: { label: string; value: string; detail?: string }) {
  return (
    <div className="rounded-lg border border-slate-200 bg-white p-4 shadow-sm">
      <p className="text-xs font-medium uppercase tracking-wide text-slate-500">{label}</p>
      <p className="mt-2 text-xl font-semibold text-slate-950">{value}</p>
      {detail ? <p className="mt-1 text-xs text-slate-500">{detail}</p> : null}
    </div>
  );
}

export function TileGridLoading({ tiles = 4 }: { tiles?: number }) {
  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      {Array.from({ length: tiles }, (_, index) => (
        <div className="h-24 animate-pulse rounded-lg border border-slate-200 bg-slate-100" key={index} />
      ))}
    </div>
  );
}

export function SectionCard({ title, subtitle, actions, children }: { title: string; subtitle?: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="rounded-lg border border-slate-200 bg-white shadow-sm">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-slate-100 px-5 py-4">
        <div>
          <h2 className="text-base font-semibold text-slate-950">{title}</h2>
          {subtitle ? <p className="text-xs text-slate-500">{subtitle}</p> : null}
        </div>
        {actions}
      </div>
      {children}
    </section>
  );
}

type SummaryShape = {
  requests_total: number;
  requests_failed: number;
  error_rate: number | null;
  total_tokens: number;
  estimated_cost_nano_usd: number;
  priced_requests: number;
  unpriced_requests: number;
  requests_succeeded: number;
  avg_latency_ms: number | null;
  avg_ttft_ms: number | null;
  prompt_tokens: number;
  completion_tokens: number;
};

export function SummaryTiles({ summary }: { summary: SummaryShape }) {
  return (
    <section className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <Tile label="Requests" value={formatCount(summary.requests_total)} detail={`${formatCount(summary.requests_failed)} failed`} />
      <Tile label="Error rate" value={formatErrorRate(summary.error_rate)} detail="failed / total" />
      <Tile label="Tokens" value={formatCount(summary.total_tokens)} detail="total reported" />
      <Tile label="Est. cost (base rate)" value={formatNanoUSD(summary.estimated_cost_nano_usd)} detail={`cost coverage ${formatCount(summary.priced_requests)} priced · ${formatCount(summary.unpriced_requests)} unpriced`} />
      <Tile label="Avg latency" value={summary.avg_latency_ms === null ? "—" : `${summary.avg_latency_ms.toFixed(0)} ms`} detail="finished requests" />
      <Tile label="Avg TTFT" value={summary.avg_ttft_ms === null ? "—" : `${summary.avg_ttft_ms.toFixed(0)} ms`} detail="first chunk, streams" />
      <Tile label="Succeeded" value={formatCount(summary.requests_succeeded)} detail="final status" />
      <Tile label="Prompt / completion" value={`${formatCount(summary.prompt_tokens)} / ${formatCount(summary.completion_tokens)}`} detail="reported tokens" />
    </section>
  );
}

export function RequestTable({ rows }: { rows: GatewayRequest[] }) {
  if (rows.length === 0) {
    return <p className="px-5 py-8 text-center text-sm text-slate-500">No requests recorded yet.</p>;
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-sm">
        <thead className="border-b border-slate-100 text-xs uppercase tracking-wide text-slate-500">
          <tr>
            <th className="px-5 py-2 font-medium">Started (UTC)</th>
            <th className="px-3 py-2 font-medium">Provider / model</th>
            <th className="px-3 py-2 font-medium">Key</th>
            <th className="px-3 py-2 font-medium">Status</th>
            <th className="px-3 py-2 text-right font-medium">Latency</th>
            <th className="px-5 py-2 text-right font-medium">Est. cost</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-100">
          {rows.map((row) => (
            <tr className="text-slate-700" key={row.id}>
              <td className="whitespace-nowrap px-5 py-2.5 font-mono text-xs">{formatTime(row.started_at)}</td>
              <td className="px-3 py-2.5">
                <span className="text-slate-500">{row.provider}/</span>
                <span className="font-medium text-slate-800">{row.model}</span>
              </td>
              <td className="px-3 py-2.5 font-mono text-xs text-slate-500">{row.virtual_key_prefix}</td>
              <td className="px-3 py-2.5">
                <StatusBadge status={row.status} />
              </td>
              <td className="px-3 py-2.5 text-right">{row.latency_ms === null ? "—" : `${row.latency_ms} ms`}</td>
              <td className="px-5 py-2.5 text-right">{formatNanoUSD(row.estimated_cost_nano_usd)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
