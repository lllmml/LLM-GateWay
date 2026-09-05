import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { fetchRequest, listRequests, type GatewayRequest, type RequestPage } from "../api/analytics";
import { formatCount, formatNanoUSD, formatTime } from "../lib/format";
import { SectionCard } from "../components/AnalyticsShared";
import { StatusBadge } from "../components/StatusBadge";

type Filters = {
  provider: string;
  status: string;
  stream: string;
};

const emptyFilters: Filters = { provider: "", status: "", stream: "" };

export function RequestsPage() {
  // draftFilters is what the form currently shows; appliedFilters is what the
  // request list query uses. Apply copies draft -> applied; Reset returns both
  // to empty so the visible selects and the sent query can never disagree.
  const [draftFilters, setDraftFilters] = useState<Filters>(emptyFilters);
  const [appliedFilters, setAppliedFilters] = useState<Filters>(emptyFilters);
  const [cursor, setCursor] = useState<string | null>(null);
  const [history, setHistory] = useState<string[]>([]);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [detailOpen, setDetailOpen] = useState(false);

  const page = useQuery({
    queryKey: ["requests", appliedFilters, cursor ?? "start"],
    queryFn: () =>
      listRequests({
        provider: appliedFilters.provider || undefined,
        status: appliedFilters.status || undefined,
        stream: appliedFilters.stream === "" ? undefined : appliedFilters.stream === "true",
        limit: 50,
        cursor,
      }),
  });

  const detail = useQuery({
    queryKey: ["request", selectedID],
    queryFn: () => (selectedID ? fetchRequest(selectedID) : Promise.resolve(null)),
    enabled: detailOpen && selectedID !== null,
  });

  const data: RequestPage | undefined = page.data;

  const resetPagination = () => {
    setCursor(null);
    setHistory([]);
    setSelectedID(null);
    setDetailOpen(false);
  };

  const applyDraft = () => {
    setAppliedFilters(draftFilters);
    resetPagination();
  };

  const resetFilters = () => {
    setDraftFilters(emptyFilters);
    setAppliedFilters(emptyFilters);
    resetPagination();
  };

  const goNext = () => {
    if (!data?.next_cursor) {
      return;
    }
    setHistory((previous) => [...previous, cursor ?? "start"].filter((value): value is string => value !== null));
    setCursor(data.next_cursor);
  };
  const goBack = () => {
    const previous = history[history.length - 1];
    if (previous === undefined) {
      return;
    }
    setHistory((items) => items.slice(0, -1));
    setCursor(previous === "start" ? null : previous);
  };

  const openDetail = (id: string) => {
    setSelectedID(id);
    setDetailOpen(true);
  };

  return (
    <main className="min-w-0 space-y-6">
      <header className="rounded-lg border border-slate-200 bg-white p-6 shadow-sm">
        <p className="text-sm font-medium uppercase tracking-wide text-slate-500">Requests</p>
        <h1 className="mt-1 text-2xl font-semibold text-slate-950">Request history</h1>
        <p className="mt-2 max-w-3xl text-sm leading-6 text-slate-600">
          Durable metadata for every gateway request: lifecycle, provider/model, latency, TTFT, retry, error category,
          usage, and estimated cost. Prompt and response bodies are never stored.
        </p>
      </header>

      <SectionCard
        actions={
          <button
            className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-100"
            onClick={resetFilters}
            type="button"
          >
            Reset filters
          </button>
        }
        title="Filters"
      >
        <form
          className="flex flex-wrap items-end gap-3 px-5 py-4"
          onSubmit={(event) => {
            event.preventDefault();
            applyDraft();
          }}
        >
          <label className="block text-sm">
            <span className="text-xs font-medium uppercase tracking-wide text-slate-500">Provider</span>
            <select
              aria-label="Provider filter"
              className="mt-1 block rounded-md border border-slate-300 bg-white px-3 py-2 text-sm"
              name="provider"
              onChange={(event) => setDraftFilters((current) => ({ ...current, provider: event.target.value }))}
              value={draftFilters.provider}
            >
              <option value="">All</option>
              <option value="openai">openai</option>
              <option value="anthropic">anthropic</option>
              <option value="deepseek">deepseek</option>
            </select>
          </label>
          <label className="block text-sm">
            <span className="text-xs font-medium uppercase tracking-wide text-slate-500">Status</span>
            <select
              aria-label="Status filter"
              className="mt-1 block rounded-md border border-slate-300 bg-white px-3 py-2 text-sm"
              name="status"
              onChange={(event) => setDraftFilters((current) => ({ ...current, status: event.target.value }))}
              value={draftFilters.status}
            >
              <option value="">All</option>
              <option value="succeeded">succeeded</option>
              <option value="failed">failed</option>
              <option value="in_progress">in_progress</option>
            </select>
          </label>
          <label className="block text-sm">
            <span className="text-xs font-medium uppercase tracking-wide text-slate-500">Stream</span>
            <select
              aria-label="Stream filter"
              className="mt-1 block rounded-md border border-slate-300 bg-white px-3 py-2 text-sm"
              name="stream"
              onChange={(event) => setDraftFilters((current) => ({ ...current, stream: event.target.value }))}
              value={draftFilters.stream}
            >
              <option value="">All</option>
              <option value="true">streaming</option>
              <option value="false">non-streaming</option>
            </select>
          </label>
          <button className="rounded-md bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800" type="submit">
            Apply
          </button>
        </form>
      </SectionCard>

      {page.isError ? <p className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">Requests could not be loaded.</p> : null}

      <SectionCard
        actions={
          <div className="flex items-center gap-2">
            <span className="text-xs text-slate-500">{data ? `${data.items.length} shown` : "…"}</span>
            <button
              className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-50"
              disabled={history.length === 0}
              onClick={goBack}
              type="button"
            >
              ← Newer
            </button>
            <button
              className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-50"
              disabled={!data?.has_more}
              onClick={goNext}
              type="button"
            >
              Older →
            </button>
          </div>
        }
        subtitle="Ordered by started_at DESC (keyset pagination; no snapshot isolation for concurrent inserts)."
        title="Requests"
      >
        {data && data.items.length === 0 ? (
          <p className="px-5 py-8 text-center text-sm text-slate-500">No requests match the current filters.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-slate-100 text-xs uppercase tracking-wide text-slate-500">
                <tr>
                  <th className="px-5 py-2 font-medium">Started (UTC)</th>
                  <th className="px-3 py-2 font-medium">Provider / model</th>
                  <th className="px-3 py-2 font-medium">Key</th>
                  <th className="px-3 py-2 font-medium">Status</th>
                  <th className="px-3 py-2 text-right font-medium">Latency / TTFT</th>
                  <th className="px-3 py-2 text-right font-medium">Tokens</th>
                  <th className="px-5 py-2 text-right font-medium">Est. cost</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {(data?.items ?? []).map((row) => (
                  <RequestRow key={row.id} onClick={() => openDetail(row.id)} row={row} selected={detailOpen && selectedID === row.id} />
                ))}
              </tbody>
            </table>
          </div>
        )}

        {detailOpen && detail.data ? <RequestDetail request={detail.data} /> : null}
        {detailOpen && !detail.data && !detail.isError ? (
          <p className="border-t border-slate-100 px-5 py-4 text-sm text-slate-500">Loading request detail…</p>
        ) : null}
        {detail.isError ? <p className="border-t border-slate-100 px-5 py-4 text-sm text-red-600">Request detail could not be loaded.</p> : null}
      </SectionCard>
    </main>
  );
}

function RequestRow({ row, onClick, selected }: { row: GatewayRequest; onClick: () => void; selected: boolean }) {
  return (
    <tr className={`cursor-pointer text-slate-700 hover:bg-slate-50 ${selected ? "bg-slate-50" : ""}`} onClick={onClick}>
      <td className="whitespace-nowrap px-5 py-2.5 font-mono text-xs">{formatTime(row.started_at)}</td>
      <td className="px-3 py-2.5">
        <span className="text-slate-500">{row.provider}/</span>
        <span className="font-medium text-slate-800">{row.model}</span>
        {row.is_stream ? <span className="ml-2 rounded bg-slate-100 px-1.5 py-0.5 text-[10px] font-medium text-slate-500">stream</span> : null}
      </td>
      <td className="px-3 py-2.5 font-mono text-xs text-slate-500">{row.virtual_key_prefix}</td>
      <td className="px-3 py-2.5">
        <StatusBadge status={row.status} />
        {row.error_category ? <p className="mt-0.5 font-mono text-[10px] text-slate-400">{row.error_category}</p> : null}
      </td>
      <td className="px-3 py-2.5 text-right font-mono text-xs">
        {row.latency_ms === null ? "—" : `${row.latency_ms}`}
        {row.ttft_ms !== null ? ` / ${row.ttft_ms}` : ""}
        {row.latency_ms !== null ? " ms" : ""}
      </td>
      <td className="px-3 py-2.5 text-right font-mono text-xs">{row.total_tokens === null ? "—" : formatCount(row.total_tokens)}</td>
      <td className="px-5 py-2.5 text-right">{formatNanoUSD(row.estimated_cost_nano_usd)}</td>
    </tr>
  );
}

function RequestDetail({ request }: { request: GatewayRequest }) {
  const fields: [string, string][] = [
    ["Request ID", request.id],
    ["Project", `${request.project_name} (${request.project_id})`],
    ["Virtual key", `${request.virtual_key_prefix} (${request.virtual_key_id})`],
    ["Provider / model", `${request.provider} / ${request.model}`],
    ["Stream", String(request.is_stream)],
    ["Status", request.status],
    ["Error category", request.error_category ?? "—"],
    ["Retry count", String(request.retry_count)],
    ["Upstream HTTP status", request.upstream_http_status === null ? "—" : String(request.upstream_http_status)],
    ["Upstream request ID", request.upstream_request_id ?? "—"],
    ["Trace ID", request.trace_id ?? "—"],
    ["Started at", formatTime(request.started_at)],
    ["First chunk at", formatTime(request.first_chunk_at)],
    ["Completed at", formatTime(request.completed_at)],
    ["Latency (ms)", request.latency_ms === null ? "—" : String(request.latency_ms)],
    ["TTFT (ms)", request.ttft_ms === null ? "—" : String(request.ttft_ms)],
    ["Tokens (prompt / completion / total)", request.total_tokens === null ? "—" : `${request.prompt_tokens} / ${request.completion_tokens} / ${request.total_tokens}`],
    ["Usage source", request.usage_source ?? "—"],
    ["Pricing version", request.pricing_id ?? "—"],
    ["Estimated cost (base rate)", formatNanoUSD(request.estimated_cost_nano_usd)],
  ];
  return (
    <div className="border-t border-slate-100 bg-slate-50/60 px-5 py-4">
      <h3 className="mb-2 text-sm font-semibold text-slate-900">Request detail</h3>
      <dl className="grid gap-x-8 gap-y-1.5 sm:grid-cols-2 xl:grid-cols-3">
        {fields.map(([label, value]) => (
          <div className="min-w-0" key={label}>
            <dt className="text-[11px] font-medium uppercase tracking-wide text-slate-500">{label}</dt>
            <dd className="truncate font-mono text-xs text-slate-700" title={value}>
              {value}
            </dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
