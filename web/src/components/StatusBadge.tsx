export function StatusBadge({ status }: { status: "in_progress" | "succeeded" | "failed" | string }) {
  const tone =
    status === "succeeded"
      ? "bg-emerald-50 text-emerald-700 ring-emerald-600/20"
      : status === "failed"
        ? "bg-red-50 text-red-700 ring-red-600/20"
        : "bg-amber-50 text-amber-700 ring-amber-600/20";
  return (
    <span className={`inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium ring-1 ring-inset ${tone}`}>
      {status}
    </span>
  );
}
