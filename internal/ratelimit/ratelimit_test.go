package ratelimit

import (
	"math"
	"sync"
	"testing"
	"time"
)

const (
	testKeyID     = "key-11111111-1111-4111-8111-111111111111"
	testOtherKey  = "key-22222222-2222-4222-8222-222222222222"
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
	clock := newFakeClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
	// 2 requests/minute: burst 2, one token refilled every 30s.
	registry := testRegistry(t, func() Config {
		cfg := testConfig()
		cfg.KeyRPM = 2
		cfg.ProjectRPM = 0
		return cfg
	}(), clock)

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
	clock := newFakeClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
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
	clock := newFakeClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
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
	if got := registry.limiterTokens(ScopeProject, testProjID); !nearTokens(got, projectBefore) {
		t.Fatalf("project tokens after rejected admission = %v, want unchanged %v", got, projectBefore)
	}
}

func TestConcurrentAdmissionsLeakNothingAcrossScopes(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
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
	clock := newFakeClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
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
	clock := newFakeClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
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
	registry.Admit("key-3", "proj-3")
	if registry.len() != cfg.EntryCap {
		t.Fatalf("entries = %d, want cap %d", registry.len(), cfg.EntryCap)
	}
	if registry.has(ScopeVirtualKey, testKeyID) {
		t.Fatal("least-recently-used entry was not evicted")
	}
	if !registry.has(ScopeVirtualKey, testOtherKey) || !registry.has(ScopeVirtualKey, "key-3") {
		t.Fatal("newer entries were evicted instead of the oldest")
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

func (r *Registry) limiterTokens(scope Scope, id string) float64 {
	r.mu.RLock()
	current := r.entries[scopeKey(scope, id)]
	r.mu.RUnlock()
	if current == nil {
		return -1
	}
	current.mu.Lock()
	defer current.mu.Unlock()
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
