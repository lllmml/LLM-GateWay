package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestNewLoggerEnrichesContextLogsOnly confirms the single-logger enrichment
// contract (ADR-019 D6): InfoContext/ErrorContext with an active span carries
// trace_id/span_id (and client_request_id when present), while plain
// Info/Error keeps today's exact behavior with no extra fields.
func TestNewLoggerEnrichesContextLogsOnly(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, slog.LevelInfo)

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	ctx, span := provider.Tracer("gateway").Start(context.Background(), "gateway.request")
	ctx = WithClientRequestID(ctx, "req-1")
	logger.InfoContext(ctx, "ctx-message", "field", "value")
	logger.Info("plain-message", "field", "value")

	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2 (%q)", len(lines), output.String())
	}
	records := make([]map[string]any, 0, 2)
	for _, line := range lines {
		record := map[string]any{}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		records = append(records, record)
	}
	var contextRecord, plainRecord map[string]any
	for _, record := range records {
		switch record["msg"] {
		case "ctx-message":
			contextRecord = record
		case "plain-message":
			plainRecord = record
		}
	}
	if contextRecord == nil || plainRecord == nil {
		t.Fatalf("records = %v, want both messages", records)
	}
	if contextRecord["trace_id"] != span.SpanContext().TraceID().String() {
		t.Fatalf("context trace_id = %v, want %q", contextRecord["trace_id"], span.SpanContext().TraceID().String())
	}
	if contextRecord["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatalf("context span_id = %v, want %q", contextRecord["span_id"], span.SpanContext().SpanID().String())
	}
	if contextRecord["client_request_id"] != "req-1" {
		t.Fatalf("context client_request_id = %v, want req-1", contextRecord["client_request_id"])
	}
	for _, key := range []string{"trace_id", "span_id", "client_request_id"} {
		if _, ok := plainRecord[key]; ok {
			t.Fatalf("plain log unexpectedly carries %s=%v", key, plainRecord[key])
		}
	}
	if plainRecord["field"] != "value" {
		t.Fatalf("plain log fields = %v, want unchanged field=value", plainRecord)
	}
}
