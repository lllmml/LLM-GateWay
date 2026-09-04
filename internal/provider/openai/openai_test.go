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
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.sawEOF = true
	}
	return n, err
}

func (r *trackingReadCloser) Close() error {
	return nil
}
