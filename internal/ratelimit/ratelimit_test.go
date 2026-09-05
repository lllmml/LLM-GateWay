package ratelimit

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

const (
	testKeyID     = "key-11111111-1111-4111-8111-111111111111"
	testOtherKey  = "key-22222222-2222-4222-8222-222222222222"
	testThirdKey  = "key-33333333-3333-4333-8333-333333333333"
	testProjID    = "proj-11111111-1111-4111-8111-111111111111"
	testOtherProj = "proj-22222222-2222-4222-8222-222222222222"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(at time.Time) *fakeClock {
	return &fakeClock{t: at}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(duration)
}

func testRegistry(t *testing.T, cfg Config, clock *fakeClock) *Registry {
	t.Helper()
	cfg.Now = clock.now
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(registry.Close)
	return registry
}

func testConfig() Config {
	return Config{
		EntryCap:      100,
		IdleTTL:       10 * time.Minute,
		SweepInterval: time.Minute,
	}
}

// nearTokens compares x/time/rate float token counters with tolerance for
// floating point drift.
func nearTokens(got, want float64) bool {
	return math.Abs(got-want) < 1e-6
}

func TestAdmitsUpToBurstThenRejectsWithRetryAfter(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	// 2 requests/minute: burst 2, one token refilled every 30s.
	cfg := testConfig()
	cfg.KeyRPM = 2
	cfg.ProjectRPM = 0
	registry := testRegistry(t, cfg, clock)

	if decision := registry.Admit(testKeyID, testProjID); !decision.Allowed {
		t.Fatalf("first admission = %+v, want allowed", decision)
	}
	if decision := registry.Admit(testKeyID, testProjID); !decision.Allowed {
		t.Fatalf("second admission = %+v, want allowed (burst)", decision)
	}
	decision := registry.Admit(testKeyID, testProjID)
	if decision.Allowed {
		t.Fatal("third admission allowed, want rejected")
	}
	if decision.BlockingScope != ScopeVirtualKey {
		t.Fatalf("BlockingScope = %q, want %q", decision.BlockingScope, ScopeVirtualKey)
	}
	if decision.RetryAfter <= 0 || decision.RetryAfter > 31*time.Second {
		t.Fatalf("RetryAfter = %v, want roughly one refill interval (30s)", decision.RetryAfter)
	}

	clock.advance(30 * time.Second)
	if decision := registry.Admit(testKeyID, testProjID); !decision.Allowed {
		t.Fatalf("admission after refill = %+v, want allowed", decision)
	}
}

func TestDisabledScopesAdmitEverything(t *testing.T) {
	clock := newFakeClock(time.Now())
	registry := testRegistry(t, testConfig(), clock)

	for index := 0; index < 1000; index++ {
		if decision := registry.Admit(testKeyID, testProjID); !decision.Allowed {
			t.Fatalf("admission %d = %+v, want allowed with both scopes disabled", index, decision)
		}
	}
}

func TestProjectRejectDoesNotConsumeKeyQuota(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = 6     // burst 6
	cfg.ProjectRPM = 1 // burst 1, binding constraint
	registry := testRegistry(t, cfg, clock)

	if decision := registry.Admit(testKeyID, testProjID); !decision.Allowed {
		t.Fatalf("first admission = %+v, want allowed", decision)
	}

	keyBefore := registry.limiterTokens(ScopeVirtualKey, testKeyID)
	projectBefore := registry.limiterTokens(ScopeProject, testProjID)

	// Second admission is rejected by the empty project scope. Its key-limb
	// reservation must have been cancelled: the key bucket keeps the token.
	decision := registry.Admit(testKeyID, testProjID)
	if decision.Allowed {
		t.Fatal("second admission allowed, want project-scope rejection")
	}
	if decision.BlockingScope != ScopeProject {
		t.Fatalf("BlockingScope = %q, want %q", decision.BlockingScope, ScopeProject)
	}
	if decision.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want positive", decision.RetryAfter)
	}
	if got := registry.limiterTokens(ScopeVirtualKey, testKeyID); !nearTokens(got, keyBefore) {
		t.Fatalf("key tokens after rejected admission = %v, want unchanged %v", got, keyBefore)
	}
	if got := registry.limiterTokens(ScopeProject, testProjID); !nearTokens(got, projectBefore) {
		t.Fatalf("project tokens after rejected admission = %v, want unchanged %v", got, projectBefore)
	}
}

func TestKeyRejectDoesNotConsumeProjectQuota(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = 1     // burst 1, binding constraint
	cfg.ProjectRPM = 6 // burst 6
	registry := testRegistry(t, cfg, clock)

	if decision := registry.Admit(testKeyID, testProjID); !decision.Allowed {
		t.Fatalf("first admission = %+v, want allowed", decision)
	}

	projectBefore := registry.limiterTokens(ScopeProject, testProjID)

	decision := registry.Admit(testKeyID, testProjID)
	if decision.Allowed {
		t.Fatal("second admission allowed, want key-scope rejection")
	}
	if decision.BlockingScope != ScopeVirtualKey {
		t.Fatalf("BlockingScope = %q, want %q", decision.BlockingScope, ScopeVirtualKey)
	}
	if got := registry.limiterTokens(ScopeProject, testProjID); !nearTokens(got, projectBefore) {
		t.Fatalf("project tokens after rejected admission = %v, want unchanged %v", got, projectBefore)
	}
}

func TestConcurrentAdmissionsLeakNothingAcrossScopes(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = 5     // burst 5
	cfg.ProjectRPM = 3 // burst 3: at most three admissions can ever succeed
	registry := testRegistry(t, cfg, clock)

	const goroutines = 50
	var wg sync.WaitGroup
	var allowed atomicCounter
	for index := 0; index < goroutines; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if registry.Admit(testKeyID, testProjID).Allowed {
				allowed.add(1)
			}
		}()
	}
	wg.Wait()

	// All decisions happened at the same frozen clock instant, so the outcome
	// is deterministic: exactly the project burst of 3 succeed, no rejected
	// admission leaks key or project tokens.
	if allowed.get() != 3 {
		t.Fatalf("allowed admissions = %d, want 3 (project burst)", allowed.get())
	}
	if got := registry.limiterTokens(ScopeProject, testProjID); !nearTokens(got, 0) {
		t.Fatalf("project tokens = %v, want 0", got)
	}
	if got := registry.limiterTokens(ScopeVirtualKey, testKeyID); !nearTokens(got, 2) {
		t.Fatalf("key tokens = %v, want 2 (5 consumed - 3 admitted)", got)
	}
}

func TestSweepRemovesIdleEntriesAndKeepsHotEntries(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = 10
	cfg.ProjectRPM = 0
	registry := testRegistry(t, cfg, clock)

	registry.Admit(testKeyID, testProjID)
	registry.Admit(testOtherKey, testOtherProj)

	// Still within IdleTTL: nothing is removed.
	clock.advance(5 * time.Minute)
	registry.sweep(clock.now())
	if registry.len() != 2 {
		t.Fatalf("entries after 5m = %d, want 2", registry.len())
	}

	// testKeyID is refreshed by another admission, testOtherKey stays idle.
	registry.Admit(testKeyID, testProjID)
	clock.advance(6 * time.Minute) // testOtherKey now idle 11m, testKeyID idle 6m
	registry.sweep(clock.now())
	if registry.len() != 1 {
		t.Fatalf("entries after sweep = %d, want 1", registry.len())
	}
	if registry.has(ScopeVirtualKey, testOtherKey) {
		t.Fatal("idle entry testOtherKey was not removed")
	}
	if !registry.has(ScopeVirtualKey, testKeyID) {
		t.Fatal("hot entry testKeyID was incorrectly removed")
	}
}

func TestEntryCapEvictsLongestIdle(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = 10
	cfg.ProjectRPM = 0
	cfg.EntryCap = 2
	registry := testRegistry(t, cfg, clock)

	registry.Admit(testKeyID, testProjID)
	clock.advance(time.Minute)
	registry.Admit(testOtherKey, testOtherProj)
	clock.advance(time.Minute)

	// Inserting a third key evicts the least-recently-used (testKeyID).
	registry.Admit(testThirdKey, "proj-3")
	if registry.len() != cfg.EntryCap {
		t.Fatalf("entries = %d, want cap %d", registry.len(), cfg.EntryCap)
	}
	if registry.has(ScopeVirtualKey, testKeyID) {
		t.Fatal("least-recently-used entry was not evicted")
	}
	if !registry.has(ScopeVirtualKey, testOtherKey) || !registry.has(ScopeVirtualKey, testThirdKey) {
		t.Fatal("newer entries were evicted instead of the oldest")
	}
}

func TestEntryCapMustFitEnabledScopes(t *testing.T) {
	tests := []struct {
		name       string
		keyRPM     float64
		projectRPM float64
		cap        int
		wantErr    bool
	}{
		{name: "single scope cap 1", keyRPM: 10, projectRPM: 0, cap: 1, wantErr: false},
		{name: "both scopes cap 1 impossible", keyRPM: 10, projectRPM: 10, cap: 1, wantErr: true},
		{name: "both scopes cap 2", keyRPM: 10, projectRPM: 10, cap: 2, wantErr: false},
		{name: "both scopes cap 0", keyRPM: 10, projectRPM: 10, cap: 0, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.KeyRPM = test.keyRPM
			cfg.ProjectRPM = test.projectRPM
			cfg.EntryCap = test.cap
			registry, err := NewRegistry(cfg)
			if registry != nil {
				registry.Close()
			}
			if test.wantErr && err == nil {
				t.Fatal("NewRegistry returned nil error, want cap validation error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
		})
	}
}

// TestCapEvictionNeverDetachesCurrentAdmissionLimb pins the required
// invariant: when a composite admission must evict to insert the project limb,
// it evicts the least-recently-used entry that is NOT the key limb it is about
// to charge. The key bucket therefore stays current (tokens decrease by one
// across the admission) instead of being detached and silently recreated as a
// full bucket.
func TestCapEvictionNeverDetachesCurrentAdmissionLimb(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = 5
	cfg.ProjectRPM = 5
	cfg.EntryCap = 2 // exactly enough for one key + one project pair
	registry := testRegistry(t, cfg, clock)

	// Two admissions fill the registry with {k1, p1}; k1 has 3 tokens left.
	registry.Admit(testKeyID, testProjID)
	registry.Admit(testKeyID, testProjID)
	if got := registry.limiterTokens(ScopeVirtualKey, testKeyID); !nearTokens(got, 3) {
		t.Fatalf("k1 tokens before third admission = %v, want 3", got)
	}

	// Third admission needs project p2, which forces an eviction. The only
	// non-protected candidate is p1 (k1 is protected as the key limb of this
	// very admission), so p1 is evicted and k1 stays current.
	decision := registry.Admit(testKeyID, testOtherProj)
	if !decision.Allowed {
		t.Fatalf("third admission = %+v, want allowed", decision)
	}
	if registry.len() != 2 {
		t.Fatalf("entries = %d, want 2", registry.len())
	}
	if !registry.has(ScopeVirtualKey, testKeyID) || !registry.has(ScopeProject, testOtherProj) {
		t.Fatalf("registry = %v keys, want k1 and p2 current", registry.scopeKeys())
	}
	if registry.has(ScopeProject, testProjID) {
		t.Fatal("p1 should have been evicted as the LRU victim")
	}
	// k1 was charged on its current entry: 3 - 1 = 2. A detached entry would
	// have left the registry without k1 (recreated later as a full burst).
	if got := registry.limiterTokens(ScopeVirtualKey, testKeyID); !nearTokens(got, 2) {
		t.Fatalf("k1 tokens = %v, want 2 (charged once on the current entry)", got)
	}
}

// TestNoSilentBurstResetWithoutEviction verifies quota persists under cap
// pressure until a documented LRU eviction actually resets that scope's entry.
func TestNoSilentBurstResetWithoutEviction(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	cfg := testConfig()
	cfg.KeyRPM = 2 // burst 2
	cfg.ProjectRPM = 0
	cfg.EntryCap = 2 // room for exactly two keys, so k1/k2 never churn
	registry := testRegistry(t, cfg, clock)

	// Deplete k1 and k2 fully.
	for _, key := range []string{testKeyID, testOtherKey} {
		if !registry.Admit(key, testProjID).Allowed || !registry.Admit(key, testProjID).Allowed {
			t.Fatalf("expected two allowed admissions for %s", key)
		}
		if registry.Admit(key, testProjID).Allowed {
			t.Fatalf("third admission for %s allowed, want denied", key)
		}
	}

	// Neither entry was evicted (no third key inserted), so the depleted
	// buckets persist: no silent fresh burst.
	if registry.Admit(testKeyID, testProjID).Allowed {
		t.Fatal("k1 regained a fresh burst without being evicted")
	}

	// Inserting a third key evicts the LRU (k1, oldest), which is the
	// documented reset path: k1's next admission is against a fresh bucket.
	registry.Admit(testThirdKey, "proj-3")
	if !registry.Admit(testKeyID, testProjID).Allowed {
		t.Fatal("k1 was evicted, so the documented reset must grant a fresh burst")
	}
	if registry.len() != 2 {
		t.Fatalf("entries = %d, want 2", registry.len())
	}
}

// TestConcurrentAdmitSweepEviction exercises the registry under -race: many
// admissions across distinct scopes run while the janitor-style sweep and cap
// eviction happen concurrently.
func TestConcurrentAdmitSweepEviction(t *testing.T) {
	clock := newFakeClock(time.Now())
	cfg := testConfig()
	cfg.KeyRPM = 20
	cfg.ProjectRPM = 10
	cfg.EntryCap = 4
	cfg.IdleTTL = 10 * time.Millisecond
	registry := testRegistry(t, cfg, clock)

	const admissions = 2000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for index := 0; index < admissions/10; index++ {
			select {
			case <-stop:
				return
			default:
				clock.advance(time.Millisecond)
				registry.sweep(clock.now())
			}
		}
	}()
	for index := 0; index < admissions; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", index%8)
			project := fmt.Sprintf("proj-%d", index%4)
			registry.Admit(key, project)
		}(index)
	}
	wg.Wait()
	close(stop)

	if registry.len() > cfg.EntryCap {
		t.Fatalf("entries = %d, want <= cap %d", registry.len(), cfg.EntryCap)
	}
}

// tickingClock returns a strictly increasing time on every read (one second
// per call under a mutex). It models a real monotonic clock: when admissions
// are serialized and each one snapshots the clock inside the serialization
// boundary, the timestamps handed to x/time/rate strictly increase.
type tickingClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tickingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// TestConcurrentAdmissionsKeepTimestampsSerialized guards the ordering
// invariant that a limiter's timestamps follow the registry serialization
// order. Burst 1 with a refill rate of one token per second means every
// admission must be allowed while snapshots increase strictly: each admission
// refills the single token from the next tick. If a waiter ever carried a
// stale pre-lock timestamp into ReserveN after another admission had advanced
// the limiter, that admission would see no refill and be denied, dropping the
// allowed count below the goroutine count.
func TestConcurrentAdmissionsKeepTimestampsSerialized(t *testing.T) {
	clock := &tickingClock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	cfg := testConfig()
	cfg.KeyRPM = 60 // 1 token/second, burst 1
	cfg.ProjectRPM = 0
	cfg.Now = clock.now
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	t.Cleanup(registry.Close)

	const goroutines = 200
	var wg sync.WaitGroup
	var allowed atomicCounter
	for index := 0; index < goroutines; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if registry.Admit(testKeyID, testProjID).Allowed {
				allowed.add(1)
			}
		}()
	}
	wg.Wait()

	if allowed.get() != goroutines {
		t.Fatalf("allowed admissions = %d, want %d (a stale timestamp would deny an admission)", allowed.get(), goroutines)
	}
}

func TestCloseIsIdempotentAndStopsJanitor(t *testing.T) {
	registry, err := NewRegistry(func() Config {
		cfg := testConfig()
		cfg.KeyRPM = 10
		return cfg
	}())
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	registry.Close()
	registry.Close() // must not panic or hang
}

func (r *Registry) len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

func (r *Registry) has(scope Scope, id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.entries[scopeKey(scope, id)]
	return ok
}

func (r *Registry) scopeKeys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.entries))
	for key := range r.entries {
		keys = append(keys, key)
	}
	return keys
}

func (r *Registry) limiterTokens(scope Scope, id string) float64 {
	r.mu.RLock()
	current := r.entries[scopeKey(scope, id)]
	r.mu.RUnlock()
	if current == nil {
		return -1
	}
	return current.lim.Tokens()
}

type atomicCounter struct {
	mu    sync.Mutex
	value int
}

func (c *atomicCounter) add(delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value += delta
}

func (c *atomicCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}
