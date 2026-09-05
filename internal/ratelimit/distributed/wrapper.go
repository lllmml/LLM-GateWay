package distributed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
)

// Limiter is the Week 9 production admission limiter (ADR-018 D6-D9): it wraps
// the raw Redis admission core (distributed.Core) with the degraded /
// recovering state machine and the bounded local emergency limiter, and is the
// only distributed type that implements ratelimit.Limiter. Redis dependency
// failures never escape through Admit.
//
// Contract (ratelimit.Limiter):
//   - Decision is the normal allow/reject result;
//   - an error is returned ONLY to propagate parent context cancellation /
//     deadline;
//   - Redis dependency failures are handled internally (degraded entry +
//     emergency fallback) and never surface.
var _ ratelimit.Limiter = (*Limiter)(nil)

type state int

const (
	stateNormal state = iota
	stateDegraded
	stateRecovering
)

func (s state) String() string {
	switch s {
	case stateNormal:
		return "normal"
	case stateDegraded:
		return "degraded"
	case stateRecovering:
		return "recovering"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// ProbeFunc is a non-mutating Redis liveness check used while degraded. It
// must NOT consume quota (it is never an admission). The production probe is a
// PING; the real Lua write path is validated by the single recovering
// admission before returning to normal (ADR-018 D9).
type ProbeFunc func(ctx context.Context) error

// WrapperConfig extends the core configuration with the wrapper state-machine
// parameters. It is produced from user-facing configuration in cmd/gateway
// (RATE_LIMITER_MODE=distributed and the REDIS_* / RATE_LIMITER_* fields).
type WrapperConfig struct {
	// Core limits and lifecycle (see Core).
	KeyRPM         int
	ProjectRPM     int
	IdleTTL        time.Duration
	CommandTimeout time.Duration

	// ReplicaFactor is the deployment's conservative expected/max replica
	// count used to derive the emergency limit (ADR-018 D7). >= 1.
	ReplicaFactor int

	// ProbeInterval is the degraded-mode probe cadence.
	ProbeInterval time.Duration
	// ProbeThreshold K is the consecutive successful probes required to move
	// degraded -> recovering.
	ProbeThreshold int

	// Now is the injectable clock for probe cadence bookkeeping (tests).
	Now func() time.Time
	// Probe is the injectable non-mutating health probe (tests). Nil means
	// the production PING probe.
	Probe ProbeFunc
	// Logger receives bounded transition events (normal->degraded,
	// degraded->recovering, recovering->normal, recovering->degraded). It
	// never logs per-request admission outcomes and never includes keys,
	// credentials, or Redis connection material.
	Logger *slog.Logger
}

// admissionCore is the internal seam around the raw Redis admission core so the
// state-machine tests are deterministic and never depend on a live Redis
// (ADR-018 B2a requirement). *Core implements it; it is intentionally not the
// public ratelimit.Limiter.
type admissionCore interface {
	AdmitCore(ctx context.Context, keyID, projectID string) (ratelimit.Decision, error)
}

// emergencyRPM derives one scope's emergency quota from its normal quota and
// the replica factor per ADR-018 D7: a disabled scope stays disabled (0); an
// enabled scope gets max(1, floor(normalRPM / replicaFactor)) so it is always a
// positive, bounded local quota and never silently zero (no unlimited
// fail-open, and never an increase above the configured intent beyond the
// documented max(1,...) floor).
func emergencyRPM(normalRPM, replicaFactor int) int {
	if normalRPM <= 0 {
		return 0
	}
	derived := normalRPM / replicaFactor
	if derived < 1 {
		derived = 1
	}
	return derived
}

// Limiter wraps a distributed.Core admissionCore with the degraded/recovering
// state machine and a bounded local emergency Registry.
type Limiter struct {
	core admissionCore
	cfg  WrapperConfig

	// emergency is the process-local bounded fallback (ratelimit.Registry with
	// the emergency-derived composite limits). It owns a janitor goroutine.
	emergency *ratelimit.Registry

	mu sync.Mutex // guards every field below; NEVER held across Redis I/O

	state state

	// inflight counts distributed attempts claimed while state was normal
	// (atomic claim boundary, ADR-018 D6). Purely bookkeeping for tests.
	inflight int

	// recovery bookkeeping.
	probeSuccess            int
	lastProbe               time.Time
	recoveryAttemptInFlight bool

	stop chan struct{}
	done chan struct{}

	// lifecycle is canceled by Close: it bounds and cancels in-flight recovery
	// probes so shutdown never depends on an external client's defaults.
	lifecycle context.Context
	cancel    context.CancelFunc
}

// NewLimiter builds the production wrapper around a real Redis core and starts
// the background degraded-mode probe loop. Close stops the loop and the
// emergency registry's janitor.
func NewLimiter(client *redis.Client, cfg WrapperConfig) (*Limiter, error) {
	if client == nil {
		return nil, errors.New("distributed wrapper redis client is required")
	}
	if err := validateWrapperConfig(cfg); err != nil {
		return nil, err
	}
	core, err := New(client, Config{ // Core's Config: raw distributed limits
		KeyRPM:         cfg.KeyRPM,
		ProjectRPM:     cfg.ProjectRPM,
		IdleTTL:        cfg.IdleTTL,
		CommandTimeout: cfg.CommandTimeout,
	})
	if err != nil {
		return nil, err
	}
	ping := func(ctx context.Context) error { return client.Ping(ctx).Err() }
	return newWrapper(core, cfg, ping)
}

func validateWrapperConfig(cfg WrapperConfig) error {
	for scope, rpm := range map[string]int{"virtual key": cfg.KeyRPM, "project": cfg.ProjectRPM} {
		if err := ValidateScopeRPM(rpm); err != nil {
			return fmt.Errorf("distributed wrapper %s: %w", scope, err)
		}
	}
	if cfg.IdleTTL < time.Millisecond {
		return errors.New("distributed wrapper idle TTL must be at least 1ms")
	}
	if cfg.CommandTimeout <= 0 {
		return errors.New("distributed wrapper command timeout must be positive")
	}
	if cfg.ReplicaFactor < 1 {
		return errors.New("distributed wrapper replica factor must be at least 1")
	}
	if cfg.ProbeInterval <= 0 {
		return errors.New("distributed wrapper probe interval must be positive")
	}
	if cfg.ProbeThreshold < 1 {
		return errors.New("distributed wrapper probe threshold must be at least 1")
	}
	return nil
}

// newWrapper is the internal constructor shared by production and tests. probe
// is the non-mutating health probe (nil disables background probing, used by
// deterministic tests which drive probeCycle directly).
func newWrapper(core admissionCore, cfg WrapperConfig, probe ProbeFunc) (*Limiter, error) {
	if err := validateWrapperConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	emergency, err := ratelimit.NewRegistry(ratelimit.Config{
		KeyRPM:        float64(emergencyRPM(cfg.KeyRPM, cfg.ReplicaFactor)),
		ProjectRPM:    float64(emergencyRPM(cfg.ProjectRPM, cfg.ReplicaFactor)),
		EntryCap:      10000,
		IdleTTL:       cfg.IdleTTL,
		SweepInterval: time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("configure emergency limiter: %w", err)
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	limiter := &Limiter{
		core:      core,
		cfg:       cfg,
		emergency: emergency,
		state:     stateNormal,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		lifecycle: lifecycle,
		cancel:    cancel,
	}
	if probe != nil {
		limiter.cfg.Probe = probe
	}
	// The probe loop always runs and self-guards on a nil probe (deterministic
	// tests drive probeCycle directly and simply Close the limiter). The stop
	// channel is captured at goroutine creation so a late-starting loop never
	// reads a nil channel after Close has cleared the field.
	go limiter.probeLoop(limiter.stop)
	return limiter, nil
}

// Close stops the background probe loop and the emergency registry's janitor.
// It is idempotent.
// Close stops the background probe loop (canceling any in-flight probe), waits
// for it, and closes the emergency registry's janitor. It is idempotent and
// never blocks longer than an in-flight probe's ctx cancellation.
func (l *Limiter) Close() {
	l.mu.Lock()
	if l.stop == nil {
		l.mu.Unlock()
		return
	}
	close(l.stop)
	l.stop = nil
	l.cancel() // cancels any in-flight probe derived from the lifecycle context
	l.mu.Unlock()
	<-l.done
	l.emergency.Close()
}

// currentStateForTest is only used by tests in this package.
func (l *Limiter) currentStateForTest() state {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// probeCountForTest is only used by tests in this package.
func (l *Limiter) probeCountForTest() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.probeSuccess
}

// Admit implements ratelimit.Limiter. See the type doc for the contract.
func (l *Limiter) Admit(ctx context.Context, keyID, projectID string) (ratelimit.Decision, error) {
	// Cancellation always wins: never consume quota, never degrade, never use
	// emergency for a cancelled request.
	if err := ctx.Err(); err != nil {
		return ratelimit.Decision{}, err
	}

	// --- Atomic claim boundary under the state mutex (no Redis I/O here) ---
	l.mu.Lock()
	mode := l.state
	claimed := false
	recoveryAttempt := false
	switch mode {
	case stateNormal:
		// A distributed attempt is in-flight from the moment it is claimed.
		claimed = true
		l.inflight++
	case stateRecovering:
		if !l.recoveryAttemptInFlight {
			// The next real request owns the single recovery admission.
			l.recoveryAttemptInFlight = true
			recoveryAttempt = true
		}
	}
	l.mu.Unlock()

	var decision ratelimit.Decision
	var err error
	if claimed || recoveryAttempt {
		decision, err = l.core.AdmitCore(ctx, keyID, projectID)
	} else {
		// degraded (or recovering while another request owns the attempt):
		// bounded local emergency admission. The state mutex is not held.
		return l.emergencyAdmit(ctx, keyID, projectID)
	}

	// --- Commit / unregister under the state mutex (no Redis I/O here) ---
	l.mu.Lock()
	if claimed {
		l.inflight--
	}
	if err != nil {
		// Parent cancellation/deadline is NOT a dependency failure: propagate
		// it, do not degrade, do not use emergency.
		if ctxErr := ctx.Err(); ctxErr != nil {
			if recoveryAttempt {
				l.recoveryAttemptInFlight = false
			}
			l.mu.Unlock()
			return ratelimit.Decision{}, ctxErr
		}
		// Genuine dependency failure (parent ctx live): enter degraded on the
		// first failure; later concurrent failures are no-ops. The current
		// request falls back to the emergency limiter.
		l.enterDegradedLocked()
		if recoveryAttempt {
			l.recoveryAttemptInFlight = false
		}
		l.mu.Unlock()
		return l.emergencyAdmit(ctx, keyID, projectID)
	}

	if recoveryAttempt {
		// A valid Redis Decision (Allowed true OR false) means the write path
		// is healthy: recovery succeeded, return to normal with the exact
		// Redis decision (never an emergency fallback after a valid rejection).
		l.recoveryAttemptInFlight = false
		l.probeSuccess = 0
		l.setStateLocked(stateNormal)
		l.mu.Unlock()
		return decision, nil
	}
	// Pre-claimed distributed success: return the exact Redis Decision. It
	// does NOT restore normal (another request may already have transitioned
	// the replica to degraded) and it does NOT perform a second emergency
	// admission.
	l.mu.Unlock()
	return decision, nil
}

// enterDegradedLocked transitions normal/recovering -> degraded exactly once;
// later calls are no-ops. Must hold l.mu.
func (l *Limiter) enterDegradedLocked() {
	if l.state == stateDegraded {
		return
	}
	l.setStateLocked(stateDegraded)
	l.probeSuccess = 0
	l.lastProbe = time.Time{}
}

// setStateLocked assigns the next state and emits the bounded transition event
// when it changed. Must hold l.mu.
func (l *Limiter) setStateLocked(next state) {
	if l.state == next {
		return
	}
	from := l.state
	l.state = next
	if l.cfg.Logger != nil {
		l.cfg.Logger.Info("limiter_state_transition",
			slog.String("event", "rate_limiter_state_transition"),
			slog.String("from", from.String()),
			slog.String("to", next.String()),
		)
	}
}

// emergencyAdmit admits via the bounded local emergency Registry. Cancellation
// still wins (the registry checks ctx). No unlimited fail-open: the emergency
// registry is strictly bounded by its derived composite limits.
func (l *Limiter) emergencyAdmit(ctx context.Context, keyID, projectID string) (ratelimit.Decision, error) {
	if err := ctx.Err(); err != nil {
		return ratelimit.Decision{}, err
	}
	return l.emergency.Admit(ctx, keyID, projectID)
}

// probeLoop is the production background cadence. It only probes while
// degraded; probes never touch quota.
func (l *Limiter) probeLoop(stop <-chan struct{}) {
	defer close(l.done)
	ticker := time.NewTicker(l.cfg.ProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// Every probe is bounded by the wrapper CommandTimeout and is
			// canceled when the lifecycle (Close) ends.
			probeCtx, cancelProbe := context.WithTimeout(l.lifecycle, l.cfg.CommandTimeout)
			l.probeCycle(probeCtx)
			cancelProbe()
		}
	}
}

// probeCycle runs one non-mutating health probe when degraded AND no
// distributed attempt claimed before the degraded transition is still
// in-flight (recovery barrier, ADR-018 D6/D9 refinement). Once degraded is
// observable no new normal claims can start, so inflight == 0 means the old
// normal generation has fully drained and no stale pre-degradation result can
// cross the recovery boundary. K consecutive successes -> recovering; a probe
// failure resets the counter. Deterministic tests drive this directly.
func (l *Limiter) probeCycle(ctx context.Context) {
	l.mu.Lock()
	if l.state != stateDegraded || l.inflight != 0 || l.cfg.Probe == nil {
		l.mu.Unlock()
		return
	}
	probe := l.cfg.Probe
	l.mu.Unlock()

	err := probe(ctx)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != stateDegraded || l.inflight != 0 {
		return // left degraded or a new in-flight generation appeared meanwhile
	}
	if err != nil {
		l.probeSuccess = 0
		return
	}
	l.probeSuccess++
	if l.probeSuccess >= l.cfg.ProbeThreshold {
		l.probeSuccess = 0
		l.setStateLocked(stateRecovering)
	}
}
