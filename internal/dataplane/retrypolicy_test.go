package dataplane

import (
	"context"
	"errors"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

func TestRetryBackoffWindow(t *testing.T) {
	const base = 100 * time.Millisecond
	tests := []struct {
		name    string
		max     time.Duration
		attempt int
		want    time.Duration
	}{
		{name: "first window is base", max: 2 * time.Second, attempt: 0, want: 100 * time.Millisecond},
		{name: "doubles per retry", max: 2 * time.Second, attempt: 1, want: 200 * time.Millisecond},
		{name: "capped by max", max: 250 * time.Millisecond, attempt: 3, want: 250 * time.Millisecond},
		{name: "large retry count stays bounded", max: 2 * time.Second, attempt: 100, want: 2 * time.Second},
		{name: "negative count treated as first", max: 2 * time.Second, attempt: -1, want: 100 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := retryBackoffWindow(base, test.max, test.attempt); got != test.want {
				t.Fatalf("retryBackoffWindow(%v, %v, %d) = %v, want %v", base, test.max, test.attempt, got, test.want)
			}
		})
	}
}

func TestIsRetryableGatewayErrorWhitelist(t *testing.T) {
	dialFailure := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name string
		err  *GatewayError
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "provider 429", err: &GatewayError{Category: provider.ProviderRateLimited, StatusCode: 429}, want: true},
		{name: "provider rate limited without 429 status", err: &GatewayError{Category: provider.ProviderRateLimited, StatusCode: 500}, want: false},
		{name: "provider 500", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 500}, want: true},
		{name: "provider 502", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 502}, want: true},
		{name: "provider 503", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 503}, want: true},
		{name: "provider 529 overload", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 529}, want: true},
		{name: "provider 501 not whitelisted", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 501}, want: false},
		{name: "provider 505 not whitelisted", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 505}, want: false},
		{name: "provider 508 unknown not whitelisted", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 508}, want: false},
		{name: "provider redirect 300 not whitelisted", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 300}, want: false},
		{name: "provider redirect 307 not whitelisted", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 307}, want: false},
		{name: "provider redirect 308 not whitelisted", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 308}, want: false},
		{name: "provider 401 must never retry", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 401}, want: false},
		{name: "provider 402 must never retry", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 402}, want: false},
		{name: "provider 403 must never retry", err: &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 403}, want: false},
		{name: "provider 504 stays under no-retry timeout", err: &GatewayError{Category: provider.ProviderTimeout, StatusCode: 504}, want: false},
		{name: "provider invalid request 400", err: &GatewayError{Category: provider.ProviderInvalidReq, StatusCode: 400}, want: false},
		{name: "stream interrupted", err: &GatewayError{Category: provider.StreamInterrupted}, want: false},
		{name: "local rate limited", err: &GatewayError{Category: provider.RateLimited, StatusCode: 429}, want: false},
		{name: "dial failure no http status", err: &GatewayError{Category: provider.ProviderUnavailable, Err: dialFailure}, want: true},
		{name: "dns failure no http status", err: &GatewayError{Category: provider.ProviderUnavailable, Err: &net.DNSError{Err: "no such host", IsNotFound: true}}, want: true},
		{name: "connection reset no http status", err: &GatewayError{Category: provider.ProviderUnavailable, Err: errors.New("read: connection reset")}, want: false},
		{name: "plain unknown error no http status", err: &GatewayError{Category: provider.ProviderUnavailable, Err: errors.New("weird")}, want: false},
		{name: "internal error", err: &GatewayError{Category: provider.InternalError}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isRetryableGatewayError(test.err); got != test.want {
				t.Fatalf("isRetryableGatewayError(%+v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestRetryableProviderStatus(t *testing.T) {
	for _, status := range []int{500, 502, 503, 529} {
		if !retryableProviderStatus(status) {
			t.Fatalf("retryableProviderStatus(%d) = false, want true", status)
		}
	}
	for _, status := range []int{501, 504, 505, 507, 508, 599, 429, 400, 401, 403, 300, 301, 307, 308} {
		if retryableProviderStatus(status) {
			t.Fatalf("retryableProviderStatus(%d) = true, want false", status)
		}
	}
}

func TestRetryDelayPolicy(t *testing.T) {
	store := &fakeStore{}
	service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{
		provider.OpenAI: &retryProbeClient{result: okResult()},
	}, nil)

	clock := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return clock }
	deadline := clock.Add(100 * time.Millisecond)

	tests := []struct {
		name    string
		hint    *time.Duration
		retries int
		wantNil bool
		want    time.Duration
	}{
		{name: "hint larger than remaining budget", hint: durationPtr(200 * time.Millisecond), retries: 0, wantNil: true},
		{name: "hint exactly remaining budget", hint: durationPtr(100 * time.Millisecond), retries: 0, wantNil: true},
		{name: "hint fits budget", hint: durationPtr(50 * time.Millisecond), retries: 0, want: 50 * time.Millisecond},
		{name: "present zero hint retries immediately", hint: durationPtr(0), retries: 0, want: 0},
		{name: "absent hint uses jittered backoff", hint: nil, retries: 0, want: 50 * time.Millisecond},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Deterministic jitter: half the window (100ms for the first retry).
			service.jitter = func(window int64) int64 { return window / 2 }
			got := service.retryDelay(test.hint, test.retries, deadline)
			if test.wantNil {
				if got != nil {
					t.Fatalf("retryDelay = %v, want nil (no budget for another attempt)", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("retryDelay = nil, want a wait")
			}
			if *got != test.want {
				t.Fatalf("retryDelay = %v, want %v", *got, test.want)
			}
		})
	}

	t.Run("expired phase deadline never retries", func(t *testing.T) {
		service.jitter = func(int64) int64 { return 0 }
		if got := service.retryDelay(durationPtr(0), 0, clock.Add(-time.Millisecond)); got != nil {
			t.Fatalf("retryDelay past deadline = %v, want nil", *got)
		}
	})

	t.Run("budget exhaustion and cancellation disable retrying end to end", func(t *testing.T) {
		// retryAllowed must be false once the attempt cap or context is gone.
		service.now = func() time.Time { return clock.Add(time.Second) }
		if service.retryAllowed(context.Background(), &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 500}, clock.Add(time.Second), 0) {
			t.Fatal("retryAllowed true after phase deadline")
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if service.retryAllowed(canceled, &GatewayError{Category: provider.ProviderUnavailable, StatusCode: 500}, clock.Add(time.Hour), 0) {
			t.Fatal("retryAllowed true after caller cancellation")
		}
	})
}

func TestFormatRetryAfter(t *testing.T) {
	maxDuration := time.Duration(math.MaxInt64)
	maxSeconds := maxDuration / time.Second
	if maxDuration%time.Second != 0 {
		maxSeconds++
	}
	tests := []struct {
		duration time.Duration
		want     string
	}{
		{duration: 0, want: "0"},
		{duration: 500 * time.Millisecond, want: "1"},
		{duration: time.Second, want: "1"},
		{duration: 1500 * time.Millisecond, want: "2"},
		{duration: time.Hour, want: "3600"},
		// Near-MaxInt64 must ceil without wrapping negative or overflowing.
		{duration: maxDuration, want: strconv.FormatInt(int64(maxSeconds), 10)},
	}
	for _, test := range tests {
		got := formatRetryAfter(test.duration)
		if got != test.want {
			t.Fatalf("formatRetryAfter(%v) = %q, want %q", test.duration, got, test.want)
		}
		if strings.HasPrefix(got, "-") {
			t.Fatalf("formatRetryAfter(%v) emitted a negative header %q", test.duration, got)
		}
	}
}

func TestWaitWithContextCancellationWins(t *testing.T) {
	t.Run("already cancelled context never waits", func(t *testing.T) {
		store := &fakeStore{}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{
			provider.OpenAI: &retryProbeClient{result: okResult()},
		}, nil)

		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if service.waitWithContext(cancelled, 0) {
			t.Fatal("waitWithContext returned true for an already-cancelled context")
		}
		if service.waitWithContext(cancelled, time.Hour) {
			t.Fatal("waitWithContext returned true for an already-cancelled context with a long wait")
		}
	})

	t.Run("zero wait on a live context fires immediately", func(t *testing.T) {
		store := &fakeStore{}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{
			provider.OpenAI: &retryProbeClient{result: okResult()},
		}, nil)
		if !service.waitWithContext(context.Background(), 0) {
			t.Fatal("waitWithContext returned false for a live context with a zero wait")
		}
	})
}

func TestRetryLoopChecksCancellationAfterWait(t *testing.T) {
	// The retry loop must not start another paid attempt when the request is
	// cancelled, even if a wait had already been computed and fired.
	store, rawKey := newAuthorizedStore(t)
	client := &retryProbeClient{errors: []error{errProviderUnavailable500()}, result: okResult()}
	service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
		options.RetryMaxRetries = 5
	})
	// A zero wait makes the wait itself trivially "fire"; the post-wait
	// cancellation check is what must stop the loop.
	service.jitter = zeroJitter

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = service.CompleteChat(cancelled, auth, "", chatRequest())
	if err == nil {
		t.Fatal("complete chat returned nil error for a cancelled request")
	}
	if client.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (pre-cancelled request must not retry)", client.calls)
	}
}
