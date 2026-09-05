package ratelimit

import "context"

// Limiter is the Week 9 admission seam (ADR-018 D1). The data plane depends on
// this interface, never on a concrete local or distributed implementation.
//
// Contract:
//   - Allowed / rejected decisions are expressed through Decision.
//   - An error is returned ONLY to propagate downstream cancellation or
//     deadline (context.Canceled / context.DeadlineExceeded). A cancelled
//     request must terminate: it must not consume quota, must not trigger
//     degraded mode, and no provider/durable work may follow.
//   - Dependency failures (e.g. Redis unavailable) never surface here as an
//     error: implementations that depend on an external limiter handle them
//     internally (degraded mode + bounded emergency fallback, ADR-018 D6-D8).
type Limiter interface {
	Admit(ctx context.Context, keyID, projectID string) (Decision, error)
}
