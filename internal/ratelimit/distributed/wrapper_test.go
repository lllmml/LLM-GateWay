package distributed

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
)

// ---------------------------------------------------------------------------
// Deterministic state-machine tests (Slice B2a). The admission core and the
// health probe are fakes, so no live Redis is needed and races are staged
// deterministically with channels.
// ---------------------------------------------------------------------------

// fakeAdmissionCore implements admissionCore with an ordered answer queue.
// Every AdmitCore call signals `started` (buffered) and then blocks until the
// test supplies the next answer, giving deterministic control over the claim /
// failure / success ordering.
type fakeAdmissionCore struct {
	calls   atomic.Int64
	started chan struct{}
	answers chan func(ctx context.Context) (ratelimit.Decision, error)
}

func newFakeAdmissionCore(_ int) *fakeAdmissionCore {
	// Large fixed buffers: pushes never block (tests may preload answers before
	// the admission goroutine starts) and every started signal is captured.
	return &fakeAdmissionCore{
		started: make(chan struct{}, 256),
		answers: make(chan func(ctx context.Context) (ratelimit.Decision, error), 256),
	}
}

func (f *fakeAdmissionCore) AdmitCore(ctx context.Context, _, _ string) (ratelimit.Decision, error) {
	f.calls.Add(1)
	f.started <- struct{}{}
	answer := <-f.answers
	return answer(ctx)
}

func (f *fakeAdmissionCore) push(answer func(ctx context.Context) (ratelimit.Decision, error)) {
	f.answers <- answer
}

func (f *fakeAdmissionCore) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("core admission did not start")
	}
}

// drainStarted consumes n already-signalled core starts (from synchronous
// admissions that happened before a goroutine was launched).
func (f *fakeAdmissionCore) drainStarted(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.started:
		case <-time.After(5 * time.Second):
			t.Fatal("missing expected core start signal")
		}
	}
}

func depFailure(context.Context) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, &DependencyError{Err: errors.New("redis down")}
}

func redisAllow(context.Context) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true}, nil
}

func redisReject(retry time.Duration) func(context.Context) (ratelimit.Decision, error) {
	return func(context.Context) (ratelimit.Decision, error) {
		return ratelimit.Decision{Allowed: false, RetryAfter: retry, BlockingScope: ratelimit.ScopeVirtualKey}, nil
	}
}

func ctxErrAnswer(ctx context.Context) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, ctx.Err()
}

// wrapperTestConfig returns a minimal valid wrapper config for deterministic
// tests; mutate per test.
func wrapperTestConfig() WrapperConfig {
	return WrapperConfig{
		KeyRPM:         100,
		ProjectRPM:     100,
		IdleTTL:        time.Minute,
		CommandTimeout: time.Second,
		ReplicaFactor:  2,
		ProbeInterval:  time.Minute, // probes driven manually in tests
		ProbeThreshold: 3,
	}
}

// newTestWrapper builds a Limiter over the fake core without live Redis.
func newTestWrapper(t *testing.T, core admissionCore, cfg WrapperConfig) *Limiter {
	t.Helper()
	cfg.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	limiter, err := newWrapper(core, cfg, nil)
	if err != nil {
		t.Fatalf("new wrapper: %v", err)
	}
	t.Cleanup(limiter.Close)
	return limiter
}

func mustAdmit(t *testing.T, l *Limiter, keyID, projectID string) ratelimit.Decision {
	t.Helper()
	decision, err := l.Admit(context.Background(), keyID, projectID)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return decision
}

func TestEmergencyRPMDerivation(t *testing.T) {
	cases := []struct {
		normal, factor, want int
	}{
		{normal: 0, factor: 2, want: 0},   // disabled stays disabled
		{normal: 20, factor: 2, want: 10}, // floor(normal / factor)
		{normal: 1, factor: 10, want: 1},  // max(1, floor(...)) keeps a positive bound
		{normal: 20, factor: 1, want: 20}, // single replica: emergency == normal
		{normal: 21, factor: 4, want: 5},  // floor, not rounding up
	}
	for _, tc := range cases {
		if got := emergencyRPM(tc.normal, tc.factor); got != tc.want {
			t.Fatalf("emergencyRPM(%d, %d) = %d, want %d", tc.normal, tc.factor, got, tc.want)
		}
	}
}

func TestWrapperConfigValidation(t *testing.T) {
	cfg := wrapperTestConfig()
	if err := validateWrapperConfig(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := []struct {
		name string
		mut  func(*WrapperConfig)
	}{
		{"zero replica factor", func(c *WrapperConfig) { c.ReplicaFactor = 0 }},
		{"zero probe interval", func(c *WrapperConfig) { c.ProbeInterval = 0 }},
		{"zero probe threshold", func(c *WrapperConfig) { c.ProbeThreshold = 0 }},
		{"sub-ms idle ttl", func(c *WrapperConfig) { c.IdleTTL = time.Microsecond }},
		{"rpm over max", func(c *WrapperConfig) { c.KeyRPM = MaxSafeRPM() + 1 }},
	}
	for _, tc := range bad {
		modified := cfg
		tc.mut(&modified)
		if err := validateWrapperConfig(modified); err == nil {
			t.Fatalf("config %q accepted", tc.name)
		}
	}
}

func TestWrapperNormalAllowAndReject(t *testing.T) {
	core := newFakeAdmissionCore(2)
	core.push(redisAllow)
	core.push(redisReject(3000 * time.Millisecond))
	limiter := newTestWrapper(t, core, wrapperTestConfig())

	decision := mustAdmit(t, limiter, "k", "p")
	if !decision.Allowed {
		t.Fatalf("normal allow decision = %+v", decision)
	}
	if got := core.calls.Load(); got != 1 {
		t.Fatalf("core calls = %d, want 1", got)
	}
	if limiter.currentStateForTest() != stateNormal {
		t.Fatalf("state = %v, want normal", limiter.currentStateForTest())
	}

	rejected := mustAdmit(t, limiter, "k", "p")
	if rejected.Allowed || rejected.RetryAfter != 3000*time.Millisecond || rejected.BlockingScope != ratelimit.ScopeVirtualKey {
		t.Fatalf("normal reject decision = %+v, want the exact Redis rejection", rejected)
	}
}

func TestWrapperFirstDependencyFailureEntersDegradedAndUsesEmergency(t *testing.T) {
	// Emergency project quota = 1 (ProjectRPM 2 / factor 2), key quota large:
	// after the first emergency admission the composite must reject (bounded,
	// no unlimited fail-open, both scopes preserved).
	cfg := wrapperTestConfig()
	cfg.ProjectRPM = 2
	core := newFakeAdmissionCore(1)
	core.push(depFailure)
	limiter := newTestWrapper(t, core, cfg)

	first := mustAdmit(t, limiter, "k", "p")
	if !first.Allowed {
		t.Fatalf("first request did not fall back to emergency allow: %+v", first)
	}
	if limiter.currentStateForTest() != stateDegraded {
		t.Fatalf("state = %v, want degraded after first dependency failure", limiter.currentStateForTest())
	}
	if got := core.calls.Load(); got != 1 {
		t.Fatalf("core calls = %d, want 1", got)
	}

	// Second emergency admission: project emergency quota is exhausted -> the
	// composite must reject via the project scope (no unlimited fail-open).
	second := mustAdmit(t, limiter, "k", "p")
	if second.Allowed {
		t.Fatal("emergency limiter allowed past its bounded composite quota")
	}
	if second.BlockingScope != ratelimit.ScopeProject {
		t.Fatalf("blocking scope = %q, want project", second.BlockingScope)
	}
}

func TestWrapperCancellationNeverDegradesOrConsumesEmergency(t *testing.T) {
	cfg := wrapperTestConfig()
	cfg.ProjectRPM = 6 // emergency project quota = 3: degraded fallback + 2 live proves no leak
	core := newFakeAdmissionCore(1)
	core.push(depFailure)
	limiter := newTestWrapper(t, core, cfg)

	// Cancelled before any Redis work: ctx error, no core call, state normal.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.Admit(cancelled, "k", "p"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel error = %v, want context.Canceled", err)
	}
	if got := core.calls.Load(); got != 0 {
		t.Fatalf("core called for a cancelled request (calls = %d)", got)
	}
	if limiter.currentStateForTest() != stateNormal {
		t.Fatalf("state = %v, want normal after pre-cancel", limiter.currentStateForTest())
	}

	// Enter degraded through a real dependency failure (emergency capacity 2).
	mustAdmit(t, limiter, "k", "p")
	if limiter.currentStateForTest() != stateDegraded {
		t.Fatalf("state = %v, want degraded", limiter.currentStateForTest())
	}

	// A cancelled request while degraded must not consume emergency quota.
	if _, err := limiter.Admit(cancelled, "k", "p"); !errors.Is(err, context.Canceled) {
		t.Fatalf("degraded pre-cancel error = %v, want context.Canceled", err)
	}
	// Two live emergency admissions still succeed => the cancelled one consumed
	// nothing (capacity 2).
	if !mustAdmit(t, limiter, "k", "p").Allowed {
		t.Fatal("first live emergency admission after cancel rejected")
	}
	if !mustAdmit(t, limiter, "k", "p").Allowed {
		t.Fatal("second live emergency admission after cancel rejected (cancelled request leaked quota)")
	}
}

func TestWrapperCancellationMidRedisPathNoDegrade(t *testing.T) {
	core := newFakeAdmissionCore(2)
	limiter := newTestWrapper(t, core, wrapperTestConfig())

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := limiter.Admit(ctx, "k", "p")
		result <- err
	}()
	core.waitStarted(t) // request claimed and is inside Core
	cancel()
	core.push(ctxErrAnswer)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-path cancel error = %v, want context.Canceled", err)
	}
	if limiter.currentStateForTest() != stateNormal {
		t.Fatalf("state = %v, want normal (cancellation must not degrade)", limiter.currentStateForTest())
	}
	// The next live request still uses the Redis path.
	core.push(redisAllow)
	if !mustAdmit(t, limiter, "k", "p").Allowed {
		t.Fatal("live request after cancellation was not admitted via the Redis path")
	}
	if got := core.calls.Load(); got != 2 {
		t.Fatalf("core calls = %d, want 2", got)
	}
}

// TestWrapperClaimBoundaryR3CannotClaimAfterDegraded stages R1/R2 claims while
// normal, fails R1 -> degraded, and proves R3 (starting after degraded) cannot
// claim a Redis attempt and uses emergency instead.
func TestWrapperClaimBoundaryR3CannotClaimAfterDegraded(t *testing.T) {
	cfg := wrapperTestConfig()
	cfg.ProjectRPM = 100
	core := newFakeAdmissionCore(0)
	limiter := newTestWrapper(t, core, cfg)

	r1 := make(chan error, 1)
	r2 := make(chan error, 1)
	go func() { _, err := limiter.Admit(context.Background(), "k", "p"); r1 <- err }()
	core.waitStarted(t) // R1 claimed (normal)
	go func() { _, err := limiter.Admit(context.Background(), "k", "p"); r2 <- err }()
	core.waitStarted(t) // R2 claimed (still normal)
	if limiter.currentStateForTest() != stateNormal {
		t.Fatalf("state before R1 failure = %v, want normal", limiter.currentStateForTest())
	}

	core.push(depFailure) // R1 dependency failure -> degraded, R1 -> emergency
	if err := <-r1; err != nil {
		t.Fatalf("R1 error: %v", err)
	}
	if limiter.currentStateForTest() != stateDegraded {
		t.Fatalf("state after R1 failure = %v, want degraded", limiter.currentStateForTest())
	}
	coreCallsAfterR1 := core.calls.Load()
	if coreCallsAfterR1 != 2 {
		t.Fatalf("core calls after R1 = %d, want 2 (R1 and R2 claimed before degraded)", coreCallsAfterR1)
	}

	// R3 starts after degraded: it must NOT claim a Redis attempt (no new core
	// call) and must be served by the emergency limiter.
	if !mustAdmit(t, limiter, "k", "p").Allowed {
		t.Fatal("R3 emergency admission rejected")
	}
	if got := core.calls.Load(); got != coreCallsAfterR1 {
		t.Fatalf("R3 opened a new Redis attempt (core calls = %d, want %d)", got, coreCallsAfterR1)
	}

	// R2 (pre-claimed) now completes with a valid Redis rejection: it returns
	// that exact Redis decision and must NOT restore normal.
	core.push(redisReject(7777 * time.Millisecond))
	select {
	case err := <-r2:
		if err != nil {
			t.Fatalf("R2 error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("R2 did not complete")
	}
	if limiter.currentStateForTest() != stateDegraded {
		t.Fatalf("pre-claimed success restored normal; state = %v, want degraded", limiter.currentStateForTest())
	}
}

// TestWrapperPreClaimedSuccessAfterDegraded pins both Allowed=true and
// Allowed=false pre-claimed completions: the exact Redis Decision is returned,
// no emergency re-admission happens, and normal is not restored.
func TestWrapperPreClaimedSuccessAfterDegraded(t *testing.T) {
	for _, answer := range []struct {
		name string
		fn   func(context.Context) (ratelimit.Decision, error)
	}{
		{"allowed", redisAllow},
		{"rejected", redisReject(4242 * time.Millisecond)},
	} {
		t.Run(answer.name, func(t *testing.T) {
			cfg := wrapperTestConfig()
			cfg.ProjectRPM = 2 // emergency project quota = 1 (consumed by R1's fallback)
			core := newFakeAdmissionCore(0)
			limiter := newTestWrapper(t, core, cfg)

			r1 := make(chan error, 1)
			r2 := make(chan error, 1)
			r2Result := make(chan ratelimit.Decision, 1)
			go func() {
				_, err := limiter.Admit(context.Background(), "k", "p")
				r1 <- err
			}()
			core.waitStarted(t) // R1 claimed
			go func() {
				decision, err := limiter.Admit(context.Background(), "k", "p")
				r2 <- err
				r2Result <- decision
			}()
			core.waitStarted(t) // R2 claimed

			core.push(depFailure) // R1 fails -> degraded, R1 uses emergency (quota 1 consumed)
			if err := <-r1; err != nil {
				t.Fatalf("R1 error: %v", err)
			}
			if limiter.currentStateForTest() != stateDegraded {
				t.Fatalf("state = %v, want degraded", limiter.currentStateForTest())
			}

			core.push(answer.fn) // R2 (pre-claimed) completes with its Redis Decision
			if err := <-r2; err != nil {
				t.Fatalf("R2 error: %v", err)
			}
			got := <-r2Result
			// R2's decision must be exactly the Redis decision, not an
			// emergency re-admission (the emergency project quota is spent, so
			// an emergency admission would have rejected).
			if answer.name == "allowed" {
				if !got.Allowed {
					t.Fatalf("R2 = %+v, want the exact Redis allow", got)
				}
			} else {
				if got.Allowed || got.RetryAfter != 4242*time.Millisecond {
					t.Fatalf("R2 = %+v, want the exact Redis rejection (4242ms)", got)
				}
			}
			if limiter.currentStateForTest() != stateDegraded {
				t.Fatalf("pre-claimed success restored normal; state = %v, want degraded", limiter.currentStateForTest())
			}
		})
	}
}

// TestWrapperPreClaimedFailureAfterDegradedFallsBackToEmergency: a pre-claimed
// attempt whose Redis call fails after degraded uses the emergency limiter.
func TestWrapperPreClaimedFailureAfterDegradedFallsBackToEmergency(t *testing.T) {
	cfg := wrapperTestConfig()
	cfg.ProjectRPM = 4 // emergency project quota 2: R1 and R2 fallbacks both allowed
	core := newFakeAdmissionCore(0)
	limiter := newTestWrapper(t, core, cfg)

	r1 := make(chan error, 1)
	r2 := make(chan error, 1)
	go func() { _, err := limiter.Admit(context.Background(), "k", "p"); r1 <- err }()
	core.waitStarted(t)
	go func() { _, err := limiter.Admit(context.Background(), "k", "p"); r2 <- err }()
	core.waitStarted(t)

	core.push(depFailure) // R1 fails -> degraded
	if err := <-r1; err != nil {
		t.Fatalf("R1 error: %v", err)
	}
	core.push(depFailure) // R2 was pre-claimed and also fails -> emergency fallback
	if err := <-r2; err != nil {
		t.Fatalf("R2 error: %v", err)
	}
	if limiter.currentStateForTest() != stateDegraded {
		t.Fatalf("state = %v, want degraded", limiter.currentStateForTest())
	}
	if got := core.calls.Load(); got != 2 {
		t.Fatalf("core calls = %d, want 2", got)
	}
}

func TestWrapperRecoveryProbeQualificationAndReset(t *testing.T) {
	core := newFakeAdmissionCore(1)
	core.push(depFailure)
	var probeOK atomic.Bool
	probeOK.Store(true)
	cfg := wrapperTestConfig()
	cfg.Probe = func(context.Context) error {
		if !probeOK.Load() {
			return errors.New("redis unreachable")
		}
		return nil
	}
	limiter := newTestWrapper(t, core, cfg)

	mustAdmit(t, limiter, "k", "p") // degrade
	if limiter.currentStateForTest() != stateDegraded {
		t.Fatalf("state = %v, want degraded", limiter.currentStateForTest())
	}

	// Two successes, then a failure: counter resets and the state stays
	// degraded.
	limiter.probeCycle(context.Background())
	limiter.probeCycle(context.Background())
	if got := limiter.probeCountForTest(); got != 2 {
		t.Fatalf("probe successes = %d, want 2", got)
	}
	probeOK.Store(false)
	limiter.probeCycle(context.Background())
	if got := limiter.probeCountForTest(); got != 0 {
		t.Fatalf("probe successes after failure = %d, want reset to 0", got)
	}
	if limiter.currentStateForTest() != stateDegraded {
		t.Fatalf("state after probe failure = %v, want degraded", limiter.currentStateForTest())
	}

	// K = 3 consecutive successes qualify for recovering.
	probeOK.Store(true)
	for i := 0; i < cfg.ProbeThreshold; i++ {
		limiter.probeCycle(context.Background())
	}
	if limiter.currentStateForTest() != stateRecovering {
		t.Fatalf("state after K successes = %v, want recovering", limiter.currentStateForTest())
	}
	// Probes are non-mutating: the core was only ever called for the one real
	// admission that degraded the replica.
	if got := core.calls.Load(); got != 1 {
		t.Fatalf("probes touched the admission core (calls = %d, want 1)", got)
	}
}

func TestWrapperRecoveringSingleFlightOthersUseEmergency(t *testing.T) {
	cfg := wrapperTestConfig()
	cfg.ProjectRPM = 100
	core := newFakeAdmissionCore(0)
	limiter := newTestWrapper(t, core, cfg)

	// Enter degraded then qualify for recovering via probes (no probe quota).
	core.push(depFailure)
	mustAdmit(t, limiter, "k", "p")
	core.drainStarted(t, 1)
	cfg.Probe = func(context.Context) error { return nil }
	limiter.cfg.Probe = cfg.Probe
	for i := 0; i < cfg.ProbeThreshold; i++ {
		limiter.probeCycle(context.Background())
	}
	if limiter.currentStateForTest() != stateRecovering {
		t.Fatalf("state = %v, want recovering", limiter.currentStateForTest())
	}

	// A: the single-flight real recovery admission (blocked until we answer).
	doneA := make(chan ratelimit.Decision, 1)
	errA := make(chan error, 1)
	go func() {
		decision, err := limiter.Admit(context.Background(), "k", "p")
		errA <- err
		doneA <- decision
	}()
	core.waitStarted(t)
	if got := core.calls.Load(); got != 2 { // 1 degrade + 1 recovery attempt
		t.Fatalf("core calls = %d, want 2", got)
	}

	// B and C arrive while A's Redis attempt is blocked: they must use the
	// emergency limiter and NOT block behind the Redis round trip.
	others := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			decision, err := limiter.Admit(context.Background(), "k", "p")
			if err != nil {
				others <- err
				return
			}
			if !decision.Allowed {
				others <- errors.New("concurrent request during recovery was rejected by emergency")
				return
			}
			others <- nil
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-others:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent request blocked behind the recovery Redis round trip")
		}
	}
	if got := core.calls.Load(); got != 2 {
		t.Fatalf("concurrent requests opened Redis attempts (core calls = %d, want 2)", got)
	}

	// Release A with a valid Redis allow: recovery succeeds -> normal.
	core.push(redisAllow)
	if err := <-errA; err != nil {
		t.Fatalf("recovery attempt error: %v", err)
	}
	if decision := <-doneA; !decision.Allowed {
		t.Fatalf("recovery allow decision = %+v", decision)
	}
	if limiter.currentStateForTest() != stateNormal {
		t.Fatalf("state after successful recovery = %v, want normal", limiter.currentStateForTest())
	}
}

func TestWrapperRecoveryRejectionReturnsExactDecisionAndGoesNormal(t *testing.T) {
	cfg := wrapperTestConfig()
	cfg.ProjectRPM = 2 // emergency quota would allow; a valid Redis reject must NOT fall back
	core := newFakeAdmissionCore(0)
	limiter := newTestWrapper(t, core, cfg)

	core.push(depFailure)
	mustAdmit(t, limiter, "k", "p")
	core.drainStarted(t, 1)
	limiter.cfg.Probe = func(context.Context) error { return nil }
	for i := 0; i < cfg.ProbeThreshold; i++ {
		limiter.probeCycle(context.Background())
	}

	done := make(chan ratelimit.Decision, 1)
	errs := make(chan error, 1)
	go func() {
		decision, err := limiter.Admit(context.Background(), "k", "p")
		errs <- err
		done <- decision
	}()
	core.waitStarted(t)
	core.push(redisReject(999 * time.Millisecond))

	if err := <-errs; err != nil {
		t.Fatalf("recovery attempt error: %v", err)
	}
	decision := <-done
	if decision.Allowed || decision.RetryAfter != 999*time.Millisecond {
		t.Fatalf("recovery reject decision = %+v, want the exact Redis rejection (999ms), not an emergency fallback", decision)
	}
	if limiter.currentStateForTest() != stateNormal {
		t.Fatalf("valid Redis rejection during recovery must still return to normal; state = %v", limiter.currentStateForTest())
	}
}

func TestWrapperRecoveryDependencyFailureDegradesAndRestartsProbes(t *testing.T) {
	cfg := wrapperTestConfig()
	core := newFakeAdmissionCore(0)
	limiter := newTestWrapper(t, core, cfg)

	core.push(depFailure)
	mustAdmit(t, limiter, "k", "p")
	core.drainStarted(t, 1)
	limiter.cfg.Probe = func(context.Context) error { return nil }
	limiter.probeCycle(context.Background()) // 1 success (K=3 -> still degraded)

	// Manually pre-advance the probe counter to 2 then a 3rd to enter
	// recovering for a clean failure test:
	limiter.probeCycle(context.Background())
	limiter.probeCycle(context.Background())
	if limiter.currentStateForTest() != stateRecovering {
		t.Fatalf("state = %v, want recovering", limiter.currentStateForTest())
	}

	done := make(chan ratelimit.Decision, 1)
	errs := make(chan error, 1)
	go func() {
		decision, err := limiter.Admit(context.Background(), "k", "p")
		errs <- err
		done <- decision
	}()
	core.waitStarted(t)
	core.push(depFailure) // recovery attempt dependency-fails

	if err := <-errs; err != nil {
		t.Fatalf("recovery failure error: %v", err)
	}
	if decision := <-done; !decision.Allowed {
		t.Fatalf("recovery failure should fall back to emergency allow: %+v", decision)
	}
	if limiter.currentStateForTest() != stateDegraded {
		t.Fatalf("state after failed recovery = %v, want degraded", limiter.currentStateForTest())
	}
	if got := limiter.probeCountForTest(); got != 0 {
		t.Fatalf("probe count after failed recovery = %d, want reset to 0", got)
	}
}
