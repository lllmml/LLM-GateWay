package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatCompletionsReturnsFixedResponse(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"openai/gpt-test"}`))
	request.Header.Set("Authorization", "Bearer sk-test")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("X-Request-ID"); got != "req_mock_123" {
		t.Fatalf("X-Request-ID = %q", got)
	}
	body := response.Body.String()
	for _, want := range []string{`"id":"chatcmpl_mock_123"`, `"object":"chat.completion"`, `"content":"mock response"`, `"total_tokens":10`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
	if hits := handler.(*mockHandler).Hits(); hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
}

func TestChatCompletionsReturnsConfiguredProviderError(t *testing.T) {
	handler := newHandler(config{status: http.StatusTooManyRequests})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{}`))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", response.Code)
	}
	if body := response.Body.String(); !strings.Contains(body, `"mock provider error"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestChatCompletionsCanReturnMalformedJSON(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK, malformed: true})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{}`))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if body := response.Body.String(); body != `{"id":` {
		t.Fatalf("body = %q", body)
	}
}

func TestStreamChatCompletionsReturnsSSEChunksUsageAndDone(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK, streamChunks: 2, streamUsage: true})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"stream":true}`))
	response := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header().Get("X-Request-ID"); got != "req_mock_123" {
		t.Fatalf("X-Request-ID = %q", got)
	}
	body := response.Body.String()
	for _, want := range []string{`data: {`, `"object":"chat.completion.chunk"`, `"content":"mock chunk 1"`, `"content":"mock chunk 2"`, `"total_tokens":10`, "data: [DONE]\n\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	if first := strings.Index(body, `"content":"mock chunk 1"`); first < 0 {
		t.Fatalf("first chunk missing: %s", body)
	} else if second := strings.Index(body, `"content":"mock chunk 2"`); second < first {
		t.Fatalf("chunks out of order: %s", body)
	}
	if response.flushes < 4 {
		t.Fatalf("flushes = %d, want at least 4", response.flushes)
	}
}

func TestStreamChatCompletionsUsesJSONForConfiguredProviderError(t *testing.T) {
	handler := newHandler(config{status: http.StatusTooManyRequests, streamUsage: true})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"stream":true}`))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if body := response.Body.String(); strings.Contains(body, "data:") || !strings.Contains(body, `"mock provider error"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestStreamChatCompletionsCanReturnMalformedSSE(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK, streamMalformed: true})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"stream":true}`))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if body := response.Body.String(); body != "data: {\"id\":\n\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestStreamChatCompletionsCanReturnOversizedSSEEvent(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK, streamOversized: true})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"stream":true}`))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("body prefix = %.16q", body)
	}
	if len(body) <= maxRequestBodyBytes {
		t.Fatalf("body length = %d, want > %d", len(body), maxRequestBodyBytes)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("oversized stream should stop before DONE: %s", body[len(body)-32:])
	}
}

func TestStreamChatCompletionsCanTerminateBeforeDone(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK, streamChunks: 2, streamAbrupt: true})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"stream":true}`))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if !strings.Contains(body, `"content":"mock chunk 1"`) || !strings.Contains(body, `"content":"mock chunk 2"`) {
		t.Fatalf("body = %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("abrupt stream should not include DONE: %s", body)
	}
}

func TestStreamFirstTokenDelayRespectsRequestCancellation(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK, streamFirstTokenDelay: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"stream":true}`)).WithContext(ctx)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if body := response.Body.String(); body != "" {
		t.Fatalf("body = %q", body)
	}
}

func TestRejectsUnsupportedMethodAndRoute(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK})

	methodResponse := httptest.NewRecorder()
	handler.ServeHTTP(methodResponse, httptest.NewRequest(http.MethodGet, chatCompletionsPath, nil))
	if methodResponse.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", methodResponse.Code)
	}
	if got := methodResponse.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q", got)
	}

	routeResponse := httptest.NewRecorder()
	handler.ServeHTTP(routeResponse, httptest.NewRequest(http.MethodPost, "/v1/models", nil))
	if routeResponse.Code != http.StatusNotFound {
		t.Fatalf("route status = %d", routeResponse.Code)
	}
}

func TestRejectsOversizedRequestBody(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK})
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(strings.Repeat("x", maxRequestBodyBytes+1)))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestHeaderDelayRespectsRequestCancellation(t *testing.T) {
	handler := newHandler(config{status: http.StatusOK, headerDelay: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{}`)).WithContext(ctx)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if body := response.Body.String(); body != "" {
		t.Fatalf("body = %q", body)
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}
