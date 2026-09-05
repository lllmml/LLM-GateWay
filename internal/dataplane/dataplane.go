package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/pricing"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

var (
	ErrNotFound = errors.New("dataplane record not found")
)

// retryBaseDelay is the fixed internal base of the default exponential
// backoff window used when the provider gives no Retry-After hint. It is a
// code constant, not configuration (Tech Design §14.4 keeps the policy small);
// RETRY_BACKOFF_MAX still caps the window and full-jitter keeps the actual
// wait at or below that cap.
const retryBaseDelay = 100 * time.Millisecond

// maxRetryRetries bounds Options.RetryMaxRetries at the service boundary so a
// misconfigured caller can never request an unbounded replay of paid upstream
// attempts.
const maxRetryRetries = 5

const finalizeTimeout = 5 * time.Second

// pricingLookupBudget bounds the price-version lookup independently of the
// durable row finalization. It must be strictly smaller than finalizeTimeout
// so a slow/hung pricing lookup can never consume the whole finalization
// budget: pricing failure degrades to NULL cost, while the final durable write
// still receives a live context (ADR-016).
const pricingLookupBudget = 1 * time.Second

type AuthContext struct {
	ProjectID    string
	VirtualKeyID string
	KeyPrefix    string
}

type ProviderCredential struct {
	ID               string
	ProjectID        string
	Provider         provider.Name
	SecretCiphertext []byte
	SecretNonce      []byte
	KeyVersion       int16
	BaseURLOverride  string
}

type GatewayRequest struct {
	ID                   string
	ProjectID            string
	VirtualKeyID         string
	ProviderCredentialID string
	Provider             provider.Name
	Model                string
	IsStream             bool
	Status               string
	StartedAt            time.Time
	// RetryCount is populated by the service after the attempt loop completes
	// (0 when no retry ran). It is an in-memory propagation field used for the
	// X-Gateway-Retry-Count response header; the durable row receives the same
	// value exactly once through FinalizeGatewayRequest.
	RetryCount int16
}

type CreateRequestParams struct {
	ProjectID            string
	VirtualKeyID         string
	ProviderCredentialID string
	Provider             provider.Name
	Model                string
	IsStream             bool
	StartedAt            time.Time
	TraceID              string
}

type FinalizeParams struct {
	ID                   string
	Status               string
	FirstChunkAt         *time.Time
	CompletedAt          time.Time
	LatencyMS            *int64
	TTFTMS               *int64
	UpstreamHTTPStatus   *int32
	ErrorCategory        *provider.ErrorCategory
	RetryCount           int16
	PromptTokens         *int64
	CompletionTokens     *int64
	TotalTokens          *int64
	UsageSource          *string
	PricingID            *string
	EstimatedCostNanoUSD *int64
	UpstreamRequestID    *string
}

type ModelPrice struct {
	ID                      string
	InputNanoUSDPerMillion  int64
	OutputNanoUSDPerMillion int64
}

type Store interface {
	AuthenticateVirtualKey(context.Context, string, []byte) (AuthContext, error)
	ResolveProviderCredential(context.Context, string, provider.Name) (ProviderCredential, error)
	CreateGatewayRequest(context.Context, CreateRequestParams) (GatewayRequest, error)
	FindModelPrice(context.Context, provider.Name, string, time.Time) (ModelPrice, error)
	FinalizeGatewayRequest(context.Context, FinalizeParams) error
}

type StreamSink interface {
	Prepare(GatewayRequest) error
	WriteEvent(provider.StreamEvent) error
	Committed() bool
}

type Options struct {
	Store                     Store
	VirtualKeyPepper          []byte
	CredentialCipher          *security.CredentialCipher
	UpstreamRequestTimeout    time.Duration
	UpstreamStreamMaxDuration time.Duration
	ProviderRegistry          *provider.Registry
	Logger                    *slog.Logger

	// Week 8 Reliability Baseline. All admission controls default to disabled
	// (nil / 0). RateLimit is the shared in-memory registry (owned by the
	// caller, closed by the caller); when nil no rate limiting runs.
	RateLimiter *ratelimit.Registry
	// MaxConcurrentRequests bounds total in-flight chat operations (stream and
	// non-stream); 0 disables it. MaxConcurrentStreams additionally bounds
	// in-flight streams; stream requests must satisfy both caps.
	MaxConcurrentRequests int
	MaxConcurrentStreams  int
	// RetryMaxRetries is the number of retries allowed after the initial
	// attempt (0 disables retries; bounded by maxRetryRetries). Each retry
	// shares the single overall upstream phase budget.
	RetryMaxRetries int
	// RetryBackoffMax caps the default exponential backoff window used when
	// the provider sends no Retry-After hint.
	RetryBackoffMax time.Duration
}

type Service struct {
	store                     Store
	virtualKeyPepper          []byte
	credentialCipher          *security.CredentialCipher
	upstreamRequestTimeout    time.Duration
	upstreamStreamMaxDuration time.Duration
	providers                 *provider.Registry
	logger                    *slog.Logger

	rateLimiter           *ratelimit.Registry
	maxConcurrentRequests int
	maxConcurrentStreams  int
	requestSlots          chan struct{}
	streamSlots           chan struct{}
	retryMaxRetries       int
	retryBackoffMax       time.Duration
	jitter                func(int64) int64
	now                   func() time.Time
	// onRetryWait is an unexported test hook invoked immediately before the
	// retry loop blocks on its backoff/Retry-After wait. Tests use it to
	// cancel the request context at a known point (the loop is provably inside
	// the wait) instead of racing real sleeps.
	onRetryWait func()
}

func NewService(options Options) (*Service, error) {
	if options.Store == nil {
		return nil, errors.New("dataplane store is required")
	}
	if err := apikey.ValidatePepper(options.VirtualKeyPepper); err != nil {
		return nil, err
	}
	if options.CredentialCipher == nil {
		return nil, errors.New("credential cipher is required")
	}
	if options.UpstreamRequestTimeout <= 0 {
		return nil, errors.New("upstream request timeout must be positive")
	}
	if options.UpstreamStreamMaxDuration <= 0 {
		return nil, errors.New("upstream stream max duration must be positive")
	}
	if options.ProviderRegistry == nil {
		return nil, errors.New("provider registry is required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.MaxConcurrentRequests < 0 || options.MaxConcurrentStreams < 0 {
		return nil, errors.New("concurrency caps must be non-negative")
	}
	if options.RetryMaxRetries < 0 || options.RetryMaxRetries > maxRetryRetries {
		return nil, fmt.Errorf("retry max retries must be between 0 and %d", maxRetryRetries)
	}
	retryBackoffMax := options.RetryBackoffMax
	if retryBackoffMax < 0 {
		return nil, errors.New("retry backoff max must be non-negative")
	}
	if retryBackoffMax == 0 {
		retryBackoffMax = 2 * time.Second
	}
	service := &Service{
		store:                     options.Store,
		virtualKeyPepper:          append([]byte(nil), options.VirtualKeyPepper...),
		credentialCipher:          options.CredentialCipher,
		upstreamRequestTimeout:    options.UpstreamRequestTimeout,
		upstreamStreamMaxDuration: options.UpstreamStreamMaxDuration,
		providers:                 options.ProviderRegistry,
		logger:                    logger,
		rateLimiter:               options.RateLimiter,
		maxConcurrentRequests:     options.MaxConcurrentRequests,
		maxConcurrentStreams:      options.MaxConcurrentStreams,
		retryMaxRetries:           options.RetryMaxRetries,
		retryBackoffMax:           retryBackoffMax,
		jitter:                    rand.Int64N,
		now:                       time.Now,
	}
	if options.MaxConcurrentRequests > 0 {
		service.requestSlots = make(chan struct{}, options.MaxConcurrentRequests)
	}
	if options.MaxConcurrentStreams > 0 {
		service.streamSlots = make(chan struct{}, options.MaxConcurrentStreams)
	}
	return service, nil
}

func (s *Service) Authenticate(ctx context.Context, rawKey string) (AuthContext, error) {
	parsed, err := apikey.ParseRawKey(rawKey)
	if err != nil {
		return AuthContext{}, NewError(provider.AuthenticationFailed, "authentication failed")
	}
	keyHash, err := apikey.HashKey(rawKey, s.virtualKeyPepper)
	if err != nil {
		return AuthContext{}, err
	}
	auth, err := s.store.AuthenticateVirtualKey(ctx, parsed.Prefix, keyHash)
	if errors.Is(err, ErrNotFound) {
		return AuthContext{}, NewError(provider.AuthenticationFailed, "authentication failed")
	}
	if err != nil {
		return AuthContext{}, err
	}
	return auth, nil
}

func (s *Service) CompleteChat(ctx context.Context, auth AuthContext, traceID string, chat provider.ChatRequest) (provider.Result, GatewayRequest, error) {
	return s.CompleteChatStartedAt(ctx, auth, traceID, time.Now().UTC(), chat)
}

func (s *Service) CompleteChatStartedAt(ctx context.Context, auth AuthContext, traceID string, requestStartedAt time.Time, chat provider.ChatRequest) (provider.Result, GatewayRequest, error) {
	return s.completeChat(ctx, auth, traceID, normalizedStartedAt(requestStartedAt), chat)
}

func (s *Service) completeChat(ctx context.Context, auth AuthContext, traceID string, requestStartedAt time.Time, chat provider.ChatRequest) (provider.Result, GatewayRequest, error) {
	modelRef, err := provider.ParseModel(chat.Model)
	if err != nil {
		return provider.Result{}, GatewayRequest{}, NewError(provider.ModelNotSupported, "model must use provider/model-id format")
	}
	if chat.Stream {
		return provider.Result{}, GatewayRequest{}, NewError(provider.UnsupportedFeature, "streaming chat completions are not supported in this milestone")
	}
	release, admissionErr := s.admit(ctx, auth, false)
	if admissionErr != nil {
		return provider.Result{}, GatewayRequest{}, admissionErr
	}
	defer release()
	client, ok := s.providers.Lookup(modelRef.Provider)
	if !ok {
		return provider.Result{}, GatewayRequest{}, NewError(provider.ProviderNotConfigured, "provider is not configured")
	}
	credential, err := s.store.ResolveProviderCredential(ctx, auth.ProjectID, modelRef.Provider)
	if errors.Is(err, ErrNotFound) {
		return provider.Result{}, GatewayRequest{}, NewError(provider.ProviderNotConfigured, "provider is not configured")
	}
	if err != nil {
		return provider.Result{}, GatewayRequest{}, err
	}

	record, err := s.store.CreateGatewayRequest(ctx, CreateRequestParams{
		ProjectID:            auth.ProjectID,
		VirtualKeyID:         auth.VirtualKeyID,
		ProviderCredentialID: credential.ID,
		Provider:             modelRef.Provider,
		Model:                modelRef.Model,
		IsStream:             chat.Stream,
		StartedAt:            requestStartedAt,
		TraceID:              traceID,
	})
	if err != nil {
		return provider.Result{}, GatewayRequest{}, err
	}
	record.Provider = modelRef.Provider

	apiKey, err := s.decryptCredential(credential)
	if err != nil {
		category := provider.ProviderNotConfigured
		if finalizeErr := s.finalize(ctx, record, nil, &category, nil); finalizeErr != nil {
			return provider.Result{}, record, s.persistenceError(record, category, finalizeErr)
		}
		return provider.Result{}, record, NewError(provider.ProviderNotConfigured, "provider credential could not be decrypted")
	}
	defer clear(apiKey)

	upstreamChat := chat
	upstreamChat.Model = modelRef.Model

	// Week 8 bounded retry. The whole upstream phase (attempts and backoff)
	// shares a single budget equal to UpstreamRequestTimeout measured from the
	// phase start, so retries never multiply the configured timeout into
	// attempts*timeout. Only the provider invocation is retried: the durable
	// row is created once and finalized once with the final retry count. A
	// retry starts only when the last error is explicitly retryable, the phase
	// budget still has room for the wait, the attempt cap is not exhausted,
	// and the caller has not cancelled.
	phaseDeadline := s.now().Add(s.upstreamRequestTimeout)
	var lastResult provider.Result
	var lastGatewayErr *GatewayError
	retryCount := 0
	for {
		attemptCtx, cancel := context.WithDeadline(ctx, phaseDeadline)
		lastResult, err = client.CompleteChat(attemptCtx, upstreamChat, provider.Credential{
			APIKey:          apiKey,
			BaseURLOverride: credential.BaseURLOverride,
		})
		cancel()
		if err == nil {
			record.RetryCount = int16(retryCount)
			if finalizeErr := s.finalize(ctx, record, &lastResult, nil, lastResult.Usage); finalizeErr != nil {
				return lastResult, record, s.persistenceError(record, "", finalizeErr)
			}
			return lastResult, record, nil
		}
		lastGatewayErr = s.classifyAttemptError(err)
		if !s.retryAllowed(ctx, lastGatewayErr, phaseDeadline, retryCount) {
			break
		}
		wait := s.retryDelay(lastGatewayErr.RetryAfter, retryCount, phaseDeadline)
		if wait == nil {
			break
		}
		if s.onRetryWait != nil {
			s.onRetryWait()
		}
		// Cancellation is checked again after the wait returns: even if the
		// timer won the select, a cancelled request must not start another
		// paid attempt.
		if !s.waitWithContext(ctx, *wait) || ctx.Err() != nil {
			break
		}
		retryCount++
	}

	record.RetryCount = int16(retryCount)
	if finalizeErr := s.finalize(ctx, record, &lastResult, &lastGatewayErr.Category, nil); finalizeErr != nil {
		return lastResult, record, s.persistenceError(record, lastGatewayErr.Category, finalizeErr)
	}
	return lastResult, record, lastGatewayErr
}

func (s *Service) StreamChat(ctx context.Context, auth AuthContext, traceID string, chat provider.ChatRequest, sink StreamSink) (GatewayRequest, error) {
	return s.StreamChatStartedAt(ctx, auth, traceID, time.Now().UTC(), chat, sink)
}

func (s *Service) StreamChatStartedAt(ctx context.Context, auth AuthContext, traceID string, requestStartedAt time.Time, chat provider.ChatRequest, sink StreamSink) (GatewayRequest, error) {
	return s.streamChat(ctx, auth, traceID, normalizedStartedAt(requestStartedAt), chat, sink)
}

func (s *Service) streamChat(ctx context.Context, auth AuthContext, traceID string, requestStartedAt time.Time, chat provider.ChatRequest, sink StreamSink) (GatewayRequest, error) {
	if sink == nil {
		return GatewayRequest{}, NewError(provider.InternalError, "request could not be completed")
	}
	modelRef, err := provider.ParseModel(chat.Model)
	if err != nil {
		return GatewayRequest{}, NewError(provider.ModelNotSupported, "model must use provider/model-id format")
	}
	client, ok := s.providers.Lookup(modelRef.Provider)
	if !ok {
		return GatewayRequest{}, NewError(provider.ProviderNotConfigured, "provider is not configured")
	}
	streamingClient, ok := client.(provider.StreamingClient)
	if !ok {
		return GatewayRequest{}, NewError(provider.UnsupportedFeature, "streaming chat completions are not supported for this provider")
	}
	release, admissionErr := s.admit(ctx, auth, true)
	if admissionErr != nil {
		return GatewayRequest{}, admissionErr
	}
	defer release()
	credential, err := s.store.ResolveProviderCredential(ctx, auth.ProjectID, modelRef.Provider)
	if errors.Is(err, ErrNotFound) {
		return GatewayRequest{}, NewError(provider.ProviderNotConfigured, "provider is not configured")
	}
	if err != nil {
		return GatewayRequest{}, err
	}

	record, err := s.store.CreateGatewayRequest(ctx, CreateRequestParams{
		ProjectID:            auth.ProjectID,
		VirtualKeyID:         auth.VirtualKeyID,
		ProviderCredentialID: credential.ID,
		Provider:             modelRef.Provider,
		Model:                modelRef.Model,
		IsStream:             true,
		StartedAt:            requestStartedAt,
		TraceID:              traceID,
	})
	if err != nil {
		return GatewayRequest{}, err
	}
	record.Provider = modelRef.Provider

	apiKey, err := s.decryptCredential(credential)
	if err != nil {
		category := provider.ProviderNotConfigured
		if finalizeErr := s.finalizeStream(ctx, record, nil, &category, nil, nil, nil); finalizeErr != nil {
			return record, s.persistenceError(record, category, finalizeErr)
		}
		return record, NewError(provider.ProviderNotConfigured, "provider credential could not be decrypted")
	}
	defer clear(apiKey)

	upstreamChat := chat
	upstreamChat.Model = modelRef.Model
	upstreamChat.Stream = true

	// Week 8 streaming open retry. The retryable window is deliberately
	// narrower than "no downstream bytes committed": only failures while
	// StreamChat is still trying to ESTABLISH a ChatStream (no stream object
	// returned) may retry. The moment a ChatStream is returned the provider
	// has started executing/billing, so every later failure - malformed SSE,
	// stream read failure, upstream close/reset, client write failure - follows
	// the existing stream_interrupted terminal semantics and is never retried,
	// even before the first downstream byte is committed. Attempts and backoff
	// share one overall budget equal to UpstreamStreamMaxDuration from the
	// phase start.
	phaseDeadline := s.now().Add(s.upstreamStreamMaxDuration)
	var result provider.StreamResult
	var lastOpenErr *GatewayError
	retryCount := 0
	for {
		attemptCtx, cancel := context.WithDeadline(ctx, phaseDeadline)
		result, err = streamingClient.StreamChat(attemptCtx, upstreamChat, provider.Credential{
			APIKey:          apiKey,
			BaseURLOverride: credential.BaseURLOverride,
		})
		if err == nil && result.Stream != nil {
			// STREAM ESTABLISHED: the retryable open window is closed and the
			// attempt context must stay alive for the whole downstream read
			// loop, so its cancellation is deferred instead of run now.
			defer cancel()
			break
		}
		cancel()
		if err == nil {
			lastOpenErr = NewError(provider.ProviderUnavailable, "provider stream could not be opened")
		} else {
			lastOpenErr = classifyStreamOpenError(ctx, s.classifyAttemptError(err))
		}
		if !s.retryAllowed(ctx, lastOpenErr, phaseDeadline, retryCount) {
			break
		}
		wait := s.retryDelay(lastOpenErr.RetryAfter, retryCount, phaseDeadline)
		if wait == nil {
			break
		}
		if s.onRetryWait != nil {
			s.onRetryWait()
		}
		// Cancellation is checked again after the wait returns: even if the
		// timer won the select, a cancelled request must not start another
		// paid stream-open attempt.
		if !s.waitWithContext(ctx, *wait) || ctx.Err() != nil {
			break
		}
		retryCount++
	}

	if err != nil || result.Stream == nil {
		lastOpenErr = classifyStreamOpenError(ctx, lastOpenErr)
		record.RetryCount = int16(retryCount)
		if finalizeErr := s.finalizeStream(ctx, record, &result, &lastOpenErr.Category, nil, nil, nil); finalizeErr != nil {
			return record, s.persistenceError(record, lastOpenErr.Category, finalizeErr)
		}
		return record, lastOpenErr
	}

	record.RetryCount = int16(retryCount)
	defer result.Stream.Close()

	if err := sink.Prepare(record); err != nil {
		category := provider.StreamInterrupted
		if finalizeErr := s.finalizeStream(ctx, record, &result, &category, nil, nil, nil); finalizeErr != nil {
			return record, s.persistenceError(record, category, finalizeErr)
		}
		return record, NewError(provider.StreamInterrupted, "stream interrupted")
	}

	var firstChunkAt *time.Time
	var ttftMS *int64
	for {
		event, err := result.Stream.Next()
		if err != nil {
			category := provider.StreamInterrupted
			if finalizeErr := s.finalizeStream(ctx, record, &result, &category, nil, firstChunkAt, ttftMS); finalizeErr != nil {
				return record, s.persistenceError(record, category, finalizeErr)
			}
			return record, NewError(provider.StreamInterrupted, "stream interrupted")
		}
		if err := sink.WriteEvent(event); err != nil {
			category := provider.StreamInterrupted
			if finalizeErr := s.finalizeStream(ctx, record, &result, &category, nil, firstChunkAt, ttftMS); finalizeErr != nil {
				return record, s.persistenceError(record, category, finalizeErr)
			}
			return record, NewError(provider.StreamInterrupted, "stream interrupted")
		}
		if !event.Done && firstChunkAt == nil {
			now := time.Now().UTC()
			ttft := now.Sub(record.StartedAt).Milliseconds()
			firstChunkAt = &now
			ttftMS = &ttft
		}
		if event.Done {
			if err := s.finalizeStream(ctx, record, &result, nil, result.Stream.Usage(), firstChunkAt, ttftMS); err != nil {
				return record, s.persistenceError(record, "", err)
			}
			return record, nil
		}
	}
}

func classifyStreamOpenError(ctx context.Context, gatewayErr *GatewayError) *GatewayError {
	if gatewayErr == nil || ctx.Err() == nil {
		return gatewayErr
	}
	return NewError(provider.StreamInterrupted, "stream interrupted")
}

// admit applies the Week 8 admission controls in order: rate limiting (per
// virtual key and per project, local registry or an external Limiter), then
// the concurrency slots. A rejected request returns a stable rate_limited
// gateway error (never a provider call, never a durable gateway_requests row)
// and is visible through a bounded structured slog event, because Week 8
// rejections intentionally create no durable record and Prometheus metrics
// land with the Week 10 observability milestone. A cancelled/deadline-exceeded
// context propagates from the limiter as-is (no quota is consumed by a
// cancelled request, ADR-018 D1). The returned release function must be
// called exactly once when the admitted operation finishes; it is safe under
// early returns and panics when deferred.
func (s *Service) admit(ctx context.Context, auth AuthContext, stream bool) (func(), error) {
	if s.rateLimiter != nil {
		decision, err := s.rateLimiter.Admit(ctx, auth.VirtualKeyID, auth.ProjectID)
		if err != nil {
			return nil, err
		}
		if !decision.Allowed {
			s.logAdmissionRejection(auth, "rate_limit", string(decision.BlockingScope), decision.RetryAfter)
			retryAfter := decision.RetryAfter
			return nil, &GatewayError{
				Category:   provider.RateLimited,
				Message:    "rate limit exceeded",
				RetryAfter: &retryAfter,
			}
		}
	}

	releaseGeneral := func() {}
	if s.requestSlots != nil {
		select {
		case s.requestSlots <- struct{}{}:
			releaseGeneral = func() { <-s.requestSlots }
		default:
			s.logAdmissionRejection(auth, "concurrent_requests", "", 0)
			return nil, NewError(provider.RateLimited, "concurrent request limit reached")
		}
	}
	if !stream || s.streamSlots == nil {
		return releaseGeneral, nil
	}
	select {
	case s.streamSlots <- struct{}{}:
	default:
		releaseGeneral() // never leak the general slot when the stream slot is full
		s.logAdmissionRejection(auth, "concurrent_streams", "", 0)
		return nil, NewError(provider.RateLimited, "concurrent stream limit reached")
	}
	// The composite release is deferred by the caller and therefore runs
	// exactly once: general slot first, then the stream slot.
	return func() {
		releaseGeneral()
		<-s.streamSlots
	}, nil
}

// logAdmissionRejection emits the bounded, non-secret visibility event for a
// Week 8 admission rejection. Fields never include raw virtual keys,
// authorization material, credentials, prompt/response bodies, or provider
// headers. scope is empty for concurrency rejections; retryAfter is only set
// where the gateway computed a client wait hint.
func (s *Service) logAdmissionRejection(auth AuthContext, reason, scope string, retryAfter time.Duration) {
	fields := []any{
		slog.String("event", "admission_rejected"),
		slog.String("reason", reason),
		slog.String("project_id", auth.ProjectID),
		slog.String("virtual_key_id", auth.VirtualKeyID),
	}
	if scope != "" {
		fields = append(fields, slog.String("scope", scope))
	}
	if retryAfter > 0 {
		fields = append(fields, slog.Int64("retry_after_seconds", int64(formatRetryAfterSeconds(retryAfter))))
	}
	s.logger.Info("admission rejected", fields...)
}

// formatRetryAfterSeconds returns the whole-second ceiling used for the
// Retry-After response header and the rejection log field.
func formatRetryAfterSeconds(duration time.Duration) time.Duration {
	if duration <= 0 {
		return 0
	}
	seconds := duration / time.Second
	if duration%time.Second != 0 {
		seconds++
	}
	return seconds
}

// retryAllowed reports whether ANOTHER retry attempt may start after the given
// retryable-classified failure. Boundary semantics (no off-by-one): retryCount
// is the number of retries already executed; a retry is allowed only while
// retryCount < retryMaxRetries. The caller context must still be live and the
// overall phase budget must still have time left.
func (s *Service) retryAllowed(ctx context.Context, gatewayErr *GatewayError, phaseDeadline time.Time, retryCount int) bool {
	if gatewayErr == nil || !isRetryableGatewayError(gatewayErr) {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if retryCount >= s.retryMaxRetries {
		return false
	}
	return s.now().Before(phaseDeadline)
}

// retryDelay returns the wait before the next retry, or nil when the wait
// cannot fit in the remaining overall phase budget (the provider hint or the
// jittered backoff must leave room for another attempt). A present provider
// Retry-After hint is honored verbatim; when the hint is absent the default
// exponential backoff window applies with full jitter.
func (s *Service) retryDelay(hint *time.Duration, retryCount int, phaseDeadline time.Time) *time.Duration {
	var wait time.Duration
	if hint != nil {
		wait = *hint
	} else {
		window := retryBackoffWindow(retryBaseDelay, s.retryBackoffMax, retryCount)
		if window > 0 {
			wait = time.Duration(s.jitter(int64(window)))
		}
	}
	remaining := phaseDeadline.Sub(s.now())
	if remaining <= 0 || wait >= remaining {
		return nil
	}
	return &wait
}

// retryBackoffWindow is the pure exponential window for the retry that begins
// after retryCount completed retries: base doubled per retry, capped by max
// and by an exponent guard so pathological retry counts cannot overflow.
func retryBackoffWindow(base, max time.Duration, retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	shift := uint(retryCount)
	if shift > 16 {
		shift = 16
	}
	window := base << shift
	if window <= 0 || window > max {
		window = max
	}
	return window
}

func (s *Service) waitWithContext(ctx context.Context, duration time.Duration) bool {
	// Cancellation wins semantically even if the timer case were selected: an
	// already-cancelled request never waits, and a timer that fires at the
	// same moment as ctx.Done() is treated as cancelled.
	if ctx.Err() != nil {
		return false
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// retryableProviderStatuses is the explicit transient-failure whitelist for
// provider HTTP responses (ADR-017 D2). It is deliberately not "any 5xx":
// only statuses the three adapters classify as transient overload/server
// failures may be replayed. 504 stays under ProviderTimeout's no-retry policy.
func retryableProviderStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, 529:
		return true
	}
	return false
}

// isRetryableGatewayError is the Week 8 retry whitelist. It never retries on
// category alone: provider 401/402/403 are provider_unavailable yet must never
// retry, so the decision always considers the category together with the
// upstream HTTP status (or the absence of any HTTP response). Unknown 5xx
// statuses such as 501/505 are not whitelisted and are not retried.
func isRetryableGatewayError(gatewayErr *GatewayError) bool {
	if gatewayErr == nil {
		return false
	}
	switch gatewayErr.Category {
	case provider.ProviderRateLimited:
		// Provider 429 with a real HTTP status (respect Retry-After when
		// present).
		return gatewayErr.StatusCode == http.StatusTooManyRequests
	case provider.ProviderUnavailable:
		// A real HTTP response was received: retry only the explicit transient
		// status whitelist, which structurally excludes 401/402/403 (also
		// classified provider_unavailable) and unlisted 5xx.
		if gatewayErr.StatusCode != 0 && retryableProviderStatus(gatewayErr.StatusCode) {
			return true
		}
		if gatewayErr.StatusCode == 0 {
			// Transport-level failure with no HTTP response at all: retry only
			// the provably safe subset (dial / DNS) where the request never
			// reached the provider, never arbitrary errors.
			return isRetryableDialOrDNSFailure(gatewayErr.Err)
		}
	}
	return false
}

// isRetryableDialOrDNSFailure reports whether an error chain proves the
// request never reached the provider: a dial-phase net.OpError or a DNS
// resolution failure. Connection resets, EOFs, TLS failures, and other
// ambiguous transport errors are deliberately not retried because the request
// may already have reached the provider and incurred cost.
func isRetryableDialOrDNSFailure(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

// classifyAttemptError converts an upstream attempt error into a stable
// gateway error, preserving the upstream HTTP status, the provider Retry-After
// hint, and the underlying error chain for retry classification.
func (s *Service) classifyAttemptError(err error) *GatewayError {
	gatewayErr := errorFromProvider(err)
	if gatewayErr == nil {
		gatewayErr = NewError(provider.ProviderUnavailable, "provider request failed")
		gatewayErr.Err = err
	}
	return gatewayErr
}

func normalizedStartedAt(startedAt time.Time) time.Time {
	if startedAt.IsZero() {
		return time.Now().UTC()
	}
	return startedAt.UTC()
}

func (s *Service) decryptCredential(credential ProviderCredential) ([]byte, error) {
	return s.credentialCipher.Decrypt(security.EncryptedCredential{
		Ciphertext: credential.SecretCiphertext,
		Nonce:      credential.SecretNonce,
		KeyVersion: credential.KeyVersion,
	}, security.CredentialIdentity{
		CredentialID: credential.ID,
		ProjectID:    credential.ProjectID,
		Provider:     string(credential.Provider),
	})
}

func (s *Service) finalize(ctx context.Context, record GatewayRequest, result *provider.Result, category *provider.ErrorCategory, usage *provider.Usage) error {
	var streamResult *provider.StreamResult
	if result != nil {
		streamResult = &provider.StreamResult{
			UpstreamStatus:    result.UpstreamStatus,
			UpstreamRequestID: result.UpstreamRequestID,
		}
	}
	return s.finalizeStream(ctx, record, streamResult, category, usage, nil, nil)
}

func (s *Service) finalizeStream(ctx context.Context, record GatewayRequest, result *provider.StreamResult, category *provider.ErrorCategory, usage *provider.Usage, firstChunkAt *time.Time, ttftMS *int64) error {
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
	defer cancel()

	completedAt := time.Now().UTC()
	latency := completedAt.Sub(record.StartedAt).Milliseconds()
	status := "succeeded"
	if category != nil {
		status = "failed"
	}
	var upstreamStatus *int32
	var upstreamRequestID *string
	if result != nil {
		if result.UpstreamStatus != 0 {
			status := int32(result.UpstreamStatus)
			upstreamStatus = &status
		}
		if result.UpstreamRequestID != "" {
			upstreamRequestID = &result.UpstreamRequestID
		}
	}
	var promptTokens, completionTokens, totalTokens *int64
	var usageSource *string
	if usage != nil {
		promptTokens = &usage.PromptTokens
		completionTokens = &usage.CompletionTokens
		totalTokens = &usage.TotalTokens
		source := "provider"
		usageSource = &source
	}
	// Cost attribution only for succeeded requests. pricing_id records the
	// matched price version (provenance); estimated cost is derived and stays
	// NULL when usage is missing or the calculation fails. Pricing problems
	// never fail the request: they degrade to NULL plus a bounded log line.
	// The pricing lookup gets its own child budget strictly smaller than
	// finalizeCtx, so a lookup that burns its budget cannot starve the durable
	// row write below.
	var pricingID *string
	var estimatedCostNanoUSD *int64
	if status == "succeeded" {
		pricingCtx, pricingCancel := context.WithTimeout(finalizeCtx, pricingLookupBudget)
		pricingID, estimatedCostNanoUSD = s.resolvePricing(pricingCtx, record, usage)
		pricingCancel()
	}
	return s.store.FinalizeGatewayRequest(finalizeCtx, FinalizeParams{
		ID:                   record.ID,
		Status:               status,
		FirstChunkAt:         firstChunkAt,
		CompletedAt:          completedAt,
		LatencyMS:            &latency,
		TTFTMS:               ttftMS,
		UpstreamHTTPStatus:   upstreamStatus,
		ErrorCategory:        category,
		RetryCount:           record.RetryCount,
		PromptTokens:         promptTokens,
		CompletionTokens:     completionTokens,
		TotalTokens:          totalTokens,
		UsageSource:          usageSource,
		PricingID:            pricingID,
		EstimatedCostNanoUSD: estimatedCostNanoUSD,
		UpstreamRequestID:    upstreamRequestID,
	})
}

// resolvePricing looks up the price version effective at the request start
// time and computes the base-rate estimated cost. It returns (nil, nil) when
// no price version is effective, and (matched, nil) when the price version is
// found but the request has no usage or the integer calculation fails.
func (s *Service) resolvePricing(ctx context.Context, record GatewayRequest, usage *provider.Usage) (*string, *int64) {
	logFields := []any{
		slog.String("gateway_request_id", record.ID),
		slog.String("project_id", record.ProjectID),
		slog.String("provider", string(record.Provider)),
		slog.String("model", record.Model),
	}
	price, err := s.store.FindModelPrice(ctx, record.Provider, record.Model, record.StartedAt)
	if errors.Is(err, ErrNotFound) {
		// No effective price version (for example DeepSeek in Week 7, whose
		// cache/time-tier-aware pricing is deliberately deferred). Cost stays
		// NULL; this is expected and not an error.
		s.logger.Debug("no pricing version effective for model", logFields...)
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			s.logger.Warn("pricing lookup budget exceeded; finalizing without estimated cost", logFields...)
		} else {
			s.logger.Warn("pricing lookup failed; finalizing without estimated cost", append(logFields, slog.String("error", err.Error()))...)
		}
		return nil, nil
	}
	if usage == nil {
		return &price.ID, nil
	}
	costNano, ok := pricing.Estimate(usage.PromptTokens, usage.CompletionTokens, price.InputNanoUSDPerMillion, price.OutputNanoUSDPerMillion)
	if !ok {
		s.logger.Warn("estimated cost calculation failed; recording pricing version without cost", logFields...)
		return &price.ID, nil
	}
	return &price.ID, &costNano
}

func (s *Service) persistenceError(record GatewayRequest, category provider.ErrorCategory, err error) *GatewayError {
	s.logger.Error("gateway request finalization failed",
		slog.String("gateway_request_id", record.ID),
		slog.String("provider", string(record.Provider)),
		slog.String("project_id", record.ProjectID),
		slog.String("category", string(category)),
		slog.String("error", err.Error()),
	)
	return &GatewayError{
		Category: provider.UsagePersistenceFail,
		Message:  "usage persistence failed",
		Err:      err,
	}
}

type GatewayError struct {
	Category provider.ErrorCategory
	Message  string
	// StatusCode is the upstream HTTP status of the final attempt when the
	// error came from a provider HTTP response (0 for transport-level
	// failures). The retry whitelist needs it because provider 401/402/403 are
	// classified provider_unavailable yet must never retry.
	StatusCode int
	// RetryAfter carries a provider-supplied wait hint (presence preserved:
	// nil = no hint, a present 0 = retry immediately). It flows from the
	// adapter through to the HTTP handler so an un-retried provider 429 can
	// still write a Retry-After response header.
	RetryAfter *time.Duration
	Err        error
}

func NewError(category provider.ErrorCategory, message string) *GatewayError {
	return &GatewayError{Category: category, Message: message}
}

func (e *GatewayError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Category, e.Err)
	}
	return string(e.Category)
}

func (e *GatewayError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func errorFromProvider(err error) *GatewayError {
	if providerErr, ok := provider.AsError(err); ok {
		return &GatewayError{
			Category:   providerErr.Category,
			Message:    clientMessage(providerErr.Category),
			StatusCode: providerErr.StatusCode,
			RetryAfter: providerErr.RetryAfter,
			Err:        providerErr.Err,
		}
	}
	return nil
}

func clientMessage(category provider.ErrorCategory) string {
	switch category {
	case provider.ProviderRateLimited:
		return "provider rate limit exceeded"
	case provider.ProviderTimeout:
		return "provider request timed out"
	case provider.ProviderInvalidReq:
		return "provider rejected the request"
	case provider.ProviderUnavailable:
		return "provider is unavailable"
	case provider.StreamInterrupted:
		return "stream interrupted"
	case provider.UnsupportedFeature:
		return "request includes a feature not supported by the gateway for the selected provider"
	default:
		return "request could not be completed"
	}
}
