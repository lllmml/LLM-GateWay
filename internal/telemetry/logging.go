package telemetry

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// NewLogger builds the single slog entry point (ADR-019 D6): a JSON handler
// wrapped by a context-aware enrichment handler. There is no second logger
// API. Context logging (InfoContext/ErrorContext/...) with an active span in
// the context additionally carries trace_id/span_id (and client_request_id
// when the request context carried a sanitized X-Request-ID); plain logging
// (Info/Error/...) passes context.Background(), so no span is found and output
// is byte-identical to today. The enrichment only adds identifiers - never
// prompts, responses, authorization material, keys, or credentials.
func NewLogger(output io.Writer, level slog.Level) *slog.Logger {
	jsonHandler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})
	return slog.New(&enrichmentHandler{next: jsonHandler})
}

type enrichmentHandler struct {
	next slog.Handler
}

func (h *enrichmentHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *enrichmentHandler) Handle(ctx context.Context, record slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if clientRequestID, ok := ClientRequestIDFromContext(ctx); ok {
		record.AddAttrs(slog.String("client_request_id", clientRequestID))
	}
	return h.next.Handle(ctx, record)
}

func (h *enrichmentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &enrichmentHandler{next: h.next.WithAttrs(attrs)}
}

func (h *enrichmentHandler) WithGroup(name string) slog.Handler {
	return &enrichmentHandler{next: h.next.WithGroup(name)}
}
