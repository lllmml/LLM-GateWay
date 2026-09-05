package provider

import (
	"math"
	"net/http"
	"strings"
	"time"
)

// maxRetryAfterSeconds is the largest delta-seconds value that can be
// represented as a time.Duration without overflow (math.MaxInt64 nanoseconds).
// Hints beyond it are clamped to this value instead of wrapping negative.
const maxRetryAfterSeconds = math.MaxInt64 / int64(time.Second)

// ParseRetryAfter parses an HTTP Retry-After header value (delta-seconds or an
// HTTP-date, per RFC 9110) into a presence-preserving duration.
//
// Semantics:
//   - empty or malformed header -> nil (no usable hint)
//   - "Retry-After: 0" -> present, duration 0
//   - HTTP-date in the future -> present, positive duration until that time
//   - HTTP-date already passed -> present, duration 0 (retry immediately)
//   - delta-seconds that overflow time.Duration -> present, clamped to the
//     largest representable positive duration (never a negative wrap), which
//     cannot fit a normal gateway retry budget and therefore stops retrying
//
// The explicit now parameter keeps the function deterministic in tests; it
// never reads the wall clock itself. Adapters parse the wire header here and
// attach the result to provider.Error.RetryAfter; the executor decides whether
// to wait and retry.
func ParseRetryAfter(header string, now time.Time) *time.Duration {
	value := strings.TrimSpace(header)
	if value == "" {
		return nil
	}
	if isDeltaSeconds(value) {
		duration, ok := parseDeltaSeconds(value)
		if !ok {
			return nil
		}
		return &duration
	}
	if when, err := http.ParseTime(value); err == nil {
		duration := time.Duration(0)
		if when.After(now) {
			duration = when.Sub(now)
		}
		return &duration
	}
	return nil
}

// parseDeltaSeconds converts a digit-only delta-seconds string into a
// time.Duration without machine-int-width or overflow hazards. It parses as an
// unsigned value with explicit saturation: any value beyond the largest
// representable duration is clamped to that maximum rather than wrapping.
func parseDeltaSeconds(value string) (time.Duration, bool) {
	var seconds uint64
	for index := 0; index < len(value); index++ {
		digit := uint64(value[index] - '0')
		if seconds > (uint64(maxRetryAfterSeconds)-digit)/10 {
			return time.Duration(maxRetryAfterSeconds) * time.Second, true
		}
		seconds = seconds*10 + digit
	}
	return time.Duration(seconds) * time.Second, true
}

func isDeltaSeconds(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}
