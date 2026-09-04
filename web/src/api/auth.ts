import { readCookie } from "../lib/cookies";

export type ConsoleUser = {
  id: string;
  github_id?: number;
  github_login: string;
  avatar_url?: string;
};

export type AuthState = {
  user: ConsoleUser | null;
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function optionalString(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() !== "" ? value : undefined;
}

export function parseMeResponse(value: unknown): AuthState {
  if (!isRecord(value) || !isRecord(value.user)) {
    throw new Error("invalid /api/v1/me response");
  }

  const id = value.user.id;
  const login = value.user.github_login;
  if (typeof id !== "string" || id.trim() === "" || typeof login !== "string" || login.trim() === "") {
    throw new Error("invalid /api/v1/me user");
  }

  const githubID = value.user.github_id;
  if (githubID !== undefined && typeof githubID !== "number") {
    throw new Error("invalid /api/v1/me github_id");
  }

  return {
    user: {
      id,
      github_id: githubID,
      github_login: login,
      avatar_url: optionalString(value.user.avatar_url),
    },
  };
}

export async function fetchAuthState(): Promise<AuthState> {
  const response = await fetch("/api/v1/me", {
    credentials: "include",
    headers: {
      Accept: "application/json",
    },
  });

  if (response.status === 401) {
    return { user: null };
  }
  if (!response.ok) {
    throw new Error(`session check failed with ${response.status}`);
  }

  return parseMeResponse(await response.json());
}

export async function logout(): Promise<void> {
  const csrfToken = readCookie("gateway_csrf");
  if (csrfToken === undefined) {
    throw new Error("missing CSRF token");
  }

  const response = await fetch("/auth/logout", {
    method: "POST",
    credentials: "include",
    headers: {
      "X-CSRF-Token": csrfToken,
    },
  });

  if (!response.ok) {
    throw new Error(`logout failed with ${response.status}`);
  }
}
