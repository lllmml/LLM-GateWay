package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// runtimeWithProvider builds a Runtime over an explicit provider for white-box
// lifecycle tests (enabled = a real shutdown function is supplied).
func runtimeWithProvider(provider trace.TracerProvider, shutdown func(context.Context) error) *Runtime {
	runtime := &Runtime{
		enabled:  shutdown != nil,
		provider: provider,
		shutdown: shutdown,
	}
	if runtime.provider == nil {
		runtime.provider = trace.NewNoopTracerProvider()
		runtime.enabled = false
		runtime.shutdown = func(context.Context) error { return nil }
	}
	return runtime.withTracer()
}

func TestDisabledRuntimeReturnsNoopTracerWithoutExporter(t *testing.T) {
	runtime, err := NewRuntime("", "gateway")
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if runtime.Enabled() {
		t.Fatal("empty endpoint must disable the runtime")
	}
	tracer := runtime.Tracer()
	if tracer == nil {
		t.Fatal("disabled runtime Tracer() returned nil; a noop tracer is required")
	}
	ctx, span := tracer.Start(context.Background(), "gateway.request")
	defer span.End()
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		t.Fatalf("disabled (noop) span context is valid, want invalid so no trace_id is ever derived")
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("disabled Shutdown = %v, want nil", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second disabled Shutdown = %v, want nil", err)
	}
}

func TestEnabledRuntimeRecordsAlwaysSampledSpans(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	runtime := runtimeWithProvider(provider, provider.Shutdown)
	if !runtime.Enabled() {
		t.Fatal("runtime with a real provider must be enabled")
	}
	_, span := runtime.Tracer().Start(context.Background(), "gateway.request")
	if !span.IsRecording() {
		t.Fatal("AlwaysSample runtime produced a non-recording span")
	}
	span.End()

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(spans))
	}
	if spans[0].Name() != "gateway.request" {
		t.Fatalf("span name = %q, want gateway.request", spans[0].Name())
	}
	traceID := spans[0].SpanContext().TraceID().String()
	if len(traceID) != 32 {
		t.Fatalf("trace id length = %d, want 32 hex chars", len(traceID))
	}
}

func TestRuntimeShutdownRunsUnderlyingShutdownExactlyOnce(t *testing.T) {
	var calls atomic.Int64
	countingShutdown := func(context.Context) error {
		calls.Add(1)
		return nil
	}
	runtime := runtimeWithProvider(trace.NewNoopTracerProvider(), countingShutdown)

	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown = %v, want nil", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v, want nil (idempotent no-op)", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("underlying shutdown calls = %d, want exactly 1 (ADR-019 N1)", calls.Load())
	}
}

func TestRuntimeShutdownReportsFirstErrorThenNoOps(t *testing.T) {
	wantErr := errors.New("flush failed")
	failing := func(context.Context) error { return wantErr }
	runtime := runtimeWithProvider(trace.NewNoopTracerProvider(), failing)

	if err := runtime.Shutdown(context.Background()); err != wantErr {
		t.Fatalf("first Shutdown error = %v, want the underlying error", err)
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v, want nil", err)
	}
}

func TestRuntimeConcurrentShutdownDoesNotBlockSecondCaller(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	wantErr := errors.New("flush failed")
	var calls atomic.Int64
	shutdown := func(context.Context) error {
		calls.Add(1)
		close(started)
		<-release
		return wantErr
	}
	runtime := runtimeWithProvider(trace.NewNoopTracerProvider(), shutdown)

	// First caller becomes the owner and blocks inside the real cleanup.
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- runtime.Shutdown(context.Background())
	}()
	<-started

	// A second caller while the first is still running must return nil
	// immediately: no waiting, no second cleanup.
	secondStarted := time.Now()
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second caller Shutdown = %v, want immediate nil", err)
	}
	if elapsed := time.Since(secondStarted); elapsed > 250*time.Millisecond {
		t.Fatalf("second caller blocked for %v behind the first shutdown", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("cleanup calls while first is blocked = %d, want exactly 1", calls.Load())
	}

	// Only the owning caller receives the real error.
	close(release)
	if err := <-firstErr; err != wantErr {
		t.Fatalf("first caller error = %v, want %v", err, wantErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("cleanup calls after completion = %d, want exactly 1", calls.Load())
	}
	// Post-completion callers also return nil immediately without re-running.
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("post-completion Shutdown = %v, want nil", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cleanup calls after post-completion Shutdown = %d, want 1", calls.Load())
	}
}

func TestNewRuntimeWithEndpointDoesNotRequireLiveCollector(t *testing.T) {
	// A closed port proves construction is lazy (no dial at startup) and that
	// shutdown is bounded: nothing listens at :1.
	runtime, err := NewRuntime("http://127.0.0.1:1", "gateway")
	if err != nil {
		t.Fatalf("new runtime with unreachable endpoint: %v", err)
	}
	if !runtime.Enabled() {
		t.Fatal("non-empty endpoint must enable the runtime")
	}
	if runtime.Tracer() == nil {
		t.Fatal("enabled runtime returned a nil tracer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown against unreachable collector = %v, want nil or bounded", err)
	}
}

func TestNewRuntimeRejectsMalformedEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"not a url",
		"://missing-scheme",
		"ftp://127.0.0.1:4317",
		"http://user:pass@127.0.0.1:4317",
		"http://127.0.0.1:4317/with/path",
		"http://127.0.0.1:4317?query=1",
	} {
		if runtime, err := NewRuntime(endpoint, "gateway"); err == nil {
			runtime.Shutdown(context.Background())
			t.Fatalf("NewRuntime(%q) succeeded, want an error", endpoint)
		}
	}
}
