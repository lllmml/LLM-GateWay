package deepseek

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

func TestCompleteChatDoesNotFollowRedirects(t *testing.T) {
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		posts.Add(1)
		response.Header().Set("Location", "http://127.0.0.1:1/nowhere") // would fail to dial if followed
		response.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := New(NewHTTPClient())
	result, err := client.CompleteChat(
		context.Background(),
		provider.ChatRequest{Model: "deepseek/deepseek-chat"},
		provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
	)
	if err == nil {
		t.Fatal("CompleteChat returned nil error for a redirect response")
	}
	providerErr, ok := provider.AsError(err)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if providerErr.StatusCode != http.StatusTemporaryRedirect || result.UpstreamStatus != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d / upstream %d, want 307", providerErr.StatusCode, result.UpstreamStatus)
	}
	if posts.Load() != 1 {
		t.Fatalf("provider POSTs = %d, want exactly 1 (redirect must not be followed)", posts.Load())
	}
	if policy := NewHTTPClient().CheckRedirect; policy == nil || policy(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("default adapter client does not refuse redirects")
	}
}
