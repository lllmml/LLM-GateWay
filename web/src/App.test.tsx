import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";

afterEach(() => {
	cleanup();
  vi.unstubAllGlobals();
});

function renderApp(initialPath = "/") {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  });

  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialPath]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("App", () => {
  it("shows the GitHub login entry point when there is no session", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 401 })));

    renderApp();

    const login = await screen.findByRole("link", { name: /sign in with github/i });
    expect(login).toHaveAttribute("href", "/auth/github/login");
  });

  it("renders authenticated navigation and placeholder routes", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        Response.json({
          user: {
            id: "user-1",
            github_id: 123,
            github_login: "octo",
          },
        }),
      ),
    );

    renderApp("/provider-credentials");

    expect(await screen.findByText("octo")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /provider credentials/i })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Provider Credentials" })).toBeInTheDocument();
    expect(screen.getByText("Encrypted at rest")).toBeInTheDocument();
  });

  it("posts logout with CSRF and returns to the login state", async () => {
    document.cookie = "gateway_csrf=csrf-token";
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        Response.json({
          user: {
            id: "user-1",
            github_login: "octo",
          },
        }),
      )
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    renderApp();

    await userEvent.click(await screen.findByRole("button", { name: /logout/i }));

    await waitFor(() => expect(screen.getByRole("link", { name: /sign in with github/i })).toBeInTheDocument());
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/auth/logout",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        headers: {
          "X-CSRF-Token": "csrf-token",
        },
      }),
    );
  });
});
