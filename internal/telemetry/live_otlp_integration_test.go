//go:build integration

// Package telemetry: live OTLP round-trip evidence against the A3a local
// observability stack (ADR-019 D10). Requires `make observability-up` first:
// collector and Tempo running on 127.0.0.1:4317 / :3200. This is the
// "compose healthy but traces lost" guard: the test only passes when a real
// span exported over OTLP gRPC is queryable through the Tempo API.
//
// The test relies on the Tempo 2.10.x HTTP API contract
// (grafana/tempo:2.10.8): GET /api/traces/{traceID} returns the trace once
// ingested and 404 until then. If the pinned Tempo image is bumped,
// re-verify this contract on that version.
package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel/attribute"
)

func liveEndpoint(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func TestLiveOTLPTraceRoundTrip(t *testing.T) {
	collectorEndpoint := liveEndpoint("OTEL_LIVE_COLLECTOR_ENDPOINT", "http://127.0.0.1:4317")
	tempoAddr := liveEndpoint("OTEL_LIVE_TEMPO_ADDR", "http://127.0.0.1:3200")

	marker := fmt.Sprintf("evidence-%d", time.Now().UnixNano())
	runtime, err := NewRuntime(collectorEndpoint, "gateway-evidence")
	if err != nil {
		t.Fatalf("new runtime: %v (is the collector running? run `make observability-up`)", err)
	}
	tracer := runtime.Tracer()
	ctx, root := tracer.Start(context.Background(), "gateway.request", trace.WithAttributes(
		attribute.String("evidence.marker", marker),
		attribute.String("llm.provider", "openai"),
	))
	traceID := root.SpanContext().TraceID().String()
	_, attempt := tracer.Start(ctx, "provider.attempt")
	attempt.End()
	root.End()

	flushCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := runtime.Shutdown(flushCtx); err != nil {
		t.Fatalf("runtime shutdown (export flush): %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("%s/api/traces/%s", tempoAddr, traceID)
	deadline := time.Now().Add(20 * time.Second)
	for {
		response, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK {
				if bytes.Contains(body, []byte(marker)) &&
					bytes.Contains(body, []byte("gateway.request")) &&
					bytes.Contains(body, []byte("provider.attempt")) {
					t.Logf("trace %s queryable through Tempo with the exported spans", traceID)
					return
				}
				t.Fatalf("trace %s found but spans/attributes missing; body=%s", traceID, truncate(body))
			}
			if response.StatusCode == http.StatusNotFound {
				// Trace not visible yet; keep polling.
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("trace %s never became queryable through Tempo (collector or tempo down? run `make observability-up`)", traceID)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func truncate(body []byte) string {
	const limit = 2048
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "..."
}
