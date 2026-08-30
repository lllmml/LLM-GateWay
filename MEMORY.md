# Memory

Update this after major decisions, completed phases, or bugs that future agents need to know about. Keep it short.

## Current State

- Current task: Week 2 provider credential encrypted CRUD and the minimal React console shell are implemented; automated and local HTTP runtime checks pass.
- Current phase: Week 2 Control Plane Core implementation is complete; its manual visual acceptance check remains.
- Next step: Review the console at desktop and mobile widths, then propose the bounded Week 3 first-provider/non-streaming foundation plan.
- Blocked by: This agent environment has no browser runtime, so the required visual desktop/mobile check could not be executed. Default port `:8080` remains occupied; use address overrides or identify its owner before using defaults.

## Decisions

- 2026-08-28: Use a Go 1.26 `net/http` modular monolith with separate Data, Control, and private Operations listeners so the architectural boundary is real without premature microservices.
- 2026-08-28: Use PostgreSQL as the durable source of truth; create the request lifecycle record before starting paid upstream work.
- 2026-08-28: Use direct provider HTTP adapters and an explicit OpenAI-compatible subset; preserve or reject provider-specific behavior rather than silently erasing it.
- 2026-08-28: Start rate limiting in-process and add Redis only after demonstrating the distributed consistency problem.
- 2026-08-28: Defer automatic provider fallback and baseline circuit breaking until evidence justifies explicit semantics.
- 2026-08-28: Use Go 1.26 and module path `github.com/lllmml/production-go-llm-gateway` as explicitly approved by the project owner.
- 2026-08-28: Pin local PostgreSQL to `postgres:18.6-alpine3.24`, `pgx/v5` to v5.10.0, and the PostgreSQL-only `golang-migrate` CLI to v4.19.1.
- 2026-08-28: Require Go 1.26.7 and `golang.org/x/text` v0.39.0 to clear reachable standard-library and text-processing vulnerabilities; `x/sync` v0.21.0 is the required transitive update.
- 2026-08-28: Pin `sqlc` to v1.31.1 for explicit PostgreSQL query generation and `golang.org/x/oauth2` to v0.36.0 for GitHub Authorization Code exchange.
- 2026-08-28: GitHub login uses no requested scopes, validates one-time random state, and uses S256 PKCE; each login re-fetches `/user`, and the provider access token is not persisted.
- 2026-08-28: Web sessions use random tokens stored only as HMAC-SHA256 digests with a 32-byte pepper; production cookies require HTTPS while loopback HTTP remains available for local development.
- 2026-08-28: The MVP tenant boundary is enforced by ownership-scoped PostgreSQL queries using authenticated `owner_user_id`; cross-owner project lookup/update is indistinguishable from not found.
- 2026-08-28: Virtual API keys use independent random 8-character display prefixes and 256-bit secrets, are shown once, and persist only HMAC-SHA256 digests under a dedicated 32-byte `VIRTUAL_KEY_PEPPER`; disable/revoke mutations remain ownership-scoped and atomic.
- 2026-08-28: Shared virtual-key format, generation, parsing, and hashing primitives live in `internal/apikey`; Control Plane lifecycle code depends on that package, and future Data Plane authentication must do the same rather than importing Control Plane code.
- 2026-08-30: Provider credentials use AES-256-GCM with a fresh nonce per encryption and persisted key version `1`; `CREDENTIAL_MASTER_KEY` must decode from standard Base64 to exactly 32 bytes, and metadata APIs never return the secret envelope.
- 2026-08-30: Provider credential create/list/rotate/disable operations are ownership-scoped; rotation atomically replaces ciphertext/nonce/key-version metadata, while credential deletion, re-enable, live provider validation, and master-key re-encryption remain outside the Week 2 slice.
- 2026-08-30: The minimal React/Vite console uses same-origin `/api` and `/auth` development proxies, server-side session discovery through `/api/v1/me`, CSRF-protected logout, and placeholder operational routes; full management screens remain later milestones.

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
- [x] Control Plane users/sessions/projects foundation
- [x] Virtual API key creation/list/disable/revoke lifecycle
- [x] Provider credential encrypted create/list/rotate/disable lifecycle
- [x] Minimal React/Vite control-plane shell
- [ ] Core data model
- [ ] Auth
- [ ] Core MVP flow
- [ ] Launch checks
