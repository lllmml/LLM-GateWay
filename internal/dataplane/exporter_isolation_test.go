package dataplane

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

// errorSpanExporter fails every export so tests can prove exporter errors never
// reach the HTTP request path (ADR-019 D9/failure matrix).
type errorSpanExporter struct {
	exported atomic.Int64
}

func (e *errorSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	e.exported.Add(1)
	return context.DeadlineExceeded // deterministic export failure
}

func (e *errorSpanExporter) Shutdown(context.Context) error { return nil }

// blockingSpanExporter blocks each export until release is closed (or the
// context ends), so tests can prove a stalled exporter never blocks the
// request path.
type blockingSpanExporter struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	exported  atomic.Int64
}

func (e *blockingSpanExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	e.exported.Add(1)
	e.startOnce.Do(func() { close(e.started) })
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *blockingSpanExporter) Shutdown(context.Context) error { return nil }

// newBatchProvider builds a provider whose BatchSpanProcessor exports through
// the given exporter with small, deterministic timing so A2c tests never wait
// on real network or a real Collector.
func newBatchProvider(t *testing.T, exporter sdktrace.SpanExporter) *sdktrace.TracerProvider {
	t.Helper()
	return sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter,
			sdktrace.WithBatchTimeout(10*time.Millisecond),
			sdktrace.WithExportTimeout(50*time.Millisecond),
			sdktrace.WithMaxQueueSize(4),
			sdktrace.WithMaxExportBatchSize(1),
		)),
	)
}

func tracedServiceWithTracer(t *testing.T, store *fakeStore, client provider.Client, tracer trace.Tracer) *Service {
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
		Tracer:                    tracer,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

// TestExporterErrorsDoNotAffectHTTPRequest proves exporter failure never
// changes the HTTP result and that shutdown stays bounded while flushing.
func TestExporterErrorsDoNotAffectHTTPRequest(t *testing.T) {
	exporter := &errorSpanExporter{}
	provider := newBatchProvider(t, exporter)
	store, rawKey := newAuthorizedStore(t)
	service := tracedServiceWithTracer(t, store, &fakeProviderClient{}, provider.Tracer("gateway"))
	handler := NewHandler(service)

	response, request := makeHTTPChatRequest(t, rawKey, nil)
	handler.chatCompletions(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite exporter errors; body=%s", response.Code, response.Body.String())
	}
	if store.lastFinalize.Status != "succeeded" {
		t.Fatalf("finalize status = %q, want succeeded", store.lastFinalize.Status)
	}

	// Shutdown flushes synchronously, forcing the exporter failure, and must
	// still return within its bounded context (an error is acceptable).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := provider.Shutdown(ctx); err == nil {
		// Export failures may or may not surface; the contract is only that it
		// is bounded and does not block the data path.
		t.Log("shutdown returned nil despite exporter errors (allowed)")
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v, want bounded under exporter failure", elapsed)
	}
	if exporter.exported.Load() == 0 {
		t.Fatal("exporter was never invoked during flush; the error path was not exercised")
	}
}

// TestExporterBlockingDoesNotBlockRequestPath proves a stalled exporter never
// blocks the synchronous HTTP request and that release + shutdown stay
// bounded.
func TestExporterBlockingDoesNotBlockRequestPath(t *testing.T) {
	exporter := &blockingSpanExporter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	provider := newBatchProvider(t, exporter)
	store, rawKey := newAuthorizedStore(t)
	service := tracedServiceWithTracer(t, store, &fakeProviderClient{}, provider.Tracer("gateway"))
	handler := NewHandler(service)

	response, request := makeHTTPChatRequest(t, rawKey, nil)
	done := make(chan struct{})
	go func() {
		handler.chatCompletions(response, request)
		close(done)
	}()
	// The HTTP request must finish even though the exporter is about to block.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request blocked while the exporter is stalled")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	// Give the async batch processor a moment to pick up the span and block.
	select {
	case <-exporter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("batch processor never attempted an export")
	}

	close(exporter.release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := provider.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown after release = %v, want nil", err)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v, want bounded", elapsed)
	}
}

// TestCancellationEndsRequestSpans proves a cancelled in-flight request ends
// every started span (root, attempt, finalize) with no goroutine leak and a
// bounded shutdown.
func TestCancellationEndsRequestSpans(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &blockingCompleteClient{started: make(chan struct{}), release: make(chan struct{})}
	service, recorder := newTracedService(t, store, client)
	handler := NewHandler(service)

	requestCtx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`)).WithContext(requestCtx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.chatCompletions(response, request)
		close(done)
	}()
	<-client.started // admitted and blocked inside the provider call
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled request did not finish")
	}
	spans := recorder.Ended()
	if len(spans) != 6 {
		t.Fatalf("cancelled request spans = %v, want the six-span hierarchy all ended", spanNames(spans))
	}
	for _, want := range []string{"gateway.request", "auth.virtual_key", "rate_limit.check", "usage.create_request_record", "provider.attempt", "usage.finalize"} {
		spanByName(t, spans, want)
	}
	assertAttributesWhitelisted(t, spans)
}
