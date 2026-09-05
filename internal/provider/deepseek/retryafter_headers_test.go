package deepseek

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
		{name: "429 with delta hint", header: "30", status: http.StatusTooManyRequests, present: true, want: 30 * time.Second},
		{name: "429 without hint", header: "", status: http.StatusTooManyRequests, present: false},
		{name: "500 without hint", header: "", status: http.StatusInternalServerError, present: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if test.header != "" {
					response.Header().Set("Retry-After", test.header)
				}
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte(`{"error":{"message":"provider error"}}`))
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
