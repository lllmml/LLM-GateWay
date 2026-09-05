// Package ratelimit implements the Week 8 single-instance in-memory rate
// limiter registry used by the Data Plane.
//
// The registry holds one token bucket per virtual key and one per project
// (golang.org/x/time/rate). Admission for a request is composite: the virtual
// key scope and the project scope must BOTH have a token available. To keep
// that two-limb decision free of quota leakage under concurrency, every
// composite admission serializes the two involved entries with per-entry
// mutexes in a fixed order (virtual key first, then project) and holds both
// locks while reserving and while committing or cancelling, so the two limiter
// states can never interleave with another admission touching either of them.
//
// Registry growth is bounded: entries idle past a TTL are removed by a janitor
// goroutine, and when the entry cap is reached the least-recently-used entry
// is evicted before a new one is inserted. Eviction can only reset a bucket to
// a full burst (never corrupt counters), which is a documented, bounded
// over-admission window for that scope.
package ratelimit

import (
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
}

type entry struct {
	mu       sync.Mutex // serializes every reserve/commit-or-cancel touching this limiter
	lim      *rate.Limiter
	lastSeen time.Time // guarded by mu
}

type Registry struct {
	cfg     Config
	mu      sync.RWMutex // guards entries
	entries map[string]*entry
	stop    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
}

// NewRegistry validates the configuration and starts the janitor goroutine.
// Call Close to stop the janitor. A configuration with both scopes disabled is
// valid and admits everything.
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
// project scope for this request and consumes it when it is. Fixed lock order:
// key entry first, then project entry, never the reverse, so composite
// admissions cannot deadlock. A rejected admission cancels every reservation,
// so neither scope loses a token.
func (r *Registry) Admit(keyID, projectID string) Decision {
	now := r.now()
	keyEntry := r.getOrCreate(ScopeVirtualKey, keyID, now, r.cfg.KeyRPM)
	projectEntry := r.getOrCreate(ScopeProject, projectID, now, r.cfg.ProjectRPM)

	locked := make([]*entry, 0, 2)
	if keyEntry != nil {
		keyEntry.mu.Lock()
		locked = append(locked, keyEntry)
	}
	if projectEntry != nil {
		projectEntry.mu.Lock()
		locked = append(locked, projectEntry)
	}
	defer func() {
		for index := len(locked) - 1; index >= 0; index-- {
			locked[index].mu.Unlock()
		}
	}()

	if len(locked) == 0 {
		return Decision{Allowed: true}
	}
	for _, current := range locked {
		current.lastSeen = now
	}

	reservations := make([]*rate.Reservation, 0, len(locked))
	var maxDelay time.Duration
	for _, current := range locked {
		reservation := current.lim.ReserveN(now, 1)
		reservations = append(reservations, reservation)
		// DelayFrom(now) is used instead of Delay() so the decision is made
		// against the single clock snapshot taken for this admission.
		if delay := reservation.DelayFrom(now); delay > maxDelay {
			maxDelay = delay
		}
	}
	if maxDelay > 0 {
		for _, reservation := range reservations {
			reservation.CancelAt(now)
		}
		return Decision{Allowed: false, RetryAfter: maxDelay}
	}
	return Decision{Allowed: true}
}

// getOrCreate returns the entry for a scope/id, creating it (with a full
// bucket) when it does not exist. A disabled scope returns nil. When the entry
// cap is reached the least-recently-used entry is evicted first so the map
// stays bounded.
func (r *Registry) getOrCreate(scope Scope, id string, now time.Time, rpm float64) *entry {
	if rpm <= 0 {
		return nil
	}
	key := scopeKey(scope, id)
	r.mu.RLock()
	current := r.entries[key]
	r.mu.RUnlock()
	if current != nil {
		return current
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.entries[key]; current != nil {
		return current
	}
	if len(r.entries) >= r.cfg.EntryCap {
		r.evictLongestIdleLocked(now)
	}
	created := &entry{lim: newLimiter(rpm), lastSeen: now}
	r.entries[key] = created
	return created
}

// evictLongestIdleLocked removes the single least-recently-used entry. It must
// be called with r.mu held for writing. The victim's mutex is taken so an
// in-flight admission on that entry finishes before it is discarded.
func (r *Registry) evictLongestIdleLocked(now time.Time) {
	var (
		victimKey string
		victim    *entry
		oldest    time.Time
	)
	for candidateKey, candidate := range r.entries {
		candidate.mu.Lock()
		idleSince := candidate.lastSeen
		candidate.mu.Unlock()
		if victim == nil || idleSince.Before(oldest) {
			victimKey, victim, oldest = candidateKey, candidate, idleSince
		}
	}
	if victim == nil {
		return
	}
	victim.mu.Lock()
	delete(r.entries, victimKey)
	victim.mu.Unlock()
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

// sweep removes entries idle for longer than IdleTTL. It snapshots candidates
// under a read lock and re-checks each one under its entry mutex before
// deleting, so an entry refreshed between the snapshot and the delete survives.
func (r *Registry) sweep(now time.Time) {
	idleBefore := now.Add(-r.cfg.IdleTTL)
	candidates := make([]string, 0)
	r.mu.RLock()
	for candidateKey, candidate := range r.entries {
		candidate.mu.Lock()
		idle := candidate.lastSeen.Before(idleBefore)
		candidate.mu.Unlock()
		if idle {
			candidates = append(candidates, candidateKey)
		}
	}
	r.mu.RUnlock()
	if len(candidates) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, candidateKey := range candidates {
		candidate, ok := r.entries[candidateKey]
		if !ok {
			continue
		}
		candidate.mu.Lock()
		if candidate.lastSeen.Before(idleBefore) {
			delete(r.entries, candidateKey)
		}
		candidate.mu.Unlock()
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
