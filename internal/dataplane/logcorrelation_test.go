package dataplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
	"github.com/lllmml/production-go-llm-gateway/internal/telemetry"
)

// TestRequestPathLogCorrelation verifies the slog enrichment end to end: a
// request-path ErrorContext log (finalization persistence failure) carries the
// active root span's trace_id/span_id plus the sanitized client_request_id,
// and the gateway error is returned unchanged.
func TestRequestPathLogCorrelation(t *testing.T) {
	var output bytes.Buffer
	logger := telemetry.NewLogger(&output, slog.LevelInfo)

	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	store, rawKey := newAuthorizedStore(t)
	store.finalizeErr = errors.New("postgres unavailable")
	service, err := NewService(Options{
		Store:                     store,
		VirtualKeyPepper:          bytes.Repeat([]byte{9}, 32),
		CredentialCipher:          cipher,
		UpstreamRequestTimeout:    time.Second,
		UpstreamStreamMaxDuration: time.Second,
		ProviderRegistry:          newTestProviderRegistry(t, map[provider.Name]provider.Client{provider.OpenAI: &fakeProviderClient{}}),
		Logger:                    logger,
		Tracer:                    tracerProvider.Tracer("gateway"),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	handler := NewHandler(service)
	response, request := makeHTTPChatRequest(t, rawKey, func(request *http.Request) {
		request.Header.Set("X-Request-ID", " client-9 ")
	})
	handler.chatCompletions(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (usage_persistence_failed); body=%s", response.Code, response.Body.String())
	}

	var rootTraceID string
	for _, span := range recorder.Ended() {
		if span.Name() == "gateway.request" {
			rootTraceID = span.SpanContext().TraceID().String()
			break
		}
	}
	if rootTraceID == "" {
		t.Fatal("no gateway.request span recorded")
	}

	found := false
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode log record %q: %v", line, err)
		}
		if record["msg"] != "gateway request finalization failed" {
			continue
		}
		found = true
		if record["trace_id"] != rootTraceID {
			t.Fatalf("log trace_id = %v, want the root span trace id %q", record["trace_id"], rootTraceID)
		}
		if spanID, ok := record["span_id"].(string); !ok || spanID == "" {
			t.Fatalf("log span_id = %v, want the active root span id", record["span_id"])
		}
		if record["client_request_id"] != "client-9" {
			t.Fatalf("log client_request_id = %v, want client-9", record["client_request_id"])
		}
	}
	if !found {
		t.Fatalf("no finalization-failure log found in output %q", output.String())
	}
}
