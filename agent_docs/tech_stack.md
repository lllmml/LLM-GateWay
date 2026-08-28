# Tech Stack

Last verified: 2026-08

## Stack

| Area | Choice | Notes |
|------|--------|-------|
| Frontend | React + Vite + TypeScript | Static infrastructure-console SPA; React Router and TanStack Query for routing/server state |
| Backend | Go 1.26.7 + `net/http` | Go 1.26 remains the selected release line; 1.26.7 is the minimum security patch. One modular monolith with Data, Control, and private Operations listeners; direct provider HTTP adapters |
| Database | PostgreSQL 18.6 + `pgx/v5` v5.10.0 + `sqlc` | Durable source of truth; local image is `postgres:18.6-alpine3.24`; `sqlc` begins with the first domain schema |
| Migrations | `golang-migrate` v4.19.1 SQL migrations | PostgreSQL-only CLI installed to `.tools/`; version-controlled up/down migrations begin with real Week 2 schema changes |
| Auth | GitHub OAuth + server-side sessions; hashed virtual keys | Same-origin Web control plane; project ownership enforced by backend services/queries |
| Security | AES-256-GCM provider-secret encryption | Versioned master keys; virtual keys remain non-recoverable and are shown once |
| Rate limiting | In-process token bucket first; optional Redis + `go-redis` later | Redis is added only after a demonstrated multi-instance need; local emergency limiter on Redis failure |
| Styling | Tailwind CSS + shadcn/ui | Clean, desktop-first, low-distraction infrastructure console |
| Observability | `log/slog`, Prometheus, OpenTelemetry Collector, Tempo, Grafana, `pprof` | Bounded metric labels; request-level IDs remain in logs/traces |
| Deployment | Docker Compose on one Linux VM behind Caddy | Long-lived streaming supported; managed PostgreSQL remains an evidence/cost-based option |

## Runtime topology

- Data Plane `:8080`: virtual-key auth, routing, rate limiting, provider execution, streaming, retry policy, and usage finalization.
- Control Plane `:8081`: GitHub OAuth/session, projects, credentials, virtual keys, request history, usage/cost aggregates, and audit events.
- Operations Plane `:9090`: `/metrics`, liveness/readiness, protected `pprof`, and internal diagnostics; never publicly routed.
- PostgreSQL is authoritative. Redis is never the source of truth for control-plane or usage data.

## Commands

- Setup: Copy `.env.example` to ignored `.env`, set `CREDENTIAL_MASTER_KEY`, then run `make bootstrap`
- Dev: `make dev`
- Test: `make test`
- Typecheck: `make typecheck`
- Lint/format: `make lint`
- Build: `make build`
- Integration: `make integration`
- Race: `make race`
- Benchmark/profile: `make bench`
- Database lifecycle: `make postgres-up` / `make postgres-down`
- Browser/device check: Run `make dev`, complete the primary journey in a current Chrome/Edge/Firefox/Safari desktop browser, then confirm the layout remains usable at a mobile viewport.

## LLM provider runtime

- Provider/runtime: Direct HTTP adapters for OpenAI, Anthropic, and DeepSeek behind an explicitly documented OpenAI-compatible Chat Completions subset.
- Model addressing: `openai/<model-id>`, `anthropic/<model-id>`, and `deepseek/<model-id>`; pricing and model catalogs are versioned data.
- Model/provider can see: Request content an authenticated client intentionally sends through the gateway to the explicitly selected provider.
- Never send or expose: Provider/virtual keys, auth headers, cookies, encryption keys, private logs, production exports, or data belonging to another project.
- Retention: Raw prompt/response bodies are not retained by default. Verify each provider's retention/training control before launch.
- Approval gates: Real provider smoke tests, provider/model routing changes, retention changes, prompt logging, cost-bearing calls, and production actions.
- Fallback: Return a stable gateway error with attributable telemetry; automatic cross-provider fallback is deferred.

## Important Patterns

- Provider boundary: Adapter owns protocol translation; executor owns cross-provider policy.
- Streaming: Bounded SSE decoder and synchronous read → parse → write → flush; no full-response buffering or unbounded queues.
- Persistence: Create `gateway_requests` before upstream work and finalize after completion; unknown usage/cost stays nullable.
- Data fetching: REST from React through TanStack Query; pagination and aggregations remain server-side.
- State management: TanStack Query for server state; keep local UI state minimal and do not introduce Redux without evidence.
- Validation: Validate HTTP/provider boundaries explicitly; use strong Go types and TypeScript `unknown` plus runtime validation where external data enters.
- Error handling: Stable gateway error categories for clients; redacted upstream detail in logs/traces.
- Logging/monitoring: Structured `slog` JSON, bounded Prometheus labels, OTel spans across request/auth/rate-limit/provider/usage paths.
