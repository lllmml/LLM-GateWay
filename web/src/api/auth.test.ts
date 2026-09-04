import { describe, expect, it, vi, afterEach } from "vitest";
import { fetchAuthState, logout, parseMeResponse } from "./auth";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("parseMeResponse", () => {
  it("accepts the backend /api/v1/me envelope", () => {
    expect(
      parseMeResponse({
        user: {
          id: "user-1",
          github_id: 123,
          github_login: "octo",
          avatar_url: "https://example.test/avatar.png",
        },
      }),
    ).toEqual({
      user: {
        id: "user-1",
        github_id: 123,
        github_login: "octo",
        avatar_url: "https://example.test/avatar.png",
      },
    });
  });

  it("rejects malformed user data", () => {
    expect(() => parseMeResponse({ user: { id: "", github_login: "octo" } })).toThrow(/invalid/);
    expect(() => parseMeResponse({ user: { id: "user-1", github_login: 99 } })).toThrow(/invalid/);
  });
});

describe("fetchAuthState", () => {
  it("returns an unauthenticated state for 401", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("{}", { status: 401 })),
    );

    await expect(fetchAuthState()).resolves.toEqual({ user: null });
  });
});

describe("logout", () => {
  it("sends the readable CSRF cookie as a header", async () => {
    document.cookie = "gateway_csrf=csrf-token";
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await logout();

    expect(fetchMock).toHaveBeenCalledWith(
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
