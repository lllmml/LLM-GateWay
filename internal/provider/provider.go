package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type Name string

const (
	OpenAI    Name = "openai"
	Anthropic Name = "anthropic"
	DeepSeek  Name = "deepseek"
)

type ErrorCategory string

const (
	InvalidRequest        ErrorCategory = "invalid_request"
	AuthenticationFailed  ErrorCategory = "authentication_failed"
	AuthorizationFailed   ErrorCategory = "authorization_failed"
	RateLimited           ErrorCategory = "rate_limited"
	ProviderNotConfigured ErrorCategory = "provider_not_configured"
	ModelNotSupported     ErrorCategory = "model_not_supported"
	ProviderRateLimited   ErrorCategory = "provider_rate_limited"
	ProviderTimeout       ErrorCategory = "provider_timeout"
	ProviderUnavailable   ErrorCategory = "provider_unavailable"
	ProviderInvalidReq    ErrorCategory = "provider_invalid_request"
	StreamInterrupted     ErrorCategory = "stream_interrupted"
	UsagePersistenceFail  ErrorCategory = "usage_persistence_failed"
	InternalError         ErrorCategory = "internal_error"
)

type ChatRequest struct {
	Model    string
	Messages []Message
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason,omitempty"`
}

type ResponseMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type Credential struct {
	APIKey          []byte
	BaseURLOverride string
}

type Result struct {
	Response          ChatResponse
	Usage             *Usage
	UpstreamStatus    int
	UpstreamRequestID string
}

type Client interface {
	CompleteChat(context.Context, ChatRequest, Credential) (Result, error)
}

type ModelRef struct {
	Provider Name
	Model    string
}

func ParseModel(model string) (ModelRef, error) {
	providerName, providerModel, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok || providerName == "" || providerModel == "" || strings.Contains(providerModel, "/") {
		return ModelRef{}, &Error{Category: ModelNotSupported, Message: "model must use provider/model-id format"}
	}
	switch Name(providerName) {
	case OpenAI:
		return ModelRef{Provider: OpenAI, Model: providerModel}, nil
	case Anthropic:
		return ModelRef{Provider: Anthropic, Model: providerModel}, nil
	case DeepSeek:
		return ModelRef{Provider: DeepSeek, Model: providerModel}, nil
	default:
		return ModelRef{}, &Error{Category: ModelNotSupported, Message: "provider namespace is not supported"}
	}
}

type Error struct {
	Category          ErrorCategory
	StatusCode        int
	UpstreamRequestID string
	Message           string
	Err               error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return string(e.Category)
	}
	return fmt.Sprintf("%s: %v", e.Category, e.Err)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func AsError(err error) (*Error, bool) {
	var providerErr *Error
	if errors.As(err, &providerErr) {
		return providerErr, true
	}
	return nil, false
}
