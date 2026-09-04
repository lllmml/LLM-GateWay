package openai

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
	defaultBaseURL       = "https://api.openai.com"
	chatCompletionsPath  = "/v1/chat/completions"
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
	request.Header.Set("Authorization", "Bearer "+string(credential.APIKey))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return provider.Result{}, classifyTransportError(err)
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

	var decoded responseBody
	decoder := json.NewDecoder(bytes.NewReader(bodyBytes))
	if err := decoder.Decode(&decoded); err != nil {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, &provider.Error{
			Category:          provider.ProviderUnavailable,
			StatusCode:        response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
			Err:               fmt.Errorf("decode provider response: %w", err),
		}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, &provider.Error{
			Category:          provider.ProviderUnavailable,
			StatusCode:        response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
			Err:               fmt.Errorf("provider response contained multiple JSON values: %w", err),
		}
	}
	if err := validateResponse(decoded); err != nil {
		return provider.Result{UpstreamStatus: response.StatusCode, UpstreamRequestID: upstreamRequestID}, &provider.Error{
			Category:          provider.ProviderUnavailable,
			StatusCode:        response.StatusCode,
			UpstreamRequestID: upstreamRequestID,
			Err:               err,
		}
	}

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
	var usage *provider.Usage
	if decoded.Usage != nil {
		usage = usageFromResponse(decoded.Usage)
		result.Usage = usage
	}
	return provider.Result{
		Response:          result,
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
	request.Header.Set("Authorization", "Bearer "+string(credential.APIKey))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

	response, err := c.httpClient.Do(request)
	if err != nil {
		return provider.StreamResult{}, classifyTransportError(err)
	}

	upstreamRequestID := upstreamRequestID(response)
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
	body := requestBody{
		Model:    model,
		Messages: make([]message, 0, len(chat.Messages)),
		Stream:   stream,
	}
	if stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	for _, current := range chat.Messages {
		body.Messages = append(body.Messages, message{Role: current.Role, Content: current.Content})
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", &provider.Error{Category: provider.InvalidRequest, Err: err}
	}
	return encoded, endpoint, nil
}

func chatEndpoint(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid OpenAI base URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("invalid OpenAI base URL")
	}
	return parsed.String() + chatCompletionsPath, nil
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

func upstreamRequestID(response *http.Response) string {
	id := response.Header.Get("X-Request-ID")
	if id == "" {
		id = response.Header.Get("OpenAI-Request-ID")
	}
	return id
}

func isSSEContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "text/event-stream"
}

func classifyResponseError(response *http.Response, upstreamRequestID string) error {
	category := provider.ProviderUnavailable
	message := "provider request failed"
	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		category = provider.ProviderRateLimited
	case response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusGatewayTimeout:
		category = provider.ProviderTimeout
	case response.StatusCode >= 400 && response.StatusCode < 500:
		category = provider.ProviderInvalidReq
	case response.StatusCode >= 500:
		category = provider.ProviderUnavailable
	}
	var decoded errorBody
	bodyBytes, tooLarge := readBoundedErrorBody(response.Body)
	if tooLarge {
		message = "provider error body exceeded limit"
	} else if len(bodyBytes) > 0 {
		if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&decoded); err == nil && strings.TrimSpace(decoded.Error.Message) != "" {
			message = decoded.Error.Message
		}
	}
	return &provider.Error{
		Category:          category,
		StatusCode:        response.StatusCode,
		UpstreamRequestID: upstreamRequestID,
		Message:           message,
	}
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

func validateResponse(decoded responseBody) error {
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
	if decoded.Usage != nil {
		if decoded.Usage.PromptTokens < 0 || decoded.Usage.CompletionTokens < 0 || decoded.Usage.TotalTokens < 0 {
			return errors.New("provider response usage values must be nonnegative")
		}
		if decoded.Usage.TotalTokens < decoded.Usage.PromptTokens+decoded.Usage.CompletionTokens {
			return errors.New("provider response usage totals are inconsistent")
		}
	}
	return nil
}

func validateUsage(usage *responseUsage) error {
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

func usageFromResponse(usage *responseUsage) *provider.Usage {
	if usage == nil {
		return nil
	}
	return &provider.Usage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
}

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
		var decoded streamChunk
		decoder := json.NewDecoder(bytes.NewReader(event.Data))
		if err := decoder.Decode(&decoded); err != nil {
			return provider.StreamEvent{}, s.fail(&provider.Error{
				Category:          provider.StreamInterrupted,
				StatusCode:        s.upstreamStatus,
				UpstreamRequestID: s.upstreamRequestID,
				Err:               fmt.Errorf("decode provider stream event: %w", err),
			})
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return provider.StreamEvent{}, s.fail(&provider.Error{
				Category:          provider.StreamInterrupted,
				StatusCode:        s.upstreamStatus,
				UpstreamRequestID: s.upstreamRequestID,
				Err:               fmt.Errorf("provider stream event contained multiple JSON values: %w", err),
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
			s.pendingUsage = usageFromResponse(decoded.Usage)
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

func validateStreamChunk(decoded streamChunk) error {
	if err := validateUsage(decoded.Usage); err != nil {
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

type requestBody struct {
	Model         string         `json:"model"`
	Messages      []message      `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseBody struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []responseChoice `json:"choices"`
	Usage   *responseUsage   `json:"usage"`
}

type responseChoice struct {
	Index        int             `json:"index"`
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type responseMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type streamChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices *[]streamChoice `json:"choices"`
	Usage   *responseUsage  `json:"usage"`
}

type streamChoice struct {
	Index        int          `json:"index"`
	Delta        *streamDelta `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

type streamDelta struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
}

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}
