import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { listRequests, fetchUsageSummary } from "../api/analytics";
import { CostNotice } from "../components/CostNotice";
import { RequestTable, SectionCard, SummaryTiles, TileGridLoading } from "../components/AnalyticsShared";

export function OverviewPage() {
  const summary = useQuery({ queryKey: ["usage-summary"], queryFn: () => fetchUsageSummary() });
  const recent = useQuery({ queryKey: ["requests", "recent"], queryFn: () => listRequests({ limit: 8 }) });

  return (
    <main className="min-w-0 space-y-6">
      <header className="rounded-lg border border-slate-200 bg-white p-6 shadow-sm">
        <p className="text-sm font-medium uppercase tracking-wide text-slate-500">Overview</p>
        <h1 className="mt-1 text-2xl font-semibold text-slate-950">Gateway traffic at a glance</h1>
        <p className="mt-2 max-w-3xl text-sm leading-6 text-slate-600">
          Request volume, token usage, estimated cost, and failure rate across all of your projects over the trailing 30 days.
        </p>
      </header>

      {summary.isError || recent.isError ? (
        <p className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">Usage data could not be loaded.</p>
      ) : null}

      {summary.data ? (
        <SummaryTiles summary={summary.data} />
      ) : (
        <TileGridLoading />
      )}

      <CostNotice />

      <SectionCard
        actions={
          <Link className="rounded-md border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100" to="/requests">
            View all
          </Link>
        }
        subtitle="Metadata only — prompt and response bodies are never stored."
        title="Recent requests"
      >
        {recent.data ? <RequestTable rows={recent.data.items} /> : null}
      </SectionCard>
    </main>
  );
}
