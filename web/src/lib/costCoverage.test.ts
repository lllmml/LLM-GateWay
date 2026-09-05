import { describe, expect, it } from "vitest";
import { costCoverageDetail, costCoverageState } from "./costCoverage";

describe("costCoverageState (shared UI semantics)", () => {
  it("empty bucket: requests_total == 0", () => {
    expect(costCoverageState(0, 0, 0)).toBe("empty");
  });

  it("fully priced: priced > 0 and no unpriced succeeded requests", () => {
    expect(costCoverageState(5, 5, 0)).toBe("attributed");
    // failed requests do not affect the state when priced succeed exist
    expect(costCoverageState(9, 5, 0)).toBe("attributed");
  });

  it("mixed partial: priced and unpriced both present", () => {
    expect(costCoverageState(9, 5, 3)).toBe("partial");
  });

  it("succeeded unpriced only: priced == 0, unpriced > 0", () => {
    expect(costCoverageState(5, 0, 5)).toBe("unpriced");
  });

  it("failed-only: traffic exists but no priced and no unpriced (failed requests are neither)", () => {
    expect(costCoverageState(5, 0, 0)).toBe("not_estimated");
    expect(costCoverageState(5, 0, 0)).not.toBe("unpriced");
  });
});

describe("costCoverageDetail", () => {
  it("explains partial and never looks like a full estimate", () => {
    expect(costCoverageDetail("partial", 4, 2)).toContain("4 priced");
    expect(costCoverageDetail("partial", 4, 2)).toContain("2 unpriced");
  });

  it("explains unpriced without claiming a zero", () => {
    expect(costCoverageDetail("unpriced", 0, 5)).toContain("unpriced");
  });

  it("explains failed-only as not estimated", () => {
    expect(costCoverageDetail("not_estimated", 0, 0)).toContain("not estimated");
  });
});
