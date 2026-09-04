package deepseek

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

type wireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream        bool            `json:"stream"`
	StreamOptions json.RawMessage `json:"stream_options"`
}

func decodeWireRequest(t *testing.T, raw []byte) (wireRequest, string) {
	t.Helper()
	var body wireRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	rawOptions := ""
	if len(body.StreamOptions) > 0 {
		rawOptions = string(body.StreamOptions)
	}
	return body, rawOptions
}

func TestCompleteChatTranslatesRequestAndExtractsUsage(t *testing.T) {
	var seenAuthorization string
	var seenContentType string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seenAuthorization = request.Header.Get("Authorization")
		seenContentType = request.Header.Get("Content-Type")
		if request.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Accept") != "application/json" {
			t.Fatalf("accept = %q", request.Header.Get("Accept"))
		}
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		body, options := decodeWireRequest(t, rawBody)
		if body.Model != "deepseek-chat" || body.Stream {
			t.Fatalf("request body = %+v", body)
		}
		if len(body.Messages) != 1 || body.Messages[0].Content != "hello" {
			t.Fatalf("messages = %+v", body.Messages)
		}
		if options != "" {
			t.Fatalf("stream_options = %q, want absent", options)
		}
		response.Header().Set("X-Request-ID", "req_provider_123")
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"id":"ddchat_1",
			"object":"chat.completion",
			"created":123,
			"model":"deepseek-chat",
			"system_fingerprint":"fp_test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":17,"completion_tokens":9,"total_tokens":26}
		}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).CompleteChat(context.Background(), provider.ChatRequest{
		Model:    "deepseek/deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}, provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
	if seenAuthorization != "Bearer sk-test" || seenContentType != "application/json" {
		t.Fatalf("headers: auth=%q content-type=%q", seenAuthorization, seenContentType)
	}
	if result.UpstreamStatus != http.StatusOK || result.UpstreamRequestID != "req_provider_123" {
		t.Fatalf("result = %+v", result)
	}
	if result.Response.ID != "ddchat_1" || result.Response.Object != "chat.completion" {
		t.Fatalf("response = %+v", result.Response)
	}
	if result.Response.Choices[0].Message.Content != "world" {
		t.Fatalf("response choices = %+v", result.Response.Choices)
	}
	if result.Usage == nil || result.Usage.PromptTokens != 17 || result.Usage.TotalTokens != 26 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

// DeepSeek V4 thinking mode is enabled by default, so provider messages carry
// reasoning_content. The gateway-normalized response has no reasoning field,
// so the final answer content is forwarded and the reasoning channel is
// intentionally not part of the normalized envelope.
func TestCompleteChatNormalizesReasoningResponseToContentOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"id":"ddchat_1",
			"object":"chat.completion",
			"created":123,
			"model":"deepseek-chat",
			"choices":[{"index":0,"message":{
				"role":"assistant",
				"content":"The final answer.",
				"reasoning_content":"Internal chain of thought."
			},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":30,"total_tokens":50,"prompt_cache_hit_tokens":10,"prompt_cache_miss_tokens":10,"completion_tokens_details":{"reasoning_tokens":15}}
		}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).CompleteChat(context.Background(), provider.ChatRequest{
		Model:    "deepseek/deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}, provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
	if result.Response.Choices[0].Message.Content != "The final answer." {
		t.Fatalf("content = %q", result.Response.Choices[0].Message.Content)
	}
	if result.Response.Choices[0].Message.Role != "assistant" {
		t.Fatalf("role = %q", result.Response.Choices[0].Message.Role)
	}
	if result.Usage == nil || result.Usage.PromptTokens != 20 || result.Usage.CompletionTokens != 30 || result.Usage.TotalTokens != 50 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestCompleteChatClassifiesProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Request-ID", "req_429")
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = response.Write([]byte(`{"error":{"message":"rate limit reached","type":"rate_limit_error","code":"rate_limit"}}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if providerErr.Category != provider.ProviderRateLimited || providerErr.Message != "rate limit reached" {
		t.Fatalf("provider error = %+v", providerErr)
	}
	if result.UpstreamStatus != http.StatusTooManyRequests || result.UpstreamRequestID != "req_429" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCompleteChatClassifiesInsufficientBalanceAsInvalidRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusPaymentRequired)
		_, _ = response.Write([]byte(`{"error":{"message":"insufficient balance","code":"insufficient_balance"}}`))
	}))
	defer server.Close()

	_, err := New(server.Client()).CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderInvalidReq || providerErr.Message != "insufficient balance" {
		t.Fatalf("provider error = %+v", err)
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
		provider.ChatRequest{Model: "deepseek/deepseek-chat"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	if strings.Contains(err.Error(), "secret-upstream-body") {
		t.Fatalf("raw provider body leaked through error: %v", err)
	}
}

// The happy stream: role chunk, content chunks, then usage rides on the final
// content chunk (single choice, empty content, finish_reason stop), then
// [DONE]. A keep-alive SSE comment is interleaved to prove it is tolerated.
func TestStreamChatTranslatesRequestAndExtractsUsage(t *testing.T) {
	var seenAccept string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seenAccept = request.Header.Get("Accept")
		if request.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		body, options := decodeWireRequest(t, rawBody)
		if body.Model != "deepseek-chat" || !body.Stream {
			t.Fatalf("request body = %+v", body)
		}
		if options != "" {
			t.Fatalf("stream_options = %q, want absent", options)
		}
		response.Header().Set("X-Request-ID", "req_stream_123")
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte(": keep-alive\n\n"))
		_, _ = response.Write([]byte("data: {\"id\":\"ddchat_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n"))
		_, _ = response.Write([]byte("data: {\"id\":\"ddchat_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}],\"usage\":null}\n\n"))
		_, _ = response.Write([]byte("data: {\"id\":\"ddchat_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":9,\"total_tokens\":26}}\n\n"))
		_, _ = response.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(context.Background(), provider.ChatRequest{
		Model:    "deepseek/deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
	}, provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	if seenAccept != "text/event-stream" {
		t.Fatalf("accept header = %q", seenAccept)
	}
	if result.UpstreamStatus != http.StatusOK || result.UpstreamRequestID != "req_stream_123" {
		t.Fatalf("result = %+v", result)
	}

	event, err := result.Stream.Next()
	if err != nil {
		t.Fatalf("first next: %v", err)
	}
	if event.Done || !strings.Contains(string(event.Data), `"role":"assistant"`) {
		t.Fatalf("first event = %+v", event)
	}
	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("content next: %v", err)
	}
	if event.Done || !strings.Contains(string(event.Data), `"content":"Hello"`) {
		t.Fatalf("content event = %+v", event)
	}
	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("usage next: %v", err)
	}
	if event.Done || !strings.Contains(string(event.Data), `"usage":{"prompt_tokens":17`) {
		t.Fatalf("usage event = %+v", event)
	}
	if usage := result.Stream.Usage(); usage != nil {
		t.Fatalf("usage before DONE = %+v, want nil", usage)
	}
	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("done next: %v", err)
	}
	if !event.Done || string(event.Data) != "[DONE]" {
		t.Fatalf("done event = %+v", event)
	}
	usage := result.Stream.Usage()
	if usage == nil || usage.TotalTokens != 26 {
		t.Fatalf("usage = %+v", usage)
	}
	if _, err := result.Stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("post done err = %v, want EOF", err)
	}
}

// A stream that never reaches [DONE] is never a success, even when the final
// usage chunk arrived, and must not expose usage.
func TestStreamChatDoesNotExposeUsageWhenFinalUsageIsInterrupted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"id\":\"ddchat_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":9,\"total_tokens\":26}}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	if _, err := result.Stream.Next(); err != nil {
		t.Fatalf("usage next: %v", err)
	}
	if usage := result.Stream.Usage(); usage != nil {
		t.Fatalf("usage before interruption = %+v, want nil", usage)
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

func TestStreamChatRejectsMalformedFinalUsageSequences(t *testing.T) {
	tests := []struct {
		name      string
		events    []string
		forbidden []string
	}{
		{
			name: "usage on chunk that still carries content",
			events: []string{
				`data: {"id":"secret_usage_content","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"late"}}],"usage":{"prompt_tokens":17,"completion_tokens":9,"total_tokens":26}}` + "\n\n",
			},
			forbidden: []string{"secret_usage_content"},
		},
		{
			name: "openai style usage chunk with empty choices",
			events: []string{
				`data: {"id":"secret_openai_style","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[],"usage":{"prompt_tokens":17,"completion_tokens":9,"total_tokens":26}}` + "\n\n",
			},
			forbidden: []string{"secret_openai_style"},
		},
		{
			name: "usage without finish reason",
			events: []string{
				`data: {"id":"secret_no_finish","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":""}}],"usage":{"prompt_tokens":17,"completion_tokens":9,"total_tokens":26}}` + "\n\n",
			},
			forbidden: []string{"secret_no_finish"},
		},
		{
			name: "usage chunk with multiple choices",
			events: []string{
				`data: {"id":"secret_two_choices","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"},{"index":1,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":9,"total_tokens":26}}` + "\n\n",
			},
			forbidden: []string{"secret_two_choices"},
		},
		{
			name: "content after final usage chunk",
			events: []string{
				`data: {"id":"chunk_usage","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":9,"total_tokens":26}}` + "\n\n",
				`data: {"id":"secret_after_usage","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"late"}}]}` + "\n\n",
			},
			forbidden: []string{"secret_after_usage"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				for _, event := range tt.events {
					_, _ = response.Write([]byte(event))
				}
				_, _ = response.Write([]byte("data: [DONE]\n\n"))
			}))
			defer server.Close()

			result, err := New(server.Client()).StreamChat(
				context.Background(),
				provider.ChatRequest{Model: "deepseek/deepseek-chat", Stream: true},
				provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
			)
			if err != nil {
				t.Fatalf("stream chat: %v", err)
			}
			defer result.Stream.Close()

			_, err = result.Stream.Next()
			if err == nil {
				_, err = result.Stream.Next()
			}
			providerErr, ok := provider.AsError(err)
			if !ok || providerErr.Category != provider.StreamInterrupted {
				t.Fatalf("error = %#v", err)
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("raw provider body leaked through error: %v", err)
				}
			}
			if usage := result.Stream.Usage(); usage != nil {
				t.Fatalf("malformed stream exposed usage: %+v", usage)
			}
		})
	}
}

func TestStreamChatClassifiesProviderErrorBeforeStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Request-ID", "req_429")
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = response.Write([]byte(`{"error":{"message":"rate limit reached"}}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("StreamChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderRateLimited || providerErr.Message != "rate limit reached" {
		t.Fatalf("provider error = %+v", providerErr)
	}
	if result.UpstreamStatus != http.StatusTooManyRequests || result.UpstreamRequestID != "req_429" {
		t.Fatalf("result = %+v", result)
	}
}

func TestStreamChatRejectsInvalidSuccessfulContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"not":"sse"}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
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

func TestStreamChatReportsEOFBeforeDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"id\":\"ddchat_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()
	if _, err := result.Stream.Next(); err != nil {
		t.Fatalf("first next: %v", err)
	}
	_, err = result.Stream.Next()
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.StreamInterrupted {
		t.Fatalf("error = %#v", err)
	}
}

func TestStreamChatRejectsOversizedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: "))
		_, _ = response.Write([]byte(strings.Repeat("x", maxStreamEventBytes+1)))
		_, _ = response.Write([]byte("\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
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

func TestChatEndpointValidatesOriginAndAppendsDeepSeekPath(t *testing.T) {
	for _, raw := range []string{"https://u:p@example.test", "https://example.test?x=1", "ftp://example.test"} {
		if _, err := chatEndpoint(raw); err == nil {
			t.Fatalf("chatEndpoint(%q) returned nil error", raw)
		}
	}
	endpoint, err := chatEndpoint("https://example.test/base/")
	if err != nil {
		t.Fatalf("chatEndpoint: %v", err)
	}
	if !strings.HasSuffix(endpoint, "/base/chat/completions") {
		t.Fatalf("endpoint = %q", endpoint)
	}
	// An override that already carries a /v1 prefix stays compatible with
	// OpenAI-SDK-style base URLs without the adapter duplicating the path.
	endpoint, err = chatEndpoint("https://example.test/v1")
	if err != nil {
		t.Fatalf("chatEndpoint: %v", err)
	}
	if !strings.HasSuffix(endpoint, "/v1/chat/completions") {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestStreamChatSkipsKeepAliveCommentsBetweenEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"id\":\"ddchat_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"))
		_, _ = response.Write([]byte(": keep-alive\n\n"))
		_, _ = response.Write([]byte(": keep-alive\n\n"))
		_, _ = response.Write([]byte("data: {\"id\":\"ddchat_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"))
		_, _ = response.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	if _, err := result.Stream.Next(); err != nil {
		t.Fatalf("first next: %v", err)
	}
	event, err := result.Stream.Next()
	if err != nil {
		t.Fatalf("usage next: %v", err)
	}
	if event.Done || !strings.Contains(string(event.Data), `"finish_reason":"stop"`) {
		t.Fatalf("usage event = %+v", event)
	}
	event, err = result.Stream.Next()
	if err != nil || !event.Done {
		t.Fatalf("done event = %+v, err=%v", event, err)
	}
	if usage := result.Stream.Usage(); usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
}
