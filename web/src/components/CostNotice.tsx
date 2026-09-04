export function CostNotice() {
  return (
    <p className="rounded-md border border-slate-200 bg-slate-50 px-4 py-3 text-xs leading-5 text-slate-500">
      <span className="font-semibold text-slate-600">Estimated base-rate cost.</span> Calculated from the price version
      effective at each request start (nano-USD, integer math). Providers may bill additional dimensions (cached input,
      cache writes, long context, batch/fast/regional, time tiers) that are not modeled, so these figures are estimates,
      not invoices. Requests without an effective price version or usage contribute to counts but not to cost.
    </p>
  );
}
