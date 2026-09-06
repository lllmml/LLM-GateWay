package telemetry

import (
	"context"
	"strings"
)

// maxClientRequestIDBytes bounds a client X-Request-ID when it is used as log
// correlation metadata (ADR-019 D7). The bound matches the existing
// gateway_requests.trace_id column bound (200 bytes) and prevents an
// arbitrarily long header from causing log amplification.
const maxClientRequestIDBytes = 200

type clientRequestIDContextKey struct{}

// SanitizeClientRequestID validates a client X-Request-ID for use as
// correlation metadata only (ADR-019 D7). It returns the trimmed value and
// true when the value is non-empty, at most maxClientRequestIDBytes long, and
// free of control characters; otherwise it returns ("", false). An invalid
// value is dropped - it never rejects or alters the LLM request itself.
func SanitizeClientRequestID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxClientRequestIDBytes {
		return "", false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return value, true
}

// WithClientRequestID stores a sanitized client correlation ID in the context.
// It must only be called with a value SanitizeClientRequestID accepted.
func WithClientRequestID(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, clientRequestIDContextKey{}, value)
}

// ClientRequestIDFromContext returns the sanitized client correlation ID, if
// any, stored in the context. It is only ever surfaced as a log field
// (client_request_id), never as a metric label or a span attribute (ADR-019
// D7).
func ClientRequestIDFromContext(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(clientRequestIDContextKey{}).(string)
	return value, ok && value != ""
}
