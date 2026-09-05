// Package distributed implements the Week 9 Slice B Redis-backed rate limit
// algorithm core (ADR-018 D2-D5). This slice deliberately delivers only the
// algorithm + runtime-assumption foundation; the degraded/emergency wrapper
// that exposes a ratelimit.Limiter to the data plane is a later slice.
package distributed

import "fmt"

// Fixed-point integer accounting shared by the Go validation layer and the
// Redis Lua admission script (ADR-018 D2/D7 mandatory invariant #3).
//
// Redis Lua 5.1 numbers are IEEE-754 doubles, NOT Go int64. Every integer that
// participates in Lua arithmetic must therefore stay within the exact-integer
// range (< 2^53) and no config may rely on math.MaxInt64.
//
// Units: 1 token = 60000 units and a scope with N requests/minute holds
// capacity = N*60000 units, refills at N units/ms, and one request costs 60000
// units. Elapsed time is clamped to 60000 ms so a single refill never exceeds
// capacity; token additions are additionally capped by capacity - tokens, so
// no intermediate value exceeds capacity < 2^53.
const (
	// UnitsPerToken is the integer unit value of one request token
	// (milliseconds per minute). It is also the request cost in units.
	UnitsPerToken = 60000
	// MaxElapsedMS clamps a single refill computation to one minute, so the
	// refill term can never exceed capacity (N * 60000 units).
	MaxElapsedMS = UnitsPerToken
	// exactIntegerLimit is 2^53, the largest double-precision integer that is
	// exactly representable (Redis Lua numbers).
	exactIntegerLimit = int64(1) << 53
)

// MaxSafeRPM is the largest integer RPM whose capacity (RPM*60000 units) and
// every arithmetic intermediate stay below 2^53, plus a margin so that Lua
// integer division (used for the integer retry-after ceiling) can never round
// across an integer boundary. Config validation rejects any RPM above it.
func MaxSafeRPM() int {
	return int((exactIntegerLimit - 1) / UnitsPerToken)
}

// ValidateScopeRPM validates one scope's requests-per-minute value. 0 disables
// the scope (must be enabled explicitly); an enabled scope must be a whole
// number of at least 1 and at most MaxSafeRPM so every integer that reaches
// Lua arithmetic is exactly representable (mandatory invariant #3).
func ValidateScopeRPM(rpm int) error {
	if rpm == 0 {
		return nil // disabled
	}
	if rpm < 1 {
		return fmt.Errorf("requests per minute must be 0 (disabled) or at least 1, got %d", rpm)
	}
	if rpm > MaxSafeRPM() {
		return fmt.Errorf("requests per minute %d exceeds the Lua exact-integer safe maximum %d", rpm, MaxSafeRPM())
	}
	return nil
}
