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
