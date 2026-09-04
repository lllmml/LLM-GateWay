package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

type wireRequestBody struct {
	Model     string `json:"model"`
	MaxTokens int64  `json:"max_tokens"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	System string `json:"system"`
	Stream bool   `json:"stream"`
}

func decodeRequestBody(t *testing.T, raw []byte) wireRequestBody {
	t.Helper()
	var body wireRequestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return body
}

func TestCompleteChatTranslatesRequestAndNormalizesResponse(t *testing.T) {
	var seenHeaders http.Header
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seenHeaders = request.Header.Clone()
		seenPath = request.URL.Path
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		body := decodeRequestBody(t, rawBody)
		if body.Model != "claude-sonnet-x" || body.Stream {
			t.Fatalf("request body = %+v", body)
		}
		// max_tokens is required by the Messages API; the gateway default
		// applies when the client omits it.
		if body.MaxTokens != defaultMaxTokens {
			t.Fatalf("max_tokens = %d, want %d", body.MaxTokens, defaultMaxTokens)
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != "hello" {
			t.Fatalf("messages = %+v", body.Messages)
		}
		if body.System != "You are a helpful assistant." {
			t.Fatalf("system = %q", body.System)
		}
		response.Header().Set("request-id", "req_018EeWyXxfu5pfWkrYcMdjWG")
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"id":"msg_01ABC",
			"type":"message",
			"role":"assistant",
			"model":"claude-sonnet-x",
			"content":[{"type":"text","text":"Hello"},{"type":"text","text":" world"}],
			"stop_reason":"end_turn",
			"stop_sequence":null,
			"usage":{"input_tokens":17,"output_tokens":9}
		}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).CompleteChat(context.Background(), provider.ChatRequest{
		Model:    "anthropic/claude-sonnet-x",
		Messages: []provider.Message{{Role: "system", Content: "You are a helpful assistant."}, {Role: "user", Content: "hello"}},
	}, provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
	if seenPath != "/v1/messages" {
		t.Fatalf("path = %q", seenPath)
	}
	if seenHeaders.Get("x-api-key") != "sk-ant-test" {
		t.Fatalf("x-api-key = %q", seenHeaders.Get("x-api-key"))
	}
	if seenHeaders.Get("anthropic-version") != apiVersion {
		t.Fatalf("anthropic-version = %q", seenHeaders.Get("anthropic-version"))
	}
	if seenHeaders.Get("Authorization") != "" {
		t.Fatalf("Authorization must not be set for Anthropic, got %q", seenHeaders.Get("Authorization"))
	}
	if seenHeaders.Get("Content-Type") != "application/json" || seenHeaders.Get("Accept") != "application/json" {
		t.Fatalf("content headers: content-type=%q accept=%q", seenHeaders.Get("Content-Type"), seenHeaders.Get("Accept"))
	}
	if result.UpstreamStatus != http.StatusOK || result.UpstreamRequestID != "req_018EeWyXxfu5pfWkrYcMdjWG" {
		t.Fatalf("result = %+v", result)
	}
	if result.Response.ID != "msg_01ABC" || result.Response.Object != "chat.completion" || result.Response.Model != "claude-sonnet-x" {
		t.Fatalf("response envelope = %+v", result.Response)
	}
	if len(result.Response.Choices) != 1 || result.Response.Choices[0].Message.Content != "Hello world" {
		t.Fatalf("response choices = %+v", result.Response.Choices)
	}
	if result.Response.Choices[0].Message.Role != "assistant" || result.Response.Choices[0].FinishReason != "stop" {
		t.Fatalf("response choice = %+v", result.Response.Choices[0])
	}
	if result.Usage == nil || result.Usage.PromptTokens != 17 || result.Usage.CompletionTokens != 9 || result.Usage.TotalTokens != 26 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestCompleteChatForwardsExplicitMaxTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		body := decodeRequestBody(t, rawBody)
		if body.MaxTokens != 128 {
			t.Fatalf("max_tokens = %d, want 128", body.MaxTokens)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet-x",
			"content":[{"type":"text","text":"ok"}],
			"stop_reason":"end_turn","stop_sequence":null,
			"usage":{"input_tokens":2,"output_tokens":3}
		}`))
	}))
	defer server.Close()

	explicit := int64(128)
	_, err := New(server.Client()).CompleteChat(context.Background(), provider.ChatRequest{
		Model:     "anthropic/claude-sonnet-x",
		Messages:  []provider.Message{{Role: "user", Content: "hello"}},
		MaxTokens: &explicit,
	}, provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
}

// Leading system messages (before the first user/assistant turn) are the only
// system context Anthropic can express in its top-level system field. One or
// more consecutive leading system messages are joined with "\n\n".
func TestCompleteChatTranslatesLeadingSystemMessages(t *testing.T) {
	for _, tt := range []struct {
		name       string
		messages   []provider.Message
		wantSystem string
		wantRoles  []string
	}{
		{
			name:       "single leading system message",
			messages:   []provider.Message{{Role: "system", Content: "rule"}, {Role: "user", Content: "hi"}},
			wantSystem: "rule",
			wantRoles:  []string{"user"},
		},
		{
			name: "multiple consecutive leading system messages",
			messages: []provider.Message{
				{Role: "system", Content: "first rule"},
				{Role: "system", Content: "second rule"},
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
			wantSystem: "first rule\n\nsecond rule",
			wantRoles:  []string{"user", "assistant"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				rawBody, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				body := decodeRequestBody(t, rawBody)
				if body.System != tt.wantSystem {
					t.Fatalf("system = %q, want %q", body.System, tt.wantSystem)
				}
				roles := make([]string, 0, len(body.Messages))
				for _, current := range body.Messages {
					roles = append(roles, current.Role)
				}
				if strings.Join(roles, ",") != strings.Join(tt.wantRoles, ",") {
					t.Fatalf("messages roles = %v, want %v", roles, tt.wantRoles)
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{
					"id":"msg_03","type":"message","role":"assistant","model":"claude-sonnet-x",
					"content":[{"type":"text","text":"ok"}],
					"stop_reason":"end_turn","stop_sequence":null,
					"usage":{"input_tokens":2,"output_tokens":3}
				}`))
			}))
			defer server.Close()

			_, err := New(server.Client()).CompleteChat(context.Background(), provider.ChatRequest{
				Model:    "claude-sonnet-x",
				Messages: tt.messages,
			}, provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL})
			if err != nil {
				t.Fatalf("complete chat: %v", err)
			}
		})
	}
}

// A system message after the first user/assistant turn cannot be expressed by
// the Messages API without silently moving it to the front of the prompt. The
// adapter must reject it before any upstream HTTP work instead of changing
// conversation semantics.
func TestCompleteChatRejectsMidConversationSystemMessage(t *testing.T) {
	for _, tt := range []struct {
		name     string
		messages []provider.Message
	}{
		{
			name:     "system after user",
			messages: []provider.Message{{Role: "user", Content: "hi"}, {Role: "system", Content: "late rule"}},
		},
		{
			name:     "system after assistant",
			messages: []provider.Message{{Role: "assistant", Content: "hello"}, {Role: "system", Content: "late rule"}},
		},
		{
			name:     "system between user and assistant turns",
			messages: []provider.Message{{Role: "system", Content: "lead rule"}, {Role: "user", Content: "hi"}, {Role: "system", Content: "late rule"}},
		},
		{
			name: "system after several turns",
			messages: []provider.Message{
				{Role: "system", Content: "a"},
				{Role: "user", Content: "u"},
				{Role: "assistant", Content: "a"},
				{Role: "user", Content: "u2"},
				{Role: "system", Content: "z"},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstreamCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				upstreamCalls++
				t.Fatalf("upstream must not be called for a rejected request")
			}))
			defer server.Close()

			_, err := New(server.Client()).CompleteChat(context.Background(), provider.ChatRequest{
				Model:    "anthropic/claude-sonnet-x",
				Messages: tt.messages,
			}, provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL})
			if err == nil {
				t.Fatal("CompleteChat returned nil error")
			}
			providerErr, ok := provider.AsError(err)
			if !ok {
				t.Fatalf("error type = %T", err)
			}
			if providerErr.Category != provider.UnsupportedFeature {
				t.Fatalf("category = %q, want %q (error=%+v)", providerErr.Category, provider.UnsupportedFeature, providerErr)
			}
			if providerErr.Message != "system messages must appear before the first user or assistant message" {
				t.Fatalf("message = %q, want stable rejection message", providerErr.Message)
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
			}
		})
	}
}

// buildRequest is shared by the stream path, so a mid-conversation system
// message must also be rejected before the upstream stream request is sent.
func TestStreamChatRejectsMidConversationSystemMessageBeforeUpstream(t *testing.T) {
	upstreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		upstreamCalls++
		t.Fatalf("upstream must not be called for a rejected request")
	}))
	defer server.Close()

	_, err := New(server.Client()).StreamChat(context.Background(), provider.ChatRequest{
		Model:    "anthropic/claude-sonnet-x",
		Messages: []provider.Message{{Role: "user", Content: "hi"}, {Role: "system", Content: "late rule"}},
		Stream:   true,
	}, provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL})
	if err == nil {
		t.Fatal("StreamChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if providerErr.Category != provider.UnsupportedFeature {
		t.Fatalf("category = %q, want %q (error=%+v)", providerErr.Category, provider.UnsupportedFeature, providerErr)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestCompleteChatMapsStopReasons(t *testing.T) {
	for _, tt := range []struct {
		name       string
		stopReason string
		want       string
	}{
		{"end_turn to stop", "end_turn", "stop"},
		{"max_tokens to length", "max_tokens", "length"},
		{"model_context_window_exceeded to length", "model_context_window_exceeded", "length"},
		{"stop_sequence to stop", "stop_sequence", "stop"},
		{"tool_use to tool_calls", "tool_use", "tool_calls"},
		{"refusal to content_filter", "refusal", "content_filter"},
		{"pause_turn is outside the supported subset and maps to empty", "pause_turn", ""},
		{"genuinely unknown reason maps to empty", "future_stop_reason", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{
					"id":"msg_04","type":"message","role":"assistant","model":"claude-sonnet-x",
					"content":[{"type":"text","text":"done"}],
					"stop_reason":"` + tt.stopReason + `","stop_sequence":null,
					"usage":{"input_tokens":2,"output_tokens":3}
				}`))
			}))
			defer server.Close()

			result, err := New(server.Client()).CompleteChat(context.Background(), provider.ChatRequest{
				Model:    "claude-sonnet-x",
				Messages: []provider.Message{{Role: "user", Content: "hi"}},
			}, provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL})
			if err != nil {
				t.Fatalf("complete chat: %v", err)
			}
			if got := result.Response.Choices[0].FinishReason; got != tt.want {
				t.Fatalf("finish reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompleteChatClassifiesProviderError(t *testing.T) {
	for _, tt := range []struct {
		name         string
		status       int
		wantCategory provider.ErrorCategory
		body         string
	}{
		{
			name:         "400 invalid request",
			status:       http.StatusBadRequest,
			wantCategory: provider.ProviderInvalidReq,
			body:         `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens exceeds model limit"}}`,
		},
		{
			name:         "404 model not found",
			status:       http.StatusNotFound,
			wantCategory: provider.ProviderInvalidReq,
			body:         `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`,
		},
		{
			name:         "413 request too large",
			status:       http.StatusRequestEntityTooLarge,
			wantCategory: provider.ProviderInvalidReq,
			body:         `{"type":"error","error":{"type":"request_too_large","message":"request too large"}}`,
		},
		{
			name:         "429 rate limit",
			status:       http.StatusTooManyRequests,
			wantCategory: provider.ProviderRateLimited,
			body:         `{"type":"error","error":{"type":"rate_limit_error","message":"rate limit reached"}}`,
		},
		{
			name:         "504 timeout",
			status:       http.StatusGatewayTimeout,
			wantCategory: provider.ProviderTimeout,
			body:         `{"type":"error","error":{"type":"timeout_error","message":"request timed out"}}`,
		},
		{
			name:         "500 api error",
			status:       http.StatusInternalServerError,
			wantCategory: provider.ProviderUnavailable,
			body:         `{"type":"error","error":{"type":"api_error","message":"internal error"}}`,
		},
		{
			name:         "529 overloaded",
			status:       http.StatusServiceUnavailable,
			wantCategory: provider.ProviderUnavailable,
			body:         `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("request-id", "req_"+tt.name)
				response.WriteHeader(tt.status)
				_, _ = response.Write([]byte(tt.body))
			}))
			defer server.Close()

			result, err := New(server.Client()).CompleteChat(
				context.Background(),
				provider.ChatRequest{Model: "claude-sonnet-x"},
				provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
			)
			if err == nil {
				t.Fatal("CompleteChat returned nil error")
			}
			providerErr, ok := provider.AsError(err)
			if !ok {
				t.Fatalf("error type = %T", err)
			}
			if providerErr.Category != tt.wantCategory {
				t.Fatalf("category = %q, want %q (error=%+v)", providerErr.Category, tt.wantCategory, providerErr)
			}
			if providerErr.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", providerErr.StatusCode, tt.status)
			}
			if result.UpstreamStatus != tt.status || result.UpstreamRequestID != "req_"+tt.name {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

// Anthropic 401 (bad/expired key), 402 (billing), and 403 (key lacks access to
// the resource) are server-side conditions of the configured provider access,
// not client request defects. They must stay provider_unavailable and must
// never surface as provider_invalid_request (client HTTP 400).
func TestCompleteChatClassifiesServerSideStatusesAsProviderUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"401 authentication", http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key","extra":"secret-raw-provider-body"}}`},
		{"402 billing", http.StatusPaymentRequired, `{"type":"error","error":{"type":"billing_error","message":"billing issue","extra":"secret-raw-provider-body"}}`},
		{"403 permission", http.StatusForbidden, `{"type":"error","error":{"type":"permission_error","message":"no access","extra":"secret-raw-provider-body"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("request-id", "req_"+tt.name)
				response.WriteHeader(tt.status)
				_, _ = response.Write([]byte(tt.body))
			}))
			defer server.Close()

			result, err := New(server.Client()).CompleteChat(
				context.Background(),
				provider.ChatRequest{Model: "claude-sonnet-x"},
				provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
			)
			if err == nil {
				t.Fatal("CompleteChat returned nil error")
			}
			providerErr, ok := provider.AsError(err)
			if !ok {
				t.Fatalf("error type = %T", err)
			}
			if providerErr.Category != provider.ProviderUnavailable {
				t.Fatalf("category = %q, want %q; error=%+v", providerErr.Category, provider.ProviderUnavailable, providerErr)
			}
			if result.UpstreamStatus != tt.status || result.UpstreamRequestID != "req_"+tt.name {
				t.Fatalf("result = %+v", result)
			}
			if strings.Contains(err.Error(), "secret-raw-provider-body") {
				t.Fatalf("raw provider body leaked through error: %v", err)
			}
		})
	}
}

func TestCompleteChatRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{"id":"secret-upstream-body"`))
	}))
	defer server.Close()

	_, err := New(server.Client()).CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x"},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	if strings.Contains(err.Error(), "secret-upstream-body") {
		t.Fatalf("raw provider body leaked through error: %v", err)
	}
}

// The happy stream: message_start (usage.input_tokens), text content deltas,
// message_delta (finish reason + cumulative usage.output_tokens), message_stop.
// Ping and unknown named events are interleaved to prove they are tolerated,
// and content is re-encoded into OpenAI-compatible chunk envelopes.
func TestStreamChatTranslatesNamedEventsIntoChunksAndExtractsUsage(t *testing.T) {
	var seenAccept string
	var seenPath string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seenAccept = request.Header.Get("Accept")
		seenPath = request.URL.Path
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		body := decodeRequestBody(t, rawBody)
		if body.Model != "claude-sonnet-x" || !body.Stream {
			t.Fatalf("request body = %+v", body)
		}
		response.Header().Set("request-id", "req_stream_123")
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01ABC\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-x\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":17,\"output_tokens\":1}}}\n\n"))
		_, _ = response.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
		_, _ = response.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = response.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n"))
		_, _ = response.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n"))
		_, _ = response.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		_, _ = response.Write([]byte("event: future_event_type\ndata: {\"type\":\"future_event_type\",\"unknown\":true}\n\n"))
		_, _ = response.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":9}}\n\n"))
		_, _ = response.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(context.Background(), provider.ChatRequest{
		Model:    "anthropic/claude-sonnet-x",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
	}, provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	if seenPath != "/v1/messages" || seenAccept != "text/event-stream" {
		t.Fatalf("path=%q accept=%q", seenPath, seenAccept)
	}
	if result.UpstreamStatus != http.StatusOK || result.UpstreamRequestID != "req_stream_123" {
		t.Fatalf("result = %+v", result)
	}

	event, err := result.Stream.Next()
	if err != nil {
		t.Fatalf("content next: %v", err)
	}
	if event.Done || event.Event != "" {
		t.Fatalf("content event = %+v", event)
	}
	if !strings.Contains(string(event.Data), `"id":"msg_01ABC"`) ||
		!strings.Contains(string(event.Data), `"object":"chat.completion.chunk"`) ||
		!strings.Contains(string(event.Data), `"role":"assistant","content":"Hello"`) {
		t.Fatalf("content event data = %s", event.Data)
	}

	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("second content next: %v", err)
	}
	if !strings.Contains(string(event.Data), `"content":" world"`) {
		t.Fatalf("second content event data = %s", event.Data)
	}

	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("finish next: %v", err)
	}
	if event.Done || !strings.Contains(string(event.Data), `"finish_reason":"stop"`) {
		t.Fatalf("finish event = %+v", event)
	}
	if usage := result.Stream.Usage(); usage != nil {
		t.Fatalf("usage before message_stop = %+v, want nil", usage)
	}

	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("done next: %v", err)
	}
	if !event.Done || string(event.Data) != "[DONE]" {
		t.Fatalf("done event = %+v", event)
	}
	usage := result.Stream.Usage()
	if usage == nil || usage.PromptTokens != 17 || usage.CompletionTokens != 9 || usage.TotalTokens != 26 {
		t.Fatalf("usage = %+v", usage)
	}
	if _, err := result.Stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("post done err = %v, want EOF", err)
	}
}

// Output usage is cumulative across message_delta events, so the final delta
// wins. The text channel is reconstructed from all text deltas across any
// number of content blocks.
func TestStreamChatAccumulatesCumulativeOutputUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":10}}}\n\n"))
		_, _ = response.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"output_tokens\":5}}\n\n"))
		_, _ = response.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":8}}\n\n"))
		_, _ = response.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	event, err := result.Stream.Next()
	if err != nil {
		t.Fatalf("finish next: %v", err)
	}
	if !strings.Contains(string(event.Data), `"finish_reason":"length"`) {
		t.Fatalf("finish event data = %s", event.Data)
	}
	event, err = result.Stream.Next()
	if err != nil || !event.Done {
		t.Fatalf("done event = %+v, err=%v", event, err)
	}
	usage := result.Stream.Usage()
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 8 || usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", usage)
	}
}

// refusal is a documented, normal successful stop reason; the stream must emit
// an explicit content_filter finish chunk before [DONE] so clients observe a
// terminal state instead of an open-ended stop.
func TestStreamChatEmitsRefusalFinishChunkBeforeDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_r\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":5}}}\n\n"))
		_, _ = response.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"I cannot help with that.\"}}\n\n"))
		_, _ = response.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}\n\n"))
		_, _ = response.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	event, err := result.Stream.Next()
	if err != nil {
		t.Fatalf("content next: %v", err)
	}
	if !strings.Contains(string(event.Data), `"content":"I cannot help with that."`) {
		t.Fatalf("content event data = %s", event.Data)
	}
	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("finish next: %v", err)
	}
	if event.Done || !strings.Contains(string(event.Data), `"finish_reason":"content_filter"`) {
		t.Fatalf("refusal finish event = %+v (data=%s)", event, event.Data)
	}
	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("done next: %v", err)
	}
	if !event.Done || string(event.Data) != "[DONE]" {
		t.Fatalf("done event = %+v", event)
	}
	usage := result.Stream.Usage()
	if usage == nil || usage.PromptTokens != 5 || usage.CompletionTokens != 7 || usage.TotalTokens != 12 {
		t.Fatalf("usage = %+v", usage)
	}
}

// A stream that never reaches message_stop is never a success, even when
// content and the final message_delta arrived, and must not expose usage.
func TestStreamChatDoesNotExposeUsageWhenInterruptedBeforeMessageStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":10}}}\n\n"))
		_, _ = response.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	if _, err := result.Stream.Next(); err != nil {
		t.Fatalf("content next: %v", err)
	}
	_, err = result.Stream.Next()
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.StreamInterrupted {
		t.Fatalf("error = %#v", err)
	}
	if usage := result.Stream.Usage(); usage != nil {
		t.Fatalf("usage after interruption = %+v, want nil", usage)
	}
}

func TestStreamChatSkipsNonTextDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":1}}}\n\n"))
		// A thinking delta must never leak into the OpenAI text channel.
		_, _ = response.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"secret chain of thought\"}}\n\n"))
		_, _ = response.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n"))
		_, _ = response.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"))
		_, _ = response.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	event, err := result.Stream.Next()
	if err != nil {
		t.Fatalf("content next: %v", err)
	}
	if strings.Contains(string(event.Data), "secret chain of thought") {
		t.Fatalf("non-text content leaked into the text channel: %s", event.Data)
	}
	if !strings.Contains(string(event.Data), `"content":"answer"`) {
		t.Fatalf("content event data = %s", event.Data)
	}
}

// Anthropic's versioning policy permits new content block delta types; unknown
// named events are skipped gracefully, but a bare data frame (no event name)
// is outside the contract and fails loudly.
func TestStreamChatFailsOnBareDataFrameAndSkipsUnknownEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"type\":\"mystery\"}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	_, err = result.Stream.Next()
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.StreamInterrupted {
		t.Fatalf("error = %#v", err)
	}
}

func TestStreamChatFailsOnSequenceViolations(t *testing.T) {
	tests := []struct {
		name   string
		events []string
	}{
		{
			name: "content before message_start",
			events: []string{
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"early\"}}\n\n",
			},
		},
		{
			name: "duplicate message_start",
			events: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_b\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":1}}}\n\n",
			},
		},
		{
			name: "message_delta before message_start",
			events: []string{
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
			},
		},
		{
			name: "message_stop before message_start",
			events: []string{
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
		},
		{
			name: "message_stop without a message_delta",
			events: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
		},
		{
			name: "negative input_tokens",
			events: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":-1}}}\n\n",
			},
		},
		{
			name: "negative output_tokens",
			events: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":-1}}\n\n",
			},
		},
		{
			name: "cumulative output_tokens decreased",
			events: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"output_tokens\":8}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n",
			},
		},
		{
			name: "content after a message_delta",
			events: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":1}}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"late\"}}\n\n",
			},
		},
		{
			name: "malformed message_start payload",
			events: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"\",\"model\":\"\"}}\n\n",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				for _, event := range tt.events {
					_, _ = response.Write([]byte(event))
				}
			}))
			defer server.Close()

			result, err := New(server.Client()).StreamChat(
				context.Background(),
				provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
				provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
			)
			if err != nil {
				t.Fatalf("stream chat: %v", err)
			}
			defer result.Stream.Close()

			for {
				event, nextErr := result.Stream.Next()
				if nextErr != nil {
					err = nextErr
					break
				}
				if event.Done {
					t.Fatalf("invalid stream synthesized [DONE]")
				}
			}
			providerErr, ok := provider.AsError(err)
			if !ok || providerErr.Category != provider.StreamInterrupted {
				t.Fatalf("error = %#v", err)
			}
			if usage := result.Stream.Usage(); usage != nil {
				t.Fatalf("malformed stream exposed usage: %+v", usage)
			}
		})
	}
}

// An Anthropic error event (event: error) is the streamed analogue of a
// non-2xx status; its error.type drives the classification.
func TestStreamChatClassifiesErrorEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"model\":\"claude-sonnet-x\",\"usage\":{\"input_tokens\":1}}}\n\n"))
		_, _ = response.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	// message_start produces no downstream event, so the first Next drains it
	// and surfaces the error event that follows.
	_, err = result.Stream.Next()
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v", err)
	}
	if usage := result.Stream.Usage(); usage != nil {
		t.Fatalf("usage after error event = %+v, want nil", usage)
	}
}

func TestStreamChatClassifiesProviderErrorBeforeStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("request-id", "req_529")
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = response.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("StreamChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderUnavailable || providerErr.Message != "Overloaded" {
		t.Fatalf("provider error = %+v", providerErr)
	}
	if result.UpstreamStatus != http.StatusServiceUnavailable || result.UpstreamRequestID != "req_529" {
		t.Fatalf("result = %+v", result)
	}
}

func TestStreamChatRejectsInvalidSuccessfulContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"not sse"}}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("StreamChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v", err)
	}
	if result.UpstreamStatus != http.StatusOK {
		t.Fatalf("result = %+v", result)
	}
}

func TestStreamChatRejectsOversizedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("event: message_start\ndata: "))
		_, _ = response.Write([]byte(strings.Repeat("x", maxStreamEventBytes+1)))
		_, _ = response.Write([]byte("\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "claude-sonnet-x", Stream: true},
		provider.Credential{APIKey: []byte("sk-ant-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()
	_, err = result.Stream.Next()
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v", err)
	}
}

func TestMessagesEndpointValidatesOriginAndAppendsAnthropicPath(t *testing.T) {
	for _, raw := range []string{"https://u:p@example.test", "https://example.test?x=1", "ftp://example.test"} {
		if _, err := messagesEndpoint(raw); err == nil {
			t.Fatalf("messagesEndpoint(%q) returned nil error", raw)
		}
	}
	endpoint, err := messagesEndpoint("https://example.test/base/")
	if err != nil {
		t.Fatalf("messagesEndpoint: %v", err)
	}
	if !strings.HasSuffix(endpoint, "/base/v1/messages") {
		t.Fatalf("endpoint = %q", endpoint)
	}
}
