// Small formatting helpers shared by analytics pages.

const usd = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

const compact = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 2 });

export function formatNanoUSD(nano: number | null | undefined): string {
  if (nano === null || nano === undefined) {
    return "—";
  }
  return usd.format(nano / 1e9);
}

export function formatCount(value: number | null | undefined): string {
  if (value === null || value === undefined) {
    return "—";
  }
  return new Intl.NumberFormat("en-US").format(value);
}

export function formatCompact(value: number | null | undefined): string {
  if (value === null || value === undefined) {
    return "—";
  }
  return compact.format(value);
}

export function formatTime(iso: string | null | undefined): string {
  if (!iso) {
    return "—";
  }
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) {
    return "—";
  }
  return date.toISOString().replace("T", " ").replace(/\.\d{3}Z$/, "Z");
}

export function formatErrorRate(rate: number | null | undefined): string {
  if (rate === null || rate === undefined) {
    return "—";
  }
  return `${(rate * 100).toFixed(1)}%`;
}
