package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

func TestCompleteChatTranslatesRequestAndExtractsUsage(t *testing.T) {
	var seenAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seenAuthorization = request.Header.Get("Authorization")
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		var body requestBody
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Model != "gpt-test" || body.Stream {
			t.Fatalf("request body = %+v", body)
		}
		if len(body.Messages) != 1 || body.Messages[0].Content != "hello" {
			t.Fatalf("messages = %+v", body.Messages)
		}
		response.Header().Set("X-Request-ID", "req_provider_123")
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"id":"chatcmpl_1",
			"object":"chat.completion",
			"created":123,
			"model":"gpt-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}
		}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).CompleteChat(context.Background(), provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}, provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
	if seenAuthorization != "Bearer sk-test" {
		t.Fatalf("authorization header = %q", seenAuthorization)
	}
	if result.UpstreamStatus != http.StatusOK || result.UpstreamRequestID != "req_provider_123" {
		t.Fatalf("result = %+v", result)
	}
	if result.Response.ID != "chatcmpl_1" || result.Usage == nil || result.Usage.TotalTokens != 10 {
		t.Fatalf("response = %+v", result.Response)
	}
}

func TestCompleteChatClassifiesProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("OpenAI-Request-ID", "req_429")
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = response.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit"}}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if providerErr.Category != provider.ProviderRateLimited || providerErr.Message != "slow down" {
		t.Fatalf("provider error = %+v", providerErr)
	}
	if result.UpstreamStatus != http.StatusTooManyRequests || result.UpstreamRequestID != "req_429" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCompleteChatRejectsMultipleJSONValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{"id":"x"}`))
		_, _ = response.Write([]byte(`{"id":"y"}`))
	}))
	defer server.Close()

	_, err := New(server.Client()).CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v", err)
	}
}

func TestCompleteChatRejectsMalformedResponseWithoutRawBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{"id":"secret-upstream-body"`))
	}))
	defer server.Close()

	_, err := New(server.Client()).CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v", err)
	}
	if strings.Contains(err.Error(), "secret-upstream-body") {
		t.Fatalf("raw provider body leaked through error: %v", err)
	}
}

func TestCompleteChatRejectsOversizedResponseWithoutRawBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{"id":"secret-upstream-body","padding":"`))
		_, _ = response.Write([]byte(strings.Repeat("x", maxResponseBodyBytes+1)))
		_, _ = response.Write([]byte(`"}`))
	}))
	defer server.Close()

	_, err := New(server.Client()).CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	if strings.Contains(err.Error(), "secret-upstream-body") {
		t.Fatalf("raw provider body leaked through error: %v", err)
	}
}

func TestCompleteChatRejectsNegativeUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{
			"id":"chatcmpl_1",
			"object":"chat.completion",
			"created":123,
			"model":"gpt-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":-1,"completion_tokens":3,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	_, err := New(server.Client()).CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v", err)
	}
}

func TestCompleteChatReusesConnectionAcrossSequentialRequests(t *testing.T) {
	var mu sync.Mutex
	connections := map[net.Conn]struct{}{}
	requests := 0
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		requests++
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"id":"chatcmpl_1",
			"object":"chat.completion",
			"created":123,
			"model":"gpt-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state != http.StateNew {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		connections[conn] = struct{}{}
	}
	server.Start()
	defer server.Close()

	transport := NewTransport()
	defer transport.CloseIdleConnections()
	client := New(&http.Client{Transport: transport})
	for range 2 {
		if _, err := client.CompleteChat(context.Background(), provider.ChatRequest{
			Model:    "openai/gpt-test",
			Messages: []provider.Message{{Role: "user", Content: "hello"}},
		}, provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL}); err != nil {
			t.Fatalf("complete chat: %v", err)
		}
	}

	mu.Lock()
	connectionCount := len(connections)
	mu.Unlock()
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if connectionCount != 1 {
		t.Fatalf("connections = %d, want 1", connectionCount)
	}
}

func TestStreamChatTranslatesRequestAndExtractsUsage(t *testing.T) {
	var seenAuthorization string
	var seenAccept string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seenAuthorization = request.Header.Get("Authorization")
		seenAccept = request.Header.Get("Accept")
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		var body requestBody
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Model != "gpt-test" || !body.Stream {
			t.Fatalf("request body = %+v", body)
		}
		if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
			t.Fatalf("stream options = %+v", body.StreamOptions)
		}
		response.Header().Set("X-Request-ID", "req_stream_123")
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}],\"usage\":null}\n\n"))
		_, _ = response.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-test\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n"))
		_, _ = response.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(context.Background(), provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
	}, provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()

	if seenAuthorization != "Bearer sk-test" {
		t.Fatalf("authorization header = %q", seenAuthorization)
	}
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
	if event.Done || !strings.Contains(string(event.Data), `"delta":{"role":"assistant"}`) {
		t.Fatalf("first event = %+v", event)
	}
	event, err = result.Stream.Next()
	if err != nil {
		t.Fatalf("usage next: %v", err)
	}
	if event.Done || !strings.Contains(string(event.Data), `"usage":{"prompt_tokens":7`) {
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
	if usage == nil || usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v", usage)
	}
	if _, err := result.Stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("post done err = %v, want EOF", err)
	}
}

func TestStreamChatRejectsMalformedFinalUsageSequences(t *testing.T) {
	tests := []struct {
		name   string
		events []string
	}{
		{
			name: "usage with choices",
			events: []string{
				`data: {"id":"secret_usage_choices","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"bad"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n",
			},
		},
		{
			name: "usage without choices",
			events: []string{
				`data: {"id":"secret_usage_without_choices","object":"chat.completion.chunk","created":123,"model":"gpt-test","usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n",
			},
		},
		{
			name: "content after usage",
			events: []string{
				`data: {"id":"chunk_usage","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n",
				`data: {"id":"secret_after_usage","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"late"}}],"usage":null}` + "\n\n",
			},
		},
		{
			name: "conflicting duplicate usage",
			events: []string{
				`data: {"id":"chunk_usage","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n",
				`data: {"id":"secret_second_usage","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}` + "\n\n",
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
				_, _ = response.Write([]byte("data: [DONE]\n\n"))
			}))
			defer server.Close()

			result, err := New(server.Client()).StreamChat(
				context.Background(),
				provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
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
			for _, forbidden := range []string{"secret_usage_choices", "secret_usage_without_choices", "secret_after_usage", "secret_second_usage"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("raw provider body leaked through error: %v", err)
				}
			}
			if usage := result.Stream.Usage(); usage != nil {
				t.Fatalf("malformed stream exposed usage: %+v", usage)
			}
			next, nextErr := result.Stream.Next()
			if !errors.Is(nextErr, io.EOF) || next.Done {
				t.Fatalf("post-failure next = %+v, %v; want no DONE and EOF", next, nextErr)
			}
			if usage := result.Stream.Usage(); usage != nil {
				t.Fatalf("post-failure stream exposed usage: %+v", usage)
			}
		})
	}
}

func TestStreamChatDoesNotExposeUsageWhenFinalUsageIsInterrupted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte(`data: {"id":"chunk_usage","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
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
	if _, err := result.Stream.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("post-interruption next err = %v, want EOF", err)
	}
}

func TestStreamChatClassifiesProviderErrorBeforeStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("OpenAI-Request-ID", "req_429")
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = response.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit"}}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("StreamChat returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderRateLimited || providerErr.Message != "slow down" {
		t.Fatalf("provider error = %+v", providerErr)
	}
	if result.UpstreamStatus != http.StatusTooManyRequests || result.UpstreamRequestID != "req_429" {
		t.Fatalf("result = %+v", result)
	}
}

func TestStreamChatRejectsInvalidSuccessfulContentType(t *testing.T) {
	body := &trackingReadCloser{reader: strings.NewReader(`{"not":"sse"}`)}
	client := New(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
			Request:    request,
		}, nil
	})})

	result, err := client.StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: "https://example.test"},
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
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestStreamChatAcceptsFinalChunkWithEmptyDelta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = response.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n"))
		_, _ = response.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()
	event, err := result.Stream.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if event.Done || !strings.Contains(string(event.Data), `"finish_reason":"stop"`) {
		t.Fatalf("event = %+v", event)
	}
}

func TestStreamChatRejectsMalformedEventWithoutRawBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"id\":\"secret-upstream-body\"\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()
	_, err = result.Stream.Next()
	if err == nil {
		t.Fatal("Next returned nil error")
	}
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.StreamInterrupted {
		t.Fatalf("error = %#v", err)
	}
	if strings.Contains(err.Error(), "secret-upstream-body") {
		t.Fatalf("raw provider body leaked through error: %v", err)
	}
}

func TestStreamChatReportsEOFBeforeDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}],\"usage\":null}\n\n"))
	}))
	defer server.Close()

	result, err := New(server.Client()).StreamChat(
		context.Background(),
		provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
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
		provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
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

func TestStreamChatCancellationInterruptsRead(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		if flusher, ok := response.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result, err := New(server.Client()).StreamChat(
		ctx,
		provider.ChatRequest{Model: "openai/gpt-test", Stream: true},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	defer result.Stream.Close()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start stream")
	}
	cancel()
	_, err = result.Stream.Next()
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.StreamInterrupted {
		t.Fatalf("error = %#v", err)
	}
}

func TestProviderErrorBodyIsDrainedWhenBounded(t *testing.T) {
	body := &trackingReadCloser{reader: strings.NewReader(`{"error":{"message":"slow down"}}`)}
	err := classifyResponseError(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       body,
	}, "req_429")
	providerErr, ok := provider.AsError(err)
	if !ok || providerErr.Category != provider.ProviderRateLimited || providerErr.Message != "slow down" {
		t.Fatalf("error = %#v", err)
	}
	if !body.sawEOF {
		t.Fatal("error body was not drained to EOF")
	}
}

func TestChatEndpointRejectsCredentialsAndQuery(t *testing.T) {
	for _, raw := range []string{"https://u:p@example.test", "https://example.test?x=1", "ftp://example.test"} {
		if _, err := chatEndpoint(raw); err == nil {
			t.Fatalf("chatEndpoint(%q) returned nil error", raw)
		}
	}
	endpoint, err := chatEndpoint("https://example.test/base/")
	if err != nil {
		t.Fatalf("chatEndpoint: %v", err)
	}
	if !strings.HasSuffix(endpoint, "/base/v1/chat/completions") {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

type trackingReadCloser struct {
	reader *strings.Reader
	sawEOF bool
	closed bool
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.sawEOF = true
	}
	return n, err
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
