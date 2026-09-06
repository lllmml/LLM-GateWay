package telemetry

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type failingSpanExporter struct {
	exported atomic.Int64
}

func (e *failingSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	e.exported.Add(1)
	return context.DeadlineExceeded
}

func (e *failingSpanExporter) Shutdown(context.Context) error { return nil }

type stalledSpanExporter struct {
	release  chan struct{}
	once     sync.Once
	started  chan struct{}
	exported atomic.Int64
}

func (e *stalledSpanExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	e.exported.Add(1)
	e.once.Do(func() { close(e.started) })
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *stalledSpanExporter) Shutdown(context.Context) error { return nil }

type noopSpanExporter struct{}

func (noopSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (noopSpanExporter) Shutdown(context.Context) error                             { return nil }

// TestRuntimeShutdownIsBoundedAndSingleUnderExporterErrors proves the first
// Runtime.Shutdown flushes through a failing exporter within its bounded
// context and that a second call is an idempotent no-op (ADR-019 N1).
func TestRuntimeShutdownIsBoundedAndSingleUnderExporterErrors(t *testing.T) {
	exporter := &failingSpanExporter{}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter,
			sdktrace.WithBatchTimeout(10*time.Millisecond),
			sdktrace.WithExportTimeout(50*time.Millisecond),
		)),
	)
	runtime := runtimeWithProvider(provider, provider.Shutdown)
	_, span := runtime.Tracer().Start(context.Background(), "gateway.request")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_ = runtime.Shutdown(ctx) // exporter errors may or may not surface; the contract is boundedness
	if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
		t.Fatalf("first Shutdown took %v, want bounded under exporter errors", elapsed)
	}
	if exporter.exported.Load() == 0 {
		t.Fatal("flush never reached the failing exporter")
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v, want nil idempotent no-op", err)
	}
}

// TestRuntimeShutdownBoundedWhileExporterStalled proves shutdown does not hang
// when the exporter is stuck: the bounded context expires, shutdown returns,
// and the exporter goroutine is released afterwards.
func TestRuntimeShutdownBoundedWhileExporterStalled(t *testing.T) {
	exporter := &stalledSpanExporter{
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter,
			sdktrace.WithBatchTimeout(10*time.Millisecond),
			sdktrace.WithExportTimeout(50*time.Millisecond),
		)),
	)
	runtime := runtimeWithProvider(provider, provider.Shutdown)
	_, span := runtime.Tracer().Start(context.Background(), "gateway.request")
	span.End()
	select {
	case <-exporter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("exporter never stalled on an export")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = runtime.Shutdown(ctx) // forced flush hits the stalled exporter and times out
	if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
		t.Fatalf("Shutdown took %v, want bounded while exporter is stalled", elapsed)
	}
	close(exporter.release) // release the exporter goroutine after the assert
}

// TestBatchSpanProcessorNoGoroutineLeakAfterShutdown proves the batch
// processor's background goroutines stop once the provider is shut down.
func TestBatchSpanProcessorNoGoroutineLeakAfterShutdown(t *testing.T) {
	before := runtime.NumGoroutine()
	func() {
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
			sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(noopSpanExporter{},
				sdktrace.WithBatchTimeout(10*time.Millisecond),
			)),
		)
		tracer := provider.Tracer("gateway")
		for index := 0; index < 25; index++ {
			_, span := tracer.Start(context.Background(), "gateway.request")
			span.End()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := provider.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}()
	runtime.GC()
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutines before=%d after=%d, want no leak (allow +2 ambient)", before, after)
	}
}
