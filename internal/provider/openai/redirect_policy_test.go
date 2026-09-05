package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

// TestCompleteChatDoesNotFollowRedirects pins the redirect policy: provider
// chat POSTs must never be transparently replayed to a redirect target. A 3xx
// is returned to the adapter and classified with its real status, so a
// redirect-chain dial/DNS failure can never be misread as a safe pre-provider
// transport error.
func TestCompleteChatDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				posts.Add(1)
				response.Header().Set("Location", "http://127.0.0.1:1/nowhere") // would fail to dial if followed
				response.WriteHeader(status)
			}))
			defer server.Close()

			client := New(NewHTTPClient())
			result, err := client.CompleteChat(
				context.Background(),
				provider.ChatRequest{Model: "openai/gpt-test"},
				provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
			)
			if err == nil {
				t.Fatal("CompleteChat returned nil error for a redirect response")
			}
			providerErr, ok := provider.AsError(err)
			if !ok {
				t.Fatalf("error type = %T", err)
			}
			// The real 3xx status is classified - not a transport dial error
			// (status 0), which is what following the redirect would produce.
			if providerErr.StatusCode != status || result.UpstreamStatus != status {
				t.Fatalf("status = %d / upstream %d, want %d", providerErr.StatusCode, result.UpstreamStatus, status)
			}
			if posts.Load() != 1 {
				t.Fatalf("provider POSTs = %d, want exactly 1 (redirect must not be followed)", posts.Load())
			}
			if policy := NewHTTPClient().CheckRedirect; policy == nil || policy(nil, nil) != http.ErrUseLastResponse {
				t.Fatal("default adapter client does not refuse redirects")
			}
		})
	}
}
