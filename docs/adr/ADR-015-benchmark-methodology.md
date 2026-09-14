# ADR-015: Benchmark methodology (Week 11 Performance Engineering Foundation)

- Status: Draft
- Date: 2026-09-14
- Related: [Tech Design](../TechDesign-Production-Go-LLM-Gateway-MVP.md) sections 32, 35 (Week 11), 36 (reserved ADR-015), and 43; [PRD](../PRD-Production-Go-LLM-Gateway-MVP.md); [testing contract](../../agent_docs/testing.md); ADR-017, ADR-018, ADR-019.

## Status and authorization

The owner approved A0 Draft documentation only. This document records the
approved foundation plan and binding review corrections; its status remains
Draft pending review. The sequence is **Draft -> owner review -> Accepted ->
authorized A1a implementation -> A1a review -> A1b**. Plan approval does not
authorize starting A1 while this ADR is Draft.

A0 changes only this ADR and AGENTS.md. It does not change Makefile, Go,
runtime, configuration, database code, dependencies, or infrastructure. No
load generator or formal performance measurements exist as an outcome of A0.

## Context and alternatives

Week 10 supplies metrics instrumentation and protected pprof. The existing
`cmd/mockprovider` supplies deterministic non-streaming and SSE responses,
delays, and failure modes. `make bench` currently runs Go microbenchmarks via
`go test -run '^$' -bench=. ./...`; it does not establish an E2E baseline.

Real-provider timing is dominated by variable upstream work and can incur
cost. Microbenchmarks are simpler and useful for local hotspots, but omit the
complete HTTP and durable database path. The foundation therefore compares
Client -> Mock Provider (D) with Client -> Gateway -> Mock Provider (G), using
real PostgreSQL and the full gateway path. Authentication, credential
decryption, admission, provider translation, durable create before upstream,
and finalization remain intact. No real-provider calls are required.

A standard-library fixed-concurrency closed-loop client is the initial
choice. Its limitation is that slower responses reduce the offered request
rate; results are not fixed-arrival-rate queueing or universal capacity
claims. No third-party benchmark dependency is introduced. Fixed-arrival-rate
generation can be reconsidered if a later experiment requires it.

## Binding measurement contracts

### D1. Workloads, timing, and result semantics

- D and G use equivalent synthetic request content, response size, and mock
  timing, allowing only required authentication and model namespace differences.
  Initial coverage uses the existing OpenAI-compatible mock, without claiming
  performance equivalence for other adapters.
- Initial scenarios include zero artificial delay non-streaming, fixed-delay
  non-streaming, and SSE with fixed first-content delay, chunk count, and
  interval. Long streams, slow consumers, cancellation, and incomplete streams
  are separate scenarios, excluded from the happy-path baseline.
- Latency is client request start through complete response consumption. TTFT
  is request start through the first valid content delta, not headers, role,
  usage, or terminal events. Non-streaming TTFT is not applicable; streams
  without content have no fabricated TTFT sample.
- Validate response completion. HTTP 200 with incomplete/abrupt/malformed SSE
  is a failure, including missing protocol completion. Do not count it as a
  successful latency sample or successful throughput.
- Report D and G raw P50/P95/P99 distributions separately, sample counts,
  successful throughput, and errors by category (including timeout, rejection,
  and interruption). Preserve failures rather than silently excluding them
  from the result's error denominator.
- Differences between quantiles are named **distribution delta** or
  **gateway-vs-direct delta**, never per-request overhead percentiles.
- Declare measurement-window membership and throughput denominator before
  execution. Warm-up is excluded; in-flight work at the measurement boundary
  has a bounded drain and explicit accounting. No silent dropping of samples.
- Client-observed completion and durable DB finalization are distinct events.
  Finalization verification occurs after timing, under a bounded wait, and
  does not extend individual client latency measurements.

### D2. Database, warm-up, and repetition isolation

Every formal repetition's G run begins with the same **fixture version,
schema/index definitions, and initial logical row cardinality** for all
relevant tables. Record and verify those values before measurement. Use an
explicitly isolated benchmark database; never reset a development or production
database. Future orchestration must verify isolation before any restoration
and stop if isolation or baseline verification fails.

Each D and G run has independent warm-up excluded from statistics. G warm-up
uses separate data state; restore the fixed fixture before formal measurement,
then verify cardinality. Fix and document the subsequent connection/cache
preparation procedure; it must not accumulate business rows. Reset measurement
counters or capture run-scoped starting counters after preparation, so warm-up
cannot enter either the statistical or accounting cohort.

Do not carry `gateway_requests` growth from warm-up or earlier repetitions
into the next formal run. Normal growth within a run remains part of the
workload and is recorded, including final cardinality. Restoration is outside
timing and occurs only after the preceding run has drained and accounting has
been checked. A baseline mismatch makes the run invalid.

This contract fixes logical data and schema, not physical PostgreSQL/OS cache
contents or index page state. Document restore, connection preparation, cache
methodology, and database maintenance conditions consistently. Do not claim
identical physical caches or mix cold/warm methodologies under one result.

### D3. Load-generator validity

D and G use the same long-lived HTTP client/Transport configuration and reuse
policy. Each run reuses its client rather than constructing one per request;
client construction, warm-up, and shutdown policy are identical between paths.
Record connection limits, timeouts, keepalive, protocol, and transport settings
for both loadgen and gateway upstream transports.

A run is **invalid** if loadgen CPU, FD, connection, ephemeral-port, or another
client resource becomes the bottleneck, or if client-side generation errors
occur. Establish generator headroom with recorded resource limits and separate
validation/diagnostic evidence. If headroom cannot be established, do not
publish the run as gateway performance. Report invalid reasons and retain
sanitized diagnostic metadata; do not relabel generator failures as gateway
errors. Expected injected cancellation is classified separately from a
generation failure. Diagnostic instrumentation is isolated from formal timing.

### D4. Frozen main-baseline reliability and observability posture

Record and freeze rate-limit settings, request/stream concurrency limits,
Redis mode, metrics, tracing, pprof, retry policy, and all relevant deadlines.
The main latency/TTFT baseline uses fixed **non-binding admission** with the
local limiter and Redis mode disabled. Keep the admission path intact; choose
and document settings that do not constrain the specified workload. Unexpected
admission 429 makes that baseline run invalid. Do not retune between D/G
repetitions or remove rejections from the evidence.

Main baseline observability is fixed as follows:

- Gateway metrics instrumentation remains enabled by default.
- Tracing is disabled; pprof is disabled.
- Prometheus, Grafana, OTel Collector, and Tempo do not participate in main
  latency timing. No active gateway scrape, trace export, or profiling occurs.
- Redis distributed limiting, tracing enabled, and observability stack
  enabled/scraped are separately identified control experiments. Their results
  are never pooled into the main baseline; record each experiment's settings.

### D5. Execution order and lifecycle budgets

Use **alternating execution order**, initially three repetitions:
`D -> G`, `G -> D`, `D -> G`. This is not strictly balanced/counterbalanced;
strict order balance would require an even repetition count. Each formal run
has its own warm-up and consistent preparation/drain procedures.

Suggested starting parameters are concurrency 1/8/32/64, 10s warm-up, 60s
measurement, and three repetitions per scenario/concurrency. These are
experimental parameters, not measured performance or promised targets; freeze
the selected values in the run manifest before execution.

The existing mock server has `WriteTimeout=30s`. Initial **single-request /
single-stream lifecycle budgets are <=20s**, leaving margin to that response
timeout. This is not a cap on the whole loadgen run: warm-up and measurement
windows may be longer, including 60s measurement.

Account for header delay, first-content delay, chunk spacing, slow reads, and
drain in each request's scenario budget; check client and gateway deadlines
too. A run hitting the mock write timeout is not evidence of gateway behavior.
Only if a scenario demonstrably requires a longer lifecycle may A1b propose
a minimal configurable mock timeout extension, reviewed separately. Do not
silently cross the current boundary or extend it preemptively.

### D6. Happy-path correctness and accounting gate

For every formal normal G run, after bounded drain/finalization, verify that
the same measured cohort has equal counts of:

1. Loadgen successful requests.
2. Corresponding mock-provider completed requests.
3. Newly finalized successful gateway request rows in the benchmark database.

There must also be **no residual benchmark `in_progress` row**. Verify new rows
against the initial fixture and run scope, excluding warm-up and fixture rows.
Provider completion means successful response completion, not merely request
arrival/hit count. Reconcile attempts/retries so duplicates cannot disappear
behind totals. Existing mock hit counts alone do not satisfy this gate.

For D, likewise reconcile loadgen successful samples with corresponding mock
completed requests. Any mismatch, residual in-progress work, or inability to
complete verification makes the **entire run invalid**: do not publish its
latency/throughput conclusions. Keep sanitized invalid-run evidence. This gate
does not substitute for error-rate reporting or failure-scenario tests.

### D7. Profiling, resources, and connection evidence

Later profiling/diagnostic acceptance includes CPU, heap, allocations,
goroutines, and gateway-to-upstream connection reuse, new connections, pool
waits, and connection-limit effects. Capture mutex/block profiles when evidence
suggests contention. Verify resource recovery after cancellation and load.

Profiling, active scraping, trace exporting, and connection instrumentation
(such as optional HTTP tracing) run separately from formal latency timing.
Record the diagnostic configuration and its potential overhead. Never infer
upstream reuse solely from loadgen-to-gateway reuse. Reuse existing protected
private Ops pprof; do not weaken its security or enable it in the main baseline.

### D8. Reproducibility, data boundaries, and optimization

The report records commit SHA/dirty state, Go version, hardware/VM, OS, process
placement, mock configuration, synthetic workload dimensions, concurrency,
warm-up/measurement/drain durations, repetitions and execution order, fixture
version, schema/index definitions, initial/final logical cardinalities,
transport/DB pool settings, reliability/observability posture, validity and
accounting outcomes, and exact commands. Record measurement methodology and
limitations, not just headline numbers.

Use synthetic data. Do not print, commit, or include credentials, full virtual
keys, auth headers, raw request/response bodies, private logs, or production
data in reports/artifacts. Fixture identity must not expose secret material.

Tune only demonstrated transport, allocation, buffering, SQL, or concurrency
hotspots, one factor at a time with comparable before/after runs. Caching,
async durability, schema, auth, dependency, or routing changes require their
own review; this ADR does not authorize them. Preserve cancellation,
backpressure, create-before-upstream, and retry semantics. Week 11 succeeds
with a credible baseline and explainable evidence even if no optimization is
justified or retained.

## Implementation slices and acceptance

### A0: Draft methodology (current authorization)

Create this Draft ADR and synchronize AGENTS.md phase and command semantics.
Check consistency against the owner corrections, Tech Design, Makefile, and
mock timeout. Commit only the two documents and stop for review. No formal
numbers or performance claims are produced.

### A1a: Loadgen core (not started)

After acceptance and authorization, implement standard-library direct/gateway
non-streaming and SSE happy paths, independent warm-up, fixed concurrency,
bounded drain, latency/TTFT/completion/error/throughput statistics, and validity
metadata. Deterministic tests cover timing/statistical boundaries, first valid
content, protocol completion, and normal resource cleanup using controlled
time and responses. Use no third-party benchmark dependency.

Keep `make bench` for microbenchmarks; add the separate `make bench-e2e` tool
entry point in this implementation slice. It is **planned, not available** in
A0. Until real PostgreSQL fixture orchestration and D6 reconciliation are
implemented and verified, runs are tool validation only, not formal gateway
baselines. Submit A1a independently and review before A1b.

### A1b: Failure scenarios (not started)

Implement cancellation, slow consumers, abrupt/incomplete SSE, failure
classification, generator-invalid cases, and connection/body/goroutine release
checks. Confirm timeout attribution and the per-request budget. Any justified
mock timeout extension is a separate reviewed change. Do not combine A1a and
A1b into one commit.

### Later gates

A2 establishes real PostgreSQL fixture isolation, full gateway execution,
completed mock accounting, and reproducible formal baselines under all validity
gates above. A3 captures separate profiles and connection diagnostics. Any
evidence-driven tuning follows separately, then the first benchmark report
and owner learning review. The owner should explain measurement boundaries,
DB/cache limitations, generator validity, connection reuse, bottlenecks, and
the evidence that would justify redesign.

For implementation slices, run relevant deterministic tests plus `make test`,
`make typecheck`, `make lint`, `make build`, and `make race`; use
`make integration` for real database paths. Timing runs use normal builds,
without race or profiling instrumentation. A0 requires document consistency
and whitespace checks only, not runtime tests or benchmark execution.

## Consequences, rollback, and reopen triggers

Isolation and accounting add preparation work but make reported results
attributable. Three alternating repetitions reduce a fixed-order bias without
claiming strict balance. Closed-loop and single-host/mock results have explicit
scope; generator or shared-host contention can invalidate a run.

A0 has no runtime or schema effect; rollback is a documentation-only revert.
Later tooling and each tuning change remain independently reversible. Revisit
the methodology if generator headroom fails, database preparation is not
repeatable, measurement variability prevents conclusions, longer streams are
needed, strict order balance is required, or evidence calls for an open-loop
experiment. No such trigger authorizes silently weakening a validity gate.

## Evidence status

A0 records a plan only. A1a/A1b, E2E orchestration, profiles, and formal
benchmark results are not implemented or claimed by this ADR.
