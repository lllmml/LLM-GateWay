// Package anthropic implements the Anthropic Messages API adapter.
//
// Anthropic is deliberately NOT an OpenAI-compatible provider, so this package
// does not import internal/provider/oaiwire. It shares only the generic
// provider interfaces in internal/provider and the provider-neutral bounded
// SSE decoder in internal/provider/sse. Contract facts were verified against
// the official Anthropic Messages, Messages Streaming, and Errors API docs and
// the anthropic-sdk-typescript source, all fetched 2026-09; see
// docs/provider-capabilities.md for the evidence matrix. Provider API behavior
// is fast-moving: re-verify each row before launch.
//
// The adapter owns the genuine Anthropic differences (each documented in the
// matrix):
//
//   - endpoint POST /v1/messages on https://api.anthropic.com;
//   - authentication via the x-api-key header plus the required
//     anthropic-version: 2023-06-01 header (no Authorization: Bearer);
//   - max_tokens is REQUIRED by the Messages API. The public gateway subset
//     gained an optional max_tokens in Week 6; when the client omits it the
//     adapter applies a documented default (defaultMaxTokens) instead of
//     silently inventing per-request behavior;
//   - system messages are hoisted from the OpenAI-style messages array into
//     the top-level Anthropic system field, because the Messages API expects
//     system context there and treats messages as alternating user/assistant
//     turns;
//   - responses carry content blocks and a stop_reason instead of
//     choices[].message; the adapter joins text blocks into the OpenAI-
//     compatible content channel, maps stop reasons onto OpenAI finish
//     reasons, synthesizes the object/created envelope fields Anthropic does
//     not provide, and normalizes usage input_tokens/output_tokens into
//     prompt/completion/total (total is computed; Anthropic does not report
//     it);
//   - streaming uses NAMED SSE events (message_start, content_block_delta,
//     message_delta, message_stop, ping, error) and has no data: [DONE]
//     marker. The adapter re-encodes the text deltas into OpenAI-compatible
//     chunk envelopes (unlike the OpenAI/DeepSeek byte passthrough) and emits
//     a synthetic [DONE] at message_stop so the shared Data Plane stream
//     contract stays intact. usage.input_tokens arrives at message_start and
//     usage.output_tokens arrives (cumulative) at message_delta, so usage is
//     committed only at message_stop;
//   - the HTTP-status -> gateway-category policy is adapter-owned
//     (anthropicErrorCategory), keeping server-side conditions (401/402/403,
//     5xx/529) out of the client-facing invalid-request bucket.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/sse"
)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	messagesPath     = "/v1/messages"
	apiVersionHeader = "anthropic-version"
	apiVersion       = "2023-06-01"
	// defaultMaxTokens is applied when the client omits max_tokens, because
	// the Messages API requires it. This is a gateway policy default, not a
	// provider contract: Anthropic models may stop before it, and different
	// models cap max_tokens differently (an over-cap value is rejected by the
	// provider with a 400 the gateway forwards honestly as
	// provider_invalid_request).
	defaultMaxTokens = 4096

	// Byte bounds mirror the values shared by the OpenAI-compatible adapters
	// (oaiwire). They are duplicated here deliberately so the non-OpenAI
	// provider stays independent of the OpenAI wire package; main.go injects
	// one shared long-lived http.Client across all adapters, so production
	// never relies on these local defaults.
	maxErrorBodyBytes    = 8 << 10
	maxResponseBodyBytes = 8 << 20
	maxStreamEventBytes  = 1 << 20
)

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	return &Client{httpClient: httpClient, baseURL: defaultBaseURL}
}

// NewHTTPClient exists for adapter parity and tests. main.go builds ONE shared
// transport and injects the resulting client, so the tuning below must stay in
// lockstep with oaiwire.NewTransport (the Week 5 source of truth).
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: NewTransport()}
}

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

func (c *Client) CompleteChat(ctx context.Context, chat provider.ChatRequest, credential provider.Credential) (provider.Result, error) {
	encoded, endpoint, err := c.buildRequest(chat, credential, false)
	if err != nil {
		return provider.Result{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return provider.Result{}, &provider.Error{Category: provider.InternalError, Err: err}
	}
	applyHeaders(request.Header, credential.APIKey, "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return provider.Result{}, classifyTransportError(err)
	}
	defer response.Body.Close()

	upstreamRequestID := requestID(response)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.Result{
			UpstreamStatus:    response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
		}, classifyResponseError(response, upstreamRequestID)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyBytes+1))
	if err != nil {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, &provider.Error{
			Category:          provider.ProviderUnavailable,
			StatusCode:        response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
			Err:               fmt.Errorf("read provider response: %w", err),
		}
	}
	if len(bodyBytes) > maxResponseBodyBytes {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, &provider.Error{
			Category:          provider.ProviderUnavailable,
			StatusCode:        response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
			Err:               errors.New("provider response body exceeded limit"),
		}
	}

	var decoded wireResponse
	if err := decodeSingle(bodyBytes, &decoded); err != nil {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, malformedResponse(response.StatusCode, upstreamRequestID, err)
	}
	if err := validateResponse(&decoded); err != nil {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, malformedResponse(response.StatusCode, upstreamRequestID, err)
	}

	responseText := joinTextBlocks(decoded.Content)
	usage := normalizeUsage(decoded.Usage)
	normalized := provider.ChatResponse{
		ID:      decoded.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   decoded.Model,
		Choices: []provider.Choice{{
			Index: 0,
			Message: provider.ResponseMessage{
				Role:    "assistant",
				Content: responseText,
			},
			FinishReason: finishReason(decoded.StopReason),
		}},
		Usage: usage,
	}
	return provider.Result{
		Response:          normalized,
		Usage:             usage,
		UpstreamStatus:    response.StatusCode,
		UpstreamRequestID: upstreamRequestID,
	}, nil
}

func (c *Client) StreamChat(ctx context.Context, chat provider.ChatRequest, credential provider.Credential) (provider.StreamResult, error) {
	encoded, endpoint, err := c.buildRequest(chat, credential, true)
	if err != nil {
		return provider.StreamResult{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return provider.StreamResult{}, &provider.Error{Category: provider.InternalError, Err: err}
	}
	applyHeaders(request.Header, credential.APIKey, "text/event-stream")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return provider.StreamResult{}, classifyTransportError(err)
	}

	upstreamRequestID := requestID(response)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return provider.StreamResult{
			UpstreamStatus:    response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
		}, classifyResponseError(response, upstreamRequestID)
	}
	if !isSSEContentType(response.Header.Get("Content-Type")) {
		defer response.Body.Close()
		return provider.StreamResult{
				UpstreamStatus:    response.StatusCode,
				UpstreamRequestID: upstreamRequestID,
			}, &provider.Error{
				Category:          provider.ProviderUnavailable,
				StatusCode:        response.StatusCode,
				UpstreamRequestID: upstreamRequestID,
				Message:           "provider stream response content type is invalid",
			}
	}

	return provider.StreamResult{
		Stream: &chatStream{
			body:              response.Body,
			decoder:           sse.NewDecoder(response.Body, maxStreamEventBytes),
			upstreamStatus:    response.StatusCode,
			upstreamRequestID: upstreamRequestID,
		},
		UpstreamStatus:    response.StatusCode,
		UpstreamRequestID: upstreamRequestID,
	}, nil
}

func applyHeaders(header http.Header, apiKey []byte, accept string) {
	header.Set("x-api-key", string(apiKey))
	header.Set(apiVersionHeader, apiVersion)
	header.Set("Content-Type", "application/json")
	header.Set("Accept", accept)
}

func (c *Client) buildRequest(chat provider.ChatRequest, credential provider.Credential, stream bool) ([]byte, string, error) {
	baseURL := c.baseURL
	if credential.BaseURLOverride != "" {
		baseURL = credential.BaseURLOverride
	}
	endpoint, err := messagesEndpoint(baseURL)
	if err != nil {
		return nil, "", &provider.Error{Category: provider.ProviderNotConfigured, Err: err}
	}

	model := chat.Model
	if parsed, err := provider.ParseModel(chat.Model); err == nil && parsed.Provider == provider.Anthropic {
		model = parsed.Model
	}
	var systemParts []string
	messages := make([]wireRequestMessage, 0, len(chat.Messages))
	for _, current := range chat.Messages {
		if current.Role == "system" {
			systemParts = append(systemParts, current.Content)
			continue
		}
		messages = append(messages, wireRequestMessage{Role: current.Role, Content: current.Content})
	}
	maxTokens := int64(defaultMaxTokens)
	if chat.MaxTokens != nil && *chat.MaxTokens > 0 {
		maxTokens = *chat.MaxTokens
	}
	body := wireRequest{
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  messages,
		Stream:    stream,
	}
	if len(systemParts) > 0 {
		body.System = strings.Join(systemParts, "\n\n")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", &provider.Error{Category: provider.InvalidRequest, Err: err}
	}
	return encoded, endpoint, nil
}

func messagesEndpoint(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid provider base URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("invalid provider base URL")
	}
	return parsed.String() + messagesPath, nil
}

func requestID(response *http.Response) string {
	// Anthropic documents that every API response carries a unique request-id
	// header. Header lookup is case-insensitive.
	return response.Header.Get("request-id")
}

// classifyResponseError maps a non-2xx Anthropic response onto a stable
// provider error. The adapter owns this HTTP-status -> category policy. The
// documented statuses are mapped explicitly: 400 invalid_request_error (also
// spend-limit and other 4xx cases), 401 authentication_error, 402
// billing_error, 403 permission_error, 404 not_found_error, 409
// conflict_error, 413 request_too_large, 429 rate_limit_error, 500 api_error,
// 504 timeout_error, 529 overloaded_error. Server-side conditions of the
// configured provider access (401/402/403, 5xx/529) stay provider_unavailable
// and never surface as a client invalid-request 400.
func classifyResponseError(response *http.Response, upstreamRequestID string) error {
	message := "provider request failed"
	bodyBytes, tooLarge := readBoundedErrorBody(response.Body)
	if tooLarge {
		message = "provider error body exceeded limit"
	} else if extracted := errorMessage(bodyBytes); extracted != "" {
		message = extracted
	}
	return &provider.Error{
		Category:          anthropicErrorCategory(response.StatusCode),
		StatusCode:        response.StatusCode,
		UpstreamRequestID: upstreamRequestID,
		Message:           message,
	}
}

func anthropicErrorCategory(statusCode int) provider.ErrorCategory {
	switch statusCode {
	case http.StatusTooManyRequests:
		return provider.ProviderRateLimited
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return provider.ProviderTimeout
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return provider.ProviderUnavailable
	}
	if statusCode >= 500 {
		return provider.ProviderUnavailable
	}
	if statusCode >= 400 {
		return provider.ProviderInvalidReq
	}
	return provider.ProviderUnavailable
}

func malformedResponse(statusCode int, upstreamRequestID string, err error) error {
	return &provider.Error{
		Category:          provider.ProviderUnavailable,
		StatusCode:        statusCode,
		UpstreamRequestID: upstreamRequestID,
		Err:               fmt.Errorf("decode provider response: %w", err),
	}
}

// errorTypeCategory classifies an Anthropic error event delivered mid-stream.
// The Data Plane finalizes any post-commit failure as stream_interrupted
// regardless of category; the classification is still kept adapter-owned so
// adapter-level tests and logs see the provider's actual failure class.
func errorTypeCategory(errorType string) provider.ErrorCategory {
	switch errorType {
	case "rate_limit_error":
		return provider.ProviderRateLimited
	case "timeout_error":
		return provider.ProviderTimeout
	case "authentication_error", "permission_error", "billing_error", "api_error", "overloaded_error":
		return provider.ProviderUnavailable
	default:
		return provider.ProviderInvalidReq
	}
}

func classifyTransportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &provider.Error{Category: provider.ProviderTimeout, Err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &provider.Error{Category: provider.ProviderTimeout, Err: err}
	}
	return &provider.Error{Category: provider.ProviderUnavailable, Err: err}
}

func readBoundedErrorBody(body io.Reader) ([]byte, bool) {
	bodyBytes, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes+1))
	if err != nil {
		return nil, false
	}
	if len(bodyBytes) > maxErrorBodyBytes {
		return bodyBytes[:maxErrorBodyBytes], true
	}
	_, _ = io.Copy(io.Discard, body)
	return bodyBytes, false
}

// errorMessage extracts the provider message from the Anthropic error envelope
// {"type":"error","error":{"type":...,"message":...,"request_id":...}}. It
// returns "" when the payload does not decode or carries no message. Callers
// must never forward the raw payload.
func errorMessage(bodyBytes []byte) string {
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

func decodeSingle(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("decoded value contained multiple JSON values")
	}
	return nil
}

func isSSEContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "text/event-stream"
}

// Wire types for the Anthropic Messages API. Structs tolerate unknown fields
// so forward-compatible additions do not break parsing.
type wireRequest struct {
	Model     string               `json:"model"`
	MaxTokens int64                `json:"max_tokens"`
	Messages  []wireRequestMessage `json:"messages"`
	System    string               `json:"system,omitempty"`
	Stream    bool                 `json:"stream"`
}

type wireRequestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireResponse struct {
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	Model      string      `json:"model"`
	Content    []wireBlock `json:"content"`
	StopReason *string     `json:"stop_reason"`
	Usage      *wireUsage  `json:"usage"`
}

type wireBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func validateResponse(decoded *wireResponse) error {
	if strings.TrimSpace(decoded.ID) == "" || strings.TrimSpace(decoded.Model) == "" || strings.TrimSpace(decoded.Type) == "" {
		return errors.New("provider response missing required fields")
	}
	if decoded.Type != "message" {
		return errors.New("provider response is not a message")
	}
	if decoded.Usage != nil && (decoded.Usage.InputTokens < 0 || decoded.Usage.OutputTokens < 0) {
		return errors.New("provider response usage values must be nonnegative")
	}
	return nil
}

// joinTextBlocks concatenates the text content blocks in order. Anthropic
// returns content as an array of blocks; the gateway subset only has a single
// OpenAI-compatible text channel, and the gateway never requests tools or
// thinking, so only type "text" blocks are forwarded and any other block type
// is intentionally not represented (the same documented limitation as DeepSeek
// reasoning_content). Concatenation without a separator keeps the non-stream
// result identical to what a stream of text deltas would produce.
func joinTextBlocks(content []wireBlock) string {
	var builder strings.Builder
	for _, block := range content {
		if block.Type == "text" {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

// normalizeUsage maps Anthropic usage onto the gateway usage shape. Anthropic
// reports input_tokens and output_tokens only (no total); total is computed.
// Cache/citation token breakdowns are not separately persisted (revisit in
// Week 7 pricing).
func normalizeUsage(usage *wireUsage) *provider.Usage {
	if usage == nil {
		return nil
	}
	return &provider.Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
	}
}

// finishReason maps an Anthropic stop_reason onto the OpenAI-compatible
// finish_reason vocabulary of the public subset. Reasons that cannot occur
// under the gateway subset still map defensively (tool_use -> tool_calls even
// though the gateway never requests tools); genuinely unknown reasons map to
// "" because Anthropic's versioning policy allows new values and a completed
// turn must not be turned into a failure by an unfamiliar stop reason.
func finishReason(stopReason *string) string {
	if stopReason == nil {
		return ""
	}
	switch *stopReason {
	case "end_turn":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return ""
	}
}

// chatStream decodes the Anthropic named SSE event stream and re-encodes it as
// OpenAI-compatible chunk events plus a synthetic data: [DONE] on
// message_stop, so the shared Data Plane contract (DONE is the only success
// marker; usage is committed only at DONE) is preserved. Usage.input_tokens is
// captured from message_start; usage.output_tokens is captured (cumulative)
// from message_delta. Non-text content deltas (thinking, signature,
// input_json) and unknown named event types are skipped gracefully, per the
// Anthropic versioning policy.
type chatStream struct {
	body              io.ReadCloser
	decoder           *sse.Decoder
	upstreamStatus    int
	upstreamRequestID string

	// Synthesis state drawn from message_start.
	messageID string
	model     string
	created   int64

	emittedFirstContent bool
	stopSent            bool
	inputTokens         int64
	outputTokens        int64

	usage  *provider.Usage
	done   bool
	failed bool
}

func (s *chatStream) Next() (provider.StreamEvent, error) {
	if s.done || s.failed {
		return provider.StreamEvent{}, io.EOF
	}
	for {
		event, err := s.decoder.Next()
		if err != nil {
			category := provider.StreamInterrupted
			if errors.Is(err, sse.ErrEventTooLarge) {
				category = provider.ProviderUnavailable
			}
			return provider.StreamEvent{}, s.fail(&provider.Error{
				Category:          category,
				StatusCode:        s.upstreamStatus,
				UpstreamRequestID: s.upstreamRequestID,
				Err:               err,
			})
		}
		switch event.Name {
		case "message_start":
			if s.messageID != "" {
				return provider.StreamEvent{}, s.fail(s.sequenceError("provider stream sent message_start more than once"))
			}
			var payload struct {
				Message struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage struct {
						InputTokens int64 `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := decodeSingle(event.Data, &payload); err != nil {
				return provider.StreamEvent{}, s.fail(s.decodeError(err))
			}
			if strings.TrimSpace(payload.Message.ID) == "" || strings.TrimSpace(payload.Message.Model) == "" {
				return provider.StreamEvent{}, s.fail(s.sequenceError("provider stream message_start missing required fields"))
			}
			s.messageID = payload.Message.ID
			s.model = payload.Message.Model
			s.created = time.Now().Unix()
			s.inputTokens = payload.Message.Usage.InputTokens
			continue
		case "content_block_delta":
			if s.stopSent {
				return provider.StreamEvent{}, s.fail(s.sequenceError("provider stream sent content after the final message delta"))
			}
			if s.messageID == "" {
				return provider.StreamEvent{}, s.fail(s.sequenceError("provider stream sent content before message_start"))
			}
			var payload struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := decodeSingle(event.Data, &payload); err != nil {
				return provider.StreamEvent{}, s.fail(s.decodeError(err))
			}
			if payload.Delta.Type != "" && payload.Delta.Type != "text_delta" {
				// Thinking/signature/input-json or a future delta type: the
				// gateway has no channel for it, so skip it gracefully.
				continue
			}
			if payload.Delta.Text == "" {
				continue
			}
			encoded, err := json.Marshal(s.contentChunk(payload.Delta.Text))
			if err != nil {
				return provider.StreamEvent{}, s.fail(&provider.Error{Category: provider.InternalError, Err: err})
			}
			return provider.StreamEvent{Data: encoded}, nil
		case "message_delta":
			var payload struct {
				Delta struct {
					StopReason *string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := decodeSingle(event.Data, &payload); err != nil {
				return provider.StreamEvent{}, s.fail(s.decodeError(err))
			}
			s.outputTokens = payload.Usage.OutputTokens
			if reason := finishReason(payload.Delta.StopReason); reason != "" && !s.stopSent {
				s.stopSent = true
				encoded, err := json.Marshal(s.finishChunk(reason))
				if err != nil {
					return provider.StreamEvent{}, s.fail(&provider.Error{Category: provider.InternalError, Err: err})
				}
				return provider.StreamEvent{Data: encoded}, nil
			}
			continue
		case "message_stop":
			s.done = true
			s.usage = &provider.Usage{
				PromptTokens:     s.inputTokens,
				CompletionTokens: s.outputTokens,
				TotalTokens:      s.inputTokens + s.outputTokens,
			}
			return provider.StreamEvent{Data: []byte("[DONE]"), Done: true}, nil
		case "error":
			var payload struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := decodeSingle(event.Data, &payload); err != nil {
				return provider.StreamEvent{}, s.fail(s.decodeError(err))
			}
			return provider.StreamEvent{}, s.fail(&provider.Error{
				Category:          errorTypeCategory(payload.Error.Type),
				StatusCode:        s.upstreamStatus,
				UpstreamRequestID: s.upstreamRequestID,
				Message:           "provider stream error",
			})
		case "ping":
			continue
		case "content_block_start", "content_block_stop":
			continue
		case "":
			// The Anthropic stream contract names every event. A bare data
			// frame is not part of it, so fail loudly rather than guessing.
			return provider.StreamEvent{}, s.fail(s.sequenceError("provider stream event is missing its event name"))
		default:
			// Anthropic's versioning policy allows new event types; unknown
			// named events must be handled gracefully.
			continue
		}
	}
}

func (s *chatStream) fail(err error) error {
	s.failed = true
	return err
}

func (s *chatStream) sequenceError(message string) error {
	return &provider.Error{
		Category:          provider.StreamInterrupted,
		StatusCode:        s.upstreamStatus,
		UpstreamRequestID: s.upstreamRequestID,
		Err:               errors.New(message),
	}
}

func (s *chatStream) decodeError(err error) error {
	return &provider.Error{
		Category:          provider.StreamInterrupted,
		StatusCode:        s.upstreamStatus,
		UpstreamRequestID: s.upstreamRequestID,
		Err:               fmt.Errorf("decode provider stream event: %w", err),
	}
}

func (s *chatStream) Close() error {
	return s.body.Close()
}

func (s *chatStream) Usage() *provider.Usage {
	if s.usage == nil {
		return nil
	}
	copied := *s.usage
	return &copied
}

// contentChunk builds the OpenAI-compatible chunk envelope for one text delta.
// The first content event also carries the assistant role marker, matching the
// conventional OpenAI chunk sequence.
func (s *chatStream) contentChunk(text string) chunkEnvelope {
	delta := chunkDelta{Content: text}
	if !s.emittedFirstContent {
		delta.Role = "assistant"
		s.emittedFirstContent = true
	}
	return s.chunk(delta, nil)
}

func (s *chatStream) finishChunk(reason string) chunkEnvelope {
	return s.chunk(chunkDelta{}, &reason)
}

func (s *chatStream) chunk(delta chunkDelta, finishReason *string) chunkEnvelope {
	return chunkEnvelope{
		ID:      s.messageID,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []chunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	}
}

type chunkEnvelope struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason,omitempty"`
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}
