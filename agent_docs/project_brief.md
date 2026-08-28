# Project Brief

## Product

- One-line vision: One production-oriented gateway and management surface for multi-provider LLM access, credentials, traffic attribution, cost, reliability, and observability.
- Target users: Developers, small engineering teams, and backend/AI infrastructure engineers integrating OpenAI, Anthropic, and DeepSeek.
- Primary user outcome: Replace scattered provider credentials and dashboards with one project-scoped gateway endpoint, then understand and operate every request through attributable usage and failure data.

## Scope

- Must ship:
  - Multi-provider gateway API for OpenAI, Anthropic, and DeepSeek.
  - Projects, encrypted provider credentials, and revocable virtual API keys.
  - Real streaming/SSE proxying with cancellation, backpressure, TTFT, and end-of-stream accounting.
  - Durable request, token, cost, latency, error, provider/model, and retry attribution.
  - Rate limiting, bounded retry semantics, Prometheus metrics, OpenTelemetry traces, Grafana, and failure tests.
  - A small desktop-first Web console for projects, keys, credentials, usage, requests, and operations.
  - Reproducible direct-vs-gateway benchmarks and `pprof` evidence.
- Not in v1:
  - Autonomous agents, RAG/chatbot features, AI routing, or AI-generated policy.
  - Kafka, Kubernetes, service mesh, microservices, or custom distributed primitives.
  - Automatic cross-provider fallback, large provider catalogs, or unvalidated circuit breakers.
  - Enterprise RBAC, billing/invoicing, multi-region active-active, or a heavy analytics frontend.
  - Raw prompt/response-body retention by default.

## Principles

- Backend and gateway engineering depth outrank frontend breadth.
- Start with the simplest working mechanism, expose a real limitation, then add complexity backed by failure or benchmark evidence.
- Preserve meaningful provider differences; an unsupported field is rejected explicitly rather than silently ignored.
- Make cost, retry, failure, data retention, and degraded behavior visible and deliberate.
- Every major resume claim must map to a repeatable test, benchmark, profile, ADR, or operational artifact.
- The project owner must be able to explain each core mechanism and trade-off before the milestone is complete.

## AI Position

- AI is used for: Transporting authenticated client requests to explicitly selected LLM providers, plus development assistance for code, tests, documentation, and reviews.
- AI is not used for: Autonomous planning, AI routing, policy generation, hidden provider fallback, or production decisions.
- Human approval required for: Architecture, schema, security/cryptography, dependencies, cost-bearing provider tests, retention, deployment, production writes, and destructive actions.
