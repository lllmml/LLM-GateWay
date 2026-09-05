import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RequestsPage } from "./RequestsPage";

const emptyRequest = {
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

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/requests"]}>
        <RequestsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RequestsPage filters", () => {
  it("keeps the form and the applied query in sync through Apply and Reset", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/api/v1/requests/")) {
        return jsonResponse(emptyRequest);
      }
      return jsonResponse({ items: [emptyRequest], next_cursor: null, has_more: false });
    });
    vi.stubGlobal("fetch", fetchMock);

    renderPage();

    // Initial unfiltered request fires once mounted.
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(expect.stringContaining("limit=50"), expect.anything()));
    const initialCalls = fetchMock.mock.calls.length;

    await userEvent.selectOptions(screen.getByLabelText("Provider filter"), "openai");
    await userEvent.selectOptions(screen.getByLabelText("Status filter"), "failed");
    await userEvent.selectOptions(screen.getByLabelText("Stream filter"), "true");
    await userEvent.click(screen.getByRole("button", { name: /apply/i }));

    // The query sent must carry exactly the applied filters.
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(initialCalls));
    const appliedCall = fetchMock.mock.calls[fetchMock.mock.calls.length - 1][0] as string;
    expect(appliedCall).toContain("provider=openai");
    expect(appliedCall).toContain("status=failed");
    expect(appliedCall).toContain("stream=true");

    await userEvent.click(screen.getByRole("button", { name: /reset filters/i }));

    // Selects visibly return to All and the next request is unfiltered.
    await waitFor(() => expect(screen.getByLabelText("Provider filter")).toHaveValue(""));
    expect(screen.getByLabelText("Status filter")).toHaveValue("");
    expect(screen.getByLabelText("Stream filter")).toHaveValue("");

    await waitFor(() => {
      const lastCall = fetchMock.mock.calls[fetchMock.mock.calls.length - 1][0] as string;
      expect(lastCall).toContain("limit=50");
      expect(lastCall).not.toContain("provider=");
      expect(lastCall).not.toContain("status=");
      expect(lastCall).not.toContain("stream=");
    });
  });

  it("shows the no-requests message for an empty page", async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ items: [], next_cursor: null, has_more: false }));
    vi.stubGlobal("fetch", fetchMock);
    renderPage();
    expect(await screen.findByText(/no requests match the current filters/i)).toBeInTheDocument();
  });
});
