package dataplane

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

// newTracedService builds a data-plane Service wired to an AlwaysSampled
// in-memory span recorder so tests can assert the A2b1 span/trace-id contract
// without any real Collector.
func newTracedService(t *testing.T, store *fakeStore, client provider.Client) (*Service, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
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
		Tracer:                    tracerProvider.Tracer("gateway"),
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service, recorder
}

func recordedRoot(t *testing.T, recorder *tracetest.SpanRecorder, wantSpanCount int) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := recorder.Ended()
	if len(spans) != wantSpanCount {
		t.Fatalf("recorded spans = %d, want %d (%v)", len(spans), wantSpanCount, spanNames(spans))
	}
	for _, span := range spans {
		if span.Name() == "gateway.request" {
			return span
		}
	}
	t.Fatalf("no gateway.request span recorded: %v", spanNames(spans))
	return nil
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name())
	}
	return names
}

func attributeValue(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, attribute := range span.Attributes() {
		if string(attribute.Key) == key {
			return attribute.Value.AsString(), true
		}
	}
	return "", false
}

func makeHTTPChatRequest(t *testing.T, rawKey string, mutate func(*http.Request)) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	if mutate != nil {
		mutate(request)
	}
	return httptest.NewRecorder(), request
}

func TestRootSpanExistsBeforeAuthAndValidationFailures(t *testing.T) {
	// Auth failure (no authorized store): the root span must already exist,
	// created before auth, with no child spans yet.
	authService, authRecorder := newTracedService(t, &fakeStore{}, &fakeProviderClient{})
	handler := NewHandler(authService)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer not-a-gateway-key")
	response := httptest.NewRecorder()
	handler.chatCompletions(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("auth failure status = %d, want 401", response.Code)
	}
	root := recordedRoot(t, authRecorder, 1)
	if root.SpanKind() != trace.SpanKindServer {
		t.Fatalf("root span kind = %v, want server", root.SpanKind())
	}
	if traceID := root.SpanContext().TraceID().String(); len(traceID) != 32 {
		t.Fatalf("root trace id length = %d, want 32", len(traceID))
	}

	// Content-type validation failure: the root span exists before the
	// content-type check, still with no children.
	validationService, validationRecorder := newTracedService(t, &fakeStore{}, &fakeProviderClient{})
	validationHandler := NewHandler(validationService)
	validationRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	validationRequest.Header.Set("Content-Type", "text/plain")
	validationResponse := httptest.NewRecorder()
	validationHandler.chatCompletions(validationResponse, validationRequest)
	if validationResponse.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type failure status = %d, want 415", validationResponse.Code)
	}
	recordedRoot(t, validationRecorder, 1)
}

func TestPersistedTraceIDEqualsRootSpanTraceID(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	service, recorder := newTracedService(t, store, &fakeProviderClient{})
	handler := NewHandler(service)
	response, request := makeHTTPChatRequest(t, rawKey, nil)
	handler.chatCompletions(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}

	root := recordedRoot(t, recorder, 1)
	wantTraceID := root.SpanContext().TraceID().String()
	if len(wantTraceID) != 32 {
		t.Fatalf("root trace id length = %d, want 32", len(wantTraceID))
	}
	if store.lastCreate.TraceID != wantTraceID {
		t.Fatalf("persisted trace id = %q, want the root span trace id %q (single source of truth; ADR-019 D1)", store.lastCreate.TraceID, wantTraceID)
	}
	rowID, ok := attributeValue(root, attrGatewayRequestRowID)
	if !ok || rowID != testRequestID {
		t.Fatalf("root gateway.request_row_id = (%q, %v), want (%q, true)", rowID, ok, testRequestID)
	}
	// Whitelist: A2b1 sets exactly one attribute on the root span - the durable
	// row id. The llm.* attributes and error status land with the child-span
	// slice (A2b2).
	if attributes := root.Attributes(); len(attributes) != 1 {
		t.Fatalf("root span attributes = %v, want exactly gateway.request_row_id", attributes)
	}
}

func TestXRequestIDNeverBecomesPersistedTraceID(t *testing.T) {
	cases := map[string]func(*http.Request){
		"valid trimmed": func(request *http.Request) {
			request.Header.Set("X-Request-ID", " client-correlation-1 ")
		},
		"over long": func(request *http.Request) {
			request.Header.Set("X-Request-ID", strings.Repeat("a", 300))
		},
		"control character": func(request *http.Request) {
			request.Header.Set("X-Request-ID", "client-\x01-id")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			store, rawKey := newAuthorizedStore(t)
			service, recorder := newTracedService(t, store, &fakeProviderClient{})
			handler := NewHandler(service)
			response, request := makeHTTPChatRequest(t, rawKey, mutate)
			handler.chatCompletions(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			root := recordedRoot(t, recorder, 1)
			if store.lastCreate.TraceID != root.SpanContext().TraceID().String() {
				t.Fatalf("persisted trace id = %q, want the root span trace id (X-Request-ID is correlation metadata only; ADR-019 D1/D7)", store.lastCreate.TraceID)
			}
			if _, ok := attributeValue(root, "client_request_id"); ok {
				t.Fatal("client X-Request-ID leaked into a span attribute (ADR-019 D7)")
			}
		})
	}
}

func TestIncomingTraceparentHeaderIgnoredAndNotLeaked(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	service, recorder := newTracedService(t, store, &fakeProviderClient{})
	handler := NewHandler(service)
	rawTraceparent := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	response, request := makeHTTPChatRequest(t, rawKey, func(request *http.Request) {
		request.Header.Set("Traceparent", rawTraceparent)
	})
	handler.chatCompletions(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), rawTraceparent) {
		t.Fatal("incoming traceparent leaked into the response body")
	}
	root := recordedRoot(t, recorder, 1)
	clientTraceID := strings.Split(rawTraceparent, "-")[1]
	if got := root.SpanContext().TraceID().String(); got == clientTraceID {
		t.Fatalf("gateway adopted the client traceparent as its trace id: %q", got)
	}
	if store.lastCreate.TraceID != root.SpanContext().TraceID().String() {
		t.Fatal("persisted trace id does not equal the gateway root span trace id")
	}
}
