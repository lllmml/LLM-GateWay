# ADR-017: Reliability baseline - rate limiting, concurrency bounds, and bounded retry (Week 8)

- Status: Accepted
- Date: 2026-09-05
- Related: Tech Design §14 (Retry Semantics), §16 (Rate Limiting Evolution, Phase A), §37 (Failure Matrix), §15 (Circuit Breaker deferral); Week 8 roadmap entry; packages `internal/ratelimit`; migrations: none (the `retry_count` column already exists in `000004`).

## Context

Week 8 adds the single-instance reliability baseline: in-memory rate limiting,
in-process concurrency bounds, and bounded retried upstream execution with an
explicit failure matrix. Constraints agreed during review:

- retries must not multiply the existing per-request upstream timeout;
- token-bucket admission across two scopes must not leak quota under
  concurrency;
- `retry_count` is maintained in memory and persisted by exactly one finalize;
- streaming retry must stop as soon as a `ChatStream` is established, not only
  once downstream bytes are committed;
- rate-limit/concurrency rejections happen before any provider call and before
  any durable row;
- local (`rate_limited`) and provider (`provider_rate_limited`) 429 categories
  stay distinct;
- no schema, frontend, public-contract, Redis, circuit-breaker, or fallback
  work this week.

## Decisions

### D1. One overall upstream phase budget shared by attempts and backoff

A gateway request's upstream phase has a single deadline:
`phase start + UpstreamRequestTimeout` (non-stream) or `phase start +
UpstreamStreamMaxDuration` (stream), earlier of that and the downstream
context deadline. Every attempt context is `WithDeadline` against it, and
Retry-After/backoff waits are charged against the same deadline. Total duration
is therefore never `attempts × timeout + backoff`. No new timeout configuration
was needed: the existing variables already express the semantics. A retry only
starts when the wait fits the remaining budget and the caller has not
cancelled.

### D2. Exact retry whitelist (never category alone)

Retry only when the attempt failed with an explicit whitelisted condition:

- provider 429 (`ProviderRateLimited` **and** HTTP status 429);
- provider 429 (`ProviderRateLimited` **and** HTTP status 429);
- provider transient 5xx / overload statuses (`ProviderUnavailable`) only from
  the explicit list **500, 502, 503, 529** - not any 5xx: unlisted statuses
  such as 501/505 are never replayed, and 401/402/403 (classified
  `ProviderUnavailable` but not transient server failures) never retry;
- transport failures with no HTTP response at all
  (`ProviderUnavailable`, status 0) whose error chain proves the request never
  reached the provider: a dial-phase `net.OpError` or a `net.DNSError`.

Never retried: provider 401/402/403, provider-invalid-request 4xx,
`ProviderTimeout` (including 408/504 and context timeouts classified as such),
context cancellation, budget exhaustion, `StreamInterrupted`, unknown errors
without a `provider.Error` shape, and any failure after a `ChatStream` is
established.

### D3. retry_count semantics (no off-by-one) and single finalize

`retryCount` counts retries already executed after the initial attempt;
`RETRY_MAX_RETRIES` is the maximum number of retries allowed (0 disables
retries; bounded to 0-5 by configuration and again at the service boundary).
The boundary check is `retryCount >= maxRetries -> stop` evaluated before each
new retry; `retryCount` increments only when a real retry begins.

The durable lifecycle is unchanged: `CreateGatewayRequest` runs once,
`FinalizeGatewayRequest` runs once with the final `RetryCount`. Retry count
reaches the HTTP layer through the in-memory `GatewayRequest.RetryCount` field
and appears in `X-Gateway-Retry-Count` on success, on stream `Prepare`, and on
error responses that carry a record. No hard-coded `"0"` remains.

### D4. Streaming retry window closes at ChatStream establishment

Failures while `StreamingClient.StreamChat` is still trying to return a
`ChatStream` (open-phase HTTP/transport errors) may retry under D1/D2. The
moment `StreamChat` returns a non-nil `ChatStream`, the provider has started
executing/billing, so every later failure - malformed SSE, read failure,
upstream close/reset, client write failure - follows the existing
`stream_interrupted`/cancellation terminal semantics and is never retried, even
before the first downstream byte is committed. The boundary is structural (the
attempt loop only wraps the open call), not a boolean test.

### D5. Retry-After metadata is presence-preserving

`provider.Error.RetryAfter` and `GatewayError.RetryAfter` are
`*time.Duration`: nil when missing or malformed; present with 0 for
`Retry-After: 0` or an already-past HTTP-date; present and positive otherwise.
`provider.ParseRetryAfter(header, now)` is a deterministic pure function
(delta-seconds or HTTP-date) whose delta-seconds path is overflow-safe: it
parses as unsigned with saturation, so a value beyond the representable
`time.Duration` range clamps to the largest positive duration instead of
wrapping negative (such a hint can never fit a normal retry budget). Adapters
parse the wire header; the executor decides whether to wait and retry. A terminal, un-retried provider 429 writes a
safe `Retry-After` header computed by the gateway; no raw provider headers are
ever forwarded. Present hints are honored verbatim (clamped to the remaining
budget); absent hints use a fixed internal 100ms base with bounded exponential
full-jitter, capped by `RETRY_BACKOFF_MAX`.

### D6. Token bucket: refill, burst, and leak-free two-scope admission

Each scope is `golang.org/x/time/rate`. With N requests/minute the refill rate
is N/60 tokens/second and the burst is N - one full minute of instant allowance
- which is a **gateway policy choice**, not an x/time/rate default. Both the
virtual-key and project scopes must pass. A client-facing `Retry-After` is
only emitted by the HTTP layer for the `rate_limited` and
`provider_rate_limited` categories; upstream Retry-After metadata attached to
any other error (invalid request, auth, provider_unavailable, ...) stays
private and is never reinterpreted as a client directive.

Each composite admission holds the registry write lock for the whole decision:
entries are resolved/created under that lock and the same lock is held through
`ReserveN(now, 1)` on each limiter, `DelayFrom(now)` (never `Delay()`), and
the commit-both or `CancelAt(now)`-both step. This is the simplest
lock-ordered (registry -> entry) way to keep both participating entries
current for the entire operation: no sweep or eviction can detach a limb
mid-admission, so an admission can never consume quota on a limiter the
registry has already replaced with a full bucket. A rejected admission
consumes nothing in either scope. Retry-After for a rejection is the maximum
reservation delay and the binding scope (`virtual_key` or `project`) is
reported for operational visibility.

### D7. Bounded limiter registry lifecycle

The registry caps memory: a janitor goroutine (owned by the registry, stopped
by `Close()`, wired to process shutdown) sweeps entries idle past
`RATE_LIMITER_IDLE_TTL` every `RATE_LIMITER_SWEEP_INTERVAL` under the registry
write lock, and `RATE_LIMITER_ENTRY_CAP` bounds the map. Sweep and cap
eviction therefore run only between admissions: while an admission holds the
registry lock its two limbs stay current, and eviction additionally skips the
limbs the in-flight admission already resolved. `EntryCap` is validated to be
at least one per enabled scope, so both enabled scopes can always coexist.
Eviction can only reset a bucket to a full burst (documented bounded
over-admission window for the evicted scope only); it never corrupts counters
or detaches an entry mid-admission.

### D8. Rejection persistence contract and category distinction

Rate-limit and concurrency rejections happen before any provider call and
before `CreateGatewayRequest`: no durable row, no upstream work, stable HTTP
429 with the `rate_limited` category and a gateway-formatted `Retry-After`
where applicable. Operational visibility now comes from a bounded structured
slog event (`event=admission_rejected`, `reason`, `scope` where applicable,
`project_id`, `virtual_key_id`, `retry_after_seconds`; never raw keys,
authorization material, credentials, prompt/response bodies, or provider
headers); Prometheus counters (`gateway_rate_limit_rejections_total{scope}`
etc.) land with the Week 10 Observability milestone. Local rejections remain
`rate_limited`; a provider 429 whose retries are exhausted remains
`provider_rate_limited` (both are HTTP 429 but the stable categories are never
merged).

### D9. Concurrency bounds

`DATA_PLANE_MAX_CONCURRENT_REQUESTS` bounds all admitted chat operations
(stream and non-stream); `DATA_PLANE_MAX_CONCURRENT_STREAMS` is an additional
cap that stream requests must also satisfy. Admission is non-blocking (reject
on full, never queue) and happens in the service before row creation. Slot
release is deferred and exactly-once; the stream-cap failure path releases the
general slot first, so no path (error, early return, panic, cancellation)
leaks an active-slot counter.

### D10. Circuit breaker remains deferred

Week 8 implements no circuit breaker. There is still no evidence that repeated
failed provider calls create meaningful latency/load harm (Tech Design §15),
and the retry baseline is deliberately tiny. Re-evaluation is triggered by
either: (a) failure/retry tests showing retry storms or cascading amplification,
or (b) a controlled provider-failure experiment demonstrating measurable harm a
breaker would remove.

### D11. Configuration posture

Rate limits and concurrency caps default to `0 = disabled` and are enabled via
`.env.example`. Retries default to `RETRY_MAX_RETRIES=1` (one retry after the
initial attempt) and can be disabled with `0` - a different off-switch from the
admission controls, documented accordingly.

## Options considered (summary)

- Per-request fresh timeout per attempt - rejected: multiplies cost/latency.
- Retry whenever `!sink.Committed()` - rejected: replays requests the provider
  may already be billing after `ChatStream` establishment.
- Retry on `ProviderUnavailable` category - rejected: includes 401/402/403.
- Any `>= 500` provider status - rejected: replays unlisted statuses such as
  501/505; only the explicit 500/502/503/529 list is retried.
- Two `Allow()` calls in sequence - rejected: leaks quota when the second scope
  rejects.
- Custom token-bucket implementation - rejected: `x/time/rate` is specified by
  Tech Design §16.
- Unbounded limiter maps - rejected: D7 bounds memory.

## Migration / rollback

No schema change (the `retry_count` column predates this ADR). Rollback is per
commit slice; every admission control can be disabled through its `0`/nil
default, and retries can be disabled with `RETRY_MAX_RETRIES=0`, without code
changes.
