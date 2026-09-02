package dataplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

var (
	ErrNotFound = errors.New("dataplane record not found")
)

const finalizeTimeout = 5 * time.Second

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
	CompletedAt          time.Time
	LatencyMS            *int64
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
	FinalizeGatewayRequest(context.Context, FinalizeParams) error
}

type Options struct {
	Store            Store
	VirtualKeyPepper []byte
	CredentialCipher *security.CredentialCipher
	UpstreamTimeout  time.Duration
	ProviderRegistry *provider.Registry
	Logger           *slog.Logger
}

type Service struct {
	store            Store
	virtualKeyPepper []byte
	credentialCipher *security.CredentialCipher
	upstreamTimeout  time.Duration
	providers        *provider.Registry
	logger           *slog.Logger
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
	if options.UpstreamTimeout <= 0 {
		return nil, errors.New("upstream timeout must be positive")
	}
	if options.ProviderRegistry == nil {
		return nil, errors.New("provider registry is required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		store:            options.Store,
		virtualKeyPepper: append([]byte(nil), options.VirtualKeyPepper...),
		credentialCipher: options.CredentialCipher,
		upstreamTimeout:  options.UpstreamTimeout,
		providers:        options.ProviderRegistry,
		logger:           logger,
	}, nil
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
	modelRef, err := provider.ParseModel(chat.Model)
	if err != nil {
		return provider.Result{}, GatewayRequest{}, NewError(provider.ModelNotSupported, "model must use provider/model-id format")
	}
	if chat.Stream {
		return provider.Result{}, GatewayRequest{}, NewError(provider.InvalidRequest, "streaming chat completions are not supported in this milestone")
	}
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

	now := time.Now().UTC()
	record, err := s.store.CreateGatewayRequest(ctx, CreateRequestParams{
		ProjectID:            auth.ProjectID,
		VirtualKeyID:         auth.VirtualKeyID,
		ProviderCredentialID: credential.ID,
		Provider:             modelRef.Provider,
		Model:                modelRef.Model,
		IsStream:             chat.Stream,
		StartedAt:            now,
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

	upstreamCtx, cancel := context.WithTimeout(ctx, s.upstreamTimeout)
	defer cancel()
	upstreamChat := chat
	upstreamChat.Model = modelRef.Model
	result, err := client.CompleteChat(upstreamCtx, upstreamChat, provider.Credential{
		APIKey:          apiKey,
		BaseURLOverride: credential.BaseURLOverride,
	})
	if err != nil {
		gatewayErr := errorFromProvider(err)
		if gatewayErr == nil {
			gatewayErr = NewError(provider.ProviderUnavailable, "provider request failed")
		}
		if finalizeErr := s.finalize(ctx, record, &result, &gatewayErr.Category, nil); finalizeErr != nil {
			return result, record, s.persistenceError(record, gatewayErr.Category, finalizeErr)
		}
		return result, record, gatewayErr
	}

	if err := s.finalize(ctx, record, &result, nil, result.Usage); err != nil {
		return result, record, s.persistenceError(record, "", err)
	}
	return result, record, nil
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
	return s.store.FinalizeGatewayRequest(finalizeCtx, FinalizeParams{
		ID:                 record.ID,
		Status:             status,
		CompletedAt:        completedAt,
		LatencyMS:          &latency,
		UpstreamHTTPStatus: upstreamStatus,
		ErrorCategory:      category,
		RetryCount:         0,
		PromptTokens:       promptTokens,
		CompletionTokens:   completionTokens,
		TotalTokens:        totalTokens,
		UsageSource:        usageSource,
		UpstreamRequestID:  upstreamRequestID,
	})
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
	Err      error
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
		return NewError(providerErr.Category, clientMessage(providerErr.Category))
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
	default:
		return "request could not be completed"
	}
}
