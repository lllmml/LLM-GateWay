# Product Requirements

This is the short build-facing summary. The full source of truth remains `docs/PRD-Production-Go-LLM-Gateway-MVP.md`.

## Users

- Primary user: Developers, small engineering teams, and backend/AI infrastructure engineers building products against multiple LLM providers.
- Main problem: Provider keys, models, request histories, usage, cost, latency, and errors are fragmented across provider-specific code and dashboards.
- Primary story: A developer creates a project, stores encrypted provider credentials, generates a virtual key, sends requests through one gateway, and inspects attributable usage and operational behavior in one console.

## Must-Have Features

- Multi-Provider Gateway API — OpenAI, Anthropic, and DeepSeek work end-to-end through an explicit compatible subset while meaningful provider differences remain visible.
- Project, Virtual API Key, and Provider Credential Management — create/list/disable/rotate/revoke keys, hash gateway keys, encrypt provider credentials, enforce project ownership, and redact secrets.
- Streaming / SSE Proxy — no full-response buffering; propagate cancellation, flush chunks, apply backpressure, measure TTFT, and account at stream completion.
- Usage, Cost, and Request Attribution — durable records link project/key/credential/provider/model and contain lifecycle, token, price-version, latency, error, streaming, and retry data where available.
- Reliability and Observability Baseline — per-key/project rate limiting, bounded classified retries, Prometheus metrics, OTel traces, Grafana, and the documented dependency/provider failure matrix.

## Nice-To-Have / Conditional

- The PRD commits no separate nice-to-have feature list.
- Redis-backed distributed rate limiting is added only after the two-instance inconsistency is demonstrated.
- Circuit breaking is considered only after controlled provider-failure evidence shows a clear benefit.

## Out Of Scope

- Autonomous agents/planner-executor workflows, AI routing, and AI-generated policy.
- Kafka usage pipeline, Kubernetes, service mesh, microservices, custom distributed consensus/queues, and multi-region active-active.
- Complex enterprise RBAC/IAM, billing/invoicing, heavy analytics frontend, and broad shallow provider support.
- Automatic cross-provider model fallback and custom circuit-breaker implementations without validated semantics.
- Raw prompt/response retention by default.

## UI/UX

- Clean, fast, professional, infrastructure-oriented, and low-distraction; desktop-first but usable on mobile.
- Key screens: Overview, Projects, Virtual API Keys, Provider Credentials, Usage & Cost, Requests, and Observability.
- Prefer tables, filters, status indicators, time-series summaries, server pagination, and progressive detail over decorative UI.
- Full gateway/provider secrets are never displayed after creation/submission; request views omit prompt bodies by default.

## Success Signals

- Three providers pass automated integration and controlled manual smoke tests.
- Every successful E2E request produces a queryable record linked to project, virtual key, provider, and model.
- Streaming passes chunk delivery, cancellation, slow-consumer, upstream failure, and completion-accounting scenarios.
- A reproducible report contains measured P50/P95/P99 gateway-added latency, TTFT overhead, throughput/concurrency, CPU, memory, allocations, and error rate; numeric targets are not invented before baseline.
- Each critical dependency has a reproducible failure/degraded-mode test; PostgreSQL, provider adapters, virtual-key auth, and streaming have integration coverage.
- Prometheus metrics, OTel traces, Grafana, and representative `pprof` profiles are available in the demo.
- No provider secret, full virtual key, authorization header, or raw prompt/response body is intentionally logged.
- The complete project → credential → virtual key → gateway request → dashboard journey works without manual database edits.

## Constraints

- Timeline: 12 weeks / about 3 months; quality and learning evidence outrank an artificial deadline.
- Budget: Flexible, but prefer simple/local/self-hosted or low-cost managed infrastructure unless spending creates clear engineering value.
- Go gateway/backend quality outranks frontend scope; PostgreSQL is the expected durable source of truth.
- Redis, Kafka, Kubernetes, and other infrastructure are not added for resume keywords.
- Important architectural choices require ADRs and must be explainable without relying on generated code.
