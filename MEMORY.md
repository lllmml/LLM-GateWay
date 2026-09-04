# Memory

Update this after major decisions, completed phases, or bugs that future agents need to know about. Keep it short.

## Current State

- Current task: Week 1 HTTP foundation is implemented and fully verified.
- Current phase: Prepare Week 2 Control Plane Core.
- Next step: Propose a bounded Week 2 plan covering the initial schema/migrations, GitHub auth/session boundary, project ownership, and their tests before implementation.
- Blocked by: None. Default port `:8080` was occupied during local verification; use address overrides or identify the owner before using defaults.

## Decisions

- 2026-08-28: Use a Go 1.26 `net/http` modular monolith with separate Data, Control, and private Operations listeners so the architectural boundary is real without premature microservices.
- 2026-08-28: Use PostgreSQL as the durable source of truth; create the request lifecycle record before starting paid upstream work.
- 2026-08-28: Use direct provider HTTP adapters and an explicit OpenAI-compatible subset; preserve or reject provider-specific behavior rather than silently erasing it.
- 2026-08-28: Start rate limiting in-process and add Redis only after demonstrating the distributed consistency problem.
- 2026-08-28: Defer automatic provider fallback and baseline circuit breaking until evidence justifies explicit semantics.
- 2026-08-28: Use Go 1.26 and module path `github.com/lllmml/production-go-llm-gateway` as explicitly approved by the project owner.
- 2026-08-28: Pin local PostgreSQL to `postgres:18.6-alpine3.24`, `pgx/v5` to v5.10.0, and the PostgreSQL-only `golang-migrate` CLI to v4.19.1.
- 2026-08-28: Require Go 1.26.7 and `golang.org/x/text` v0.39.0 to clear reachable standard-library and text-processing vulnerabilities; `x/sync` v0.21.0 is the required transitive update.

## AI / Tooling Decisions

- 2026-08-28: Use Codex with repository-owned `AGENTS.md`, `MEMORY.md`, `REVIEW-CHECKLIST.md`, and progressive-disclosure docs in `agent_docs/`.
- 2026-08-28: AI leads bounded implementation; the project owner approves protected changes and must pass the learning gate for core mechanisms.
- 2026-08-28: Real provider tests are opt-in, secret-gated, low volume, and cost-bearing only with explicit human approval.

## Known Issues

- Performance targets are intentionally TBD until a reproducible direct-vs-gateway mock-provider baseline exists.
- Hosting vendor, public domain, request-metadata retention window, and final managed-vs-self-hosted PostgreSQL choice remain implementation-time decisions.
- Default Data Plane port `:8080` was occupied during local verification; the configurable address overrides were used successfully.

## Completed

- [x] Initial scaffold
- [ ] Core data model
- [ ] Auth
- [ ] Core MVP flow
- [ ] Launch checks
