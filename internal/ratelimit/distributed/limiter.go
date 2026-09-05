package distributed

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
)

// Key namespace (ADR-018 D5): a shared project hash tag colocates the two
// keys a composite admission touches in one Redis Cluster hash slot, so a
// future standalone -> Cluster move needs no namespace migration. Keys carry
// only internal UUIDs (never a raw virtual key, its digest, prefix, or any
// credential material). projectID is the request/key project UUID and the
// virtual key belongs to that project, so both scopes share the same tag.
const (
	keyPrefix = "gwrl:v1:{"
	keySuffix = "}"
)

// ScopeKey returns the Redis hash key for one virtual-key scope.
func ScopeKey(projectID, virtualKeyID string) string {
	return keyPrefix + projectID + "}:vk:" + virtualKeyID
}

// ProjectScopeKey returns the Redis hash key for one project scope.
func ProjectScopeKey(projectID string) string {
	return keyPrefix + projectID + "}:project"
}

// ErrDependency is the sentinel wrapped around Redis dependency failures
// surfaced by Core.AdmitCore. This raw core deliberately does NOT implement
// ratelimit.Limiter (its admission method is named AdmitCore, not Admit): the
// Slice B2 degraded/emergency wrapper owns the public Admit and converts these
// dependency errors into the ratelimit.Limiter contract (dependency failures
// never reach the data plane). Until then no production path uses this type.
var ErrDependency = errors.New("distributed rate limiter dependency failure")

// DependencyError wraps the concrete Redis/go-redis cause of a dependency
// failure so the future wrapper can classify timeout vs other errors.
type DependencyError struct {
	Err error
}

func (e *DependencyError) Error() string { return fmt.Sprintf("%v: %v", ErrDependency, e.Err) }
func (e *DependencyError) Unwrap() error { return e.Err }

// Is matches the ErrDependency sentinel so callers can classify any
// dependency failure with errors.Is without unwrapping the concrete cause.
func (e *DependencyError) Is(target error) bool { return target == ErrDependency }

// Config defines the composite scopes for the Redis-backed limiter core. RPM
// values of 0 disable that scope; an enabled scope must fit the Lua
// exact-integer safe range (MaxSafeRPM). IdleTTL (>= 1ms) drives the per-key
// PEXPIRE refresh on every admission (allow and reject). CommandTimeout bounds
// one Redis command context; the parent context cancellation is never confused
// with a dependency timeout (ADR-018 D8 classification; the wrapper relies on
// it).
type Config struct {
	KeyRPM         int
	ProjectRPM     int
	IdleTTL        time.Duration
	CommandTimeout time.Duration
}

func (c Config) validate() error {
	for scope, rpm := range map[string]int{"virtual key": c.KeyRPM, "project": c.ProjectRPM} {
		if err := ValidateScopeRPM(rpm); err != nil {
			return fmt.Errorf("distributed ratelimit %s: %w", scope, err)
		}
	}
	if c.IdleTTL < time.Millisecond {
		return errors.New("distributed ratelimit idle TTL must be at least 1ms (it is passed to Lua PEXPIRE as an integer ms value)")
	}
	if c.CommandTimeout <= 0 {
		return errors.New("distributed ratelimit command timeout must be positive")
	}
	return nil
}

// Core is the Redis token-bucket admission core: one atomic Lua composite
// admission over key + project scopes (ADR-018 D3/D4). It deliberately does
// NOT implement ratelimit.Limiter (its method is AdmitCore, so dependency
// errors can never escape through the public seam). The production
// distributed.Limiter wrapper implements ratelimit.Limiter and owns degraded /
// emergency / recovery handling.
type Core struct {
	client *redis.Client
	cfg    Config
	script *redis.Script
}

// New validates the configuration and prepares the Lua admission script.
func New(client *redis.Client, cfg Config) (*Core, error) {
	if client == nil {
		return nil, errors.New("distributed ratelimit redis client is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Core{
		client: client,
		cfg:    cfg,
		script: redis.NewScript(admissionScript),
	}, nil
}

// AdmitCore performs one composite distributed admission. It returns ctx
// errors (cancellation/deadline) as-is so a cancelled request consumes nothing
// and never triggers degraded behavior; Redis dependency failures are wrapped
// as *DependencyError for the future degraded wrapper. This mirrors ADR-018
// D8. The method is deliberately NOT named Admit: the raw core must not
// structurally satisfy ratelimit.Limiter (dependency errors must never escape
// through the public seam).
func (c *Core) AdmitCore(ctx context.Context, keyID, projectID string) (ratelimit.Decision, error) {
	// Cancellation is checked before the all-disabled fast path and before any
	// Redis command: a cancelled request never consumes quota and never issues
	// a Redis command.
	if err := ctx.Err(); err != nil {
		return ratelimit.Decision{}, err
	}
	if c.cfg.KeyRPM == 0 && c.cfg.ProjectRPM == 0 {
		// No enabled scope: admit without any dependency (Week 8 semantics for
		// an all-disabled registry).
		return ratelimit.Decision{Allowed: true}, nil
	}

	keys := []string{ScopeKey(projectID, keyID), ProjectScopeKey(projectID)}
	args := c.args()
	commandCtx, cancel := context.WithTimeout(ctx, c.cfg.CommandTimeout)
	defer cancel()

	raw, err := c.script.Run(commandCtx, c.client, keys, args...).Result()
	if err != nil {
		// Parent cancellation/deadline wins over a dependency classification:
		// a cancelled request must never be treated as a Redis dependency
		// failure (ADR-018 D8).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ratelimit.Decision{}, ctxErr
		}
		return ratelimit.Decision{}, &DependencyError{Err: err}
	}
	return parseDecision(raw)
}

// args builds the ARGV layout consumed by admissionScript:
//
//	[1] cost units | [2] key enabled | [3] key rpm | [4] key ttl ms
//	[5] proj enabled | [6] proj rpm | [7] proj ttl ms
func (c *Core) args() []any {
	return []any{
		strconv.Itoa(UnitsPerToken),
		boolFlag(c.cfg.KeyRPM > 0), strconv.Itoa(c.cfg.KeyRPM), strconv.Itoa(int(c.cfg.IdleTTL / time.Millisecond)),
		boolFlag(c.cfg.ProjectRPM > 0), strconv.Itoa(c.cfg.ProjectRPM), strconv.Itoa(int(c.cfg.IdleTTL / time.Millisecond)),
	}
}

func boolFlag(enabled bool) string {
	if enabled {
		return "1"
	}
	return "0"
}

// parseDecision converts the Lua reply {allowed, retry_after_ms, blocking}
// into a ratelimit.Decision and enforces its internal consistency (P2-5):
//
//	allowed=true  -> retry_after_ms == 0 and blocking scope == ""
//	allowed=false -> retry_after_ms >  0 and blocking scope is vk or proj
//
// The bundled Lua always satisfies this; an inconsistent reply is a bug and is
// rejected rather than silently producing an impossible Decision.
func parseDecision(raw any) (ratelimit.Decision, error) {
	row, ok := raw.([]any)
	if !ok || len(row) != 3 {
		return ratelimit.Decision{}, fmt.Errorf("unexpected distributed admission reply %T", raw)
	}
	allowed, ok := row[0].(int64)
	if !ok || (allowed != 0 && allowed != 1) {
		return ratelimit.Decision{}, fmt.Errorf("unexpected distributed admission allowed value %T", row[0])
	}
	retryMS, ok := row[1].(int64)
	if !ok || retryMS < 0 {
		return ratelimit.Decision{}, fmt.Errorf("unexpected distributed admission retry value %T", row[1])
	}
	scope, ok := row[2].(string)
	if !ok {
		return ratelimit.Decision{}, fmt.Errorf("unexpected distributed admission scope %T", row[2])
	}

	decision := ratelimit.Decision{Allowed: allowed == 1}
	switch scope {
	case "vk":
		decision.BlockingScope = ratelimit.ScopeVirtualKey
	case "proj":
		decision.BlockingScope = ratelimit.ScopeProject
	case "":
	default:
		return ratelimit.Decision{}, fmt.Errorf("unexpected distributed admission blocking scope %q", scope)
	}
	if decision.Allowed {
		if retryMS != 0 || decision.BlockingScope != "" {
			return ratelimit.Decision{}, fmt.Errorf("inconsistent allowed decision: retry=%d scope=%q", retryMS, scope)
		}
		return decision, nil
	}
	if retryMS <= 0 || (decision.BlockingScope != ratelimit.ScopeVirtualKey && decision.BlockingScope != ratelimit.ScopeProject) {
		return ratelimit.Decision{}, fmt.Errorf("inconsistent rejected decision: retry=%d scope=%q", retryMS, scope)
	}
	decision.RetryAfter = time.Duration(retryMS) * time.Millisecond
	return decision, nil
}
