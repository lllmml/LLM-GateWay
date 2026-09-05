package dataplane

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/openai"
	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

func zeroJitter(int64) int64 { return 0 }

func durationPtr(duration time.Duration) *time.Duration { return &duration }

// newServiceWithReliability builds a Service with the standard test store
// wiring plus Week 8 reliability options supplied through extra. RetryMaxRetries
// defaults to 0 (disabled) unless extra sets it, so existing single-attempt
// behavior stays the default here.
func newServiceWithReliability(t *testing.T, store Store, clients map[provider.Name]provider.Client, extra func(*Options)) *Service {
	t.Helper()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	registry, err := provider.NewRegistry(clients)
	if err != nil {
		t.Fatalf("new provider registry: %v", err)
	}
	options := Options{
		Store:                     store,
		VirtualKeyPepper:          bytes.Repeat([]byte{9}, 32),
		CredentialCipher:          cipher,
		UpstreamRequestTimeout:    time.Second,
		UpstreamStreamMaxDuration: time.Second,
		ProviderRegistry:          registry,
	}
	if extra != nil {
		extra(&options)
	}
	service, err := NewService(options)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

// retryProbeClient fails the first len(errors) calls with the given provider
// errors and then returns a success result.
type retryProbeClient struct {
	calls  int
	errors []error
	result provider.Result
}

func (c *retryProbeClient) CompleteChat(context.Context, provider.ChatRequest, provider.Credential) (provider.Result, error) {
	c.calls++
	if c.calls <= len(c.errors) {
		return provider.Result{}, c.errors[c.calls-1]
	}
	return c.result, nil
}

func errProviderUnavailable500() error {
	return &provider.Error{Category: provider.ProviderUnavailable, StatusCode: http.StatusInternalServerError, Message: "boom"}
}

func errProviderRateLimited(retryAfter time.Duration) error {
	return &provider.Error{
		Category:   provider.ProviderRateLimited,
		StatusCode: http.StatusTooManyRequests,
		Message:    "slow down",
		RetryAfter: durationPtr(retryAfter),
	}
}

func TestRetryCountOffByOneTable(t *testing.T) {
	tests := []struct {
		name        string
		maxRetries  int
		errors      []error
		wantCalls   int
		wantRetries int
		wantOK      bool
	}{
		{name: "max 0 disables retry on retryable 429", maxRetries: 0, errors: []error{errProviderRateLimited(0)}, wantCalls: 1, wantRetries: 0, wantOK: false},
		{name: "max 0 disables retry on 500", maxRetries: 0, errors: []error{errProviderUnavailable500()}, wantCalls: 1, wantRetries: 0, wantOK: false},
		{name: "max 1 one failure then success", maxRetries: 1, errors: []error{errProviderUnavailable500()}, wantCalls: 2, wantRetries: 1, wantOK: true},
		{name: "max 1 two failures exhausts", maxRetries: 1, errors: []error{errProviderUnavailable500(), errProviderUnavailable500()}, wantCalls: 2, wantRetries: 1, wantOK: false},
		{name: "max 2 two failures then success", maxRetries: 2, errors: []error{errProviderUnavailable500(), errProviderUnavailable500()}, wantCalls: 3, wantRetries: 2, wantOK: true},
		{name: "max 2 three failures exhausts", maxRetries: 2, errors: []error{errProviderUnavailable500(), errProviderUnavailable500(), errProviderUnavailable500()}, wantCalls: 3, wantRetries: 2, wantOK: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, rawKey := newAuthorizedStore(t)
			client := &retryProbeClient{errors: test.errors, result: okResult()}
			service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
				options.RetryMaxRetries = test.maxRetries
			})
			service.jitter = zeroJitter

			auth, err := service.Authenticate(context.Background(), rawKey)
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			_, record, err := service.CompleteChat(context.Background(), auth, "", chatRequest())
			if test.wantOK && err != nil {
				t.Fatalf("complete chat: %v", err)
			}
			if !test.wantOK {
				gatewayErr, ok := err.(*GatewayError)
				if !ok {
					t.Fatalf("error type = %T", err)
				}
				if gatewayErr.Category != provider.ProviderUnavailable && gatewayErr.Category != provider.ProviderRateLimited {
					t.Fatalf("category = %q", gatewayErr.Category)
				}
			}
			if client.calls != test.wantCalls {
				t.Fatalf("provider calls = %d, want %d", client.calls, test.wantCalls)
			}
			if store.createCalls != 1 || store.finalizeCalls != 1 {
				t.Fatalf("create=%d finalize=%d, want exactly 1 each", store.createCalls, store.finalizeCalls)
			}
			if int(record.RetryCount) != test.wantRetries {
				t.Fatalf("record retry count = %d, want %d", record.RetryCount, test.wantRetries)
			}
			if int(store.lastFinalize.RetryCount) != test.wantRetries {
				t.Fatalf("persisted retry count = %d, want %d", store.lastFinalize.RetryCount, test.wantRetries)
			}
		})
	}
}

func TestNonRetryableFailuresRunExactlyOneAttempt(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantCategory provider.ErrorCategory
	}{
		{name: "provider invalid request", err: &provider.Error{Category: provider.ProviderInvalidReq, StatusCode: http.StatusBadRequest}, wantCategory: provider.ProviderInvalidReq},
		{name: "provider 401 classified unavailable", err: &provider.Error{Category: provider.ProviderUnavailable, StatusCode: http.StatusUnauthorized}, wantCategory: provider.ProviderUnavailable},
		{name: "provider timeout", err: &provider.Error{Category: provider.ProviderTimeout, StatusCode: http.StatusGatewayTimeout}, wantCategory: provider.ProviderTimeout},
		{name: "unknown transport error", err: &provider.Error{Category: provider.ProviderUnavailable, Err: errors.New("connection reset")}, wantCategory: provider.ProviderUnavailable},
		{name: "non provider error", err: errors.New("weird"), wantCategory: provider.ProviderUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, rawKey := newAuthorizedStore(t)
			client := &retryProbeClient{errors: []error{test.err}}
			service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
				options.RetryMaxRetries = 5
			})
			service.jitter = zeroJitter

			auth, err := service.Authenticate(context.Background(), rawKey)
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			_, _, err = service.CompleteChat(context.Background(), auth, "", chatRequest())
			gatewayErr, ok := err.(*GatewayError)
			if !ok {
				t.Fatalf("error type = %T", err)
			}
			if gatewayErr.Category != test.wantCategory {
				t.Fatalf("category = %q, want %q", gatewayErr.Category, test.wantCategory)
			}
			if client.calls != 1 {
				t.Fatalf("provider calls = %d, want exactly 1 (never retried)", client.calls)
			}
			if int(store.lastFinalize.RetryCount) != 0 {
				t.Fatalf("persisted retry count = %d, want 0", store.lastFinalize.RetryCount)
			}
		})
	}
}

func TestTransportDialAndDNSFailuresRetryButAmbiguousDoNot(t *testing.T) {
	dialFailure := &provider.Error{
		Category: provider.ProviderUnavailable,
		Err:      &url.Error{Op: "Post", URL: "https://provider", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}},
	}
	dnsFailure := &provider.Error{
		Category: provider.ProviderUnavailable,
		Err:      &net.DNSError{Err: "no such host", Name: "api.example.com", IsNotFound: true},
	}

	tests := []struct {
		name      string
		err       error
		wantCalls int
		wantOK    bool
	}{
		{name: "dial failure retries then succeeds", err: dialFailure, wantCalls: 2, wantOK: true},
		{name: "dns failure retries then succeeds", err: dnsFailure, wantCalls: 2, wantOK: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, rawKey := newAuthorizedStore(t)
			client := &retryProbeClient{errors: []error{test.err}, result: okResult()}
			service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
				options.RetryMaxRetries = 1
			})
			service.jitter = zeroJitter

			auth, err := service.Authenticate(context.Background(), rawKey)
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			_, _, err = service.CompleteChat(context.Background(), auth, "", chatRequest())
			if test.wantOK && err != nil {
				t.Fatalf("complete chat: %v", err)
			}
			if client.calls != test.wantCalls {
				t.Fatalf("provider calls = %d, want %d", client.calls, test.wantCalls)
			}
		})
	}
}

func TestRetryAfterHintRespectedAndBudgetClamped(t *testing.T) {
	t.Run("hint larger than remaining budget stops retrying", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		client := &retryProbeClient{errors: []error{errProviderRateLimited(time.Hour)}, result: okResult()}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 3
		})
		service.jitter = zeroJitter

		auth, err := service.Authenticate(context.Background(), rawKey)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		_, _, err = service.CompleteChat(context.Background(), auth, "", chatRequest())
		gatewayErr, ok := err.(*GatewayError)
		if !ok {
			t.Fatalf("error type = %T", err)
		}
		if gatewayErr.Category != provider.ProviderRateLimited {
			t.Fatalf("category = %q, want %q", gatewayErr.Category, provider.ProviderRateLimited)
		}
		if gatewayErr.RetryAfter == nil || *gatewayErr.RetryAfter != time.Hour {
			t.Fatalf("RetryAfter = %v, want 1h", gatewayErr.RetryAfter)
		}
		if client.calls != 1 {
			t.Fatalf("provider calls = %d, want 1 (hint cannot fit budget)", client.calls)
		}
	})

	t.Run("present zero hint retries immediately", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		client := &retryProbeClient{errors: []error{errProviderRateLimited(0)}, result: okResult()}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 1
		})
		service.jitter = zeroJitter

		auth, err := service.Authenticate(context.Background(), rawKey)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if _, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest()); err != nil {
			t.Fatalf("complete chat: %v", err)
		}
		if client.calls != 2 {
			t.Fatalf("provider calls = %d, want 2", client.calls)
		}
	})
}

// stepClock is an injectable monotonic clock that tests advance deterministically.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStepClock(at time.Time) *stepClock {
	return &stepClock{now: at}
}

func (c *stepClock) current() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

// clockAdvancingClient advances the injected service clock by step on every
// attempt and returns a retryable 500. The attempt loop fixes its phase
// deadline from the clock when the first attempt starts; because the client
// moves the clock forward by more than the remaining budget after the second
// attempt, the budget-sharing decision is fully deterministic with no real
// sleeps. If each attempt were granted a fresh timeout (attempts x timeout),
// the decision would keep passing and far more attempts would run.
type clockAdvancingClient struct {
	clock *stepClock
	step  time.Duration
	err   error
	calls int
}

func (c *clockAdvancingClient) CompleteChat(context.Context, provider.ChatRequest, provider.Credential) (provider.Result, error) {
	c.calls++
	c.clock.advance(c.step)
	return provider.Result{}, c.err
}

func TestRetriesShareOverallBudgetNeverMultiplyTimeout(t *testing.T) {
	clock := newStepClock(time.Now())
	store, rawKey := newAuthorizedStore(t)
	client := &clockAdvancingClient{clock: clock, step: 700 * time.Millisecond, err: errProviderUnavailable500()}
	service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
		options.UpstreamRequestTimeout = time.Second
		options.RetryMaxRetries = 5
		options.RetryBackoffMax = time.Nanosecond
	})
	service.jitter = zeroJitter
	service.now = clock.current

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", chatRequest())

	gatewayErr, ok := err.(*GatewayError)
	if !ok || gatewayErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v, want provider_unavailable", err)
	}
	// Attempt 1 fails at t0+700ms (clock advanced); the shared phase deadline
	// (t0+1s) still has 300ms left, so one retry runs. Attempt 2 advances the
	// clock to t0+1.4s, past the deadline, so no third attempt starts. A
	// per-attempt fresh timeout would have allowed ~5 further attempts.
	if client.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (budget-shared attempts)", client.calls)
	}
	if int(store.lastFinalize.RetryCount) != 1 {
		t.Fatalf("persisted retry count = %d, want 1", store.lastFinalize.RetryCount)
	}
}

func TestClientCancellationStopsRetriesDuringBackoff(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &retryProbeClient{errors: []error{errProviderUnavailable500()}, result: okResult()}
	service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
		options.RetryMaxRetries = 5
	})
	// A fixed non-trivial wait that still fits the ~1s phase budget, so the
	// retry loop provably reaches its backoff-wait decision before any
	// cancellation.
	service.jitter = func(int64) int64 { return int64(900 * time.Millisecond) }

	// Cancel synchronously inside the onRetryWait hook, which fires exactly at
	// the wait boundary: the loop is provably past its retry decision and
	// about to block, and waitWithContext must refuse to wait for a cancelled
	// request instead of sleeping.
	hookRan := false
	ctx, cancel := context.WithCancel(context.Background())
	service.onRetryWait = func() {
		hookRan = true
		cancel()
	}

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(ctx, auth, "", chatRequest())
	if err == nil {
		t.Fatal("complete chat returned nil error after cancellation")
	}
	if !hookRan {
		t.Fatal("retry loop never reached its backoff-wait boundary")
	}
	if client.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (cancellation during backoff must not retry)", client.calls)
	}
	if int(store.lastFinalize.RetryCount) != 0 {
		t.Fatalf("persisted retry count = %d, want 0", store.lastFinalize.RetryCount)
	}
}

func TestStreamOpenRetriesOnlyUntilEstablished(t *testing.T) {
	t.Run("open 500 then success retries and finalizes succeeded", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		streamCalls := 0
		client := &fakeProviderClient{}
		client.streamFactory = func(ctx context.Context) (provider.StreamResult, error) {
			streamCalls++
			if streamCalls == 1 {
				return provider.StreamResult{}, errProviderUnavailable500()
			}
			return provider.StreamResult{
				Stream:         &fakeChatStream{events: []provider.StreamEvent{{Data: []byte("[DONE]"), Done: true}}},
				UpstreamStatus: http.StatusOK,
			}, nil
		}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 1
		})
		service.jitter = zeroJitter

		auth, err := service.Authenticate(context.Background(), rawKey)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		record, err := service.StreamChat(context.Background(), auth, "", streamChatRequest(), &recordingStreamSink{})
		if err != nil {
			t.Fatalf("stream chat: %v", err)
		}
		if streamCalls != 2 {
			t.Fatalf("stream open calls = %d, want 2", streamCalls)
		}
		if int(record.RetryCount) != 1 || int(store.lastFinalize.RetryCount) != 1 {
			t.Fatalf("retry count record=%d persisted=%d, want 1", record.RetryCount, store.lastFinalize.RetryCount)
		}
		if store.lastFinalize.Status != "succeeded" {
			t.Fatalf("status = %q, want succeeded", store.lastFinalize.Status)
		}
	})

	t.Run("established stream read failure is never retried", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		streamCalls := 0
		client := &fakeProviderClient{}
		client.streamFactory = func(ctx context.Context) (provider.StreamResult, error) {
			streamCalls++
			return provider.StreamResult{
				Stream: &fakeChatStream{errs: []error{errors.New("upstream reset after establishment")}},
			}, nil
		}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 5
		})
		service.jitter = zeroJitter

		auth, err := service.Authenticate(context.Background(), rawKey)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		_, err = service.StreamChat(context.Background(), auth, "", streamChatRequest(), &recordingStreamSink{})
		gatewayErr, ok := err.(*GatewayError)
		if !ok || gatewayErr.Category != provider.StreamInterrupted {
			t.Fatalf("error = %#v, want stream_interrupted", err)
		}
		if streamCalls != 1 {
			t.Fatalf("stream open calls = %d, want 1 (established streams never retry)", streamCalls)
		}
		if int(store.lastFinalize.RetryCount) != 0 {
			t.Fatalf("persisted retry count = %d, want 0", store.lastFinalize.RetryCount)
		}
	})

	t.Run("open 429 exhausted keeps provider category", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		streamCalls := 0
		client := &fakeProviderClient{}
		client.streamFactory = func(ctx context.Context) (provider.StreamResult, error) {
			streamCalls++
			return provider.StreamResult{}, errProviderRateLimited(0)
		}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 1
		})
		service.jitter = zeroJitter

		auth, err := service.Authenticate(context.Background(), rawKey)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		_, err = service.StreamChat(context.Background(), auth, "", streamChatRequest(), &recordingStreamSink{})
		gatewayErr, ok := err.(*GatewayError)
		if !ok || gatewayErr.Category != provider.ProviderRateLimited {
			t.Fatalf("error = %#v, want provider_rate_limited", err)
		}
		if gatewayErr.RetryAfter == nil || *gatewayErr.RetryAfter != 0 {
			t.Fatalf("RetryAfter = %v, want present 0", gatewayErr.RetryAfter)
		}
		if streamCalls != 2 {
			t.Fatalf("stream open calls = %d, want 2", streamCalls)
		}
	})
}

func TestHandlerReportsRetryCountAndRetryAfterHeaders(t *testing.T) {
	t.Run("success after retries", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		client := &retryProbeClient{errors: []error{errProviderUnavailable500(), errProviderUnavailable500()}, result: okResult()}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 2
		})
		service.jitter = zeroJitter
		handler := NewHandler(service)

		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(chatJSON()))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+rawKey)
		response := httptest.NewRecorder()
		handler.chatCompletions(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
		}
		if got := response.Header().Get("X-Gateway-Retry-Count"); got != "2" {
			t.Fatalf("X-Gateway-Retry-Count = %q, want 2", got)
		}
		if response.Header().Get("Retry-After") != "" {
			t.Fatalf("Retry-After set on success: %q", response.Header().Get("Retry-After"))
		}
		if int(store.lastFinalize.RetryCount) != 2 {
			t.Fatalf("persisted retry count = %d, want 2", store.lastFinalize.RetryCount)
		}
	})

	t.Run("exhausted provider 429 keeps category and writes Retry-After", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		client := &retryProbeClient{errors: []error{errProviderRateLimited(time.Second), errProviderRateLimited(time.Second)}}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 1
			options.UpstreamRequestTimeout = 30 * time.Second // room for the 1s hint to fit
		})
		service.jitter = zeroJitter
		handler := NewHandler(service)

		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(chatJSON()))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+rawKey)
		response := httptest.NewRecorder()
		handler.chatCompletions(response, request)

		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429; body=%s", response.Code, response.Body.String())
		}
		if got := response.Header().Get("X-Gateway-Retry-Count"); got != "1" {
			t.Fatalf("X-Gateway-Retry-Count = %q, want 1", got)
		}
		if got := response.Header().Get("Retry-After"); got != "1" {
			t.Fatalf("Retry-After = %q, want 1", got)
		}
		if !bytes.Contains(response.Body.Bytes(), []byte("\"type\":\"provider_rate_limited\"")) {
			t.Fatalf("body does not distinguish provider category: %s", response.Body.String())
		}
		if bytes.Contains(response.Body.Bytes(), []byte("\"type\":\"rate_limited\"")) {
			t.Fatalf("local category leaked into provider rejection body: %s", response.Body.String())
		}
	})
}

func TestRateLimitRejectionHappensBeforeRowAndProvider(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &retryProbeClient{result: okResult()}
	limiter, err := ratelimit.NewRegistry(ratelimit.Config{
		KeyRPM:        1,
		EntryCap:      10,
		IdleTTL:       time.Minute,
		SweepInterval: time.Minute,
	})
	if err != nil {
		t.Fatalf("new limiter: %v", err)
	}
	t.Cleanup(limiter.Close)
	service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
		options.RateLimiter = limiter
	})

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest()); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", chatRequest())
	gatewayErr, ok := err.(*GatewayError)
	if !ok || gatewayErr.Category != provider.RateLimited {
		t.Fatalf("error = %#v, want rate_limited", err)
	}
	if gatewayErr.RetryAfter == nil || *gatewayErr.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want positive", gatewayErr.RetryAfter)
	}
	if store.createCalls != 1 || store.finalizeCalls != 1 {
		t.Fatalf("create=%d finalize=%d, want 1 each (rejected request must not touch the durable row)", store.createCalls, store.finalizeCalls)
	}
	if client.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (rejected request must not call the provider)", client.calls)
	}
}

type blockingClient struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (c *blockingClient) CompleteChat(ctx context.Context, _ provider.ChatRequest, _ provider.Credential) (provider.Result, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.started <- struct{}{}
	select {
	case <-c.release:
		return okResult(), nil
	case <-ctx.Done():
		return provider.Result{}, &provider.Error{Category: provider.ProviderTimeout, Err: ctx.Err()}
	}
}

func TestConcurrencyGeneralSlotBound(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &blockingClient{started: make(chan struct{}, 8), release: make(chan struct{})}
	service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
		options.MaxConcurrentRequests = 1
	})

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	type outcome struct {
		err error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		_, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest())
		firstDone <- outcome{err: err}
	}()
	<-client.started

	// Second request must be rejected immediately: no provider call, no row.
	_, _, err = service.CompleteChat(context.Background(), auth, "", chatRequest())
	gatewayErr, ok := err.(*GatewayError)
	if !ok || gatewayErr.Category != provider.RateLimited {
		t.Fatalf("second request error = %#v, want rate_limited capacity rejection", err)
	}
	client.mu.Lock()
	calls := client.calls
	client.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}

	close(client.release)
	if err := (<-firstDone).err; err != nil {
		t.Fatalf("first request: %v", err)
	}

	// After release the slot is available again.
	if _, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest()); err != nil {
		t.Fatalf("request after release: %v", err)
	}
	client.mu.Lock()
	calls = client.calls
	client.mu.Unlock()
	if calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
}

func TestConcurrencyStreamCapIsAdditionalToGeneralCap(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	streamStarted := make(chan struct{}, 8)
	streamRelease := make(chan struct{})
	client := &fakeProviderClient{}
	client.streamFactory = func(ctx context.Context) (provider.StreamResult, error) {
		streamStarted <- struct{}{}
		select {
		case <-streamRelease:
			return provider.StreamResult{
				Stream:         &fakeChatStream{events: []provider.StreamEvent{{Data: []byte("[DONE]"), Done: true}}},
				UpstreamStatus: http.StatusOK,
			}, nil
		case <-ctx.Done():
			return provider.StreamResult{}, &provider.Error{Category: provider.ProviderTimeout, Err: ctx.Err()}
		}
	}
	service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
		options.MaxConcurrentRequests = 2
		options.MaxConcurrentStreams = 1
	})

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.StreamChat(context.Background(), auth, "", streamChatRequest(), &recordingStreamSink{})
		firstDone <- err
	}()
	<-streamStarted

	// A second stream holds no free stream slot -> rejected, and the rejected
	// request must not reach the provider.
	_, err = service.StreamChat(context.Background(), auth, "", streamChatRequest(), &recordingStreamSink{})
	gatewayErr, ok := err.(*GatewayError)
	if !ok || gatewayErr.Category != provider.RateLimited {
		t.Fatalf("second stream error = %#v, want rate_limited capacity rejection", err)
	}
	if client.streamCalls != 1 {
		t.Fatalf("stream calls = %d, want 1", client.streamCalls)
	}

	// A non-stream request still fits the general cap (2 general slots, only
	// one in use by the stream) while the stream cap is exhausted.
	if _, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest()); err != nil {
		t.Fatalf("non-stream request while stream cap full: %v", err)
	}

	close(streamRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("first stream: %v", err)
	}
}

func okResult() provider.Result {
	return provider.Result{
		Response: provider.ChatResponse{
			ID:      "chatcmpl_test",
			Object:  "chat.completion",
			Created: 123,
			Model:   "gpt-test",
			Choices: []provider.Choice{{
				Index:        0,
				Message:      provider.ResponseMessage{Role: "assistant", Content: "ok"},
				FinishReason: "stop",
			}},
		},
		Usage:          &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		UpstreamStatus: http.StatusOK,
	}
}

func chatRequest() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
}

func streamChatRequest() provider.ChatRequest {
	request := chatRequest()
	request.Stream = true
	return request
}

func chatJSON() string {
	return `{"model":"openai/gpt-test","messages":[{"role":"user","content":"hello"}]}`
}

func TestHandlerRetryAfterOnlyForIntendedCategories(t *testing.T) {
	t.Run("local rate limit emits retry-after", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		client := &retryProbeClient{result: okResult()}
		limiter, err := ratelimit.NewRegistry(ratelimit.Config{
			KeyRPM:        1,
			EntryCap:      10,
			IdleTTL:       time.Minute,
			SweepInterval: time.Minute,
		})
		if err != nil {
			t.Fatalf("new limiter: %v", err)
		}
		t.Cleanup(limiter.Close)
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RateLimiter = limiter
		})
		handler := NewHandler(service)

		first := httptest.NewRecorder()
		handler.chatCompletions(first, chatHTTPRequest(rawKey))
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d, want 200; body=%s", first.Code, first.Body.String())
		}
		second := httptest.NewRecorder()
		handler.chatCompletions(second, chatHTTPRequest(rawKey))
		if second.Code != http.StatusTooManyRequests {
			t.Fatalf("second status = %d, want 429; body=%s", second.Code, second.Body.String())
		}
		if !bytes.Contains(second.Body.Bytes(), []byte(`"type":"rate_limited"`)) {
			t.Fatalf("body missing local rate_limited category: %s", second.Body.String())
		}
		retryAfter := second.Header().Get("Retry-After")
		if retryAfter == "" {
			t.Fatal("local rate limit rejection did not emit Retry-After")
		}
		seconds, err := strconv.Atoi(retryAfter)
		if err != nil || seconds <= 0 {
			t.Fatalf("Retry-After = %q, want a positive whole second count", retryAfter)
		}
		if second.Header().Get("X-Gateway-Request-ID") != "" {
			t.Fatal("rate-limited request unexpectedly carried a gateway request ID")
		}
		if store.createCalls != 1 || client.calls != 1 {
			t.Fatalf("create=%d provider=%d, want 1 each (rejection before row/provider)", store.createCalls, client.calls)
		}
	})

	t.Run("provider invalid request with retry-after hint must not emit header", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		hint := 120 * time.Second
		client := &retryProbeClient{errors: []error{
			&provider.Error{Category: provider.ProviderInvalidReq, StatusCode: http.StatusBadRequest, RetryAfter: &hint},
		}}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 3
		})
		service.jitter = zeroJitter
		handler := NewHandler(service)

		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(chatJSON()))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+rawKey)
		response := httptest.NewRecorder()
		handler.chatCompletions(response, request)

		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Retry-After"); got != "" {
			t.Fatalf("Retry-After = %q leaked on a provider invalid-request response", got)
		}
		if client.calls != 1 {
			t.Fatalf("provider calls = %d, want 1", client.calls)
		}
	})

	t.Run("provider unavailable with retry-after hint must not emit header", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		hint := 2 * time.Hour
		client := &retryProbeClient{errors: []error{
			&provider.Error{Category: provider.ProviderUnavailable, StatusCode: http.StatusServiceUnavailable, RetryAfter: &hint},
		}}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RetryMaxRetries = 3
		})
		service.jitter = zeroJitter
		handler := NewHandler(service)

		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(chatJSON()))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+rawKey)
		response := httptest.NewRecorder()
		handler.chatCompletions(response, request)

		// 503 is retryable but the 2h hint cannot fit the ~1s phase budget, so
		// the single attempt is terminal with a provider hint attached; the
		// response must not forward it as a client directive.
		if response.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Retry-After"); got != "" {
			t.Fatalf("Retry-After = %q leaked on a provider-unavailable response", got)
		}
		if client.calls != 1 {
			t.Fatalf("provider calls = %d, want 1", client.calls)
		}
	})
}

// captureHandler collects slog records in memory for admission-visibility tests.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func (h *captureHandler) attrs() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := map[string]string{}
	for _, record := range h.records {
		record.Attrs(func(attr slog.Attr) bool {
			result[attr.Key] = attr.Value.String()
			return true
		})
	}
	return result
}

func (h *captureHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

func TestAdmissionRejectionVisibility(t *testing.T) {
	t.Run("rate limit rejection logs bounded fields", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		client := &retryProbeClient{result: okResult()}
		limiter, err := ratelimit.NewRegistry(ratelimit.Config{
			ProjectRPM:    1,
			EntryCap:      10,
			IdleTTL:       time.Minute,
			SweepInterval: time.Minute,
		})
		if err != nil {
			t.Fatalf("new limiter: %v", err)
		}
		t.Cleanup(limiter.Close)
		capture := &captureHandler{}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.RateLimiter = limiter
		})
		service.logger = slog.New(capture)

		auth, err := service.Authenticate(context.Background(), rawKey)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if _, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest()); err != nil {
			t.Fatalf("first request: %v", err)
		}
		if _, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest()); err == nil {
			t.Fatal("second request unexpectedly allowed")
		}
		attrs := capture.attrs()
		if attrs["reason"] != "rate_limit" {
			t.Fatalf("log reason = %q, want rate_limit (attrs=%v)", attrs["reason"], attrs)
		}
		if attrs["scope"] != "project" {
			t.Fatalf("log scope = %q, want project (attrs=%v)", attrs["scope"], attrs)
		}
		if attrs["project_id"] != testProjectID || attrs["virtual_key_id"] != testVirtualKeyID {
			t.Fatalf("log ids = project %q key %q (attrs=%v)", attrs["project_id"], attrs["virtual_key_id"], attrs)
		}
		if _, ok := attrs["retry_after_seconds"]; !ok {
			t.Fatalf("log missing retry_after_seconds (attrs=%v)", attrs)
		}
		for _, secret := range []string{"sk-", "Authorization", "Bearer", "hello"} {
			for _, value := range attrs {
				if containsFold(value, secret) {
					t.Fatalf("log leaked %q via value %q", secret, value)
				}
			}
		}
	})

	t.Run("concurrency rejection logs reason and no provider call", func(t *testing.T) {
		store, rawKey := newAuthorizedStore(t)
		client := &blockingClient{started: make(chan struct{}, 8), release: make(chan struct{})}
		capture := &captureHandler{}
		service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, func(options *Options) {
			options.MaxConcurrentRequests = 1
		})
		service.logger = slog.New(capture)

		auth, err := service.Authenticate(context.Background(), rawKey)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest())
			done <- err
		}()
		<-client.started
		defer func() {
			close(client.release)
			<-done
		}()

		if _, _, err := service.CompleteChat(context.Background(), auth, "", chatRequest()); err == nil {
			t.Fatal("second request unexpectedly allowed")
		}
		attrs := capture.attrs()
		if attrs["reason"] != "concurrent_requests" {
			t.Fatalf("log reason = %q, want concurrent_requests (attrs=%v)", attrs["reason"], attrs)
		}
		if attrs["scope"] != "" {
			t.Fatalf("log scope = %q, want empty for concurrency (attrs=%v)", attrs["scope"], attrs)
		}
		client.mu.Lock()
		calls := client.calls
		client.mu.Unlock()
		if calls != 1 {
			t.Fatalf("provider calls = %d, want 1", calls)
		}
	})
}

func containsFold(value, substring string) bool {
	return len(value) >= len(substring) && (func() bool {
		for index := 0; index+len(substring) <= len(value); index++ {
			if strings.EqualFold(value[index:index+len(substring)], substring) {
				return true
			}
		}
		return false
	}())
}

func chatHTTPRequest(rawKey string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(chatJSON()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	return request
}

// redirectBaseURLStore wraps an authorized fake store and forces every
// resolved credential to point at the httptest redirecting provider.
type redirectBaseURLStore struct {
	*fakeStore
	override string
}

func (s *redirectBaseURLStore) ResolveProviderCredential(ctx context.Context, projectID string, name provider.Name) (ProviderCredential, error) {
	credential, err := s.fakeStore.ResolveProviderCredential(ctx, projectID, name)
	if err != nil {
		return ProviderCredential{}, err
	}
	credential.BaseURLOverride = s.override
	return credential, nil
}

// TestRedirectChainDialFailureIsNotRetriedAsPreProviderFailure exercises the
// real OpenAI adapter through the service: the redirecting provider returns a
// 307 to a target that would fail to dial. With redirects disabled the 3xx is
// classified as a real HTTP status (307, provider_unavailable), which is not
// in the retry whitelist, so the executor runs exactly one attempt even with
// retries enabled - it never treats a redirect-chain dial error as a safe
// pre-provider failure.
func TestRedirectChainDialFailureIsNotRetriedAsPreProviderFailure(t *testing.T) {
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		posts.Add(1)
		response.Header().Set("Location", "http://127.0.0.1:1/nowhere") // would fail to dial if followed
		response.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	baseStore, rawKey := newAuthorizedStore(t)
	store := &redirectBaseURLStore{fakeStore: baseStore, override: server.URL}
	openAIClient := openai.New(openai.NewHTTPClient())
	service := newServiceWithReliability(t, store, map[provider.Name]provider.Client{provider.OpenAI: openAIClient}, func(options *Options) {
		options.RetryMaxRetries = 5
	})
	service.jitter = zeroJitter

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, record, err := service.CompleteChat(context.Background(), auth, "", chatRequest())
	gatewayErr, ok := err.(*GatewayError)
	if !ok || gatewayErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v, want provider_unavailable", err)
	}
	if gatewayErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("gateway status = %d, want 307 (real 3xx classification, not a dial error)", gatewayErr.StatusCode)
	}
	if posts.Load() != 1 {
		t.Fatalf("provider POSTs = %d, want exactly 1 (redirect not followed, no replay)", posts.Load())
	}
	if int(record.RetryCount) != 0 || int(store.lastFinalize.RetryCount) != 0 {
		t.Fatalf("retry count record=%d persisted=%d, want 0 (3xx is not retryable)", record.RetryCount, store.lastFinalize.RetryCount)
	}
}
