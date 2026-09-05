// Package ratelimit implements the Week 8 single-instance in-memory rate
// limiter registry used by the Data Plane.
//
// The registry holds one token bucket per virtual key and one per project
// (golang.org/x/time/rate). Admission for a request is composite: the virtual
// key scope and the project scope must BOTH have a token available, and both
// decisions are made against the same clock snapshot.
//
// Concurrency model: the registry write lock is held for the whole composite
// decision (entry resolution/creation, eviction, reservation, and
// commit-or-cancel). This is the simplest deadlock-free way to keep the two
// participating entries current for the entire operation: while the lock is
// held no sweep or cap eviction can detach either entry, so an admission can
// never reserve against a limiter the registry has already replaced with a
// fresh full bucket. Eviction never targets an entry that the in-flight
// admission itself resolved (the earlier limb is protected), so both enabled
// scopes always coexist for one admission. Sweep and LRU eviction use the same
// registry-lock ordering and therefore skip nothing mid-admission; entries are
// only ever evicted between admissions.
//
// Registry growth is bounded: entries idle past a TTL are removed by a janitor
// goroutine, and when the entry cap is reached the least-recently-used entry
// is evicted before a new one is inserted. Eviction can only reset a bucket to
// a full burst (never corrupt counters), which is a documented, bounded
// over-admission window for the evicted scope only.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Scope names the identity dimension a limiter entry protects.
type Scope string

const (
	ScopeVirtualKey Scope = "virtual_key"
	ScopeProject    Scope = "project"
)

// Config defines the two limiter scopes and the registry lifecycle bounds.
// A requests-per-minute value of 0 disables that scope. Burst is a gateway
// policy choice, not an x/time/rate default: with N requests/minute the bucket
// refills at N/60 tokens per second and allows a burst of N (one full minute
// of instant allowance), then sustains N/minute.
type Config struct {
	KeyRPM        float64 // virtual-key scope, 0 disables
	ProjectRPM    float64 // project scope, 0 disables
	EntryCap      int     // max entries across both scopes
	IdleTTL       time.Duration
	SweepInterval time.Duration
	Now           func() time.Time // injectable clock for tests (defaults to time.Now)
}

// Decision is the outcome of one composite admission.
type Decision struct {
	Allowed bool
	// RetryAfter is set when Allowed is false: the duration the caller should
	// communicate to the client before it retries. It is computed from the
	// binding limiter's reservation delay, so it is never negative.
	RetryAfter time.Duration
	// BlockingScope names the enabled scope whose quota rejected the
	// admission, or "" when Allowed is true. It is a bounded, non-secret label
	// for operational visibility (key vs project rate limiting).
	BlockingScope Scope
}

type entry struct {
	lim      *rate.Limiter
	lastSeen time.Time // guarded by Registry.mu
}

type Registry struct {
	cfg     Config
	mu      sync.RWMutex // guards entries and every entry field
	entries map[string]*entry
	stop    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
}

// NewRegistry validates the configuration and starts the janitor goroutine.
// Call Close to stop the janitor. A configuration with both scopes disabled is
// valid and admits everything. EntryCap must be large enough for every enabled
// scope to coexist (at least one per enabled scope), otherwise composite
// admissions could never keep both limbs current.
func NewRegistry(cfg Config) (*Registry, error) {
	cfg = normalize(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	registry := &Registry{
		cfg:     cfg,
		entries: make(map[string]*entry),
		stop:    make(chan struct{}),
	}
	registry.wg.Add(1)
	go registry.runJanitor()
	return registry, nil
}

func normalize(cfg Config) Config {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg
}

func enabledScopes(cfg Config) int {
	enabled := 0
	if cfg.KeyRPM > 0 {
		enabled++
	}
	if cfg.ProjectRPM > 0 {
		enabled++
	}
	return enabled
}

func validate(cfg Config) error {
	for _, scope := range []struct {
		name string
		rpm  float64
	}{
		{name: "key", rpm: cfg.KeyRPM},
		{name: "project", rpm: cfg.ProjectRPM},
	} {
		if scope.rpm < 0 || (scope.rpm > 0 && (scope.rpm < 1 || scope.rpm != math.Trunc(scope.rpm))) {
			return fmt.Errorf("ratelimit %s requests per minute must be 0 or a whole number of at least 1", scope.name)
		}
	}
	if cfg.EntryCap < 1 {
		return errors.New("ratelimit entry cap must be positive")
	}
	// Every enabled scope needs a live entry during a composite admission.
	// A cap smaller than the enabled-scope count would force the registry to
	// evict one limb while resolving the other, which can never keep both
	// buckets current.
	if cfg.EntryCap < enabledScopes(cfg) {
		return fmt.Errorf("ratelimit entry cap %d is too small for %d enabled scopes; it must be at least 1 per enabled scope", cfg.EntryCap, enabledScopes(cfg))
	}
	if cfg.IdleTTL <= 0 {
		return errors.New("ratelimit idle TTL must be positive")
	}
	if cfg.SweepInterval <= 0 {
		return errors.New("ratelimit sweep interval must be positive")
	}
	return nil
}

// Close stops the janitor goroutine and waits for it to terminate. It is
// idempotent and safe to call concurrently with admissions.
func (r *Registry) Close() {
	r.once.Do(func() { close(r.stop) })
	r.wg.Wait()
}

func (r *Registry) now() time.Time {
	return r.cfg.Now()
}

// Admit checks that one token is available in both the virtual-key and the
// project scope for this request and consumes it when it is. The registry
// write lock is held across resolution, reservation and commit-or-cancel, so
// the two participating entries stay current for the whole operation and a
// rejected admission cancels every reservation (no token is lost in either
// scope). The clock snapshot is taken AFTER the lock is acquired: admissions
// are serialized, so the timestamps supplied to a limiter follow the registry
// serialization order. Reading the clock before the lock would let a waiter
// carry a stale (earlier) timestamp into ReserveN/CancelAt after another
// admission has already advanced the limiter, which would move its accounting
// time backward and double-count refill.
//
// ctx is checked before entering the critical section and again after the
// lock is acquired, so a request that is cancelled while waiting for the lock
// never consumes a token (ADR-018 D1/D8: cancellation must not consume quota).
// The Week 8 token-bucket admission semantics are otherwise unchanged.
func (r *Registry) Admit(ctx context.Context, keyID, projectID string) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}

	// now is the single snapshot for this admission's creation, lastSeen
	// refresh, reservation, DelayFrom, and cancellation decisions.
	now := r.now()

	type limb struct {
		entry *entry
		scope Scope
	}
	limbs := make([]limb, 0, 2)
	if r.cfg.KeyRPM > 0 {
		key := scopeKey(ScopeVirtualKey, keyID)
		limbs = append(limbs, limb{entry: r.getOrCreateLocked(key, now, r.cfg.KeyRPM, nil), scope: ScopeVirtualKey})
	}
	if r.cfg.ProjectRPM > 0 {
		// The project resolution must never evict the key limb resolved above:
		// both limbs of this admission must stay current for its whole
		// decision.
		protected := map[string]struct{}{}
		if r.cfg.KeyRPM > 0 {
			protected[scopeKey(ScopeVirtualKey, keyID)] = struct{}{}
		}
		key := scopeKey(ScopeProject, projectID)
		limbs = append(limbs, limb{entry: r.getOrCreateLocked(key, now, r.cfg.ProjectRPM, protected), scope: ScopeProject})
	}

	if len(limbs) == 0 {
		return Decision{Allowed: true}, nil
	}

	reservations := make([]*rate.Reservation, 0, len(limbs))
	var (
		maxDelay      time.Duration
		blockingScope Scope
	)
	for _, current := range limbs {
		reservation := current.entry.lim.ReserveN(now, 1)
		reservations = append(reservations, reservation)
		// DelayFrom(now) is used instead of Delay() so the decision is made
		// against the single clock snapshot taken for this admission.
		if delay := reservation.DelayFrom(now); delay > maxDelay {
			maxDelay = delay
			blockingScope = current.scope
		}
	}
	if maxDelay > 0 {
		for _, reservation := range reservations {
			reservation.CancelAt(now)
		}
		return Decision{Allowed: false, RetryAfter: maxDelay, BlockingScope: blockingScope}, nil
	}
	return Decision{Allowed: true}, nil
}

// getOrCreateLocked returns the entry for a scope/id, creating it (with a full
// bucket) when it does not exist. It must be called with r.mu held for
// writing. Creating an entry when the cap is reached evicts the
// least-recently-used entry first, excluding the protected keys (limbs of the
// in-flight admission that must stay current), so the map stays bounded.
func (r *Registry) getOrCreateLocked(key string, now time.Time, rpm float64, protected map[string]struct{}) *entry {
	if current := r.entries[key]; current != nil {
		current.lastSeen = now
		return current
	}
	if len(r.entries) >= r.cfg.EntryCap {
		r.evictLongestIdleLocked(protected)
	}
	created := &entry{lim: newLimiter(rpm), lastSeen: now}
	r.entries[key] = created
	return created
}

// evictLongestIdleLocked removes the single least-recently-used entry that is
// not protected. It must be called with r.mu held for writing. The protected
// set contains the scope keys resolved earlier in the same composite
// admission; evicting one of those mid-admission would detach a limb the
// admission is about to charge.
func (r *Registry) evictLongestIdleLocked(protected map[string]struct{}) {
	var (
		victimKey string
		victim    *entry
		oldest    time.Time
	)
	for candidateKey, candidate := range r.entries {
		if _, skip := protected[candidateKey]; skip {
			continue
		}
		if victim == nil || candidate.lastSeen.Before(oldest) {
			victimKey, victim, oldest = candidateKey, candidate, candidate.lastSeen
		}
	}
	if victim != nil {
		delete(r.entries, victimKey)
	}
}

func (r *Registry) runJanitor() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.sweep(r.now())
		}
	}
}

// sweep removes entries idle for longer than IdleTTL. It runs under the
// registry write lock so it can never detach an entry while an admission is
// resolving or charging it.
func (r *Registry) sweep(now time.Time) {
	idleBefore := now.Add(-r.cfg.IdleTTL)
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, candidate := range r.entries {
		if candidate.lastSeen.Before(idleBefore) {
			delete(r.entries, key)
		}
	}
}

func newLimiter(rpm float64) *rate.Limiter {
	// Refill at N tokens/minute; burst = N (gateway policy, documented in the
	// package comment). int(rpm) is safe because validation requires whole
	// requests-per-minute values of at least 1.
	return rate.NewLimiter(rate.Limit(rpm/60.0), int(rpm))
}

func scopeKey(scope Scope, id string) string {
	return string(scope) + "\x00" + id
}
