# Memory

Update this after major decisions, completed phases, or bugs that future agents need to know about. Keep it short.

## Current State

- Current task: Week 6 Anthropic Adapter is implemented and verified: a self-contained Messages API adapter (`internal/provider/anthropic`) that imports neither `oaiwire` nor its wire types (only `provider` + `provider/sse`), named SSE event parsing with synthetic OpenAI-compatible chunk re-encoding and a synthetic `[DONE]` at `message_stop`, stop/error mapping, dual-event usage extraction (input at `message_start`, cumulative output at `message_delta`), anthropic wired into the registry on the shared transport, provider-config anthropic parity in the Control Plane, an optional `max_tokens` public-subset amendment forwarded to all three providers, and cross-provider Data Plane e2e tests against Anthropic-shaped httptest mocks.
- Current phase: Week 6 Anthropic Adapter is complete; ready to plan Week 7 Usage/Cost/Dashboard.
- Next step: Propose the bounded Week 7 plan (pricing version table, estimated-cost, request history, usage aggregation, Overview/Usage/Requests UI) before touching the pricing/data model again.
- Blocked by: None. Real-provider smoke tests remain opt-in and require explicit cost-bearing approval; the Anthropic error-envelope details and request-ID behavior should be confirmed in one approved live smoke test before launch.

## Decisions

- 2026-09-04: Week 6 adds an optional integer `max_tokens` to the public OpenAI-compatible subset (approved Option 1): `internal/dataplane` decode accepts a positive value and keeps it off the wire when absent; OpenAI/DeepSeek forward it unchanged when present; Anthropic maps it onto its required `max_tokens` and applies a documented gateway default of `4096` (`defaultMaxTokens`) when the client omits it. `max_tokens <= 0` is rejected before any upstream work.
- 2026-09-04: The Anthropic adapter (`internal/provider/anthropic`) is deliberately independent of `oaiwire`: it posts to `/v1/messages` on `https://api.anthropic.com`, authenticates with `x-api-key` plus the required `anthropic-version: 2023-06-01` (no `Authorization`), hoists `system` messages into the top-level `system` field (`"\n\n"`-joined), and captures the documented `request-id` response header. Contract facts were verified against the official Messages/Streaming/Errors docs and the anthropic-sdk-typescript source fetched 2026-09 (matrix: `docs/provider-capabilities.md`).
- 2026-09-04: Anthropic streaming requires a bidirectional translation, unlike the OpenAI/DeepSeek byte passthrough: the adapter re-encodes `content_block_delta` text into OpenAI-compatible chunk envelopes, maps `stop_reason` onto OpenAI finish reasons (end_turn→stop, max_tokens/model_context_window_exceeded→length, stop_sequence→stop, tool_use→tool_calls, unknown→""), synthesizes `object`/`created` (Anthropic does not report them), and emits a synthetic `data: [DONE]` only at `message_stop`. Usage is committed only at `[DONE]`: `input_tokens` from `message_start`, cumulative `output_tokens` from `message_delta`; `total_tokens` is computed (Anthropic reports no total).
- 2026-09-04: Anthropic differences kept explicit in the adapter: unknown named SSE events and non-text deltas (thinking/signature/input_json) are skipped gracefully per Anthropic's versioning policy, while a bare data frame without an event name fails loudly; `event: error` mid-stream classifies by `error.type` (overloaded/api/auth → provider_unavailable, rate_limit → provider_rate_limited). HTTP status mapping is adapter-owned: 400/404/409/413 → provider_invalid_request; 429 → provider_rate_limited; 504/408 → provider_timeout; 401/402/403 and 500/529 → provider_unavailable (server-side conditions never surface as client 400).
- 2026-09-04: Anthropic `content` is an array of blocks; only `type:"text"` blocks are forwarded (joined without a separator so non-stream equals a stream of text deltas). Non-text blocks never appear because the gateway subset does not request tools/thinking; thinking is intentionally not forwarded, consistent with the DeepSeek `reasoning_content` handling.
- 2026-09-04: The Control Plane provider-config boundary now accepts `anthropic` (mirror routes under `/provider-configs/{provider}`); the earlier anthropic-rejection test became a reject case for adapter-less providers (e.g. `gemini`). Credential creation already accepted anthropic since Week 2.

- 2026-09-04: OpenAI-compatible wire mechanics (transport tuning, endpoint join/validation, bounded decode, bounded error-body reading, generic envelope message extraction, usage and response validation, shared JSON wire types) live in `internal/provider/oaiwire`; each adapter keeps its base-URL/path constants, request-ID header policy, HTTP status classification, and stream terminal semantics so provider differences stay explicit. The OpenAI adapter tests pass unchanged as the regression gate for the refactor.
- 2026-09-04: DeepSeek adapter posts to `https://api.deepseek.com/chat/completions` (no `/v1` by default; an override may include it), sends no `stream_options` (official docs: last chunk carries usage either way), and its stream parser accepts usage only on the final content chunk (one choice, empty content, non-null `finish_reason`) before `data: [DONE]`; usage is committed only at `[DONE]`. Contract verified from official docs fetched 2026-09; the capability matrix lives at `docs/provider-capabilities.md`.
- 2026-09-04: DeepSeek V4 thinking mode defaults on, so provider payloads may carry `reasoning_content`. Non-stream responses are normalized into the OpenAI-compatible `ChatResponse` envelope, which has no reasoning field, so reasoning content is intentionally not forwarded (documented limitation); streaming passes raw chunks through unchanged. Cache-hit/miss and reasoning token breakdowns are not separately persisted (prompt/completion/total only) - revisit when Week 7 pricing decides cache/reasoning pricing.
- 2026-09-04: Control Plane provider-config upsert supports `openai`, `deepseek`, and since Week 6 `anthropic` (mirror routes under `/provider-configs/{provider}`); an earlier decision rejected `anthropic` at the config boundary until its adapter existed (superseded 2026-09-04). Credential creation has accepted all three names since Week 2.
- 2026-09-04: HTTP status -> gateway error category mapping is provider-owned and lives in each adapter (`openaiErrorCategory`/`deepSeekErrorCategory`), never in `internal/provider/oaiwire`. 401 (invalid upstream credential) and 402 (billing) mean the configured provider access is unusable, a server-side condition, and map to `provider_unavailable` (not `provider_invalid_request`) so they never surface to clients as HTTP 400. 400/422 map to `provider_invalid_request`, 429 to `provider_rate_limited`, 408/504 to `provider_timeout`, other 5xx to `provider_unavailable`.

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

- Anthropic official docs (2026-09) document the error envelope shape and `request-id` header; the exact error-body fields beyond `error.type`/`error.message` and header casing should still be confirmed in an approved live smoke test before launch.
- Performance targets are intentionally TBD until a reproducible direct-vs-gateway mock-provider baseline exists.
- Hosting vendor, public domain, request-metadata retention window, and final managed-vs-self-hosted PostgreSQL choice remain implementation-time decisions.
- Default Data Plane port `:8080` was occupied during local verification; the configurable address overrides were used successfully.
- DeepSeek official docs (2026-09) document error status codes but not the JSON error envelope or an upstream request-ID header; the adapter uses best-effort generic envelope parsing and `X-Request-ID`. Confirm both in an approved live smoke test before launch.

## Completed

- [x] Initial scaffold
- [x] Control Plane users/sessions/projects foundation
- [x] Virtual API key creation/list/disable/revoke lifecycle
- [x] Provider credential encrypted create/list/rotate/disable lifecycle
- [x] Minimal React/Vite control-plane shell
- [x] Core data model
- [x] Auth
- [x] OpenAI streaming core
- [x] Provider abstraction + DeepSeek adapter
- [x] Anthropic adapter
- [ ] Core MVP flow
- [ ] Launch checks
