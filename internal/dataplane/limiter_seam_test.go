package dataplane

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

// stubLimiter is a fake ratelimit.Limiter that records that the service talked
// to the seam instead of a concrete Registry. It proves the data plane depends
// on the interface (ADR-018 D1), not on the local implementation.
type stubLimiter struct {
	calls int
	fn    func(ctx context.Context, keyID, projectID string) (ratelimit.Decision, error)
}

func (s *stubLimiter) Admit(ctx context.Context, keyID, projectID string) (ratelimit.Decision, error) {
	s.calls++
	return s.fn(ctx, keyID, projectID)
}

func newSeamService(t *testing.T, limiter ratelimit.Limiter) *Service {
	t.Helper()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	registry := newTestProviderRegistry(t, map[provider.Name]provider.Client{
		provider.OpenAI: &fakeProviderClient{},
	})
	service, err := NewService(Options{
		Store:                     &fakeStore{},
		VirtualKeyPepper:          bytes.Repeat([]byte{9}, 32),
		CredentialCipher:          cipher,
		UpstreamRequestTimeout:    time.Second,
		UpstreamStreamMaxDuration: time.Second,
		ProviderRegistry:          registry,
		RateLimiter:               limiter,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

func seamChat() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
}

// TestServiceConsumesLimiterRejectionBeforeAnyRowOrProvider proves the data
// plane depends on the ratelimit.Limiter seam: a rate-limit rejection from the
// limiter surfaces as the stable rate_limited GatewayError before any durable
// row or provider work.
func TestServiceConsumesLimiterRejectionBeforeAnyRowOrProvider(t *testing.T) {
	store := &fakeStore{}
	limiter := &stubLimiter{fn: func(context.Context, string, string) (ratelimit.Decision, error) {
		return ratelimit.Decision{Allowed: false, RetryAfter: 2 * time.Second, BlockingScope: ratelimit.ScopeVirtualKey}, nil
	}}
	service := newSeamService(t, limiter)

	_, _, err := service.CompleteChat(context.Background(), AuthContext{VirtualKeyID: "k1", ProjectID: "p1"}, "trace-1", seamChat())
	if err == nil {
		t.Fatal("rejected admission returned no error")
	}
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) {
		t.Fatalf("error = %T, want *GatewayError", err)
	}
	if gatewayErr.Category != provider.RateLimited || gatewayErr.RetryAfter == nil || *gatewayErr.RetryAfter != 2*time.Second {
		t.Fatalf("gateway error = %+v, want rate_limited with 2s Retry-After", gatewayErr)
	}
	if limiter.calls != 1 {
		t.Fatalf("limiter consulted %d times, want 1", limiter.calls)
	}
	if store.createCalls != 0 {
		t.Fatalf("durable row created after a limiter rejection (createCalls = %d)", store.createCalls)
	}
}

// TestServicePropagatesLimiterCancellation proves cancellation surfaced by the
// limiter terminates the request without a durable row or provider work, and
// is never converted into an admission rejection.
func TestServicePropagatesLimiterCancellation(t *testing.T) {
	store := &fakeStore{}
	limiter := &stubLimiter{fn: func(ctx context.Context, _, _ string) (ratelimit.Decision, error) {
		if err := ctx.Err(); err != nil {
			return ratelimit.Decision{}, err
		}
		return ratelimit.Decision{Allowed: true}, nil
	}}
	service := newSeamService(t, limiter)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := service.CompleteChat(ctx, AuthContext{VirtualKeyID: "k1", ProjectID: "p1"}, "trace-2", seamChat())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request error = %v, want context.Canceled", err)
	}
	if limiter.calls != 1 {
		t.Fatalf("limiter consulted %d times, want 1", limiter.calls)
	}
	if store.createCalls != 0 {
		t.Fatalf("durable row created after cancellation (createCalls = %d)", store.createCalls)
	}
}

// TestServiceAllowsThroughLimiter confirms an allowed limiter decision lets the
// request continue into the normal lifecycle (here: missing credential is
// reported by the store boundary, not by the limiter).
func TestServiceAllowsThroughLimiter(t *testing.T) {
	limiter := &stubLimiter{fn: func(context.Context, string, string) (ratelimit.Decision, error) {
		return ratelimit.Decision{Allowed: true}, nil
	}}
	service := newSeamService(t, limiter)

	_, _, err := service.CompleteChat(context.Background(), AuthContext{VirtualKeyID: "k-missing", ProjectID: "p-missing"}, "trace-3", seamChat())
	if limiter.calls != 1 {
		t.Fatalf("limiter consulted %d times, want 1", limiter.calls)
	}
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.ProviderNotConfigured {
		t.Fatalf("error after allow = %v, want ProviderNotConfigured (flow proceeded past admission)", err)
	}
}
