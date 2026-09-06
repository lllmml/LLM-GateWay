package telemetry

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// ShutdownTimeout bounds gateway-owned telemetry shutdown calls
	// (ADR-019 D9/N1). internal/app keeps its own orchestration timeout and
	// never imports telemetry; this constant is the single source for
	// cmd/gateway's construction-time fallback.
	ShutdownTimeout = 5 * time.Second

	serviceTracerName = "gateway"
)

// Runtime owns the process tracing lifecycle (ADR-019 N1): the underlying
// TracerProvider Shutdown executes at most once, owned by the first Shutdown
// caller, and the gateway lifecycle contract never depends on the OTel SDK's
// own Shutdown idempotency. There is no package-global or
// otel.SetTracerProvider state: every Runtime builds its own provider, and
// Tracer() always returns a tracer owned by that provider.
//
// Disabled mode (empty OTLP endpoint) uses an app-owned noop tracer provider:
// Tracer() is never nil, Start() yields a non-recording span with an invalid
// SpanContext, so business code runs one uniform
// "ctx, span := tracer.Start(...); defer span.End()" path with zero guards
// and zero OTel export machinery.
type Runtime struct {
	enabled  bool
	provider trace.TracerProvider
	tracer   trace.Tracer
	shutdown func(context.Context) error

	// shutdownMu serializes the shutdown ownership handoff: the first caller
	// claims ownership under the lock; later callers observe ownership and
	// return nil immediately - they never re-run the cleanup and never wait
	// for the owning attempt to finish. shutdownDone is closed by the owning
	// caller once its real cleanup has finished.
	shutdownMu    sync.Mutex
	shutdownOwned bool
	shutdownDone  chan struct{}
	shutdownErr   error
}

// NewRuntime creates the gateway tracing runtime. A blank endpoint returns a
// disabled runtime (noop tracer, no exporter, no goroutines). A non-blank
// endpoint must be an http(s) origin URL without credentials, path, query, or
// fragment; http selects an insecure OTLP gRPC connection and https the
// default TLS connection. Errors never echo the raw endpoint value.
func NewRuntime(endpoint, serviceName string) (*Runtime, error) {
	endpoint = strings.TrimSpace(endpoint)
	if serviceName == "" {
		serviceName = "gateway"
	}
	if endpoint == "" {
		runtime := &Runtime{
			enabled:  false,
			provider: trace.NewNoopTracerProvider(),
			shutdown: func(context.Context) error { return nil },
		}
		return runtime.withTracer(), nil
	}
	hostPort, insecure, err := parseOTLPEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	options := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(hostPort)}
	if insecure {
		options = append(options, otlptracegrpc.WithInsecure())
	}
	// The exporter connects lazily (no dial at construction), so creating the
	// runtime never requires a live Collector and a Collector outage never
	// blocks startup or the request path (ADR-019 failure policy).
	exporter, err := otlptracegrpc.New(context.Background(), options...)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", serviceName))),
		sdktrace.WithBatcher(exporter),
	)
	return (&Runtime{
		enabled:  true,
		provider: provider,
		shutdown: provider.Shutdown,
	}).withTracer(), nil
}

func (r *Runtime) withTracer() *Runtime {
	if r.shutdownDone == nil {
		r.shutdownDone = make(chan struct{})
	}
	r.tracer = r.provider.Tracer(serviceTracerName)
	return r
}

// Enabled reports whether a real OTLP exporter is configured. Disabled
// runtimes still return a working noop tracer from Tracer().
func (r *Runtime) Enabled() bool {
	if r == nil {
		return false
	}
	return r.enabled
}

// Tracer returns the app-owned tracer. It is never nil: enabled runtimes
// return the SDK tracer, disabled runtimes return the noop tracer.
func (r *Runtime) Tracer() trace.Tracer {
	if r == nil {
		return trace.NewNoopTracerProvider().Tracer(serviceTracerName)
	}
	return r.tracer
}

// Shutdown performs the single real telemetry shutdown (ADR-019 D9/N1) with
// explicit, non-blocking concurrency semantics. The FIRST caller owns the
// cleanup attempt and runs the underlying provider shutdown under the
// supplied bounded context; that call's error (if any) is returned to that
// caller only, and completion is signaled by closing shutdownDone.
//
// Every other caller - before, during, or after that attempt - returns nil
// immediately: it never re-runs the cleanup and never waits for the owner to
// finish. In particular a timed-out first shutdown is NOT re-executed and NOT
// awaited by later calls.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.shutdownMu.Lock()
	if r.shutdownOwned {
		r.shutdownMu.Unlock()
		return nil
	}
	r.shutdownOwned = true
	r.shutdownMu.Unlock()

	// This caller owns the single real shutdown attempt.
	shutdownErr := r.shutdown(ctx)

	r.shutdownMu.Lock()
	r.shutdownErr = shutdownErr
	r.shutdownMu.Unlock()
	close(r.shutdownDone)
	return shutdownErr
}

func parseOTLPEndpoint(raw string) (hostPort string, insecure bool, err error) {
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || parsed.Host == "" || parsed.Scheme == "" {
		return "", false, errors.New("invalid OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", false, errors.New("invalid OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	switch parsed.Scheme {
	case "http":
		return parsed.Host, true, nil
	case "https":
		return parsed.Host, false, nil
	default:
		return "", false, errors.New("invalid OTEL_EXPORTER_OTLP_ENDPOINT")
	}
}
