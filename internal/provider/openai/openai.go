package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/oaiwire"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/sse"
)

const (
	defaultBaseURL      = "https://api.openai.com"
	chatCompletionsPath = "/v1/chat/completions"
)

// Local aliases so adapter tests keep asserting the same constants.
const (
	maxErrorBodyBytes    = oaiwire.MaxErrorBodyBytes
	maxResponseBodyBytes = oaiwire.MaxResponseBodyBytes
	maxStreamEventBytes  = oaiwire.MaxStreamEventBytes
)

// requestBody is the wire request body shared by OpenAI-compatible providers.
type requestBody = oaiwire.Request

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

func NewHTTPClient() *http.Client {
	return oaiwire.NewHTTPClient()
}

func NewTransport() *http.Transport {
	return oaiwire.NewTransport()
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
	request.Header.Set("Authorization", "Bearer "+string(credential.APIKey))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return provider.Result{}, oaiwire.ClassifyTransportError(err)
	}
	defer response.Body.Close()

	upstreamRequestID := upstreamRequestID(response)
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

	var decoded oaiwire.Response
	if err := oaiwire.DecodeSingle(bodyBytes, &decoded); err != nil {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, &provider.Error{
			Category:          provider.ProviderUnavailable,
			StatusCode:        response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
			Err:               fmt.Errorf("decode provider response: %w", err),
		}
	}
	if err := oaiwire.ValidateResponse(&decoded); err != nil {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, &provider.Error{
			Category:          provider.ProviderUnavailable,
			StatusCode:        response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
			Err:               err,
		}
	}

	result := responseFromWire(decoded)
	return provider.Result{
		Response:          result.Response,
		Usage:             result.Usage,
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
	request.Header.Set("Authorization", "Bearer "+string(credential.APIKey))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return provider.StreamResult{}, oaiwire.ClassifyTransportError(err)
	}

	upstreamRequestID := upstreamRequestID(response)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return provider.StreamResult{
			UpstreamStatus:    response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
		}, classifyResponseError(response, upstreamRequestID)
	}
	if !oaiwire.IsSSEContentType(response.Header.Get("Content-Type")) {
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

func (c *Client) buildRequest(chat provider.ChatRequest, credential provider.Credential, stream bool) ([]byte, string, error) {
	baseURL := c.baseURL
	if credential.BaseURLOverride != "" {
		baseURL = credential.BaseURLOverride
	}
	endpoint, err := chatEndpoint(baseURL)
	if err != nil {
		return nil, "", &provider.Error{Category: provider.ProviderNotConfigured, Err: err}
	}

	model := chat.Model
	if parsed, err := provider.ParseModel(chat.Model); err == nil && parsed.Provider == provider.OpenAI {
		model = parsed.Model
	}
	body := oaiwire.Request{
		Model:     model,
		Messages:  make([]oaiwire.RequestMessage, 0, len(chat.Messages)),
		Stream:    stream,
		MaxTokens: chat.MaxTokens,
	}
	if stream {
		body.StreamOptions = &oaiwire.StreamOptions{IncludeUsage: true}
	}
	for _, current := range chat.Messages {
		body.Messages = append(body.Messages, oaiwire.RequestMessage{Role: current.Role, Content: current.Content})
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", &provider.Error{Category: provider.InvalidRequest, Err: err}
	}
	return encoded, endpoint, nil
}

func chatEndpoint(baseURL string) (string, error) {
	return oaiwire.Endpoint(baseURL, chatCompletionsPath)
}

func upstreamRequestID(response *http.Response) string {
	id := response.Header.Get("X-Request-ID")
	if id == "" {
		id = response.Header.Get("OpenAI-Request-ID")
	}
	return id
}

// classifyResponseError maps a non-2xx OpenAI response onto a stable provider
// error. The adapter owns this HTTP-status -> category policy: a 401 (bad
// upstream credential) or 402 (billing) means the gateway's configured
// provider access is not usable - a server-side condition - not that the
// client request is malformed, so it must not surface as provider_invalid_request.
func classifyResponseError(response *http.Response, upstreamRequestID string) error {
	message := "provider request failed"
	bodyBytes, tooLarge := oaiwire.ReadBoundedErrorBody(response.Body)
	if tooLarge {
		message = "provider error body exceeded limit"
	} else if extracted := oaiwire.ErrorMessage(bodyBytes); extracted != "" {
		message = extracted
	}
	return &provider.Error{
		Category:          openAIErrorCategory(response.StatusCode),
		StatusCode:        response.StatusCode,
		UpstreamRequestID: upstreamRequestID,
		Message:           message,
	}
}

func openAIErrorCategory(statusCode int) provider.ErrorCategory {
	switch statusCode {
	case http.StatusTooManyRequests:
		return provider.ProviderRateLimited
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return provider.ProviderTimeout
	case http.StatusUnauthorized, http.StatusPaymentRequired:
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

func responseFromWire(decoded oaiwire.Response) provider.Result {
	result := provider.ChatResponse{
		ID:      decoded.ID,
		Object:  decoded.Object,
		Created: decoded.Created,
		Model:   decoded.Model,
		Choices: make([]provider.Choice, 0, len(decoded.Choices)),
	}
	for _, choice := range decoded.Choices {
		result.Choices = append(result.Choices, provider.Choice{
			Index: choice.Index,
			Message: provider.ResponseMessage{
				Role:    choice.Message.Role,
				Content: choice.Message.Content,
			},
			FinishReason: choice.FinishReason,
		})
	}
	usage := oaiwire.NormalizeUsage(decoded.Usage)
	result.Usage = usage
	return provider.Result{Response: result, Usage: usage}
}

// chatStream implements the OpenAI stream terminal rule: final usage arrives
// on a dedicated chunk with choices==[] and the data: [DONE] message is the
// only success marker. Usage is committed only when [DONE] is observed.
type chatStream struct {
	body              io.ReadCloser
	decoder           *sse.Decoder
	upstreamStatus    int
	upstreamRequestID string
	pendingUsage      *provider.Usage
	usage             *provider.Usage
	done              bool
	failed            bool
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
		data := bytes.TrimSpace(event.Data)
		if bytes.Equal(data, []byte("[DONE]")) {
			s.done = true
			if s.pendingUsage != nil {
				s.usage = s.pendingUsage
				s.pendingUsage = nil
			}
			return provider.StreamEvent{Event: event.Name, Data: []byte("[DONE]"), Done: true}, nil
		}
		if s.pendingUsage != nil {
			return provider.StreamEvent{}, s.fail(s.streamSequenceError("provider stream event followed final usage before done"))
		}
		var decoded oaiwire.StreamChunk
		if err := oaiwire.DecodeSingle(event.Data, &decoded); err != nil {
			return provider.StreamEvent{}, s.fail(&provider.Error{
				Category:          provider.StreamInterrupted,
				StatusCode:        s.upstreamStatus,
				UpstreamRequestID: s.upstreamRequestID,
				Err:               fmt.Errorf("decode provider stream event: %w", err),
			})
		}
		if err := validateStreamChunk(decoded); err != nil {
			return provider.StreamEvent{}, s.fail(&provider.Error{
				Category:          provider.StreamInterrupted,
				StatusCode:        s.upstreamStatus,
				UpstreamRequestID: s.upstreamRequestID,
				Err:               err,
			})
		}
		if decoded.Usage != nil {
			s.pendingUsage = oaiwire.NormalizeUsage(decoded.Usage)
		}
		return provider.StreamEvent{Event: event.Name, Data: append([]byte(nil), event.Data...)}, nil
	}
}

func (s *chatStream) fail(err error) error {
	s.pendingUsage = nil
	s.failed = true
	return err
}

func (s *chatStream) streamSequenceError(message string) error {
	return &provider.Error{
		Category:          provider.StreamInterrupted,
		StatusCode:        s.upstreamStatus,
		UpstreamRequestID: s.upstreamRequestID,
		Err:               errors.New(message),
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

// validateStreamChunk enforces the OpenAI terminal rule: a usage-bearing chunk
// must have an empty choices array (the dedicated final usage chunk), while a
// content chunk must not carry usage and must contain meaningful deltas.
func validateStreamChunk(decoded oaiwire.StreamChunk) error {
	if err := oaiwire.ValidateUsage(decoded.Usage); err != nil {
		return err
	}
	if strings.TrimSpace(decoded.ID) == "" || strings.TrimSpace(decoded.Object) == "" || strings.TrimSpace(decoded.Model) == "" {
		return errors.New("provider stream event missing required fields")
	}
	if decoded.Usage != nil {
		if decoded.Choices == nil || len(*decoded.Choices) != 0 {
			return errors.New("provider stream final usage event must have empty choices")
		}
		return nil
	}
	if decoded.Choices == nil || len(*decoded.Choices) == 0 {
		return errors.New("provider stream event missing choices")
	}
	for _, choice := range *decoded.Choices {
		if choice.Delta != nil && strings.TrimSpace(choice.Delta.Role) == "" && choice.Delta.Content == nil && choice.FinishReason == nil {
			return errors.New("provider stream event delta is empty")
		}
	}
	return nil
}
