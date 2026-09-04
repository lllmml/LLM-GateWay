import { describe, expect, it } from "vitest";
import { formatCompact, formatCount, formatErrorRate, formatNanoUSD, formatTime } from "./format";

describe("nano-USD formatting", () => {
  it("converts nano-USD to a USD figure (finding: 80_000_000 nanoUSD -> $0.08, not 80M)", () => {
    expect(formatNanoUSD(80_000_000)).toBe("$0.08");
  });

  it("formats zero nano-USD as a known zero", () => {
    expect(formatNanoUSD(0)).toBe("$0.00");
  });

  it("formats larger base-rate estimates with cents", () => {
    expect(formatNanoUSD(26_000_000)).toBe("$0.03"); // 0.026 -> rounds to 0.03
    expect(formatNanoUSD(1_234_500_000)).toBe("$1.23");
  });

  it("renders null/undefined as an em dash (unknown, not zero)", () => {
    expect(formatNanoUSD(null)).toBe("—");
    expect(formatNanoUSD(undefined)).toBe("—");
  });
});

describe("count formatting", () => {
  it("uses plain thousands separators for counts", () => {
    expect(formatCount(1234)).toBe("1,234");
  });

  it("uses compact notation for large values", () => {
    expect(formatCompact(80_000_000)).toBe("80M");
  });
});

describe("other formatting helpers", () => {
  it("formats error rate as a percentage", () => {
    expect(formatErrorRate(0.1)).toBe("10.0%");
    expect(formatErrorRate(null)).toBe("—");
  });

  it("renders UTC timestamps", () => {
    expect(formatTime("2026-09-01T00:00:00Z")).toBe("2026-09-01 00:00:00Z");
    expect(formatTime(null)).toBe("—");
  });
});
