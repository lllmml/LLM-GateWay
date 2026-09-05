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

func TestOpsPlaneServesMetricsOnlyWhenWired(t *testing.T) {
	metrics, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	options := testOptions(t)
	options.MetricsHandler = metrics.Handler()
	application := New(options, &fakeDatabase{}, logger)
	opsMux := application.opsHandler(options.MetricsHandler)

	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsResponse := httptest.NewRecorder()
	opsMux.ServeHTTP(metricsResponse, metricsRequest)
	if metricsResponse.Code != http.StatusOK {
		t.Fatalf("GET /metrics on ops mux = %d, want 200; body=%s", metricsResponse.Code, metricsResponse.Body.String())
	}
	if !strings.Contains(metricsResponse.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics content type = %q, want text/plain", metricsResponse.Header().Get("Content-Type"))
	}
	if !strings.Contains(metricsResponse.Body.String(), "gateway_active_requests") {
		t.Fatalf("metrics body does not expose gateway_active_requests")
	}

	// Health endpoints must keep working next to /metrics.
	liveRequest := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	liveResponse := httptest.NewRecorder()
	opsMux.ServeHTTP(liveResponse, liveRequest)
	if liveResponse.Code != http.StatusOK {
		t.Fatalf("GET /health/live on ops mux = %d, want 200", liveResponse.Code)
	}
}

func TestOpsPlaneWithoutMetricsHandlerRejectsMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := New(testOptions(t), &fakeDatabase{}, logger)
	opsMux := application.opsHandler(nil)

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	opsMux.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics on ops mux without handler = %d, want 404", response.Code)
	}
}
