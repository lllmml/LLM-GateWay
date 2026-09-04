// Package oaiwire implements the wire-format helpers shared by providers that
// speak the OpenAI-compatible Chat Completions HTTP/JSON/SSE protocol
// (currently OpenAI and DeepSeek).
//
// The package deliberately contains only stateless protocol mechanics: endpoint
// construction, transport tuning, bounded decoding, bounded error-body
// reading, generic error-envelope message extraction, and wire-type
// validation. It contains no provider error policy: mapping an upstream HTTP
// status onto a gateway error category belongs to each adapter, because the
// meaning of a status (for example 401 or 402) is a provider-scoped decision.
// Each adapter owns its base URL and path constants, request-ID header policy,
// HTTP status classification, stream terminal semantics, and any
// provider-specific validation, so genuine provider differences (see
// docs/provider-capabilities.md) stay visible in the adapter that owns them.
package oaiwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

const (
	MaxErrorBodyBytes    = 8 << 10
	MaxResponseBodyBytes = 8 << 20
	MaxStreamEventBytes  = 1 << 20
)

// NewTransport returns a long-lived http.Transport tuned for provider chat
// requests and streams. Callers reuse one transport and never create one per
// request.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
}

func NewHTTPClient() *http.Client {
	return &http.Client{Transport: NewTransport()}
}

// Endpoint validates that baseURL is an http(s) origin without credentials,
// query, or fragment, and appends path to it. The path is provider-owned so
// OpenAI and DeepSeek can differ without the shared layer deciding.
func Endpoint(baseURL, path string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid provider base URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("invalid provider base URL")
	}
	return parsed.String() + path, nil
}

func IsSSEContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "text/event-stream"
}

// ClassifyTransportError maps transport-level failures (timeouts,
// cancellation, connection errors) onto stable provider error categories.
func ClassifyTransportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &provider.Error{Category: provider.ProviderTimeout, Err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &provider.Error{Category: provider.ProviderTimeout, Err: err}
	}
	return &provider.Error{Category: provider.ProviderUnavailable, Err: err}
}

// ReadBoundedErrorBody reads at most MaxErrorBodyBytes+1 bytes from an
// upstream error body. When the body fits the bound it is drained to EOF so
// the connection can be reused. It reports whether the body exceeded the
// bound.
func ReadBoundedErrorBody(body io.Reader) ([]byte, bool) {
	bodyBytes, err := io.ReadAll(io.LimitReader(body, MaxErrorBodyBytes+1))
	if err != nil {
		return nil, false
	}
	if len(bodyBytes) > MaxErrorBodyBytes {
		return bodyBytes[:MaxErrorBodyBytes], true
	}
	_, _ = io.Copy(io.Discard, body)
	return bodyBytes, false
}

// ErrorMessage extracts the provider message from a generic
// {"error":{"message":...}} envelope. It returns "" when the payload does not
// decode or carries no message, so adapters can fall back to their own stable
// message. Callers must not forward raw payloads anywhere.
func ErrorMessage(bodyBytes []byte) string {
	var decoded struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&decoded); err != nil {
		return ""
	}
	return strings.TrimSpace(decoded.Error.Message)
}

// DecodeSingle decodes exactly one JSON value from data into destination and
// rejects trailing JSON values.
func DecodeSingle(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decoded value contained multiple JSON values")
	}
	return nil
}

// Wire types shared by OpenAI-compatible adapters. Structs tolerate unknown
// provider fields (encoding/json ignores them) so forward-compatible additions
// such as DeepSeek reasoning_content do not break parsing.
type Request struct {
	Model         string           `json:"model"`
	Messages      []RequestMessage `json:"messages"`
	Stream        bool             `json:"stream"`
	StreamOptions *StreamOptions   `json:"stream_options,omitempty"`
}

type RequestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type Response struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []ResponseChoice `json:"choices"`
	Usage   *Usage           `json:"usage"`
}

type ResponseChoice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
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

type StreamChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices *[]StreamChoice `json:"choices"`
	Usage   *Usage          `json:"usage"`
}

type StreamChoice struct {
	Index        int          `json:"index"`
	Delta        *StreamDelta `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

type StreamDelta struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
}

// ValidateUsage checks the numeric invariants shared by OpenAI-compatible
// usage payloads: nonnegative counters and a consistent total.
func ValidateUsage(usage *Usage) error {
	if usage == nil {
		return nil
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return errors.New("provider response usage values must be nonnegative")
	}
	if usage.TotalTokens < usage.PromptTokens+usage.CompletionTokens {
		return errors.New("provider response usage totals are inconsistent")
	}
	return nil
}

// NormalizeUsage converts wire usage into the gateway-normalized usage type.
func NormalizeUsage(usage *Usage) *provider.Usage {
	if usage == nil {
		return nil
	}
	return &provider.Usage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
}

// ValidateResponse checks the required fields of a non-streaming chat
// completion response.
func ValidateResponse(decoded *Response) error {
	if strings.TrimSpace(decoded.ID) == "" || strings.TrimSpace(decoded.Object) == "" || strings.TrimSpace(decoded.Model) == "" {
		return errors.New("provider response missing required fields")
	}
	if len(decoded.Choices) == 0 {
		return errors.New("provider response missing choices")
	}
	for _, choice := range decoded.Choices {
		if strings.TrimSpace(choice.Message.Role) == "" {
			return errors.New("provider response choice missing message role")
		}
	}
	return ValidateUsage(decoded.Usage)
}
