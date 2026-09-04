import { describe, expect, it } from "vitest";
import {
  listRequests,
  parseGatewayRequest,
  parseRequestPage,
  parseUsageBreakdown,
  parseUsageSummary,
  parseUsageTimeseries,
} from "./analytics";

const requestFixture = {
  id: "req-1",
  project_id: "proj-1",
  project_name: "Analytics",
  virtual_key_id: "key-1",
  virtual_key_prefix: "pgw_ab12",
  provider: "openai",
  model: "gpt-5.6-terra",
  is_stream: false,
  status: "succeeded",
  started_at: "2026-09-01T00:00:00Z",
  first_chunk_at: null,
  completed_at: "2026-09-01T00:00:01Z",
  latency_ms: 100,
  ttft_ms: null,
  upstream_http_status: 200,
  error_category: null,
  retry_count: 0,
  prompt_tokens: 10,
  completion_tokens: 5,
  total_tokens: 15,
  usage_source: "provider",
  pricing_id: "price-1",
  estimated_cost_nano_usd: 20000000,
  upstream_request_id: "ur-1",
  trace_id: null,
  created_at: "2026-09-01T00:00:00Z",
};

describe("analytics client parsing", () => {
  it("parses a gateway request row", () => {
    const row = parseGatewayRequest(requestFixture);
    expect(row.id).toBe("req-1");
    expect(row.status).toBe("succeeded");
    expect(row.estimated_cost_nano_usd).toBe(20_000_000);
    expect(row.trace_id).toBeNull();
  });

  it("rejects malformed request rows", () => {
    expect(() => parseGatewayRequest({ ...requestFixture, id: 5 })).toThrow();
    expect(() => parseGatewayRequest({})).toThrow();
  });

  it("parses a page with next_cursor and has_more", () => {
    const page = parseRequestPage({ items: [requestFixture], next_cursor: "opaque-token", has_more: true });
    expect(page.items).toHaveLength(1);
    expect(page.has_more).toBe(true);
    expect(page.next_cursor).toBe("opaque-token");
  });

  it("parses usage summary numbers and nullable fields", () => {
    const summary = parseUsageSummary({
      from: "2026-08-01T00:00:00Z",
      to: "2026-09-01T00:00:00Z",
      requests_total: 10,
      requests_succeeded: 9,
      requests_failed: 1,
      priced_requests: 7,
      unpriced_requests: 2,
      error_rate: 0.1,
      prompt_tokens: 100,
      completion_tokens: 50,
      total_tokens: 150,
      estimated_cost_nano_usd: 300,
      avg_latency_ms: null,
      avg_ttft_ms: 12,
      generated_at: "2026-09-01T00:00:00Z",
    });
    expect(summary.error_rate).toBe(0.1);
    expect(summary.avg_latency_ms).toBeNull();
    expect(summary.unpriced_requests).toBe(2);
  });

  it("parses timeseries with bucket and zero values", () => {
    const series = parseUsageTimeseries({
      from: "2026-09-01T00:00:00Z",
      to: "2026-09-03T00:00:00Z",
      bucket: "day",
      items: [{ ts: "2026-09-01T00:00:00Z", requests_total: 0, requests_succeeded: 0, requests_failed: 0, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0, estimated_cost_nano_usd: 0 }],
    });
    expect(series.items[0].requests_total).toBe(0);
    expect(series.bucket).toBe("day");
  });

  it("parses breakdown groups with optional key metadata", () => {
    const breakdown = parseUsageBreakdown({
      dimension: "key",
      from: "2026-09-01T00:00:00Z",
      to: "2026-09-02T00:00:00Z",
      items: [{ key: "pgw_ab12", key_id: "key-1", key_prefix: "pgw_ab12", requests_total: 1, requests_failed: 0, prompt_tokens: 1, completion_tokens: 1, total_tokens: 2, estimated_cost_nano_usd: 4 }],
    });
    expect(breakdown.items[0].key_id).toBe("key-1");
  });
});

describe("analytics client requests", () => {
  it("encodes filters and tolerates the server 204-less JSON contract", async () => {
    const fetchMock = () => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ items: [], next_cursor: null, has_more: false }) }) as Promise<Response>;
    const originalFetch = globalThis.fetch;
    globalThis.fetch = fetchMock as typeof fetch;
    try {
      const page = await listRequests({ provider: "openai", status: "failed", stream: true, limit: 25, cursor: "tok" });
      expect(page.items).toEqual([]);
      expect(page.has_more).toBe(false);
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  it("rejects non-ok responses", async () => {
    const fetchMock = () => Promise.resolve({ ok: false, status: 500 }) as Promise<Response>;
    const originalFetch = globalThis.fetch;
    globalThis.fetch = fetchMock as typeof fetch;
    try {
      await expect(listRequests({})).rejects.toThrow();
    } finally {
      globalThis.fetch = originalFetch;
    }
  });
});
