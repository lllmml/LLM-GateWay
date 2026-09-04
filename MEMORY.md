# Memory

Update this after major decisions, completed phases, or bugs that future agents need to know about. Keep it short.

## Current State

- Current task: Week 4 Streaming Core is implemented and verified with bounded OpenAI SSE parsing, synchronous Data Plane streaming, TTFT persistence, deterministic mock streaming cases, and real HTTP streaming/cancellation/shutdown tests.
- Current phase: Week 4 Streaming Core is complete.
- Week 3 PR2 review fix: provider registry now supports dynamic provider namespaces, X-Request-ID/UUID trace propagation is preserved, and the stream flag reaches the service while SSE handling remains deferred to Week 4.
- Next step: Propose the bounded Week 5 Provider Abstraction + DeepSeek plan before changing provider interfaces or adding a second provider execution path.
- Blocked by: None. Real-provider smoke tests remain opt-in and require explicit cost-bearing approval.

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
- 2026-08-30: Provider credential AES-GCM envelopes bind a versioned binary AAD context containing credential ID, project ID, provider, and key version; credential IDs are created before encryption, and rotation reloads immutable ownership-scoped metadata before resealing.
- 2026-08-30: Credential, session, and virtual-key configuration keys must be pairwise distinct. Vite fails fast if canonical port `5173` is occupied, while a `/bin/sh` supervisor uses isolated Linux process groups to terminate and reap both dev process trees.
- 2026-09-01: The first Data Plane contract is an explicit OpenAI-compatible non-streaming subset: `model`, string-content `messages`, and `stream` only when false; meaningful unknown fields fail with `unsupported_parameter` rather than being silently discarded.
- 2026-09-01: Provider credential routing is explicit through ownership-scoped `project_provider_configs`; the public selection API accepts credential identity and enabled state but does not expose arbitrary base-URL overrides.
- 2026-09-01: Every upstream-bound request creates `gateway_requests(status = in_progress)` before credential decryption/provider work and finalizes the same row before a successful response. Finalization uses a separate bounded context so downstream cancellation does not erase lifecycle evidence.
- 2026-09-01: The OpenAI adapter owns wire translation, upstream authentication, bounded response decoding, usage extraction, request-ID extraction, and error classification. The Data Plane service owns deadlines, lifecycle ordering, stable client errors, and the Week 3 no-retry policy.
- 2026-09-01: One long-lived explicit `http.Transport` is reused by the OpenAI client; the deterministic mock provider supplies non-stream success, error, delay, and malformed-response tests without real provider calls.
- 2026-09-01: Provider lookup is registry-backed and no longer limited by a hardcoded `ParseModel` namespace whitelist; unknown provider namespaces can parse and then fail later through explicit registry lookup/configuration.
- 2026-09-03: Week 4 streaming uses a bounded SSE decoder and OpenAI adapter-owned stream validation. The Data Plane owns the synchronous `Next -> write -> flush` loop, records TTFT after the first non-DONE event flush, treats `[DONE]` as the only success marker, and finalizes interrupted streams as `failed/stream_interrupted` with nullable usage.
- 2026-09-04: Week 4 review fixes tightened streaming lifecycle semantics: handler ingress time is persisted as `gateway_requests.started_at`, downstream cancellation before stream establishment is finalized as `stream_interrupted`, OpenAI final usage is accepted only from `choices=[]` followed by `[DONE]`, and ordinary upstream request timeout is split from stream max duration (`UPSTREAM_REQUEST_TIMEOUT` vs `UPSTREAM_STREAM_MAX_DURATION`).

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
- [x] Core data model
- [x] Auth
- [x] OpenAI streaming core
- [ ] Core MVP flow
- [ ] Launch checks
