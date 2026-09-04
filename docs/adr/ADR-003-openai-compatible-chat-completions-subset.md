# ADR-003: Explicit OpenAI-compatible Chat Completions subset

- Status: Accepted
- Date: 2026-09-01

## Context

The Data Plane needs a first end-to-end provider path without claiming complete
OpenAI compatibility or hiding behavior that later providers cannot preserve.
The Week 3 milestone also needs to prove virtual-key authentication, explicit
credential selection, upstream connection reuse, and durable request lifecycle
accounting before streaming and retry policy add more state transitions.

Passing arbitrary JSON through to OpenAI would be shorter, but it would silently
turn every accepted field into a public gateway contract. Selecting an arbitrary
active credential would also make routing change when credentials are added or
disabled. Calling the provider before PostgreSQL accepts a request record could
incur unattributed cost.

## Options considered

1. Transparently forward the complete OpenAI request and response bodies.
2. Expose only a documented OpenAI-compatible subset and reject everything else.
3. Delay the Data Plane until a three-provider common contract is complete.

For credential routing:

1. Select the newest active credential implicitly.
2. Require exactly one active credential per project/provider.
3. Persist an explicit project/provider-to-credential selection.

## Decision

Expose `POST /v1/chat/completions` as an explicitly limited, non-streaming subset:

- authenticate with `Authorization: Bearer <virtual-api-key>`;
- require `Content-Type: application/json`;
- require a namespaced `model` in the form `openai/<model-id>`;
- require a non-empty `messages` array;
- accept only `system`, `user`, and `assistant` roles with string `content`;
- accept `stream` only when omitted or `false`;
- reject unknown meaningful fields and `stream: true` with the stable
  `unsupported_parameter` code instead of silently discarding them;
- return only the supported non-streaming response fields: completion identity,
  model, choices with assistant messages, finish reason, and provider usage when
  present;
- return the stable gateway error envelope and
  `X-Gateway-Request-ID`, `X-Gateway-Provider`, and
  `X-Gateway-Retry-Count` headers when a durable request exists.

Persist `project_provider_configs` so credential selection is explicit and
ownership-scoped. The minimal Control Plane endpoint selects an active OpenAI
credential by ID and can enable or disable that selection. It does not expose a
base-URL override; test upstream URLs remain constructor-injected.

For each valid upstream-bound request:

1. authenticate the active virtual key and project;
2. resolve the enabled provider configuration and active encrypted credential;
3. create `gateway_requests(status = 'in_progress')`;
4. only after creation succeeds, decrypt the credential and call the provider;
5. decode and validate the bounded provider response;
6. finalize the same durable row before returning a successful response.

Finalization uses a separate bounded context so downstream cancellation does not
erase lifecycle evidence. If finalization fails, the row remains `in_progress`,
the client receives `usage_persistence_failed` when possible, and a high-severity
log contains only safe request/provider/error metadata.

The OpenAI adapter owns wire translation, auth headers, response and usage
decoding, upstream request-ID extraction, and error classification. The Data
Plane service owns request deadlines, lifecycle order, stable client errors, and
the no-retry Week 3 policy. One long-lived `http.Transport` is created at process
startup and reused across requests.

## Consequences

- The supported API is intentionally smaller than OpenAI's full API.
- Fields such as tools, structured output, multimodal content, temperature, and
  provider extensions fail explicitly until their cross-provider semantics are
  designed and tested.
- Existing clients may need to remove unsupported options for the Week 3 slice.
- A successful gateway response has a finalized, attributable request row.
- PostgreSQL unavailability before record creation prevents upstream cost.
- PostgreSQL failure after upstream work can leave an `in_progress` evidence row;
  a durable recovery mechanism remains a later evidence-driven enhancement.
- Explicit provider configuration adds one management operation but avoids
  nondeterministic credential selection.
- The adapter keeps provider-specific behavior out of the central executor,
  preparing for DeepSeek and Anthropic without prematurely implementing them.

## Explicit non-goals for Week 3

- streaming/SSE, TTFT, flushing, or slow-consumer handling;
- retries, backoff, rate limiting, Redis, circuit breaking, or fallback;
- real-provider smoke tests or cost-bearing calls;
- pricing data, cost calculation, or usage dashboards;
- arbitrary user-configurable provider base URLs;
- Anthropic or DeepSeek execution.

## Verification

- Handler tests cover authentication-before-body-processing, strict JSON,
  unsupported parameters, content type, stable errors, and gateway headers.
- Service tests cover create-before-upstream ordering, credential decryption,
  provider failure classification, cancellation propagation, finalize-before-
  success, finalization failure, and log redaction.
- OpenAI adapter tests cover request translation, response/usage validation,
  bounded bodies, error classification, cancellation, and connection reuse.
- PostgreSQL integration tests cover the additive migration, ownership-safe
  provider selection, active key/project/credential checks, request creation,
  and successful finalization.
- A deterministic mock provider supplies non-streaming success, provider error,
  delay, and malformed-response scenarios without a real provider credential.

## Revisit when

- Week 4 adds streaming states and post-header failure semantics.
- DeepSeek or Anthropic exposes a real incompatibility in the common contract.
- A requested field has documented semantics and provider contract tests.
- Benchmarks show that PostgreSQL auth/config lookup or finalization needs a
  cache or a durable asynchronous path.
