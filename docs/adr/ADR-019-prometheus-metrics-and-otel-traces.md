# ADR-019: Prometheus metrics + OTel traces with a telemetry lifecycle seam (Week 10)

- Status: Accepted
- Date: 2026-09-05
- Related: Tech Design §24 (Metrics contract), §25 (OpenTelemetry Tracing), §27.2 (Local services), §29 (Readiness and graceful shutdown), §37 (Failure Matrix), §36 minimum ADR list item "ADR-012 Prometheus metrics + OTel traces" (recorded in practice as ADR-019); Week 10 roadmap entry; ADR-017 D8 (rate-limit rejection counters planned for Week 10); packages `internal/telemetry`, `internal/app`, `internal/dataplane`, `cmd/gateway`; local Docker Compose topology (A3). No schema change.

## Status history

- 2026-09-05: created as **Draft** immediately after the project owner approved
  the revised Week 10 foundation plan, BEFORE any code/configuration change.
  Unlike ADR-018 (Week 9), Prometheus + OTel are already approved Tech Design
  baseline, so no evidence-first "should we adopt it?" experiment precedes this
  decision; the implementation evidence is appended to this ADR after the fact.
  All seven owner constraints from the plan review are binding decisions below.
- 2026-09-05: transitioned to **Accepted** by the docs-only fix commit after the
  owner reviewed the architecture baseline and approved it. The fix folds in
  the closing constraints as binding requirements: (P1) the telemetry shutdown
  covers every `App.Run` exit path including partial `listen()` failure via a
  Run-scope cleanup, and the telemetry runtime `Shutdown` is idempotent with a
  `main` fallback for the construction-failure corner where `App.Run` is never
  reached; (P2) `gateway_requests_total` is defined as a durable lifecycle
  metric with explicit finalize-persistence-failure counting semantics;
  (P3) invalid/over-long `X-Request-ID` only drops the correlation value and
  never rejects the request; (P4) pprof protection hashes the token and uses a
  single guarded handler/mux with no `DefaultServeMux`/global bypass. Slice A1
  starts only after the owner confirms this commit.

## Context

Week 10 builds the Observability milestone: Prometheus metrics, OTel traces,
the local Collector/Tempo/Prometheus/Grafana stack, protected pprof, and later
request-UI trace deep links. Tech Design already fixes the technical direction
(metrics via the Prometheus client library scraped from the Operations Plane;
traces via OTel SDK -> OTLP -> Collector -> Tempo; logs remain `slog` JSON on
stdout with no OTel logging SDK; §37: an OTel backend outage must never break
the data path). This ADR records the decision baseline the Week 10 foundation
implements, with the review constraints fixed as binding requirements. The
three-week plan after this ADR: A1 (metrics foundation) -> review -> A2
(tracing foundation + trace-id semantic migration + log correlation) -> review
-> A3 (local observability stack + protected pprof) -> review -> foundation
Evidence Gate.

## Decisions

### D1. Three identifiers keep distinct semantics; `gateway_requests.trace_id` stores the real OTel TraceID (semantic migration, no schema change)

The current code treats a client-supplied `X-Request-ID` (or a generated
UUIDv4 fallback) as "traceID" and persists it into `gateway_requests.trace_id`
(migration `000004`, `TEXT` nullable, max 200 bytes). That is a correlation ID,
not an OTel TraceID, and it must not keep that role once OTel lands.

| Identifier | Source | Role | Persisted? |
|---|---|---|---|
| `gateway_request_id` | `gateway_requests.id` (store-generated) | Gateway's own durable request-row ID; echoed as `X-Gateway-Request-ID`; log field | yes (primary key) |
| client `X-Request-ID` | client request header, optional, untrusted | correlation metadata only, as log field `client_request_id` | **no** |
| OTel `trace_id` | `gateway.request` root span `SpanContext().TraceID()` (128-bit, 32-char hex) | request history -> Tempo/Grafana deep link | yes -> `gateway_requests.trace_id` |

Migration plan (no new migration):

1. The handler creates the `gateway.request` root span at the very top of the
   request handler, before content-type validation and before auth, so the
   hierarchy `gateway.request -> auth.virtual_key -> rate_limit.check ->
   usage.create_request_record -> provider.attempt -> usage.finalize` is real.
2. The `requestTraceID()` helper's X-Request-ID-as-trace-id and UUIDv4
   fallback roles are removed.
3. The Service derives the persisted value from the active span in `ctx` at
   `CreateGatewayRequest` time: a valid span persists the 32-char OTel
   TraceID; an invalid/noop span (tracing disabled) persists NULL.
4. Structurally there is exactly one source of truth for the trace ID: the
   context's active `gateway.request` span. The historical `traceID string`
   parameters on `CompleteChat`/`StreamChat` (and their `*StartedAt` wrappers)
   are **removed**, not kept as a second source. A2 is the window for this
   cleanup because A2 already touches every call path and the tracing tests.
5. Existing rows keep their old values (historical data, no rewrite); only new
   writes carry the new semantics.

Regression tests (A2): persisted `CreateRequestParams.TraceID` equals the
exported root span `TraceID()` (fake store + in-memory span recorder asserting
all three agree); a client `X-Request-ID` never influences the persisted value
and appears only as log metadata; tracing-disabled mode persists NULL and keeps
the pre-Week-10 behavior (existing regression suite passes unchanged); auth
failure still produces an exported `gateway.request -> auth.virtual_key` span
proving the root span precedes auth.

### D2. Metrics are Prometheus client-library metrics on the private Operations Plane; the app owns the Registry; no package-global state

- Metrics use the Prometheus client library (Tech Design §24/§26 table
  decision: "Prometheus client library, teaches metric design directly").
  Business metrics register on an **app-owned `*prometheus.Registry`**
  created in `cmd/gateway` wiring and wrapped by an app-owned
  `internal/telemetry.Metrics` value. `prometheus.DefaultRegisterer` /
  `DefaultGatherer` are never used for business metrics; no `init()`-time
  registration. Tests build their own Registry per Service/App instance, so
  many Gateways/Services can exist in one test process with no duplicate
  registration.
- The Ops plane always serves `GET /metrics` via `promhttp`. There is no
  `METRICS_ENABLED` switch: the Ops plane is the private operations listener,
  metrics are part of its contract, and A1/A3 enforce that `/metrics` exists
  **only** on the Ops mux (data and control muxes never mount it).
- Scrape semantics (learning point recorded in the Week 10 docs): Prometheus
  pulls `/metrics`; the gateway never pushes metrics. Traces are the pushed
  side (OTLP). This matches §37: "Prometheus unavailable -> scrape fails
  externally; data path unaffected".

### D3. A1 metric set is exactly the four approved counters/histograms/gauges; nothing else

A1 implements only:

- `gateway_requests_total{provider, model_family, status, stream}` (observed at
  finalize; durable-row requests only);
- `gateway_request_duration_seconds{provider, stream}` (observed at finalize);
- `gateway_active_requests{}` and `gateway_active_streams{provider}` (gauges,
  boundaries in D4).

Deliberately deferred to a later, complete §24.1 slice (not "if A1 needs
them"): every other §24.1 counter/histogram/gauge
(`gateway_retries_total`, `gateway_provider_errors_total`,
`gateway_tokens_total`, `gateway_cost_nano_usd_total`,
`gateway_rate_limit_rejections_total` - already promised by ADR-017 D8 for
Week 10, `gateway_usage_finalization_failures_total`,
`gateway_ttft_seconds`, `gateway_upstream_duration_seconds`,
`gateway_usage_finalize_duration_seconds`, `gateway_degraded_dependency`).

**No ingress metric is added in the foundation.** (See counting semantics below.) The draft plan's
`gateway_ingress_requests_total{status_class}` is rejected:

- it is outside the approved §24.1 core contract;
- computing the final HTTP status would invite ResponseWriter wrapping/status
  tracking;
- streaming relies on `http.ResponseController`/Flush today and the SSE hot
  path must not absorb risk for a supplementary metric;
- auth/malformed/pre-row request visibility stays with the existing bounded
  `slog` events (ADR-017 D8 `event=admission_rejected` etc.). The pre-row
  observability gap is designed separately in the later complete-metrics slice
  if it is still needed then.

Counting semantics (binding):

- `gateway_requests_total` is a **durable gateway request lifecycle metric**,
  not an HTTP ingress metric. It is observed when a request's business
  terminal state is determined at finalize. Pre-auth, malformed-body,
  unsupported-parameter, and admission-rejected requests are intentionally
  never counted - that exclusion is part of the approved contract, and their
  visibility stays with the bounded `slog` events (ADR-017 D8) plus the
  later full-§24.1 design.
- A finalize **persistence failure does not change the request count**: the
  counter is observed as soon as the business terminal state is determined
  (the finalize call carries a decided `status=succeeded|failed`), regardless
  of whether the durable write itself succeeds. Durable-write failure is a
  separate operational signal that belongs to
  `gateway_usage_finalization_failures_total` (full §24.1 slice); it must
  never silently alter `gateway_requests_total`.

### D4. Active gauge boundaries; the admission API is not polluted with telemetry

`admit(ctx, auth, stream)` is the Week 8/9 rate-limit/concurrency seam and
stays telemetry-free. Provider labels must not be smuggled into it. Instead:

- Normal flow: parse `provider/model` (already done before admission) ->
  admission succeeds (rate limiting + slots) -> the request path creates a
  **separate metrics release closure** (increment now, decrement exactly once
  when the operation finishes) alongside the existing slot release.
- The limiter/slot release and the metrics release stay separate
  responsibilities but are both deferred in the same request lifecycle, so
  every post-admission error/cancel/stream path decrements exactly once and no
  pre-admission path ever increments.
- Boundaries: `gateway_active_requests` counts chat operations (stream and
  non-stream) that passed all admission controls and are still executing;
  `gateway_active_streams{provider}` counts the same for stream operations,
  labeled by the already-parsed provider. Increment happens immediately after
  successful admission; decrement happens when the operation completes
  (after finalize, in the deferred release). Post-admission early-exit paths
  (credential resolve/decrypt failure, stream-prepare failure, malformed
  upstream, cancellation) still decrement through the same defer.

### D5. Bounded label domains (contract fixed before code)

- `provider`: registry enum values `openai` / `anthropic` / `deepseek` only.
- `model_family`: explicit bounded mapping table (`familyFor(model)`) derived
  from the seed-catalog families per provider; anything unmapped labels
  `other`. Raw model strings never become labels. Revisit only with the later
  metrics slice or benchmark evidence.
- `status`: `succeeded` / `failed`, mirroring the durable row's final status.
- `stream`: `true` / `false`.
- Never label with: request ID, trace ID, project ID, user ID, virtual-key ID,
  raw model, raw error text, or any secret-shaped value (Tech Design §24.2).
- A redaction/cardinality unit test locks the finite domain: after a sample
  request flow, every label value belongs to the domain above, and no
  high-cardinality/secret value appears.
- Metrics with unknown provider/model (auth failure, malformed body, model
  parse failure, admission rejection) do not exist under these four metrics;
  see D3 (no ingress metric in the foundation).

Timing boundaries (recorded now, implemented across A1/earlier slices as
available): `gateway_request_duration_seconds` spans handler ingress
(`requestStartedAt`, already persisted as `gateway_requests.started_at`)
through finalize's `completedAt`; for streaming this includes the entire
stream lifecycle (until `[DONE]` or the interruption finalize), matching the
durable latency semantics. TTFT/upstream/usage/finalize histograms arrive with
the complete §24.1 slice; TTFT already has a durable measurement point
(`ttftMS`) to reuse. A mid-stream failure is recorded as `status=failed` from
the durable finalize even though the HTTP status was committed as 200;
metrics follow the durable status, never the committed HTTP status.

### D6. Logs stay `slog` on stdout; log-trace correlation is a wrapping handler over the existing logger

No OTel logging SDK (Tech Design §25: logs remain `slog` stdout in the MVP).
Correlation is a `slog.Handler` wrapper around the existing JSON handler:

- In `Handle(ctx, record)` it inspects `trace.SpanContextFromContext(ctx)`;
  when the context carries a valid active span it appends only `trace_id` and
  `span_id` attributes; when there is no active span (plain calls,
  lifecycle logs, background goroutines) output is byte-identical to today.
- Correlation is therefore only as good as the call sites that pass a context.
  A2 converts the request-path call sites (all have `ctx` in scope today):
  1. `logAdmissionRejection` gains a `ctx` parameter and uses
     `InfoContext` (`internal/dataplane/dataplane.go`);
  2. the three pricing logs in `resolvePricing` use `DebugContext` /
     `WarnContext` (`no pricing version effective`, `pricing lookup budget
     exceeded`, `pricing lookup failed`);
  3. `persistenceError` gains a `ctx` parameter and uses `ErrorContext`
     (`gateway request finalization failed`).
  A2 also audits the distributed-limiter wrapper: request-path (ctx-bearing)
  logs become `*Context`; background probe/transition logs stay plain (no
  active span) and are documented as such, not treated as a gap. App lifecycle
  logs (`internal/app`) stay plain calls - there is no request context at those
  points - and that is the intended design.
- Secrecy: the wrapper adds only `trace_id`/`span_id`. No prompt/response
  body, `Authorization` header, virtual key, or provider secret ever becomes a
  log attribute or span attribute; existing and new redaction tests enforce
  this across logs, spans, and metric labels.
- If A2 cannot convert a listed call site for any reason, log-trace correlation
  is reported as incomplete rather than claimed done.

### D7. `X-Request-ID` is untrusted client input with a bounded policy

`X-Request-ID` is correlation metadata only (D1). Bounded policy:

- it never becomes a metric label and never becomes the OTel trace ID;
- if logged as `client_request_id`, the value must be <= 200 bytes
  (the same bound as the existing `trace_id` column), must contain no control
  characters, and must be trimmed;
- an over-length or control-character value is **dropped** and replaced by a
  bounded marker (e.g. `client_request_id_present=true`) so an arbitrarily long
  header cannot cause log amplification;
- dropping the value is the only consequence. An invalid/over-long/
  control-character `X-Request-ID` never rejects, delays, or alters the LLM
  request itself: the request proceeds exactly as if the header were absent;
- it is not a span attribute unless a future slice demonstrates an explicit
  need, which requires a new decision here.

### D8. Configuration is minimal and uses standard OTel semantics

- `OTEL_EXPORTER_OTLP_ENDPOINT`: empty (default) means tracing/export is
  **disabled** (noop tracer provider; zero data-path cost, no dependency);
  non-empty enables the OTLP gRPC exporter to that endpoint. This standard
  environment variable is read by `internal/config` and passed explicitly to
  the exporter; no gateway-specific alias is invented.
- `OTEL_SERVICE_NAME`: default `gateway`, used as the OTel resource service
  name.
- Sampling is **fixed at AlwaysSample for the foundation**. Local traffic is
  tiny; sampling configuration is added only when a real need or benchmark
  evidence demands it, and then through standard OTel environment semantics
  (e.g. `OTEL_TRACES_SAMPLER`), not gateway-specific switches.
- No `METRICS_ENABLED` (D2). No other new toggles for A1/A2.
- pprof (A3): `PPROF_ENABLED` defaults false. When enabled, `PPROF_TOKEN` is
  required (non-empty). Implementation constraints are fixed now, not decided
  while coding:
  - every ops-plane `/debug/pprof*` endpoint is mounted behind the **same
    protected handler/mux** (single choke point for the token check);
  - the token is compared by hashing it once to a fixed-length digest
    (SHA-256) and using `subtle.ConstantTimeCompare` on the digests, so the
    comparison is constant-time and never touches the raw token length;
  - registration never bypasses the guard: no `http.DefaultServeMux` and no
    package-global registration anywhere; pprof/metrics exist only on the Ops
    mux built by `internal/app` from injected handlers;
  - mux-isolation tests keep locking that data and control muxes never mount
    pprof or metrics, whether enabled or not.
  Data and control muxes never mount pprof or metrics. Production does not
  rely on the token as a substitute for network isolation: the Ops plane lives
  only on a private network and is never publicly routed (D10). This security
  model is decided now and is not re-decided while coding.

### D9. Telemetry lifecycle seam inside `internal/app`; accurate shutdown order

Factual current lifecycle (verified in code): `App.Run` defers
`database.Close()` at the top; on exit it closes listeners, then `shutdown()`
drains **control, data, and operations concurrently** under one bounded
`ShutdownTimeout` context (there is no strict control -> data -> ops
sequencing today), then returns and the deferred `database.Close()` runs.
After `Run` returns, `main.run()`'s deferred functions run LIFO:
distributed-limiter `Close()` (or registry close), then `redis.Client.Close()`,
then the shared provider `Transport.CloseIdleConnections()`.

**Week 10 does not reorder the three HTTP servers' shutdown.** Concurrent
drain is unchanged unless observability evidence later proves it wrong. The
telemetry lifecycle must instead satisfy:

```
HTTP handlers/streams all ended
  -> bounded telemetry flush/shutdown
  -> PostgreSQL / limiter / Redis / provider transport close
```

Because `database.Close()` already runs inside `App.Run`, a tracer shutdown
placed after `application.Run()` returns would run **after** PostgreSQL is
closed - unacceptable. The minimal seam is therefore additive inside `App`,
with `database.Close()` ownership staying in `App.Run` and the three-server
concurrent drain untouched:

- `app.Options` gains an optional `TelemetryShutdown func(context.Context)
  error` (nil allowed; no-op when absent). `internal/app` depends only on that
  small function type - never on concrete OTel types.
- The seam is registered as a **Run-scope cleanup, not an ordinary control-flow
  step after `a.shutdown()`**. Concretely: inside `App.Run`, the telemetry
  cleanup is deferred after `database.Close()` is deferred, so the LIFO order
  guarantees the telemetry cleanup always runs **before** `database.Close()`
  on every exit path of `Run`.
- Because the cleanup is registered before `listen()` is attempted, it also
  covers the abnormal exits: a partial `listen()`/startup failure (one listener
  bound, a later one failing) runs the telemetry cleanup too, and still before
  `database.Close()`.
- In normal operation the HTTP drain has already completed inside `Run` before
  the deferred cleanup runs, so the observable order remains:
  `HTTP handlers/streams drain -> telemetry flush/shutdown -> database.Close()`.
- Exactly-once and bounded: the deferred cleanup runs once per `Run`; the hook
  is invoked under a fresh bounded context (fixed `telemetryFlushTimeout`,
  e.g. 5s, independent of the server shutdown budget). A hook error is logged
  at error level but is **not** joined into `Run`'s returned error: telemetry
  flush is best-effort and must not change the process exit status.
- **Idempotent runtime shutdown + `main` fallback for the pre-`Run` corner:**
  the TracerProvider is created by `main` before `App.Run`, so a construction
  failure after the tracer exists (e.g. handler/service wiring fails and
  `App.Run` is never called) needs its own cleanup. The telemetry runtime's
  `Shutdown` is required to be idempotent (safe to call more than once; the
  OTel SDK `TracerProvider.Shutdown` satisfies this), and `main` registers its
  own deferred fallback that calls the same `Shutdown` right after the
  TracerProvider is created. The two callers then divide responsibility:
  - the `App` hook is responsible for the correct DB-close ordering during the
    normal runtime lifecycle (flush after drain, before `database.Close()`);
  - the `main` fallback is responsible for the construction-failure corner
    where `App` never took over; when `App.Run` did run, the fallback's later
    redundant call is a safe idempotent no-op.
- Wiring in `cmd/gateway`: when a real (non-noop) TracerProvider was built,
  `main` defers `TracerProvider.Shutdown(boundedCtx)` immediately after
  creation (fallback) and passes the App hook that calls the same `Shutdown`.
  Noop mode builds nothing and passes no hook. The existing deferred close
  order for limiter/Redis/transport is unchanged; those run after `Run`
  returns, i.e. after the telemetry flush.

Deterministic lifecycle tests (A2): a fake hook and a fake database record
ordering, asserting (1) the hook runs only after all three servers report
`Shutdown` complete, (2) the hook runs before `database.Close()`, (3) the hook
receives a bounded context and returns within it, (4) the hook runs on both
normal exit paths (signal-driven shutdown and serve-error abort) **and** on a
partial `listen()`/startup-failure exit, all still before `database.Close()`,
(5) the hook runs exactly once per `Run`, (6) `Shutdown` is idempotent: a
second invocation (the `main` fallback after a successful `App.Run`) is a
safe no-op, (7) exporter goroutines do not leak (`-race`).

The exporter pipeline (A2): `BatchSpanProcessor` with a bounded, non-blocking
export queue and export timeout; `Shutdown(ctx)` is bounded; exporter failure
or queue pressure never blocks the request critical path (deterministic tests
use failing/blocking fake exporters, not real network timeouts).

### D10. Local observability topology and Ops-plane exposure (self-consistent)

- Compose-published Collector/Prometheus/Grafana (and any debugging) ports are
  bound to `127.0.0.1` on the host (Collector OTLP gRPC `127.0.0.1:4317`,
  Prometheus `127.0.0.1:9091` to avoid the Ops `:9090`, Grafana
  `127.0.0.1:3001`).
- The host Gateway's `OPS_ADDR=:9090` is an explicit **dev-only exception** in
  the A3 local topology: the Prometheus container scrapes it through
  `host.docker.internal` (`extra_hosts: ["host.docker.internal:host-gateway"]`
  on Linux). This is the only reason the dev Ops listener is not loopback-only.
- Production / Week 12: the Ops plane sits only on a private Docker network
  and is never publicly routed (Caddy terminates public TLS for the data and
  console planes only). The whole Ops plane is therefore **not** forced
  loopback-only anywhere; network isolation is a deployment property, and pprof
  stays off by default with the D8 token guard as defense in depth.

### D11. Trace hierarchy and span contract (A2 implementation target)

```
gateway.request                  created by the HTTP handler at ingress
├── auth.virtual_key             wraps Authenticate
├── rate_limit.check             inside Service.admit
├── usage.create_request_record  wraps store.CreateGatewayRequest
├── provider.attempt             one per retry attempt (attr gateway.retry_attempt)
│   ├── provider.http            wraps the single adapter call
│   └── provider.stream          streaming: the established read loop
└── usage.finalize               wraps finalize / finalizeStream
```

Span attributes (subset of Tech Design §25): `gateway.request_id` (set once the
durable row ID exists), `llm.provider`, `llm.model`, `llm.stream`,
`gateway.retry_attempt`, `http.response.status_code` where reachable,
`gateway.error_category`. A2's minimum child set is `gateway.request`,
`auth.virtual_key`, `rate_limit.check`, `usage.create_request_record`,
`provider.attempt`, `usage.finalize`; `provider.http`/`provider.stream`
placement follows the boundaries above. No prompt/response content or
credential material in any attribute (redaction test).

## Options considered (summary)

- OTel metrics SDK for gateway metrics - rejected: Tech Design already chose
  the Prometheus client library; scrape semantics teach metric design directly.
- OTel logging SDK - rejected (Tech Design §25): logs stay `slog` stdout.
- Ingress/status-class counter for pre-row requests - rejected (D3):
  outside §24.1, invites ResponseWriter status tracking and SSE hot-path risk.
- Loopback-only Ops plane - rejected (D10): conflicts with the Docker
  Prometheus scrape topology; network isolation belongs to deployment.
- Global/default registry + `init()` registration - rejected (D2).
- Keeping `CompleteChat/StreamChat(..., traceID string, ...)` as a second
  source of the trace ID - rejected (D1/D4): the active span in `ctx` is the
  single source of truth; the parameter is removed in A2.
- Pass a `provider` label into `admit` - rejected (D4): telemetry must not
  pollute the admission seam; gauges use a separate release closure.
- Reordering the three HTTP servers' shutdown to a strict sequence - rejected
  (D9): no observability evidence demands it; only the additive telemetry hook
  is added.

## Failure policy mapping (Tech Design §37)

| Failure | Expected behavior |
|---|---|
| OTel Collector / Tempo unavailable | exporter queue backs up bounded and drops; requests continue unchanged; flush is bounded and best-effort |
| Prometheus unavailable | scrape fails externally; data path unaffected |
| Ops `/metrics` misconfig / scrape error | never affects data/control planes |
| TracerProvider shutdown exceeds budget | `Shutdown` context expires; error logged, process exit status unchanged |
| Tracing disabled (no endpoint) | noop tracer; zero data-path cost; existing behavior preserved |

## Migration / rollback

- No database migration. `gateway_requests.trace_id` keeps its type/bound;
  semantics of new writes change (D1). Existing rows are left as historical
  data.
- Code rollback is per commit slice: A1 metric registrations are additive and
  removable; A2 tracing is disabled by leaving `OTEL_EXPORTER_OTLP_ENDPOINT`
  empty (noop); the removed `traceID string` parameters are internal API
  cleanup within a single slice and are not part of any public contract.
- A3 Compose additions and pprof are opt-in (`PPROF_ENABLED`, token required).
- `.env.example`, Docker/Compose, and Makefile change only in the slice that
  owns them (A2 for the OTel env keys, A3 for Compose/pprof/Make targets).

## Evidence / implementation notes

(Filled in by slices A1/A2/A3 as they land; this ADR commit is docs-only.)

## Reopen triggers

- A real need or benchmark evidence for sampling configuration (then use
  standard OTel env semantics per D8).
- The complete §24.1 metrics slice designs the pre-row observability gap (D3)
  and the full metric set including `gateway_rate_limit_rejections_total`
  (ADR-017 D8).
- Evidence that the concurrent three-server drain hurts telemetry completeness
  badly enough to reorder it (D9).
- A future slice wants `X-Request-ID` on spans or a different retention
  semantics (D7).
- Week 11 benchmark evidence shows metric/scrape overhead worth revisiting.
