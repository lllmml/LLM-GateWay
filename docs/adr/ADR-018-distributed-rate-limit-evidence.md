# ADR-018: Redis-backed distributed rate limiting (Week 9)

- Status: Accepted
- Date: 2026-09-05
- Related: Tech Design §16 (Rate Limiting Evolution, Phases A/B/C, §16 Redis Failure Policy), §30.4 (Redis Integration); Week 9 roadmap entry; ADR-017 (single-instance reliability baseline); packages `internal/ratelimit` (+ planned `internal/ratelimit/distributed`), `internal/dataplane`, `internal/store/postgres` (integration experiment); MEMORY.md "Slice B mandatory invariants".

## Status history

- 2026-09-05 (Slice A): created as **Draft** once the two-replica experiment
  evidence existed; Decision explicitly pending.
- 2026-09-05 (Evidence/Decision Gate): Slice A code review approved
  (commits `7ed439b`, `2e8dabb`); the gate decision and the full Slice B
  architecture/failure policy below are recorded, and this ADR becomes
  **Accepted** - BEFORE Slice B implementation begins, per the agreed
  transition order. If implementation later finds a design deviation, this ADR
  is amended with the reason recorded.

## Context

ADR-017 shipped the single-instance reliability baseline: an in-memory
composite (virtual-key + project) token-bucket limiter, concurrency bounds, and
bounded retried upstream execution. Tech Design §16 requires Redis only after
the distributed problem is demonstrated (Phase B): two gateway replicas behind
the reverse proxy, each with its own in-memory limiter, admitting a cluster
total above the configured per-key limit while every replica individually
stays under it.

## Evidence / Decision Gate

### Gate status: PASS (2026-09-05, owner review)

Slice A evidence (commits `7ed439b`, `2e8dabb`) demonstrated, via a test-only
HTTP/reverse-proxy experiment over shared real PostgreSQL with a shared
deterministic mock provider and a frozen limiter clock:

```
single-replica control (per-key limit 20):
  exactly 20 admitted (HTTP 200), exactly 4 rejected (HTTP 429),
  exactly 20 provider calls, exactly 20 durable gateway_requests rows

two replicas, independent replica-local limiter state (per-key limit 20):
  replica A = 12 admitted, replica B = 12 admitted,
  cluster total = 24 admitted, 0 rejected,
  24 provider calls (> intended cluster limit 20),
  24 durable gateway_requests rows
```

Conclusion accepted by the project owner: the in-process limiter is correct on
a single instance, but independent replica-local limiter state is duplicated
across replicas, so cluster aggregate admission exceeds the intended per-key
limit.

### Decision (Accepted)

**Redis-backed distributed rate limiting is justified by the measured
multi-replica inconsistency.**

The Evidence section below records what the experiment actually proved; the
architecture and failure policy that follow define how Slice B implements this
decision. Redis is not a source of truth for projects, credentials, pricing,
or durable usage; it holds only ephemeral, reproducible rate-limit counters.
The decision to implement is Week 9 roadmap work. Whether the public demo
defaults Redis on is a separate deployment/product decision (Tech Design
open question) that belongs to the Week 12 demo planning, not to this gate.

## Evidence (what the experiment proved)

Commands: `go test -tags=integration ./internal/store/postgres/ -run 'TestReplicaRateLimit' -v` (also run inside `make integration`); fast regression `go test ./internal/ratelimit -run Replica`.

### Topology and fidelity

Two independent Gateway HTTP/Service stacks model two replicas at the
HTTP + limiter-state boundary. Each stack owns its own in-memory
`ratelimit.Registry` (independent per-key quota state), while PostgreSQL and
the deterministic mock provider are shared. The real OpenAI adapter is used
end to end; the provider config row's `base_url_override` column points at the
shared mock endpoint (test-only, via the store query layer - no public API and
no production base-URL change).

Fidelity note: the two stacks run in one Go OS process and share the
non-limiter test dependencies (PostgreSQL Store, provider registry/client
wiring, mock provider). This experiment therefore demonstrates the correctness
problem caused by **duplicated local limiter state** (independent registries
-> duplicated quota -> cluster over-admission); it does NOT claim to reproduce
OS-process isolation, network boundaries, or production reverse-proxy
behavior. That is exactly the property Phase B needs to show: **the cluster
needs shared coordination for quota state; under the approved Week 9 roadmap,
Slice B uses Redis for that role.** The experiment is not an exercise in
process/network isolation.

### Determinism

- Per-key limit `KeyRPM = 20`, burst 20 - unchanged Week 8 `x/time/rate`
  policy (requests/minute semantics; the Tech Design "10 req/s" sketch is
  illustrative only and no semantics were changed).
- `ProjectRPM = 0` (project scope disabled) so only the per-virtual-key claim
  is under test and the second scope cannot interfere.
- Both registries use one **frozen clock** (`ratelimit.Config.Now`), so refill
  is exactly zero and every outcome is exact; correctness never depends on
  wall-clock timing or refill margins.
- `T = 24` sequential requests, deterministic interleave A,B,A,B,... (12 each);
  `RetryMaxRetries = 0` so one allowed request == exactly one provider call.

### Results

Distributed case (`TestReplicaRateLimitInconsistencyHTTPLevel`):

```
limit = 20 per key (cluster intended limit)
replica A admitted = 12 (HTTP 200), rejected = 0
replica B admitted = 12 (HTTP 200), rejected = 0
total HTTP 200 = 24, total HTTP 429 = 0
mock provider calls = 24 > intended cluster limit 20
durable gateway_requests rows = 24 (asserted)
```

Single-replica control (`TestReplicaRateLimitSingleReplicaControl`):

```
HTTP 200 = exactly 20, HTTP 429 = exactly 4
mock provider calls = exactly 20
durable gateway_requests rows = 20 (asserted)
```

The durable-row assertions lock in ADR-017 D8: rate-limit rejection happens
before durable row creation, so the 4 rejected requests produced no row.

Fast regression (`internal/ratelimit/replica_inconsistency_test.go`): two
registries admit 24 across them at registry level; control registry admits
exactly 20 of 24; runs in `make test` / `make race`.

## Decisions - Slice B architecture and failure policy

### D1. Limiter interface; Redis logic never reaches handler/service

`internal/ratelimit` defines the minimal seam, and `dataplane` keeps depending
on it:

```go
type Limiter interface {
    // Admit makes one composite admission decision. Errors only propagate
    // downstream cancellation/deadline (context.Canceled /
    // context.DeadlineExceeded): cancellation must terminate the request and
    // must not trigger degraded mode or emergency admission. Redis dependency
    // failures never surface here; the distributed wrapper handles them and
    // returns a normal decision from the emergency limiter.
    Admit(ctx context.Context, keyID, projectID string) (Decision, error)
}
```

- `ratelimit.Registry.Admit` gains `ctx`, checks `ctx.Err()` first (no token
  consumption on cancellation), and returns `(Decision, nil)`.
- A new `internal/ratelimit/distributed` implementation owns the Redis client,
  the Lua admission, the degraded state machine, and the emergency registry.
  `cmd/gateway` selects local vs distributed by configuration.
- The HTTP handler and `dataplane.service.admit` are unchanged in shape;
  `Options.RateLimiter` widens to the interface. Concurrency caps (general/
  stream slots) stay process-local by nature and are out of scope for Redis.

### D2. Algorithm: continuous token bucket, Redis TIME, integer fixed-point

- Continuous token bucket (same policy contract as ADR-017 D6: N requests/
  minute sustained, burst N), **not** INCR window counters, which cannot
  express the refill semantics.
- **Redis server `TIME` is the authoritative limiter clock**, read once inside
  the Lua execution so all scopes share one snapshot. The gateway application
  clock is never used for limiter state (replica clock skew/jump must not
  enter shared state).
- **Integer fixed-point only**; Lua arithmetic is NOT Go int64, so every
  integer value that participates in Lua arithmetic must stay within the
  exact-integer representable range. Units: `1 token = 60000 units`;
  `capacity = RPM * 60000`; `refill = N units/ms`; request cost 60000 units;
  `retry_after_ms = ceil((60000 - tokens) / N)`. `elapsed_ms` is clamped to
  `<= 60000` so refill `<= capacity`; config validation bounds max RPM /
  capacity / intermediates by the Lua exact-integer safe range (not by
  `math.MaxInt64`). Tests: max accepted RPM boundary, one-above rejected at
  config load, no precision drift on large elapsed, integer retry-after ceil,
  no negative/over-cap tokens.

### D3. Monotonic limiter logical time (never moves backwards)

Redis `TIME` is authoritative wall clock, but the stored limiter timestamp must
never move backwards (e.g. NTP step-back):

```
effective_now_ms = max(redis_now_ms, stored_last_ms)
elapsed_ms       = min(effective_now_ms - stored_last_ms, 60000)
new_last_ms      = effective_now_ms
```

`elapsed_ms` is additionally never negative. State loss (server restart)
equals key expiry/rebuild, which is fine because keys are reproducible.

Mandatory Slice B acceptance requirement (not yet verified - this ADR commit
is docs-only): during Slice B implementation, pin the Redis image/version and
add an integration test proving that Redis `TIME` and writes inside the
mutating Lua script behave as assumed on that exact version. Slice B is not
complete until this verification passes.

### D4. Composite two-scope atomic Lua; rejection is token-charge atomic, not storage-write-free

One atomic Lua script, invoked with both scope keys, performs one admission
decision:

1. single `TIME` snapshot; compute refilled state for **every enabled scope**
   (key and project);
2. compute `retry_after_ms` for every deficient scope;
3. if any scope is deficient: **no scope loses request-cost tokens, no partial
   mutation**; `RetryAfter = max(all blocking delays)` and `BlockingScope` is
   the scope with the max delay;
4. **reject still materializes the refilled state / logical timestamp and
   refreshes participating keys' TTL** (`PEXPIRE`), mirroring ADR-017's
   `lastSeen` refresh on every admission including rejected ones - a
   continuously rejected hot key must never idle-expire into a fresh full
   bucket. Test: continuously rejected key for longer than the idle TTL must
   NOT reset to full burst;
5. only when all enabled scopes allow is the request cost committed atomically
   (deduct both scopes + refresh TTL).

Redis single-threaded script execution makes the composite decision atomic the
way the ADR-017 registry write lock did in-process.

### D5. Key namespace, TTL, bounded state

- Namespace with a shared project hash tag so a future Redis Cluster move
  keeps both Lua `KEYS` in one hash slot without a namespace migration:
  `gwrl:v1:{projectUUID}:vk:<virtualKeyUUID>` and
  `gwrl:v1:{projectUUID}:project`. Virtual keys belong to projects, and the
  key scope and the request's project scope share the same project UUID.
- Keys hold only internal UUIDs - never a raw virtual key, its digest, its
  prefix, or any credential material.
- Every write (allow and reject) refreshes `PEXPIRE = RATE_LIMITER_IDLE_TTL`
  (default 10m). Idle state expires. TTL bounds the lifetime of idle/stale
  Redis limiter state; it is NOT a hard cardinality cap equivalent to
  `Registry.EntryCap`. Live Redis limiter cardinality is proportional to
  recently active valid virtual-key/project scopes within the TTL window.
  Week 9 does not implement a Redis-side EntryCap. Per-key state is O(1).
- **Week 9 scope: standalone Redis only.** Redis Cluster is a future reopen
  trigger, noted so the shared-hash-tag namespace is chosen now to avoid a
  migration later.

### D6. Degraded mode: entry on the FIRST dependency failure, no entry hysteresis

Concurrency contract (no over-promise about in-flight requests):

- The first Redis admission dependency error/timeout/ambiguous outcome
  **immediately transitions the replica to degraded** (no "N consecutive
  failures" threshold).
- Once degraded is observable, **no NEW admission may begin a Redis
  distributed attempt**; newly starting admissions use only the emergency
  limiter until recovery qualification succeeds.
- Redis attempts that were **already in flight before the transition may
  complete**; their results are handled per the documented policy (success of
  an in-flight attempt does not cancel the degraded state - the transition
  already happened).
- This bounded transition overlap (an in-flight Redis attempt finishing after
  the replica is degraded) is accepted and explicitly documented. This slice
  does not design an epoch/transaction mechanism to eliminate all in-flight
  overlap; the safety guarantee is unchanged:
  **no unlimited fail-open + bounded local emergency admission + no exact
  global quota guarantee during dependency failure/partition**.
- Degraded mode prevents indefinite request-by-request alternation between
  Redis and emergency admission sources.
- The request that triggered the transition is itself served by the emergency
  limiter (single admission source per request).
- Recovery may use hysteresis (see D9), entry never does.

### D7. Emergency limiter: bounded fallback, NOT an exact global quota

Emergency limiter = a local `ratelimit.Registry` (same package, same
EntryCap/TTL semantics) with a stricter derived configuration:
`emergencyRPM = max(1, floor(normalRPM / replicaFactor))` per enabled scope,
where `replicaFactor` is a deployment-time **conservative expected/max replica
count** (default 2, the reference topology). Both degraded scopes stay
composite.

Safety contract (no stronger claim):

- never unlimited fail-open;
- every degraded replica has a stricter bounded local limiter;
- outage / partial partition provider-spend risk is bounded per replica by its
  emergency limit;
- **no exact global quota is guaranteed while Redis is unreachable or under
  partial partition** (e.g. one replica normal at N + one degraded at N/2 can
  exceed N cluster-wide) - with Redis unreachable there is no shared
  coordination source, so this trade-off is explicit and accepted.

### D8. Cancellation != Redis dependency failure

- Downstream `context.Canceled`/`DeadlineExceeded` propagates as the `Admit`
  error: the request terminates, no degraded transition, no emergency
  admission, and no provider/durable work follows (existing lifecycle ordering:
  admission precedes `CreateGatewayRequest`).
- Dependency failure = parent context still live while the Redis command
  errors/times out (short `REDIS_COMMAND_TIMEOUT`) or its outcome is
  ambiguous: enter degraded, serve the current request from the emergency
  limiter.
- **Ambiguous outcomes are never retransmitted**: a mutating admission script
  that may have executed must not be replayed on unknown results. Only
  provably pre-execution paths (dial failure before write, EVALSHA NOSCRIPT
  reload) are safe to retry, analyzed separately from command retry. This
  allows conservative double-charge (Redis may have charged + emergency
  charges) bounded by the emergency limit; it never allows unlimited
  admission.
- Mandatory Slice B acceptance requirement (not yet verified - this ADR
  commit is docs-only): go-redis retry behavior (command retry, dial retry,
  `Script.Run` EVALSHA/NOSCRIPT, timeout/context semantics) must be verified
  against the pinned go-redis version during Slice B implementation before the
  mutating admission path is considered safe - not assumed from a config
  field. Core invariant (unchanged): **a mutating admission operation whose
  execution outcome is ambiguous must never be automatically
  retransmitted.**
- Client cancellation after a successful Redis admission keeps the charge (no
  rollback; the key remains until TTL); subsequent provider/durable work stops
  per the existing cancellation contract.

### D9. Recovery and the degraded->normal transition

- While degraded, a background **non-mutating probe** checks liveness; probes
  never consume user quota (a probe is not an admission).
- After `K` consecutive successful probes the state moves to `recovering`
  (admissions still via the emergency limiter).
- In `recovering`, the next real request performs one genuine distributed
  admission to validate the real Lua write path before returning to normal.
  **The state mutex protects only state transitions / counters /
  recoveryAttemptInFlight ownership. It is never held across Redis network
  I/O.** A recovering request: (1) acquires the lock; (2) claims the single
  recovery-attempt flag; (3) releases the lock; (4) performs the real Redis
  admission; (5) reacquires the lock; (6) commits the normal/degraded
  transition; (7) releases the lock. If the attempt fails, the state stays
  degraded and that request is served by the emergency limiter. Other
  concurrent requests are not blocked by the Redis round-trip: while the
  recovery attempt is in flight they continue to use the emergency limiter
  (no new distributed admission may start - see D6).
- No unlimited window on recovery: during degraded/recovering every admission
  is emergency-bounded; keys carry TTL (10m) far longer than the recovery hold
  time, so most keys retain near-current counts. Keys that expired during a
  long outage are recreated with a full burst - the same documented bounded
  over-admission window as ADR-017 cap eviction (at most one burst per affected
  key per outage). No cross-replica state resync is attempted.

### D10. Configuration posture and client lifecycle

- Default: unchanged local-only path (`RateLimiter` nil/local); distributed
  mode and Redis configuration opt in via `.env.example`/`internal/config`
  additions (REDIS_URL, command timeout, degraded/recovery thresholds,
  emergency factor, idle TTL).
- One long-lived go-redis client per process (never per request), configured
  pool and short command timeout, `Close()` on process shutdown, wired in
  `cmd/gateway`.
- Redis is never the source of truth for projects, credentials, pricing, or
  durable usage; losing Redis degrades to the emergency limiter and never
  erases durable data.

### D11. Test strategy (integration matrix summary)

- Deterministic algorithm unit tests (time injected; no production TIME
  semantic changes); Redis integration tests avoid refill-boundary flake.
- Test cleanup: unique test/run key namespace; only `DEL`/`UNLINK`/`SCAN` own
  keys; **no unconditional `FLUSHDB`** unless the test itself started a
  dedicated ephemeral Redis with misuse protection.
- Matrix rows: single-instance Redis limiter; two replicas sharing Redis
  (Slice A topology re-run: cluster total <= limit); per-key isolation;
  per-project isolation; key+project composite; concurrent admission
  atomicity; Redis unavailable (immediate degraded); Redis timeout
  (dependency vs cancellation); Redis recovery (probe -> recovering -> first
  real distributed admission); emergency limiter bound while degraded;
  rejected hot key never resets to full burst after > idle TTL; key
  expiration/bounded state; cancellation A/B/C cases (pre-execution cancel =
  no state, ambiguous = no retransmit + degraded, post-success cancel =
  charge retained); **transition race: concurrent Redis admissions + one
  dependency failure - the replica transitions to degraded exactly once, no
  new Redis admission starts after the transition, already-in-flight attempts
  may finish per the documented policy, no data race/deadlock, and subsequent
  requests consistently use emergency**; race tests.

## Failure policy summary (degraded dependency)

Per Tech Design §16 Redis Failure Policy: on Redis failure/timeout the
distributed check fails fast, a degraded event is emitted (bounded slog;
metrics land in Week 10), the process-local emergency limiter takes over, and
requests continue within the emergency bound. There is no unlimited fail-open
and no hard total fail-closed. Redis is not a readiness dependency when the
local degraded limiter is available.

## Migration / rollback

No schema change. Rollback per commit slice: distributed mode is opt-in via
config; removing the Redis configuration restores the local-only path with no
code change. The pinned Redis image/version and the go-redis dependency are
gate-approved additions recorded at Slice B implementation time.

## Reopen triggers

- Redis Cluster support (then confirm the shared hash-tag namespace still
  colocates both Lua keys).
- Evidence that Redis TIME + writes inside the mutating Lua behave differently
  on the pinned Redis version than the integration test proves.
- Benchmark/load evidence (Week 11) that the per-request Redis hop needs
  revisit.
- A controlled provider-failure experiment showing a circuit breaker would
  help (ADR-017 D10 trigger, unchanged).
