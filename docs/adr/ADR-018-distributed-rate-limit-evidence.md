# ADR-018: Distributed rate limiting evidence - two-replica inconsistency experiment (Week 9, Slice A)

- Status: Draft
- Date: 2026-09-05
- Related: Tech Design §16 (Rate Limiting Evolution, Phase B), §30.4 (Redis Integration); Week 9 roadmap entry; Week 8 ADR-017 (single-instance reliability baseline); packages `internal/ratelimit`, `internal/store/postgres` (integration experiment); MEMORY.md "Slice B mandatory invariants".

## Status note

This ADR is **Draft and its Decision section is explicitly pending**. It exists
because the Week 9 Slice A experiment has produced its evidence. It does NOT
record "Redis is the conclusion": the Evidence/Decision Gate review happens
first, and only after the project owner accepts this evidence does Slice B
start (Redis-backed distributed limiter design, implementation, and failure
policy). No Redis code, dependency, Compose service, or configuration exists in
this slice.

## Context

Tech Design §16 says rate limiting must start in-process (Phase A, shipped as
Week 8 ADR-017) and that Redis may only be introduced **after the distributed
problem is demonstrated** (Phase B): two gateway replicas behind the reverse
proxy, each with its own in-memory limiter, allowing the cluster to exceed the
configured per-key limit even though every replica individually stays under it.

Week 9 direction approval (2026-09-05) requires that demonstration to be a
test-only, HTTP / reverse-proxy level experiment that does not touch production
code/config, `.env*`, Compose, dependencies, or the public API, and does not
pre-implement Redis.

## Experiment method

### Topology

```
test-only client (real HTTP requests, same virtual key)
        |
        v
test-only round-robin reverse proxy (stdlib httptest)
        |                    |
        v                    v
    Gateway A             Gateway B      (complete dataplane.NewHandler + NewService HTTP stacks)
        |                    |
        v                    v
independent ratelimit.Registry   independent ratelimit.Registry   (per-process state)
        |                    |
        +---- shared real PostgreSQL (durable gateway_requests rows, key/credential/config)
              +---- shared deterministic mock provider (single httptest OpenAI-compatible endpoint)
```

Two complete gateway replicas (Service + Handler + HTTP server each) share one
real PostgreSQL schema and one mock provider endpoint; only the rate-limit
quota state is duplicated, exactly the production situation Redis would fix.
The real OpenAI adapter is used end to end; the provider config row's
`base_url_override` column points at the shared mock endpoint (test-only, via
the store query layer - no public API and no production base-URL change).

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

## Evidence

Commands: `go test -tags=integration ./internal/store/postgres/ -run 'TestReplicaRateLimit' -v` (also run inside `make integration`).

### Distributed case - TestReplicaRateLimitInconsistencyHTTPLevel

```
limit = 20 per key (cluster intended limit)
replica A admitted = 12 (HTTP 200), rejected = 0
replica B admitted = 12 (HTTP 200), rejected = 0
total HTTP 200 = 24, total HTTP 429 = 0
mock provider calls = 24 > intended cluster limit 20
durable gateway_requests rows = 24
```

Both replicas individually stay under the limit (12 <= 20) and reject nothing;
the cluster executes 24 requests against an intended limit of 20. The
per-virtual-key quota is effectively doubled because the state lives in two
independent in-memory registries.

### Single-replica control - TestReplicaRateLimitSingleReplicaControl

The same client load against ONE replica with fresh limiter state:

```
HTTP 200 = exactly 20, HTTP 429 = exactly 4
mock provider calls = exactly 20
durable gateway_requests rows = 20
```

The same gateway machinery does enforce the limit when quota state is not
duplicated. Rejected requests produce no provider call and no durable row
(consistent with ADR-017 D8).

### Fast regression (normal suite)

`internal/ratelimit/replica_inconsistency_test.go` locks the same root cause at
registry level under the frozen clock (two registries admit 24 across them,
control registry admits 20 of 24); runs in `make test` / `make race` with no
PostgreSQL dependency.

## Decision (PENDING - gated on owner review of the evidence above)

This section is intentionally not filled in. Once the project owner accepts the
Slice A evidence and opens the gate, this ADR will be extended (before any
Slice B code) with:

1. The go/no-go outcome and the scope of Slice B (Redis distributed limiter +
   degraded local emergency limiter + Redis failure tests).
2. The Redis design and failure-policy decisions recorded in Slice B (limiter
   interface, Lua composite admission, Redis TIME as the authoritative limiter
   clock, integer fixed-point accounting, key namespace, degraded state
   machine, emergency limiter derivation, recovery, and the standalone-Redis
   scope note).
3. The three mandatory Slice B implementation invariants agreed at direction
   approval time (also recorded in MEMORY.md):
   - limiter logical time must never move backwards
     (`effective_now_ms = max(redis_now_ms, stored_last_ms)`); Redis image/
     version pinned at gate time with an integration test proving `TIME` +
     writes inside the mutating Lua on that version;
   - rejection is token-charge atomic, not storage-write-free: reject must
     still materialize refilled state and refresh participating keys' TTL so a
     continuously rejected hot key never expires into a fresh full bucket
     (test: rejected hot key for longer than the idle TTL must NOT reset to a
     full burst);
   - Lua arithmetic is not Go int64: fixed-point values and intermediates must
     stay within the exact-integer representable range (elapsed clamped so
     refill <= capacity = RPM*60000); config validation bounds RPM/capacity by
     the Lua exact-integer range, with boundary tests (max accepted RPM,
     one-above rejected at config load, no precision drift on large elapsed,
     integer retry-after ceil, no negative/over-cap tokens).
4. When Slice B lands, this ADR becomes Accepted with
   Context -> Evidence -> Decision -> Redis design -> failure policy.
