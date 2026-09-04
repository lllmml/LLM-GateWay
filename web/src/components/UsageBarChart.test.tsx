import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { UsagePoint } from "../api/analytics";
import { UsageBarChart } from "./UsageBarChart";

const point = (overrides: Partial<UsagePoint>): UsagePoint => ({
  ts: "2026-11-02T00:00:00Z",
  requests_total: 0,
  requests_succeeded: 0,
  requests_failed: 0,
  priced_requests: 0,
  unpriced_requests: 0,
  prompt_tokens: 0,
  completion_tokens: 0,
  total_tokens: 0,
  estimated_cost_nano_usd: 0,
  ...overrides,
});

function firstTitle(container: HTMLElement): string | null {
  const title = container.querySelector("title");
  return title?.textContent ?? null;
}

afterEach(cleanup);

describe("UsageBarChart cost tooltips (coverage semantics)", () => {
  it("failed-only bucket: shows not estimated, never $0.00", () => {
    const { container } = render(
      <UsageBarChart
        metric="estimated_cost_nano_usd"
        points={[point({ requests_total: 5, requests_failed: 5 })]}
      />,
    );
    const title = firstTitle(container);
    expect(title).toContain("not estimated");
    expect(title).not.toContain("$0.00");
  });

  it("succeeded-unpriced bucket: shows unpriced, never $0.00", () => {
    const { container } = render(
      <UsageBarChart
        metric="estimated_cost_nano_usd"
        points={[point({ requests_total: 5, unpriced_requests: 5, prompt_tokens: 40 })]}
      />,
    );
    const title = firstTitle(container);
    expect(title).toContain("unpriced");
    expect(title).not.toContain("$0.00");
  });

  it("fully priced bucket: shows the attributed estimate", () => {
    const { container } = render(
      <UsageBarChart
        metric="estimated_cost_nano_usd"
        points={[point({ requests_total: 1, priced_requests: 1, estimated_cost_nano_usd: 80_000_000 })]}
      />,
    );
    expect(firstTitle(container)).toContain("$0.08");
  });

  it("empty bucket (no traffic): $0.00 is acceptable and stated as no requests", () => {
    const { container } = render(<UsageBarChart metric="estimated_cost_nano_usd" points={[point({})]} />);
    const title = firstTitle(container);
    expect(title).toContain("no requests");
    expect(title).toContain("$0.00");
  });
});
