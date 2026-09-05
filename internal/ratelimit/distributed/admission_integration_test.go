//go:build integration

package distributed

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
)

// Admission matrix for the composite Redis Lua algorithm (ADR-018 D2-D5 and
// D11). White-box scenarios set stored tokens/ts directly to make refill,
// monotonic-clock, clamp, and TTL behavior deterministic against the live
// pinned Redis rather than depending on wall-clock sleep.

func cfgWith(keyRPM, projectRPM int) Config {
	return Config{KeyRPM: keyRPM, ProjectRPM: projectRPM, IdleTTL: time.Minute, CommandTimeout: 3 * time.Second}
}

// admitResult runs one admission and cleans up the two scope keys afterwards.
func admitResult(t *testing.T, limiter *Core, projectID, keyID string) ratelimit.Decision {
	t.Helper()
	ctx := context.Background()
	decision, err := limiter.AdmitCore(ctx, keyID, projectID)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = limiter.client.Del(cleanupCtx, ScopeKey(projectID, keyID), ProjectScopeKey(projectID)).Err()
	})
	return decision
}

func TestAdmitSingleScopeBurstThenReject(t *testing.T) {
	limiter, client := newTestLimiter(t, cfgWith(3, 0))
	projectID, keyID := uniqueIDs(t)
	for i := 0; i < 3; i++ {
		if decision := admitResult(t, limiter, projectID, keyID); !decision.Allowed {
			t.Fatalf("request %d rejected before burst exhausted", i)
		}
	}
	rejected := admitResult(t, limiter, projectID, keyID)
	if rejected.Allowed {
		t.Fatal("4th request allowed despite burst 3")
	}
	if rejected.BlockingScope != ratelimit.ScopeVirtualKey {
		t.Fatalf("blocking scope = %q, want virtual_key", rejected.BlockingScope)
	}
	// Reject deducts zero request-cost tokens: remaining tokens must be the
	// tiny elapsed refill only, never negative and never >= a full token.
	ctx := context.Background()
	tokens, _, present := readScopeState(t, ctx, client, ScopeKey(projectID, keyID))
	if !present {
		t.Fatal("key scope missing after reject")
	}
	if tokens < 0 || tokens >= UnitsPerToken {
		t.Fatalf("post-reject tokens = %d, want 0 <= tokens < %d", tokens, UnitsPerToken)
	}
	if scopeTTL(t, ctx, client, ScopeKey(projectID, keyID)) <= 0 {
		t.Fatal("key scope TTL missing after reject (reject must refresh TTL)")
	}
}

func TestCompositeAllOrNoneNoPartialCharge(t *testing.T) {
	limiter, client := newTestLimiter(t, cfgWith(5, 1))
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()

	// Empty the project scope (capacity == 1 token at rpm 1) deterministically.
	setScopeState(t, ctx, client, ProjectScopeKey(projectID), 0, serverNowMS(t, ctx, client))

	decision := admitResult(t, limiter, projectID, keyID)
	if decision.Allowed {
		t.Fatal("composite admitted with an empty project scope")
	}
	if decision.BlockingScope != ratelimit.ScopeProject {
		t.Fatalf("blocking scope = %q, want project", decision.BlockingScope)
	}
	if decision.RetryAfter <= 0 {
		t.Fatal("project rejection carried no Retry-After")
	}
	// The key scope was created full (5 tokens) during the same atomic script
	// and must NOT have been charged: tokens stay at exactly capacity.
	tokens, _, present := readScopeState(t, ctx, client, ScopeKey(projectID, keyID))
	if !present {
		t.Fatal("key scope missing")
	}
	if want := int64(5) * UnitsPerToken; tokens != want {
		t.Fatalf("key tokens after rejected composite = %d, want %d (no partial charge)", tokens, want)
	}

	// Prove it operationally: the same key against a key-only limiter still has
	// its full burst available.
	keyOnly, _ := newTestLimiter(t, cfgWith(5, 0))
	for i := 0; i < 5; i++ {
		if d := admitResult(t, keyOnly, projectID, keyID); !d.Allowed {
			t.Fatalf("key-only request %d rejected after no-partial-charge check", i)
		}
	}
}

func TestCompositeRetryAfterMaxAndBlockingScope(t *testing.T) {
	limiter, client := newTestLimiter(t, cfgWith(60, 20))
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()
	now := serverNowMS(t, ctx, client)
	// Both scopes empty: key rpm 60 -> ~1000ms wait; project rpm 20 -> ~3000ms.
	setScopeState(t, ctx, client, ScopeKey(projectID, keyID), 0, now)
	setScopeState(t, ctx, client, ProjectScopeKey(projectID), 0, now)

	decision := admitResult(t, limiter, projectID, keyID)
	if decision.Allowed {
		t.Fatal("composite admitted with two empty scopes")
	}
	if decision.BlockingScope != ratelimit.ScopeProject {
		t.Fatalf("blocking scope = %q, want project (max retry-after)", decision.BlockingScope)
	}
	// Retry-After must be the maximum blocking delay (~3000ms for rpm 20),
	// allowing only for the small refill accrued during the test.
	if decision.RetryAfter < 2800*time.Millisecond || decision.RetryAfter > 3050*time.Millisecond {
		t.Fatalf("retry-after = %v, want ~3s (max of key ~1s / project ~3s)", decision.RetryAfter)
	}
}

func TestRefillUsesStoredElapsedTime(t *testing.T) {
	limiter, client := newTestLimiter(t, cfgWith(60, 0))
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()

	// Drain the full bucket (rpm 60 -> 60 tokens) so tokens == 0.
	for i := 0; i < 60; i++ {
		if d := admitResult(t, limiter, projectID, keyID); !d.Allowed {
			t.Fatalf("drain request %d rejected", i)
		}
	}
	tokens, _, _ := readScopeState(t, ctx, client, ScopeKey(projectID, keyID))
	if tokens >= UnitsPerToken {
		t.Fatalf("bucket not drained: tokens = %d", tokens)
	}

	// Simulate 5 seconds of elapsed time by moving the stored timestamp back
	// 5000ms relative to the server clock: refill = 5000ms * 60 units/ms =
	// 300000 units = 5 tokens, so the next request must be allowed.
	now := serverNowMS(t, ctx, client)
	setScopeState(t, ctx, client, ScopeKey(projectID, keyID), tokens, now-5000)
	if d := admitResult(t, limiter, projectID, keyID); !d.Allowed {
		t.Fatal("request after 5s of refill was rejected")
	}
}

func TestMonotonicClockNeverMovesBackward(t *testing.T) {
	limiter, client := newTestLimiter(t, cfgWith(60, 0))
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()

	// Put the stored clock 20s in the future with an empty bucket: while the
	// server clock is behind, elapsed must be 0 (no refill), the request must
	// be rejected, and the stored timestamp must never decrease.
	now := serverNowMS(t, ctx, client)
	future := now + 20000
	setScopeState(t, ctx, client, ScopeKey(projectID, keyID), 0, future)

	decision := admitResult(t, limiter, projectID, keyID)
	if decision.Allowed {
		t.Fatal("empty bucket with a future timestamp was allowed without refill")
	}
	_, ts, present := readScopeState(t, ctx, client, ScopeKey(projectID, keyID))
	if !present {
		t.Fatal("key scope missing")
	}
	if ts < future {
		t.Fatalf("stored timestamp moved backward: %d < %d", ts, future)
	}
	// Retry is computed from the real (unrefilled) deficit: ~1000ms at rpm 60.
	if decision.RetryAfter < 900*time.Millisecond || decision.RetryAfter > 1100*time.Millisecond {
		t.Fatalf("retry-after = %v, want ~1s despite server clock being behind", decision.RetryAfter)
	}
}

func TestRejectedHotKeyNeverResetsBeforeIdleTTL(t *testing.T) {
	cfg := Config{KeyRPM: 3, IdleTTL: 200 * time.Millisecond, CommandTimeout: 3 * time.Second}
	limiter, client := newTestLimiter(t, cfg)
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if d := admitResult(t, limiter, projectID, keyID); !d.Allowed {
			t.Fatalf("drain request %d rejected", i)
		}
	}
	// Keep rejecting well past the idle TTL. If rejections did not refresh the
	// TTL, the key would expire ~200ms in and reset to a full burst.
	for i := 0; i < 8; i++ {
		if d := admitResult(t, limiter, projectID, keyID); d.Allowed {
			t.Fatalf("hot rejected key reset to full burst on iteration %d (TTL not refreshed)", i)
		}
		time.Sleep(60 * time.Millisecond)
	}
	// A genuine idle period past the TTL must expire the key...
	time.Sleep(400 * time.Millisecond)
	ttl := scopeTTL(t, ctx, client, ScopeKey(projectID, keyID))
	if ttl > 0 {
		t.Fatalf("key survived genuine idle beyond TTL (ttl = %v)", ttl)
	}
	// ...and the next request then recreates a fresh full bucket (bounded
	// burst reset after real idle expiry, mirroring ADR-017 eviction semantics).
	if d := admitResult(t, limiter, projectID, keyID); !d.Allowed {
		t.Fatal("request after genuine idle expiry was rejected")
	}
}

func TestLargeElapsedClampExactNoDrift(t *testing.T) {
	limiter, client := newTestLimiter(t, cfgWith(7, 0))
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()

	// Two hours of stored elapsed with an empty bucket: the refill must clamp
	// to 60000ms exactly -> capacity = 60000 * 7 = 420000 units. No float drift
	// is allowed: after one allow the stored tokens must be exactly
	// capacity - 60000.
	now := serverNowMS(t, ctx, client)
	setScopeState(t, ctx, client, ScopeKey(projectID, keyID), 0, now-2*60*60*1000)
	if d := admitResult(t, limiter, projectID, keyID); !d.Allowed {
		t.Fatal("request after a two-hour elapsed clamp was rejected")
	}
	tokens, _, present := readScopeState(t, ctx, client, ScopeKey(projectID, keyID))
	if !present {
		t.Fatal("key scope missing")
	}
	if want := int64(7)*UnitsPerToken - UnitsPerToken; tokens != want {
		t.Fatalf("tokens after clamp allow = %d, want exactly %d", tokens, want)
	}
	if tokens < 0 {
		t.Fatal("negative tokens after clamp")
	}
}

func TestPerKeyAndPerProjectIsolation(t *testing.T) {
	// Key-level isolation: two virtual keys in one project have independent
	// buckets when only the virtual-key scope is enabled.
	keyLimiter, _ := newTestLimiter(t, cfgWith(2, 0))
	projectID, keyA := uniqueIDs(t)
	_, keyB := uniqueIDs(t)
	for i := 0; i < 2; i++ {
		if d := admitResult(t, keyLimiter, projectID, keyA); !d.Allowed {
			t.Fatalf("key A request %d rejected", i)
		}
	}
	if d := admitResult(t, keyLimiter, projectID, keyA); d.Allowed {
		t.Fatal("key A admitted past its burst")
	}
	for i := 0; i < 2; i++ {
		if d := admitResult(t, keyLimiter, projectID, keyB); !d.Allowed {
			t.Fatalf("key B request %d rejected despite independent bucket", i)
		}
	}

	// Project-level isolation: two projects have independent project-scope
	// buckets when only the project scope is enabled.
	projectLimiter, _ := newTestLimiter(t, cfgWith(0, 2))
	projOne, key := uniqueIDs(t)
	projTwo, _ := uniqueIDs(t)
	for i := 0; i < 2; i++ {
		if d := admitResult(t, projectLimiter, projOne, key); !d.Allowed {
			t.Fatalf("project one request %d rejected", i)
		}
	}
	if d := admitResult(t, projectLimiter, projOne, key); d.Allowed {
		t.Fatal("project one admitted past its burst")
	}
	for i := 0; i < 2; i++ {
		if d := admitResult(t, projectLimiter, projTwo, key); !d.Allowed {
			t.Fatalf("project two request %d rejected despite independent bucket", i)
		}
	}
}

func TestConcurrentAdmissionsAtomicExactlyBurst(t *testing.T) {
	limiter, client := newTestLimiter(t, cfgWith(10, 0))
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()

	const attempts = 30
	allowed := make([]bool, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			decision, err := limiter.AdmitCore(ctx, keyID, projectID)
			if err != nil {
				t.Errorf("concurrent admit: %v", err)
				return
			}
			allowed[i] = decision.Allowed
		}(i)
	}
	wg.Wait()

	countAllowed := 0
	for _, ok := range allowed {
		if ok {
			countAllowed++
		}
	}
	if countAllowed != 10 {
		t.Fatalf("concurrent allowed = %d, want exactly 10 (burst)", countAllowed)
	}
	// The bucket may have accrued only the tiny elapsed refill: tokens must
	// stay >= 0 and below one full token (no negative / over-cap drift).
	tokens, _, present := readScopeState(t, ctx, client, ScopeKey(projectID, keyID))
	if !present {
		t.Fatal("key scope missing")
	}
	if tokens < 0 || tokens >= UnitsPerToken {
		t.Fatalf("post-concurrency tokens = %d, want 0 <= tokens < %d", tokens, UnitsPerToken)
	}
}

// TestMaxSafeRPMLiveStoresExactDecimalState (P1-1) exercises the largest
// accepted RPM on the pinned Redis. Lua tostring would corrupt this value into
// scientific notation (9.00719925474e+15); passing the integer Lua number
// directly must store an exact base-10 integer the Go side can parse and
// compare exactly.
func TestMaxSafeRPMLiveStoresExactDecimalState(t *testing.T) {
	cfg := Config{KeyRPM: MaxSafeRPM(), IdleTTL: time.Minute, CommandTimeout: 5 * time.Second}
	core, client := newTestLimiter(t, cfg)
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()

	if decision := admitResult(t, core, projectID, keyID); !decision.Allowed {
		t.Fatalf("first admission at max safe RPM was rejected")
	}
	want := int64(MaxSafeRPM())*UnitsPerToken - UnitsPerToken
	tokens, ts, present := readScopeState(t, ctx, client, ScopeKey(projectID, keyID))
	if !present {
		t.Fatal("key scope missing after max-RPM admission")
	}
	if tokens != want {
		t.Fatalf("stored tokens = %d, want exactly %d (base-10 parse, no scientific drift)", tokens, want)
	}
	if ts <= 0 {
		t.Fatalf("stored timestamp not a valid positive integer: %d", ts)
	}
	// The high-value path keeps working (no HINCRBY integer error): the bucket
	// still has capacity far above one token.
	if decision := admitResult(t, core, projectID, keyID); !decision.Allowed {
		t.Fatal("second admission at max safe RPM was rejected")
	}
}

// TestRetryAfterCeilingKeepsRemainder (P2-4) pins the integer ceiling: a
// deficit of 40 units at rpm 7 needs ceil(40/7) = 6ms, NOT floor = 5ms. The
// stored timestamp is placed slightly in the future so the monotonic clamp
// freezes refill at exactly 0 and the deficit stays deterministic.
func TestRetryAfterCeilingKeepsRemainder(t *testing.T) {
	core, client := newTestLimiter(t, cfgWith(7, 0))
	projectID, keyID := uniqueIDs(t)
	ctx := context.Background()

	now := serverNowMS(t, ctx, client)
	const deficit = 40
	setScopeState(t, ctx, client, ScopeKey(projectID, keyID), UnitsPerToken-deficit, now+60000)

	decision := admitResult(t, core, projectID, keyID)
	if decision.Allowed {
		t.Fatal("deficient bucket was allowed")
	}
	if want := 6 * time.Millisecond; decision.RetryAfter != want {
		t.Fatalf("retry-after = %v, want exactly %v (ceil(40/7)=6, not floor 5)", decision.RetryAfter, want)
	}
	if decision.BlockingScope != ratelimit.ScopeVirtualKey {
		t.Fatalf("blocking scope = %q, want virtual_key", decision.BlockingScope)
	}
}
