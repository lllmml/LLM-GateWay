package provider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter parses an HTTP Retry-After header value (delta-seconds or an
// HTTP-date, per RFC 9110) into a presence-preserving duration.
//
// Semantics:
//   - empty or malformed header -> nil (no usable hint)
//   - "Retry-After: 0" -> present, duration 0
//   - HTTP-date in the future -> present, positive duration until that time
//   - HTTP-date already passed -> present, duration 0 (retry immediately)
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
		seconds, err := strconv.Atoi(value)
		if err != nil {
			return nil
		}
		duration := time.Duration(seconds) * time.Second
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
