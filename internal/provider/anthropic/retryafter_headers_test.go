package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

func TestCompleteChatSurfacesRetryAfter(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		status  int
		present bool
		want    time.Duration
	}{
		{name: "429 with delta hint", header: "60", status: http.StatusTooManyRequests, present: true, want: 60 * time.Second},
		{name: "529 without hint", header: "", status: http.StatusInternalServerError, present: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if test.header != "" {
					response.Header().Set("Retry-After", test.header)
				}
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"provider error"}}`))
			}))
			defer server.Close()

			_, err := New(server.Client()).CompleteChat(
				context.Background(),
				provider.ChatRequest{Model: "anthropic/claude-sonnet-x", MaxTokens: int64Ptr(64)},
				provider.Credential{APIKey: []byte("sk-test"), BaseURLOverride: server.URL},
			)
			if err == nil {
				t.Fatal("CompleteChat returned nil error")
			}
			providerErr, ok := provider.AsError(err)
			if !ok {
				t.Fatalf("error type = %T", err)
			}
			if test.present {
				if providerErr.RetryAfter == nil {
					t.Fatal("RetryAfter = nil, want present")
				}
				if *providerErr.RetryAfter != test.want {
					t.Fatalf("RetryAfter = %v, want %v", *providerErr.RetryAfter, test.want)
				}
			} else if providerErr.RetryAfter != nil {
				t.Fatalf("RetryAfter = %v, want nil", *providerErr.RetryAfter)
			}
		})
	}
}

func int64Ptr(value int64) *int64 {
	return &value
}
