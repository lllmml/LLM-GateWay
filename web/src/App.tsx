import { useMemo } from "react";
import { Link, NavLink, Navigate, Route, Routes } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Activity,
  BarChart3,
  Boxes,
  CircuitBoard,
  Github,
  KeyRound,
  LogOut,
  ServerCog,
  ShieldCheck,
} from "lucide-react";
import { fetchAuthState, logout, type ConsoleUser } from "./api/auth";

const navItems = [
  { to: "/", label: "Overview", icon: Activity },
  { to: "/projects", label: "Projects", icon: Boxes },
  { to: "/virtual-keys", label: "Virtual API Keys", icon: KeyRound },
  { to: "/provider-credentials", label: "Provider Credentials", icon: ShieldCheck },
  { to: "/usage-cost", label: "Usage & Cost", icon: BarChart3 },
  { to: "/requests", label: "Requests", icon: ServerCog },
  { to: "/observability", label: "Observability", icon: CircuitBoard },
];

type PageDefinition = {
  title: string;
  eyebrow: string;
  body: string;
};

const pages: Record<string, PageDefinition> = {
  overview: {
    title: "Control Plane Overview",
    eyebrow: "Week 2 shell",
    body: "Project ownership, credentials, virtual keys, request history, usage, and telemetry will land here as backend endpoints become available.",
  },
  projects: {
    title: "Projects",
    eyebrow: "Ownership boundary",
    body: "Project rows are the authorization root. Browser-supplied IDs are treated as selectors, not proof of access.",
  },
  virtualKeys: {
    title: "Virtual API Keys",
    eyebrow: "Shown once",
    body: "Gateway keys will be created here, displayed once, and stored by digest only in the backend.",
  },
  providerCredentials: {
    title: "Provider Credentials",
    eyebrow: "Encrypted at rest",
    body: "Provider secrets will be submitted to the control plane and never re-rendered after storage.",
  },
  usageCost: {
    title: "Usage & Cost",
    eyebrow: "Attribution",
    body: "Usage and nano-USD cost views will stay tied to project, key, provider, model, and pricing version.",
  },
  requests: {
    title: "Requests",
    eyebrow: "Operational history",
    body: "Request inspection will show lifecycle, provider status, latency, retry, and error category without prompt or response bodies by default.",
  },
  observability: {
    title: "Observability",
    eyebrow: "Bounded labels",
    body: "Metrics, traces, logs, and pprof links will focus on stable dimensions and keep request-level IDs out of metric labels.",
  },
};

export function App() {
  const authQuery = useQuery({
    queryKey: ["auth-state"],
    queryFn: fetchAuthState,
    retry: false,
  });

  if (authQuery.isPending) {
    return <FullScreenState title="Checking session" detail="Loading console access." />;
  }

  if (authQuery.isError) {
    return (
      <FullScreenState
        title="Control plane unavailable"
        detail="The console could not confirm the current session."
        action={<button className="rounded-md bg-slate-900 px-4 py-2 text-sm font-medium text-white" onClick={() => authQuery.refetch()}>Retry</button>}
      />
    );
  }

  if (authQuery.data.user === null) {
    return <LoginScreen />;
  }

  return <AuthenticatedConsole user={authQuery.data.user} />;
}

function LoginScreen() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-50 px-4 py-10">
      <section className="w-full max-w-md rounded-lg border border-slate-200 bg-white p-6 shadow-sm">
        <div className="mb-6 flex h-10 w-10 items-center justify-center rounded-md bg-slate-900 text-white">
          <ServerCog aria-hidden="true" size={22} />
        </div>
        <p className="text-sm font-medium uppercase tracking-wide text-slate-500">Production Go LLM Gateway</p>
        <h1 className="mt-2 text-2xl font-semibold text-slate-950">Sign in to the control plane</h1>
        <p className="mt-3 text-sm leading-6 text-slate-600">
          GitHub OAuth creates a server-side session. The browser console only receives session cookies and a CSRF token.
        </p>
        <a
          className="mt-6 inline-flex w-full items-center justify-center gap-2 rounded-md bg-slate-900 px-4 py-2.5 text-sm font-medium text-white hover:bg-slate-800 focus:outline-none focus:ring-2 focus:ring-slate-500 focus:ring-offset-2"
          href="/auth/github/login"
        >
          <Github aria-hidden="true" size={18} />
          Sign in with GitHub
        </a>
      </section>
    </main>
  );
}

function AuthenticatedConsole({ user }: { user: ConsoleUser }) {
  const queryClient = useQueryClient();
  const logoutMutation = useMutation({
    mutationFn: logout,
    onSuccess: () => {
      queryClient.setQueryData(["auth-state"], { user: null });
    },
  });

  const initials = useMemo(() => user.github_login.slice(0, 2).toUpperCase(), [user.github_login]);

  return (
    <div className="min-h-screen bg-slate-50 text-slate-950">
      <header className="border-b border-slate-200 bg-white">
        <div className="mx-auto flex max-w-7xl flex-col gap-4 px-4 py-4 sm:px-6 lg:flex-row lg:items-center lg:justify-between lg:px-8">
          <Link className="flex items-center gap-3" to="/">
            <span className="flex h-9 w-9 items-center justify-center rounded-md bg-slate-900 text-white">
              <ServerCog aria-hidden="true" size={20} />
            </span>
            <span>
              <span className="block text-sm font-semibold">LLM Gateway</span>
              <span className="block text-xs text-slate-500">Control Plane</span>
            </span>
          </Link>
          <div className="flex items-center justify-between gap-3 lg:justify-end">
            <div className="flex min-w-0 items-center gap-3">
              {user.avatar_url ? (
                <img className="h-9 w-9 rounded-md border border-slate-200" src={user.avatar_url} alt="" />
              ) : (
                <span className="flex h-9 w-9 items-center justify-center rounded-md bg-slate-200 text-xs font-semibold">{initials}</span>
              )}
              <span className="truncate text-sm font-medium">{user.github_login}</span>
            </div>
            <button
              className="inline-flex items-center gap-2 rounded-md border border-slate-300 bg-white px-3 py-2 text-sm font-medium text-slate-700 hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-60"
              disabled={logoutMutation.isPending}
              onClick={() => logoutMutation.mutate()}
              type="button"
            >
              <LogOut aria-hidden="true" size={16} />
              Logout
            </button>
          </div>
        </div>
      </header>

      <div className="mx-auto grid max-w-7xl gap-6 px-4 py-6 sm:px-6 lg:grid-cols-[240px_1fr] lg:px-8">
        <nav className="flex gap-2 overflow-x-auto rounded-lg border border-slate-200 bg-white p-2 lg:block lg:overflow-visible">
          {navItems.map((item) => {
            const Icon = item.icon;
            return (
              <NavLink
                className={({ isActive }) =>
                  [
                    "flex min-w-fit items-center gap-2 rounded-md px-3 py-2 text-sm font-medium",
                    isActive ? "bg-slate-900 text-white" : "text-slate-600 hover:bg-slate-100 hover:text-slate-950",
                  ].join(" ")
                }
                end={item.to === "/"}
                key={item.to}
                to={item.to}
              >
                <Icon aria-hidden="true" size={16} />
                {item.label}
              </NavLink>
            );
          })}
        </nav>

        <Routes>
          <Route path="/" element={<ConsolePage page={pages.overview} />} />
          <Route path="/projects" element={<ConsolePage page={pages.projects} />} />
          <Route path="/virtual-keys" element={<ConsolePage page={pages.virtualKeys} />} />
          <Route path="/provider-credentials" element={<ConsolePage page={pages.providerCredentials} />} />
          <Route path="/usage-cost" element={<ConsolePage page={pages.usageCost} />} />
          <Route path="/requests" element={<ConsolePage page={pages.requests} />} />
          <Route path="/observability" element={<ConsolePage page={pages.observability} />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </div>
    </div>
  );
}

function ConsolePage({ page }: { page: PageDefinition }) {
  return (
    <main className="min-w-0">
      <section className="rounded-lg border border-slate-200 bg-white p-6 shadow-sm">
        <p className="text-sm font-medium uppercase tracking-wide text-slate-500">{page.eyebrow}</p>
        <h1 className="mt-2 text-2xl font-semibold text-slate-950">{page.title}</h1>
        <p className="mt-3 max-w-3xl text-sm leading-6 text-slate-600">{page.body}</p>
      </section>

      <section className="mt-6 grid gap-4 md:grid-cols-3">
        <StatusTile label="Session" value="Server-side" />
        <StatusTile label="Secrets" value="Redacted" />
        <StatusTile label="API proxy" value=":8081" />
      </section>
    </main>
  );
}

function StatusTile({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-slate-200 bg-white p-4 shadow-sm">
      <p className="text-xs font-medium uppercase tracking-wide text-slate-500">{label}</p>
      <p className="mt-2 text-lg font-semibold text-slate-950">{value}</p>
    </div>
  );
}

function FullScreenState({ title, detail, action }: { title: string; detail: string; action?: React.ReactNode }) {
  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-50 px-4">
      <section className="w-full max-w-sm rounded-lg border border-slate-200 bg-white p-6 text-center shadow-sm">
        <h1 className="text-lg font-semibold text-slate-950">{title}</h1>
        <p className="mt-2 text-sm text-slate-600">{detail}</p>
        {action ? <div className="mt-5">{action}</div> : null}
      </section>
    </main>
  );
}
