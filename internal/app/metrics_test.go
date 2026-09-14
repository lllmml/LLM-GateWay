package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lllmml/production-go-llm-gateway/internal/telemetry"
)

// TestOpsPlaneServesMetricsWhenWired drives requests through the real Ops
// server handler New() built (application.opsServer.Handler), so the test
// fails if New() ever wires the wrong handler onto the ops server.
func TestOpsPlaneServesMetricsWhenWired(t *testing.T) {
	metrics, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	options := testOptions(t)
	options.MetricsHandler = metrics.Handler()
	application := New(options, &fakeDatabase{}, logger)
	opsHandler := application.opsServer.Handler

	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsResponse := httptest.NewRecorder()
	opsHandler.ServeHTTP(metricsResponse, metricsRequest)
	if metricsResponse.Code != http.StatusOK {
		t.Fatalf("GET /metrics on ops server handler = %d, want 200; body=%s", metricsResponse.Code, metricsResponse.Body.String())
	}
	if !strings.Contains(metricsResponse.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics content type = %q, want text/plain", metricsResponse.Header().Get("Content-Type"))
	}
	if !strings.Contains(metricsResponse.Body.String(), "gateway_active_requests") {
		t.Fatal("metrics body does not expose gateway_active_requests")
	}

	// Health endpoints must keep working next to /metrics on the same server.
	liveRequest := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	liveResponse := httptest.NewRecorder()
	opsHandler.ServeHTTP(liveResponse, liveRequest)
	if liveResponse.Code != http.StatusOK {
		t.Fatalf("GET /health/live on ops server handler = %d, want 200", liveResponse.Code)
	}
}

func TestOpsPlaneWithoutMetricsHandlerRejectsMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := New(testOptions(t), &fakeDatabase{}, logger)
	opsHandler := application.opsServer.Handler

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	opsHandler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics on ops server handler without MetricsHandler = %d, want 404", response.Code)
	}
}
