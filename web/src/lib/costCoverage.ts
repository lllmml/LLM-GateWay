// Shared cost-coverage semantics for console presentation.
//
// Backend definition (ADR-016, unchanged): priced_requests = succeeded
// requests with attributed cost; unpriced_requests = succeeded requests
// without attributed cost; failed requests are neither priced nor unpriced.
//
// Presentation must therefore distinguish "unknown/unpriced/not-estimated"
// from a known zero, and must not present failed-only traffic as unpriced.

export type CostCoverageState = "empty" | "attributed" | "partial" | "unpriced" | "not_estimated";

export function costCoverageState(requestsTotal: number, priced: number, unpriced: number): CostCoverageState {
  if (requestsTotal <= 0) {
    return "empty";
  }
  if (priced > 0 && unpriced === 0) {
    return "attributed";
  }
  if (priced > 0 && unpriced > 0) {
    return "partial";
  }
  if (priced === 0 && unpriced > 0) {
    return "unpriced";
  }
  // priced == 0 && unpriced == 0 && requests_total > 0: normally failed-only.
  return "not_estimated";
}

export const costCoverageLabel: Record<CostCoverageState, string> = {
  empty: "no requests",
  attributed: "estimated",
  partial: "partial estimate",
  unpriced: "unpriced",
  not_estimated: "not estimated",
};

export function costCoverageDetail(state: CostCoverageState, priced: number, unpriced: number): string {
  switch (state) {
    case "partial":
      return `partial — ${priced} priced · ${unpriced} unpriced`;
    case "unpriced":
      return "unpriced — no attributed cost (Week 7 has no DeepSeek price versions)";
    case "not_estimated":
      return "not estimated — failed-only requests are not priced";
    default:
      return costCoverageLabel[state];
  }
}
