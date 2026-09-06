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

// allowedSpanAttributeKeys is the whitelist enforced on every span recorded by
// the gateway (ADR-019 D11 / A2b2): llm.*, row id, retry attempt index, and
// error category. Raw models, prompts, keys, and error text are forbidden.
var allowedSpanAttributeKeys = map[string]bool{
	"gateway.request_row_id": true,
	"llm.provider":           true,
	"llm.model_family":       true,
	"llm.stream":             true,
	"gateway.retry_attempt":  true,
	"gateway.error_category": true,
}

// newTracedService builds a data-plane Service wired to an AlwaysSampled
// in-memory span recorder so tests can assert the span hierarchy and trace-id
// contract without any real Collector.
func newTracedService(t *testing.T, store *fakeStore, client provider.Client) (*Service, *tracetest.SpanRecorder) {
	return newTracedServiceWithRetries(t, store, client, 0)
}

func newTracedServiceWithRetries(t *testing.T, store *fakeStore, client provider.Client, retryMax int) (*Service, *tracetest.SpanRecorder) {
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
		RetryMaxRetries:           retryMax,
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

func spanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("no %q span recorded: %v", name, spanNames(spans))
	return nil
}

func attributeValue(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, attribute := range span.Attributes() {
		if string(attribute.Key) == key {
			return attribute.Value.AsString(), true
		}
	}
	return "", false
}

func assertAttributesWhitelisted(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, span := range spans {
		for _, attribute := range span.Attributes() {
			if !allowedSpanAttributeKeys[string(attribute.Key)] {
				t.Fatalf("span %q carries a non-whitelisted attribute %q", span.Name(), string(attribute.Key))
			}
		}
	}
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
	// Auth failure (no authorized store): gateway.request -> auth.virtual_key,
	// with nothing else.
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
	spans := authRecorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("auth-failure spans = %v, want [gateway.request auth.virtual_key]", spanNames(spans))
	}
	root := recordedRoot(t, authRecorder, 2)
	if root.SpanKind() != trace.SpanKindServer {
		t.Fatalf("root span kind = %v, want server", root.SpanKind())
	}
	authSpan := spanByName(t, spans, "auth.virtual_key")
	if authSpan.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatalf("auth span parent %s, want root %s", authSpan.Parent().SpanID(), root.SpanContext().SpanID())
	}
	if category, ok := attributeValue(authSpan, attrGatewayErrorCategory); !ok || category != string(provider.AuthenticationFailed) {
		t.Fatalf("auth failure category = (%q, %v), want authentication_failed", category, ok)
	}
	assertAttributesWhitelisted(t, spans)

	// Content-type validation failure: only the root span exists (created
	// before the content-type check).
	validationService, validationRecorder := newTracedService(t, &fakeStore{}, &fakeProviderClient{})
	validationHandler := NewHandler(validationService)
	validationRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	validationRequest.Header.Set("Content-Type", "text/plain")
	validationResponse := httptest.NewRecorder()
	validationHandler.chatCompletions(validationResponse, validationRequest)
	if validationResponse.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type failure status = %d, want 415", validationResponse.Code)
	}
	if spans := validationRecorder.Ended(); len(spans) != 1 || spans[0].Name() != "gateway.request" {
		t.Fatalf("content-type failure spans = %v, want [gateway.request]", spanNames(spans))
	}
}

func TestRequestSpanHierarchyAndAttributes(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	service, recorder := newTracedService(t, store, &fakeProviderClient{})
	handler := NewHandler(service)
	response, request := makeHTTPChatRequest(t, rawKey, nil)
	handler.chatCompletions(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}

	spans := recorder.Ended()
	if len(spans) != 6 {
		t.Fatalf("success spans = %v, want exactly the six-span hierarchy (no SQL/Redis/function spans)", spanNames(spans))
	}
	root := spanByName(t, spans, "gateway.request")
	want := []string{
		"gateway.request",
		"auth.virtual_key",
		"rate_limit.check",
		"usage.create_request_record",
		"provider.attempt",
		"usage.finalize",
	}
	for _, name := range want {
		span := spanByName(t, spans, name)
		if name != "gateway.request" && span.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("%s parent = %s, want root %s", name, span.Parent().SpanID(), root.SpanContext().SpanID())
		}
	}

	// Persisted trace id comes from the root span (single source of truth).
	if store.lastCreate.TraceID != root.SpanContext().TraceID().String() {
		t.Fatalf("persisted trace id = %q, want root trace id %q", store.lastCreate.TraceID, root.SpanContext().TraceID().String())
	}

	// Root attribute contract: durable row id + bounded llm.* only.
	rowID, ok := attributeValue(root, attrGatewayRequestRowID)
	if !ok || rowID != testRequestID {
		t.Fatalf("root row id = (%q, %v), want testRequestID", rowID, ok)
	}
	if providerName, ok := attributeValue(root, attrLLMProvider); !ok || providerName != "openai" {
		t.Fatalf("root llm.provider = (%q, %v), want openai", providerName, ok)
	}
	if family, ok := attributeValue(root, attrLLMModelFamily); !ok || family != "gpt" {
		t.Fatalf("root llm.model_family = (%q, %v), want gpt", family, ok)
	}
	if stream, ok := attributeValue(root, attrLLMStream); !ok || stream != "false" {
		t.Fatalf("root llm.stream = (%q, %v), want false", stream, ok)
	}
	if attempt, ok := attributeValue(spanByName(t, spans, "provider.attempt"), attrGatewayRetryAttempt); !ok || attempt != "0" {
		t.Fatalf("first attempt retry index = (%q, %v), want 0", attempt, ok)
	}
	assertAttributesWhitelisted(t, spans)
}

func TestProviderAttemptSpansPerRetry(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	zero := time.Duration(0)
	client := &fakeProviderClient{err: &provider.Error{
		Category:   provider.ProviderRateLimited,
		StatusCode: http.StatusTooManyRequests,
		RetryAfter: &zero,
		Message:    "upstream rate limit",
	}}
	service, recorder := newTracedServiceWithRetries(t, store, client, 1)
	handler := NewHandler(service)
	response, request := makeHTTPChatRequest(t, rawKey, nil)
	handler.chatCompletions(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", response.Code, response.Body.String())
	}

	spans := recorder.Ended()
	// root + auth + rate_limit + create + 2 x provider.attempt + finalize.
	if len(spans) != 7 {
		t.Fatalf("retry spans = %v, want seven spans", spanNames(spans))
	}
	attempts := make([]sdktrace.ReadOnlySpan, 0, 2)
	for _, span := range spans {
		if span.Name() == "provider.attempt" {
			attempts = append(attempts, span)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("provider.attempt spans = %d, want 2 (retries must be sibling spans, not retry_count on one span)", len(attempts))
	}
	seen := map[string]bool{}
	for _, attempt := range attempts {
		index, ok := attributeValue(attempt, attrGatewayRetryAttempt)
		if !ok {
			t.Fatal("provider.attempt missing gateway.retry_attempt")
		}
		seen[index] = true
		if category, ok := attributeValue(attempt, attrGatewayErrorCategory); !ok || category != string(provider.ProviderRateLimited) {
			t.Fatalf("attempt %s category = (%q, %v), want provider_rate_limited", index, category, ok)
		}
	}
	if !seen["0"] || !seen["1"] {
		t.Fatalf("retry indices = %v, want {0,1}", seen)
	}
	assertAttributesWhitelisted(t, spans)
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
			root := recordedRoot(t, recorder, 6)
			if store.lastCreate.TraceID != root.SpanContext().TraceID().String() {
				t.Fatalf("persisted trace id = %q, want the root span trace id (X-Request-ID is correlation metadata only; ADR-019 D1/D7)", store.lastCreate.TraceID)
			}
			for _, span := range recorder.Ended() {
				if _, ok := attributeValue(span, "client_request_id"); ok {
					t.Fatalf("span %q carries client_request_id as a span attribute (ADR-019 D7)", span.Name())
				}
			}
		})
	}
}

func TestStreamRequestHierarchyHasNoProviderStreamSpan(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	stream := &fakeChatStream{events: []provider.StreamEvent{{Done: true}}}
	client := &fakeProviderClient{streamResult: provider.StreamResult{Stream: stream}}
	service, recorder := newTracedService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()
	handler.chatCompletions(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}

	spans := recorder.Ended()
	if len(spans) != 6 {
		t.Fatalf("stream spans = %v, want the six-span hierarchy (provider.stream is deferred beyond A2)", spanNames(spans))
	}
	root := spanByName(t, spans, "gateway.request")
	for _, name := range []string{"auth.virtual_key", "rate_limit.check", "usage.create_request_record", "provider.attempt", "usage.finalize"} {
		span := spanByName(t, spans, name)
		if span.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("%s parent = %s, want root %s", name, span.Parent().SpanID(), root.SpanContext().SpanID())
		}
	}
	if attempt, ok := attributeValue(spanByName(t, spans, "provider.attempt"), attrGatewayRetryAttempt); !ok || attempt != "0" {
		t.Fatalf("stream open attempt retry index = (%q, %v), want 0", attempt, ok)
	}
	if stream, ok := attributeValue(root, attrLLMStream); !ok || stream != "true" {
		t.Fatalf("root llm.stream = (%q, %v), want true", stream, ok)
	}
	assertAttributesWhitelisted(t, spans)
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
	root := recordedRoot(t, recorder, 6)
	clientTraceID := strings.Split(rawTraceparent, "-")[1]
	if got := root.SpanContext().TraceID().String(); got == clientTraceID {
		t.Fatalf("gateway adopted the client traceparent as its trace id: %q", got)
	}
	if store.lastCreate.TraceID != root.SpanContext().TraceID().String() {
		t.Fatal("persisted trace id does not equal the gateway root span trace id")
	}
}
