package dataplane

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
	"github.com/lllmml/production-go-llm-gateway/internal/telemetry"
)

// gatherDataPlaneMetrics returns the metric families of a wired telemetry
// value so service-level tests can assert exact counting positions.
func gatherDataPlaneMetrics(t *testing.T, metrics *telemetry.Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}
	return byName
}

func dpLabel(t *testing.T, metric *dto.Metric, name string) string {
	t.Helper()
	for _, pair := range metric.Label {
		if pair.GetName() == name {
			return pair.GetValue()
		}
	}
	return ""
}

func findRequestCounter(t *testing.T, families map[string]*dto.MetricFamily, want map[string]string) *dto.Metric {
	t.Helper()
	family, ok := families["gateway_requests_total"]
	if !ok {
		return nil
	}
	for _, metric := range family.Metric {
		match := true
		for name, value := range want {
			if dpLabel(t, metric, name) != value {
				match = false
				break
			}
		}
		if match {
			return metric
		}
	}
	return nil
}

func activeRequestGauge(t *testing.T, families map[string]*dto.MetricFamily) float64 {
	t.Helper()
	family, ok := families["gateway_active_requests"]
	if !ok || len(family.Metric) != 1 {
		return -1
	}
	return family.Metric[0].GetGauge().GetValue()
}

// newMetricsService builds a data-plane Service wired to a fresh telemetry
// value with a single OpenAI fake client (same posture as the shared test
// harness, plus the Week 10 Metrics option).
func newMetricsService(t *testing.T, store *fakeStore, client provider.Client, metrics *telemetry.Metrics) *Service {
	t.Helper()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	service, err := NewService(Options{
		Store:                     store,
		VirtualKeyPepper:          bytes.Repeat([]byte{9}, 32),
		CredentialCipher:          cipher,
		UpstreamRequestTimeout:    time.Second,
		UpstreamStreamMaxDuration: time.Second,
		ProviderRegistry:          newTestProviderRegistry(t, map[provider.Name]provider.Client{provider.OpenAI: client}),
		Metrics:                   metrics,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

func TestMetricsCountSuccessAndFailureTerminalStates(t *testing.T) {
	metrics, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	store, rawKey := newAuthorizedStore(t)
	successClient := &fakeProviderClient{}
	service := newMetricsService(t, store, successClient, metrics)
	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	chat := provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
	if _, _, err := service.CompleteChat(context.Background(), auth, "", chat); err != nil {
		t.Fatalf("success request failed: %v", err)
	}
	families := gatherDataPlaneMetrics(t, metrics)
	success := findRequestCounter(t, families, map[string]string{
		"provider": "openai", "model_family": "gpt", "status": "succeeded", "stream": "false",
	})
	if success == nil || success.GetCounter().GetValue() != 1 {
		t.Fatalf("succeeded counter = %v, want exactly 1", success)
	}
	if activeRequestGauge(t, families) != 0 {
		t.Fatalf("active requests gauge = %v after success, want 0", activeRequestGauge(t, families))
	}

	// A deterministic terminal provider failure must count as status=failed.
	failingStore, failingRawKey := newAuthorizedStore(t)
	failingClient := &fakeProviderClient{err: &provider.Error{
		Category:   provider.ProviderTimeout,
		StatusCode: http.StatusGatewayTimeout,
		Message:    "upstream timeout",
	}}
	failingService := newMetricsService(t, failingStore, failingClient, metrics)
	failingAuth, err := failingService.Authenticate(context.Background(), failingRawKey)
	if err != nil {
		t.Fatalf("authenticate failing service: %v", err)
	}
	if _, _, err := failingService.CompleteChat(context.Background(), failingAuth, "", chat); err == nil {
		t.Fatal("failing request unexpectedly succeeded")
	}
	families = gatherDataPlaneMetrics(t, metrics)
	failed := findRequestCounter(t, families, map[string]string{
		"provider": "openai", "model_family": "gpt", "status": "failed", "stream": "false",
	})
	if failed == nil || failed.GetCounter().GetValue() != 1 {
		t.Fatalf("failed counter = %v, want exactly 1", failed)
	}
}

func TestMetricsFinalizePersistenceFailureStillCountsTerminalState(t *testing.T) {
	metrics, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	store, rawKey := newAuthorizedStore(t)
	store.finalizeErr = errors.New("postgres unavailable")
	service := newMetricsService(t, store, &fakeProviderClient{}, metrics)
	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, _, err := service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}); err == nil {
		t.Fatal("finalize persistence failure was not surfaced")
	}
	families := gatherDataPlaneMetrics(t, metrics)
	success := findRequestCounter(t, families, map[string]string{
		"provider": "openai", "model_family": "gpt", "status": "succeeded", "stream": "false",
	})
	if success == nil || success.GetCounter().GetValue() != 1 {
		t.Fatalf("succeeded counter with failed durable write = %v, want exactly 1 (ADR-019 D3 counting semantics)", success)
	}
}

func TestMetricsAuthFailureDoesNotCount(t *testing.T) {
	metrics, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	service := newMetricsService(t, &fakeStore{}, &fakeProviderClient{}, metrics)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"openai/gpt-test","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer not-a-gateway-key")
	response := httptest.NewRecorder()
	handler.chatCompletions(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	families := gatherDataPlaneMetrics(t, metrics)
	if findRequestCounter(t, families, map[string]string{}) != nil {
		t.Fatal("pre-row auth failure created a gateway_requests_total series")
	}
	if activeRequestGauge(t, families) != 0 {
		t.Fatalf("active requests gauge = %v after auth failure, want 0", activeRequestGauge(t, families))
	}
}

func TestMetricsActiveGaugeTracksInFlightAndReleasesOnCompletion(t *testing.T) {
	metrics, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	store, rawKey := newAuthorizedStore(t)
	client := &blockingCompleteClient{started: make(chan struct{}), release: make(chan struct{})}
	service := newMetricsService(t, store, client, metrics)
	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
			Model:    "openai/gpt-test",
			Messages: []provider.Message{{Role: "user", Content: "hello"}},
		})
		done <- err
	}()
	<-client.started // admitted and executing inside the provider call
	families := gatherDataPlaneMetrics(t, metrics)
	if activeRequestGauge(t, families) != 1 {
		t.Fatalf("active requests gauge while in flight = %v, want 1", activeRequestGauge(t, families))
	}
	close(client.release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight request failed: %v", err)
	}
	families = gatherDataPlaneMetrics(t, metrics)
	if activeRequestGauge(t, families) != 0 {
		t.Fatalf("active requests gauge after completion = %v, want 0", activeRequestGauge(t, families))
	}
}

func TestMetricsStreamCompletesAndReleasesActiveStreamGauge(t *testing.T) {
	metrics, err := telemetry.NewMetrics()
	if err != nil {
		t.Fatalf("new metrics: %v", err)
	}
	store, rawKey := newAuthorizedStore(t)
	stream := &fakeChatStream{events: []provider.StreamEvent{{Done: true}}}
	client := &fakeProviderClient{streamResult: provider.StreamResult{Stream: stream}}
	service := newMetricsService(t, store, client, metrics)
	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	sink := &recordingSink{}
	if _, err := service.StreamChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
	}, sink); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	families := gatherDataPlaneMetrics(t, metrics)
	success := findRequestCounter(t, families, map[string]string{
		"provider": "openai", "model_family": "gpt", "status": "succeeded", "stream": "true",
	})
	if success == nil || success.GetCounter().GetValue() != 1 {
		t.Fatalf("stream succeeded counter = %v, want exactly 1", success)
	}
	streams, ok := families["gateway_active_streams"]
	if !ok || len(streams.Metric) != 1 {
		t.Fatal("active streams family missing its openai series")
	}
	if streams.Metric[0].GetGauge().GetValue() != 0 || dpLabel(t, streams.Metric[0], "provider") != "openai" {
		t.Fatalf("active streams after completion = %v (provider %q), want 0 openai", streams.Metric[0].GetGauge().GetValue(), dpLabel(t, streams.Metric[0], "provider"))
	}
}

func TestDataPlaneMuxDoesNotExposeMetrics(t *testing.T) {
	service := newTestService(t, &fakeStore{})
	mux := http.NewServeMux()
	NewHandler(service).Register(mux)

	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsResponse := httptest.NewRecorder()
	mux.ServeHTTP(metricsResponse, metricsRequest)
	if metricsResponse.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics on data plane mux = %d, want 404", metricsResponse.Code)
	}

	// The documented chat route must still be the only live route: an invalid
	// bearer is rejected before the body is read (route exists), never 404.
	chatRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{`))
	chatRequest.Header.Set("Content-Type", "application/json")
	chatRequest.Header.Set("Authorization", "Bearer not-a-gateway-key")
	chatResponse := httptest.NewRecorder()
	mux.ServeHTTP(chatResponse, chatRequest)
	if chatResponse.Code != http.StatusUnauthorized {
		t.Fatalf("POST chat route = %d, want 401", chatResponse.Code)
	}
}

type blockingCompleteClient struct {
	started chan struct{}
	release chan struct{}
}

func (c *blockingCompleteClient) CompleteChat(ctx context.Context, _ provider.ChatRequest, _ provider.Credential) (provider.Result, error) {
	close(c.started)
	select {
	case <-c.release:
		return provider.Result{}, nil
	case <-ctx.Done():
		return provider.Result{}, ctx.Err()
	}
}

type recordingSink struct{}

func (s *recordingSink) Prepare(GatewayRequest) error { return nil }
func (s *recordingSink) WriteEvent(provider.StreamEvent) error {
	return nil
}
func (s *recordingSink) Committed() bool { return false }
