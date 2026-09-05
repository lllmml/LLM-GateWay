package dataplane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/anthropic"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/deepseek"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/openai"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

const (
	testProjectID    = "11111111-1111-4111-8111-111111111111"
	testVirtualKeyID = "22222222-2222-4222-8222-222222222222"
	testCredentialID = "33333333-3333-4333-8333-333333333333"
	testRequestID    = "44444444-4444-4444-8444-444444444444"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestHandlerAuthenticatesBeforeDecodingBody(t *testing.T) {
	service := newTestService(t, &fakeStore{})
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer not-a-gateway-key")
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
}

func TestCompleteChatCreatesCallsProviderAndFinalizesSuccess(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{
			Response: provider.ChatResponse{
				ID:      "chatcmpl_test",
				Object:  "chat.completion",
				Created: 123,
				Model:   "gpt-test",
				Choices: []provider.Choice{{
					Index:        0,
					Message:      provider.ResponseMessage{Role: "assistant", Content: "ok"},
					FinishReason: "stop",
				}},
			},
			Usage:             &provider.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
			UpstreamStatus:    http.StatusOK,
			UpstreamRequestID: "req_test",
		},
	}
	client.result.Response.Usage = client.result.Usage
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	result, record, err := service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
	if result.Response.ID != "chatcmpl_test" || record.ID != testRequestID {
		t.Fatalf("result=%+v record=%+v", result, record)
	}
	if client.calls != 1 || string(client.lastCredential.APIKey) != "sk-test" || client.lastChat.Model != "gpt-test" {
		t.Fatalf("client calls=%d credential=%q chat=%+v", client.calls, client.lastCredential.APIKey, client.lastChat)
	}
	if store.createCalls != 1 || store.finalizeCalls != 1 {
		t.Fatalf("create calls=%d finalize calls=%d", store.createCalls, store.finalizeCalls)
	}
	if store.lastFinalize.Status != "succeeded" || store.lastFinalize.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
	if store.lastFinalize.PromptTokens == nil || *store.lastFinalize.PromptTokens != 7 {
		t.Fatalf("tokens not persisted: %+v", store.lastFinalize)
	}
	if store.lastCreate.TraceID != "" {
		t.Fatalf("trace ID = %q, want empty", store.lastCreate.TraceID)
	}
}

func TestCompleteChatPropagatesTraceIDToGatewayRequest(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{}
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "trace-test", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
	if store.lastCreate.TraceID != "trace-test" {
		t.Fatalf("trace ID = %q, want trace-test", store.lastCreate.TraceID)
	}
}

func TestHandlerUsesIngressTimeForGatewayRequestStart(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	var authObservedAt time.Time
	store.onAuth = func() {
		authObservedAt = time.Now().UTC()
	}
	client := &fakeProviderClient{
		result: provider.Result{
			Response:       provider.ChatResponse{ID: "chatcmpl_test", Object: "chat.completion", Created: 123, Model: "gpt-test", Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}}}},
			UpstreamStatus: http.StatusOK,
		},
	}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if authObservedAt.IsZero() {
		t.Fatal("auth hook did not run")
	}
	if store.lastCreate.StartedAt.After(authObservedAt) {
		t.Fatalf("started_at = %v, want no later than auth lookup at %v", store.lastCreate.StartedAt, authObservedAt)
	}
}

func TestCreateFailureDoesNotCallProvider(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	store.createErr = errors.New("database unavailable")
	client := &fakeProviderClient{}
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("CompleteChat returned nil error")
	}
	if client.calls != 0 || store.finalizeCalls != 0 {
		t.Fatalf("client calls=%d finalize calls=%d", client.calls, store.finalizeCalls)
	}
}

func TestMissingProviderConfigDoesNotCreateRequestOrCallProvider(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	store.credential = ProviderCredential{}
	client := &fakeProviderClient{}
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.ProviderNotConfigured {
		t.Fatalf("error = %#v", err)
	}
	if store.createCalls != 0 || store.finalizeCalls != 0 || client.calls != 0 {
		t.Fatalf("create calls=%d finalize calls=%d client calls=%d", store.createCalls, store.finalizeCalls, client.calls)
	}
}

func TestUnknownProviderDoesNotCreateRequestOrCallProvider(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{}
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "ollama/llama3",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.ProviderNotConfigured {
		t.Fatalf("error = %#v", err)
	}
	if store.resolveCalls != 0 || store.createCalls != 0 || store.finalizeCalls != 0 || client.calls != 0 {
		t.Fatalf("resolve calls=%d create calls=%d finalize calls=%d client calls=%d", store.resolveCalls, store.createCalls, store.finalizeCalls, client.calls)
	}
}

func TestStreamRequestReturnsUnsupportedFeature(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{}
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.UnsupportedFeature || gatewayErr.Message != "streaming chat completions are not supported in this milestone" {
		t.Fatalf("error = %#v", err)
	}
	if store.resolveCalls != 0 || store.createCalls != 0 || store.finalizeCalls != 0 || client.calls != 0 {
		t.Fatalf("resolve calls=%d create calls=%d finalize calls=%d client calls=%d", store.resolveCalls, store.createCalls, store.finalizeCalls, client.calls)
	}
}

func TestProviderFailureFinalizesStableError(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{UpstreamStatus: http.StatusGatewayTimeout, UpstreamRequestID: "req_timeout"},
		err:    &provider.Error{Category: provider.ProviderTimeout, StatusCode: http.StatusGatewayTimeout, Message: "raw upstream text"},
	}
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.ProviderTimeout || strings.Contains(gatewayErr.Message, "raw upstream") {
		t.Fatalf("error = %#v", err)
	}
	if store.finalizeCalls != 1 || store.lastFinalize.ErrorCategory == nil || *store.lastFinalize.ErrorCategory != provider.ProviderTimeout {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
}

func TestProviderFailureWithFinalizeFailureReturnsPersistenceError(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	store.finalizeErr = errors.New("finalize write failed")
	client := &fakeProviderClient{
		result: provider.Result{UpstreamStatus: http.StatusGatewayTimeout, UpstreamRequestID: "req_timeout"},
		err:    &provider.Error{Category: provider.ProviderTimeout, StatusCode: http.StatusGatewayTimeout, Message: "raw upstream text"},
	}
	var logs bytes.Buffer
	service := newTestService(t, store, client, slog.New(slog.NewTextHandler(&logs, nil)))

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "secret prompt"}},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.UsagePersistenceFail {
		t.Fatalf("error = %#v", err)
	}
	logText := logs.String()
	for _, forbidden := range []string{rawKey, "sk-test", "secret prompt", "raw upstream text"} {
		if strings.Contains(logText, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, logText)
		}
	}
	for _, want := range []string{testRequestID, testProjectID, string(provider.OpenAI), string(provider.ProviderTimeout), "finalize write failed"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log missing %q: %s", want, logText)
		}
	}
}

func TestCanceledUpstreamStillUsesNonCanceledBoundedFinalizeContext(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		err: &provider.Error{Category: provider.ProviderTimeout, Message: "context canceled"},
	}
	client.observeContext = func(ctx context.Context) {
		<-ctx.Done()
	}
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = service.CompleteChat(ctx, auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.ProviderTimeout {
		t.Fatalf("error = %#v", err)
	}
	if !client.sawCanceledContext {
		t.Fatal("provider did not observe canceled upstream context")
	}
	if store.finalizeCalls != 1 || store.finalizeCtxErr != nil || !store.finalizeHadDeadline {
		t.Fatalf("finalize calls=%d ctx err=%v deadline=%v", store.finalizeCalls, store.finalizeCtxErr, store.finalizeHadDeadline)
	}
}

func TestSuccessfulProviderWithFinalizeFailureReturnsPersistenceError(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	store.finalizeErr = errors.New("write failed")
	client := &fakeProviderClient{result: provider.Result{
		Response:       provider.ChatResponse{ID: "chatcmpl_test", Object: "chat.completion", Created: 123, Model: "gpt-test"},
		UpstreamStatus: http.StatusOK,
	}}
	service := newTestService(t, store, client)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.UsagePersistenceFail {
		t.Fatalf("error = %#v", err)
	}
}

func TestStreamChatWritesChunksAndFinalizesUsageAndTTFT(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	stream := &fakeChatStream{
		events: []provider.StreamEvent{
			{Data: []byte(`{"id":"chunk_1","choices":[{"delta":{"content":"hello"}}]}`)},
			{Data: []byte(`{"id":"chunk_2","choices":[{"delta":{"content":" world"}}]}`)},
			{Data: []byte("[DONE]"), Done: true},
		},
		usage: &provider.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	}
	client := &fakeProviderClient{streamResult: provider.StreamResult{
		Stream:            stream,
		UpstreamStatus:    http.StatusOK,
		UpstreamRequestID: "req_stream",
	}}
	service := newTestService(t, store, client)
	sink := &recordingStreamSink{}

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	record, err := service.StreamChat(context.Background(), auth, "trace-stream", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
	}, sink)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	if record.ID != testRequestID || client.streamCalls != 1 || client.lastChat.Model != "gpt-test" || !client.lastChat.Stream {
		t.Fatalf("record=%+v calls=%d chat=%+v", record, client.streamCalls, client.lastChat)
	}
	if !stream.closed {
		t.Fatal("stream was not closed")
	}
	if store.lastCreate.TraceID != "trace-stream" || !store.lastCreate.IsStream {
		t.Fatalf("create params = %+v", store.lastCreate)
	}
	if store.lastFinalize.Status != "succeeded" || store.lastFinalize.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
	if store.lastFinalize.FirstChunkAt == nil || store.lastFinalize.TTFTMS == nil || *store.lastFinalize.TTFTMS < 0 {
		t.Fatalf("stream timing not persisted: %+v", store.lastFinalize)
	}
	if store.lastFinalize.UpstreamHTTPStatus == nil || *store.lastFinalize.UpstreamHTTPStatus != http.StatusOK {
		t.Fatalf("upstream status not persisted: %+v", store.lastFinalize)
	}
	if store.lastFinalize.UpstreamRequestID == nil || *store.lastFinalize.UpstreamRequestID != "req_stream" {
		t.Fatalf("upstream request ID not persisted: %+v", store.lastFinalize)
	}
	if store.lastFinalize.TotalTokens == nil || *store.lastFinalize.TotalTokens != 10 {
		t.Fatalf("usage not persisted: %+v", store.lastFinalize)
	}
	if len(sink.events) != 3 || !sink.events[2].Done {
		t.Fatalf("sink events = %+v", sink.events)
	}
}

func TestHandlerUsesIngressTimestampForStreamTTFT(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{streamResult: provider.StreamResult{
		Stream: &fakeChatStream{events: []provider.StreamEvent{
			{Data: []byte(`{"id":"chunk_1","choices":[{"delta":{"content":"hello"}}]}`)},
			{Data: []byte("[DONE]"), Done: true},
		}},
		UpstreamStatus: http.StatusOK,
	}}
	service := newTestService(t, store, client)
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
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.authStartedAt.IsZero() || store.resolveStartedAt.IsZero() {
		t.Fatalf("missing auth/resolve timestamps: auth=%v resolve=%v", store.authStartedAt, store.resolveStartedAt)
	}
	if store.lastCreate.StartedAt.After(store.authStartedAt) {
		t.Fatalf("started_at = %v, want no later than auth start %v", store.lastCreate.StartedAt, store.authStartedAt)
	}
	if store.lastCreate.StartedAt.After(store.resolveStartedAt) {
		t.Fatalf("started_at = %v, want no later than provider config lookup start %v", store.lastCreate.StartedAt, store.resolveStartedAt)
	}
	if store.lastFinalize.FirstChunkAt == nil || store.lastFinalize.TTFTMS == nil {
		t.Fatalf("stream timing not finalized: %+v", store.lastFinalize)
	}
	wantTTFT := store.lastFinalize.FirstChunkAt.Sub(store.lastCreate.StartedAt).Milliseconds()
	if *store.lastFinalize.TTFTMS != wantTTFT {
		t.Fatalf("TTFTMS = %d, want %d from first_chunk_at-started_at", *store.lastFinalize.TTFTMS, wantTTFT)
	}
}

func TestHandlerUsesIngressTimestampForNonStreamingLatency(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{
			Response: provider.ChatResponse{
				ID:      "chatcmpl_test",
				Object:  "chat.completion",
				Created: 123,
				Model:   "gpt-test",
				Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}}},
			},
			UpstreamStatus: http.StatusOK,
		},
	}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.lastCreate.StartedAt.After(store.authStartedAt) {
		t.Fatalf("started_at = %v, want no later than auth start %v", store.lastCreate.StartedAt, store.authStartedAt)
	}
	if store.lastCreate.StartedAt.After(store.resolveStartedAt) {
		t.Fatalf("started_at = %v, want no later than provider config lookup start %v", store.lastCreate.StartedAt, store.resolveStartedAt)
	}
}

func TestStreamChatProviderErrorBeforeCommitReturnsJSONCapableError(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		streamResult: provider.StreamResult{UpstreamStatus: http.StatusBadGateway, UpstreamRequestID: "req_bad"},
		streamErr:    &provider.Error{Category: provider.ProviderUnavailable, Message: "raw upstream text"},
	}
	service := newTestService(t, store, client)
	sink := &recordingStreamSink{}

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	record, err := service.StreamChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
	}, sink)
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.ProviderUnavailable {
		t.Fatalf("error = %#v", err)
	}
	if record.ID != testRequestID || sink.Committed() || len(sink.events) != 0 {
		t.Fatalf("record=%+v committed=%v events=%+v", record, sink.Committed(), sink.events)
	}
	if store.lastFinalize.Status != "failed" || store.lastFinalize.ErrorCategory == nil || *store.lastFinalize.ErrorCategory != provider.ProviderUnavailable {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
	if store.lastFinalize.FirstChunkAt != nil || store.lastFinalize.TotalTokens != nil {
		t.Fatalf("stream failure should not persist timing/usage: %+v", store.lastFinalize)
	}
}

func TestStreamChatInterruptionAfterCommitFinalizesFailedWithoutUsage(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	stream := &fakeChatStream{
		events: []provider.StreamEvent{{Data: []byte(`{"id":"chunk_1"}`)}},
		errs:   []error{&provider.Error{Category: provider.StreamInterrupted, Message: "raw upstream body"}},
		usage:  &provider.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	}
	client := &fakeProviderClient{streamResult: provider.StreamResult{Stream: stream, UpstreamStatus: http.StatusOK}}
	service := newTestService(t, store, client)
	sink := &recordingStreamSink{}

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, err = service.StreamChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "secret prompt"}},
		Stream:   true,
	}, sink)
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.StreamInterrupted {
		t.Fatalf("error = %#v", err)
	}
	if !sink.Committed() || len(sink.events) != 1 {
		t.Fatalf("committed=%v events=%+v", sink.Committed(), sink.events)
	}
	if store.lastFinalize.Status != "failed" || store.lastFinalize.ErrorCategory == nil || *store.lastFinalize.ErrorCategory != provider.StreamInterrupted {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
	if store.lastFinalize.TotalTokens != nil {
		t.Fatalf("interrupted stream persisted usage: %+v", store.lastFinalize)
	}
}

func TestStreamChatDoesNotReadAheadWhenSinkBlocks(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	nextCalls := make(chan int, 3)
	stream := &fakeChatStream{
		events: []provider.StreamEvent{
			{Data: []byte(`{"id":"chunk_1"}`)},
			{Data: []byte(`{"id":"chunk_2"}`)},
			{Data: []byte("[DONE]"), Done: true},
		},
		onNext: func(call int) {
			nextCalls <- call
		},
	}
	client := &fakeProviderClient{streamResult: provider.StreamResult{Stream: stream, UpstreamStatus: http.StatusOK}}
	service := newTestService(t, store, client)
	sink := &blockingStreamSink{started: make(chan struct{}), release: make(chan struct{})}

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.StreamChat(context.Background(), auth, "", provider.ChatRequest{
			Model:    "openai/gpt-test",
			Messages: []provider.Message{{Role: "user", Content: "hello"}},
			Stream:   true,
		}, sink)
		done <- err
	}()

	if call := <-nextCalls; call != 1 {
		t.Fatalf("first next call = %d", call)
	}
	<-sink.started
	select {
	case call := <-nextCalls:
		t.Fatalf("stream read ahead while sink was blocked; next call = %d", call)
	case <-time.After(50 * time.Millisecond):
	}
	close(sink.release)
	if err := <-done; err != nil {
		t.Fatalf("stream chat: %v", err)
	}
}

func TestStreamChatCancellationClosesUpstreamStreamAndFinalizes(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	stream := &contextStream{}
	client := &fakeProviderClient{
		streamFactory: func(ctx context.Context) (provider.StreamResult, error) {
			stream.ctx = ctx
			return provider.StreamResult{Stream: stream, UpstreamStatus: http.StatusOK}, nil
		},
	}
	service := newTestService(t, store, client)
	sink := &recordingStreamSink{}

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := service.StreamChat(ctx, auth, "", provider.ChatRequest{
			Model:    "openai/gpt-test",
			Messages: []provider.Message{{Role: "user", Content: "hello"}},
			Stream:   true,
		}, sink)
		done <- err
	}()
	cancel()

	err = <-done
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.StreamInterrupted {
		t.Fatalf("error = %#v", err)
	}
	if !stream.closed {
		t.Fatal("stream was not closed")
	}
	if store.finalizeCalls != 1 || store.finalizeCtxErr != nil || !store.finalizeHadDeadline {
		t.Fatalf("finalize calls=%d ctx err=%v deadline=%v", store.finalizeCalls, store.finalizeCtxErr, store.finalizeHadDeadline)
	}
}

func TestStreamChatClientCancelBeforeUpstreamHeadersFinalizesInterrupted(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	upstreamCtxSeen := make(chan context.Context, 1)
	client := &fakeProviderClient{
		streamFactory: func(ctx context.Context) (provider.StreamResult, error) {
			upstreamCtxSeen <- ctx
			<-ctx.Done()
			return provider.StreamResult{UpstreamStatus: http.StatusBadGateway}, &provider.Error{Category: provider.ProviderUnavailable, Message: "raw canceled provider request"}
		},
	}
	service := newServiceForStore(t, store, client, nil)
	sink := &recordingStreamSink{}

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := service.StreamChat(ctx, auth, "", provider.ChatRequest{
			Model:    "openai/gpt-test",
			Messages: []provider.Message{{Role: "user", Content: "hello"}},
			Stream:   true,
		}, sink)
		done <- err
	}()

	var upstreamCtx context.Context
	select {
	case upstreamCtx = <-upstreamCtxSeen:
	case <-time.After(time.Second):
		t.Fatal("provider did not receive upstream context")
	}
	cancel()
	select {
	case <-upstreamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("upstream request context was not canceled")
	}

	err = <-done
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.StreamInterrupted || strings.Contains(gatewayErr.Message, "raw canceled") {
		t.Fatalf("error = %#v", err)
	}
	if store.lastFinalize.Status != "failed" || store.lastFinalize.ErrorCategory == nil || *store.lastFinalize.ErrorCategory != provider.StreamInterrupted {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
	if sink.Committed() {
		t.Fatal("sink committed before upstream stream opened")
	}
	if store.lastFinalize.TotalTokens != nil || store.lastFinalize.FirstChunkAt != nil {
		t.Fatalf("pre-headers cancel persisted stream timing/usage: %+v", store.lastFinalize)
	}
}

func TestStreamChatUpstreamDeadlineBeforeHeadersRemainsProviderTimeout(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		streamFactory: func(ctx context.Context) (provider.StreamResult, error) {
			<-ctx.Done()
			return provider.StreamResult{UpstreamStatus: http.StatusGatewayTimeout}, &provider.Error{Category: provider.ProviderTimeout, Message: "raw timeout"}
		},
	}
	service := newServiceForStoreWithTimeouts(t, store, client, nil, time.Nanosecond, time.Nanosecond)
	sink := &recordingStreamSink{}

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, err = service.StreamChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
	}, sink)

	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) || gatewayErr.Category != provider.ProviderTimeout {
		t.Fatalf("error = %#v", err)
	}
	if store.lastFinalize.Status != "failed" || store.lastFinalize.ErrorCategory == nil || *store.lastFinalize.ErrorCategory != provider.ProviderTimeout {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
}

func TestHandlerRejectsUnknownFieldBeforeUpstream(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}],
		"temperature":0
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if store.createCalls != 0 || client.calls != 0 {
		t.Fatalf("create calls=%d client calls=%d", store.createCalls, client.calls)
	}
	if !strings.Contains(response.Body.String(), `"code":"unsupported_parameter"`) || !strings.Contains(response.Body.String(), `"type":"invalid_request"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestHandlerPassesStreamToServiceAndRejectsBeforeUpstream(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &completeOnlyClient{}
	service := newTestService(t, store, client)
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

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if store.createCalls != 0 || client.calls != 0 {
		t.Fatalf("create calls=%d client calls=%d", store.createCalls, client.calls)
	}
	if !strings.Contains(response.Body.String(), `"code":"unsupported_feature"`) || !strings.Contains(response.Body.String(), `"type":"unsupported_feature"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestHandlerStreamsSSEHeadersFramingAndDone(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{streamResult: provider.StreamResult{
		Stream: &fakeChatStream{events: []provider.StreamEvent{
			{Data: []byte(`{"id":"chunk_1","choices":[{"delta":{"content":"hello"}}]}`)},
			{Event: "done", Data: []byte("[DONE]"), Done: true},
		}},
		UpstreamStatus: http.StatusOK,
	}}
	service := newTestService(t, store, client)
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
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-cache, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header().Get("X-Gateway-Request-ID"); got != testRequestID {
		t.Fatalf("request header = %q", got)
	}
	body := response.Body.String()
	for _, want := range []string{`data: {"id":"chunk_1"`, "\n\nevent: done\ndata: [DONE]\n\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

func TestHandlerDoesNotWriteJSONErrorAfterCommittedStreamFailure(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{streamResult: provider.StreamResult{
		Stream: &fakeChatStream{
			events: []provider.StreamEvent{{Data: []byte(`{"id":"chunk_1"}`)}},
			errs:   []error{&provider.Error{Category: provider.StreamInterrupted, Message: "raw provider failure"}},
		},
		UpstreamStatus: http.StatusOK,
	}}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"secret prompt"}],
		"stream":true
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `data: {"id":"chunk_1"}`) {
		t.Fatalf("body missing first chunk: %s", body)
	}
	if strings.Contains(body, `"error"`) || strings.Contains(body, "secret prompt") || strings.Contains(body, "raw provider failure") {
		t.Fatalf("post-commit failure leaked JSON/raw content: %s", body)
	}
}

func TestHandlerStreamsThroughOpenAIClientOverHTTP(t *testing.T) {
	firstChunkFlushed := make(chan struct{})
	releaseRest := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("upstream Accept = %q", got)
		}
		var body struct {
			Model         string `json:"model"`
			Stream        bool   `json:"stream"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body.Model != "gpt-test" || !body.Stream || !body.StreamOptions.IncludeUsage {
			t.Fatalf("upstream body = %+v", body)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("X-Request-ID", "req_http_stream")

		_, _ = io.WriteString(response, `data: {"id":"chunk_1","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant"}}]}`+"\n\n")
		response.(http.Flusher).Flush()
		close(firstChunkFlushed)

		<-releaseRest
		_, _ = io.WriteString(response, `data: {"id":"chunk_2","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"hello"}}]}`+"\n\n")
		_, _ = io.WriteString(response, `data: {"id":"chunk_usage","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`+"\n\n")
		_, _ = io.WriteString(response, "data: [DONE]\n\n")
		response.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	baseStore, rawKey := newAuthorizedStore(t)
	baseStore.credential.BaseURLOverride = upstream.URL
	store := newNotifyingStore(baseStore)
	service := newServiceForStore(t, store, openai.New(upstream.Client()), nil)
	handler := NewHandler(service)
	mux := http.NewServeMux()
	handler.Register(mux)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	reader := bufio.NewReader(response.Body)
	firstEvent := readSSEEvent(t, reader)
	if !strings.Contains(firstEvent, `data: {"id":"chunk_1"`) {
		t.Fatalf("first event = %q", firstEvent)
	}
	select {
	case <-firstChunkFlushed:
	case <-time.After(time.Second):
		t.Fatal("upstream did not flush first chunk")
	}

	close(releaseRest)
	events := firstEvent
	for !strings.Contains(events, "data: [DONE]\n\n") {
		events += readSSEEvent(t, reader)
	}
	if !strings.Contains(events, `"usage":{"prompt_tokens":7`) {
		t.Fatalf("stream did not include usage chunk: %s", events)
	}
	finalize := store.waitFinalize(t)
	if finalize.params.Status != "succeeded" || finalize.params.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", finalize.params)
	}
	if finalize.params.TTFTMS == nil || finalize.params.FirstChunkAt == nil {
		t.Fatalf("missing stream timing: %+v", finalize.params)
	}
	if finalize.params.TotalTokens == nil || *finalize.params.TotalTokens != 10 {
		t.Fatalf("usage not finalized: %+v", finalize.params)
	}
	if finalize.params.UpstreamRequestID == nil || *finalize.params.UpstreamRequestID != "req_http_stream" {
		t.Fatalf("upstream request ID not finalized: %+v", finalize.params)
	}
}

func TestHandlerMalformedFinalUsageSequenceFinalizesInterruptedWithoutUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, `data: {"id":"chunk_1","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"hello"}}],"usage":null}`+"\n\n")
		_, _ = io.WriteString(response, `data: {"id":"chunk_usage","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`+"\n\n")
		_, _ = io.WriteString(response, `data: {"id":"chunk_after_usage","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"late"}}],"usage":null}`+"\n\n")
		_, _ = io.WriteString(response, "data: [DONE]\n\n")
		response.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	baseStore, rawKey := newAuthorizedStore(t)
	baseStore.credential.BaseURLOverride = upstream.URL
	store := newNotifyingStore(baseStore)
	service := newServiceForStore(t, store, openai.New(upstream.Client()), nil)
	handler := NewHandler(service)
	mux := http.NewServeMux()
	handler.Register(mux)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), `"chunk_usage"`) {
		t.Fatalf("committed body missing usage chunk: %s", body)
	}
	if strings.Contains(string(body), `"chunk_after_usage"`) || strings.Contains(string(body), `"error"`) {
		t.Fatalf("malformed usage sequence leaked forbidden body content: %s", body)
	}
	finalize := store.waitFinalize(t)
	if finalize.params.Status != "failed" || finalize.params.ErrorCategory == nil || *finalize.params.ErrorCategory != provider.StreamInterrupted {
		t.Fatalf("finalize = %+v", finalize.params)
	}
	if finalize.params.TotalTokens != nil {
		t.Fatalf("malformed interrupted stream persisted usage: %+v", finalize.params)
	}
}

func TestHandlerClientDisconnectCancelsOpenAIUpstreamAndFinalizes(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("X-Request-ID", "req_cancel")
		_, _ = io.WriteString(response, `data: {"id":"chunk_1","object":"chat.completion.chunk","created":123,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"hello"}}]}`+"\n\n")
		response.(http.Flusher).Flush()
		<-request.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	baseStore, rawKey := newAuthorizedStore(t)
	baseStore.credential.BaseURLOverride = upstream.URL
	store := newNotifyingStore(baseStore)
	service := newServiceForStore(t, store, openai.New(upstream.Client()), nil)
	handler := NewHandler(service)
	mux := http.NewServeMux()
	handler.Register(mux)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	reader := bufio.NewReader(response.Body)
	firstEvent := readSSEEvent(t, reader)
	if !strings.Contains(firstEvent, `data: {"id":"chunk_1"`) {
		t.Fatalf("first event = %q", firstEvent)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}

	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request context was not canceled")
	}
	finalize := store.waitFinalize(t)
	if finalize.params.Status != "failed" || finalize.params.ErrorCategory == nil || *finalize.params.ErrorCategory != provider.StreamInterrupted {
		t.Fatalf("finalize = %+v", finalize.params)
	}
	if finalize.ctxErr != nil || !finalize.hadDeadline {
		t.Fatalf("finalize context err=%v deadline=%v", finalize.ctxErr, finalize.hadDeadline)
	}
	if finalize.params.TotalTokens != nil {
		t.Fatalf("canceled stream persisted usage: %+v", finalize.params)
	}
}

func TestHandlerNoFlushSupportInterruptsCommittedStreamWithoutJSON(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{streamResult: provider.StreamResult{
		Stream: &fakeChatStream{events: []provider.StreamEvent{
			{Data: []byte(`{"id":"chunk_1"}`)},
			{Data: []byte("[DONE]"), Done: true},
		}},
		UpstreamStatus: http.StatusOK,
	}}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := &noFlushResponseWriter{header: make(http.Header)}

	handler.chatCompletions(response, request)

	if response.status != http.StatusOK {
		t.Fatalf("status = %d, want committed OK; body=%s", response.status, response.body.String())
	}
	body := response.body.String()
	if !strings.Contains(body, `data: {"id":"chunk_1"}`) {
		t.Fatalf("body missing committed chunk: %s", body)
	}
	if strings.Contains(body, `"error"`) || strings.Contains(body, "stream interrupted") {
		t.Fatalf("no-flush failure appended JSON/error text: %s", body)
	}
	if store.lastFinalize.Status != "failed" || store.lastFinalize.ErrorCategory == nil || *store.lastFinalize.ErrorCategory != provider.StreamInterrupted {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
}

func TestHandlerFinalizeFailureAfterCommittedStreamLogsSafeMetadataOnly(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	store.finalizeErr = errors.New("postgres finalize failed")
	client := &fakeProviderClient{streamResult: provider.StreamResult{
		Stream: &fakeChatStream{events: []provider.StreamEvent{
			{Data: []byte(`{"id":"chunk_1"}`)},
			{Data: []byte("[DONE]"), Done: true},
		}},
		UpstreamStatus:    http.StatusOK,
		UpstreamRequestID: "req_log",
	}}
	var logs bytes.Buffer
	service := newTestService(t, store, client, slog.New(slog.NewTextHandler(&logs, nil)))
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"secret prompt"}],
		"stream":true
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `data: {"id":"chunk_1"}`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body missing committed stream output: %s", body)
	}
	if strings.Contains(body, `"error"`) || strings.Contains(body, "usage persistence failed") || strings.Contains(body, "postgres finalize failed") {
		t.Fatalf("finalize failure leaked into stream body: %s", body)
	}
	logText := logs.String()
	for _, want := range []string{"gateway request finalization failed", testRequestID, testProjectID, string(provider.OpenAI), "postgres finalize failed"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log missing %q: %s", want, logText)
		}
	}
	if strings.Contains(logText, "secret prompt") || strings.Contains(logText, "sk-test") {
		t.Fatalf("log leaked sensitive data: %s", logText)
	}
}

func TestHandlerRequiresJSONContentTypeBeforeStoreOrUpstream(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnsupportedMediaType, response.Body.String())
	}
	if store.authCalls != 0 || store.createCalls != 0 || client.calls != 0 {
		t.Fatalf("auth calls=%d create calls=%d client calls=%d", store.authCalls, store.createCalls, client.calls)
	}
}

func TestHandlerAllowsJSONContentTypeParameters(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{
			Response: provider.ChatResponse{
				ID:      "chatcmpl_test",
				Object:  "chat.completion",
				Created: 123,
				Model:   "gpt-test",
				Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}}},
			},
			UpstreamStatus: http.StatusOK,
		},
	}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
}

func TestHandlerSuccessWritesStableHeadersAndEnvelope(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{
			Response: provider.ChatResponse{
				ID:      "chatcmpl_test",
				Object:  "chat.completion",
				Created: 123,
				Model:   "gpt-test",
				Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}}},
			},
			UpstreamStatus: http.StatusOK,
		},
	}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	rawTraceparent := "00-raw-traceparent-raw-traceparent-01"
	request.Header.Set("Traceparent", rawTraceparent)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := response.Header().Get("X-Gateway-Request-ID"); got != testRequestID {
		t.Fatalf("request header = %q", got)
	}
	if got := response.Header().Get("X-Gateway-Provider"); got != string(provider.OpenAI) {
		t.Fatalf("provider header = %q", got)
	}
	if got := response.Header().Get("X-Gateway-Retry-Count"); got != "0" {
		t.Fatalf("retry header = %q", got)
	}
	if strings.Contains(response.Body.String(), "raw-traceparent") {
		t.Fatalf("traceparent leaked body=%s trace=%q", response.Body.String(), store.lastCreate.TraceID)
	}
	if !uuidV4Pattern.MatchString(store.lastCreate.TraceID) {
		t.Fatalf("trace ID = %q, want UUID v4", store.lastCreate.TraceID)
	}
	if store.lastCreate.TraceID == rawTraceparent {
		t.Fatalf("trace ID = raw traceparent %q", store.lastCreate.TraceID)
	}
}

func TestHandlerUsesTrimmedXRequestIDAsTraceID(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{
			Response:       provider.ChatResponse{ID: "chatcmpl_test", Object: "chat.completion", Created: 123, Model: "gpt-test", Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}}}},
			UpstreamStatus: http.StatusOK,
		},
	}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	request.Header.Set("X-Request-ID", " trace-http ")
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.lastCreate.TraceID != "trace-http" {
		t.Fatalf("trace ID = %q, want trace-http", store.lastCreate.TraceID)
	}
}

func TestHandlerGeneratesTraceIDWhenXRequestIDMissing(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{
			Response:       provider.ChatResponse{ID: "chatcmpl_test", Object: "chat.completion", Created: 123, Model: "gpt-test", Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}}}},
			UpstreamStatus: http.StatusOK,
		},
	}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if !uuidV4Pattern.MatchString(store.lastCreate.TraceID) {
		t.Fatalf("trace ID = %q, want UUID v4", store.lastCreate.TraceID)
	}
}

func TestHandlerErrorWritesStableGatewayEnvelope(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{UpstreamStatus: http.StatusGatewayTimeout},
		err:    &provider.Error{Category: provider.ProviderTimeout, Message: "upstream raw timeout"},
	}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusGatewayTimeout, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"type":"provider_timeout"`, `"code":"provider_timeout"`, `"message":"provider request timed out"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "upstream raw timeout") {
		t.Fatalf("raw upstream message leaked: %s", body)
	}
}

func TestHandlerMapsProviderInvalidRequestToBadRequest(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{
		result: provider.Result{UpstreamStatus: http.StatusBadRequest},
		err:    &provider.Error{Category: provider.ProviderInvalidReq, Message: "raw provider message"},
	}
	service := newTestService(t, store, client)
	handler := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{
		"model":"openai/gpt-test",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response := httptest.NewRecorder()

	handler.chatCompletions(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"type":"provider_invalid_request"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func newTestService(t *testing.T, store *fakeStore, clientsAndLogger ...any) *Service {
	t.Helper()
	client := provider.Client(&fakeProviderClient{})
	var logger *slog.Logger
	for _, current := range clientsAndLogger {
		switch typed := current.(type) {
		case provider.Client:
			client = typed
		case *slog.Logger:
			logger = typed
		default:
			t.Fatalf("unsupported test dependency %T", current)
		}
	}
	return newServiceForStore(t, store, client, logger)
}

func newServiceForStore(t *testing.T, store Store, client provider.Client, logger *slog.Logger) *Service {
	return newServiceForStoreWithTimeouts(t, store, client, logger, time.Second, time.Second)
}

func newServiceForStoreWithTimeouts(t *testing.T, store Store, client provider.Client, logger *slog.Logger, upstreamTimeout, streamMaxDuration time.Duration) *Service {
	t.Helper()
	return newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{provider.OpenAI: client}, logger, upstreamTimeout, streamMaxDuration)
}

func newServiceForStoreWithClientsAndTimeouts(t *testing.T, store Store, clients map[provider.Name]provider.Client, logger *slog.Logger, upstreamTimeout, streamMaxDuration time.Duration) *Service {
	t.Helper()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	service, err := NewService(Options{
		Store:                     store,
		VirtualKeyPepper:          bytes.Repeat([]byte{9}, 32),
		CredentialCipher:          cipher,
		UpstreamRequestTimeout:    upstreamTimeout,
		UpstreamStreamMaxDuration: streamMaxDuration,
		ProviderRegistry:          newTestProviderRegistry(t, clients),
		Logger:                    logger,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

func newTestProviderRegistry(t *testing.T, clients map[provider.Name]provider.Client) *provider.Registry {
	t.Helper()
	registry, err := provider.NewRegistry(clients)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return registry
}

func newAuthorizedStore(t *testing.T) (*fakeStore, string) {
	t.Helper()
	return newAuthorizedStoreForProvider(t, provider.OpenAI, testCredentialID)
}

func newAuthorizedStoreForProvider(t *testing.T, name provider.Name, credentialID string) (*fakeStore, string) {
	t.Helper()
	rawKey, prefix, keyHash := authorizedKey(t)
	return &fakeStore{
		prefix:     prefix,
		keyHash:    keyHash,
		auth:       AuthContext{ProjectID: testProjectID, VirtualKeyID: testVirtualKeyID, KeyPrefix: prefix},
		credential: encryptTestCredential(t, name, credentialID),
	}, rawKey
}

func newMultiProviderAuthorizedStore(t *testing.T, names ...provider.Name) (*fakeStore, string) {
	t.Helper()
	rawKey, prefix, keyHash := authorizedKey(t)
	credentials := make(map[provider.Name]ProviderCredential, len(names))
	for _, name := range names {
		credentials[name] = encryptTestCredential(t, name, testCredentialID)
	}
	return &fakeStore{
		prefix:      prefix,
		keyHash:     keyHash,
		auth:        AuthContext{ProjectID: testProjectID, VirtualKeyID: testVirtualKeyID, KeyPrefix: prefix},
		credentials: credentials,
	}, rawKey
}

func authorizedKey(t *testing.T) (string, string, []byte) {
	t.Helper()
	rawKey, prefix, err := apikey.GenerateRawKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyHash, err := apikey.HashKey(rawKey, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("hash key: %v", err)
	}
	return rawKey, prefix, keyHash
}

func encryptTestCredential(t *testing.T, name provider.Name, credentialID string) ProviderCredential {
	t.Helper()
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	encrypted, err := cipher.Encrypt([]byte("sk-test"), security.CredentialIdentity{
		CredentialID: credentialID,
		ProjectID:    testProjectID,
		Provider:     string(name),
	})
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}
	return ProviderCredential{
		ID:               credentialID,
		ProjectID:        testProjectID,
		Provider:         name,
		SecretCiphertext: encrypted.Ciphertext,
		SecretNonce:      encrypted.Nonce,
		KeyVersion:       encrypted.KeyVersion,
	}
}

type fakeStore struct {
	prefix              string
	keyHash             []byte
	auth                AuthContext
	credential          ProviderCredential
	credentials         map[provider.Name]ProviderCredential
	prices              map[priceKey]ModelPrice
	authCalls           int
	resolveCalls        int
	createCalls         int
	finalizeCalls       int
	createErr           error
	finalizeErr         error
	priceLookupErr      error
	priceLookupBlock    func(context.Context)
	onAuth              func()
	authStartedAt       time.Time
	resolveStartedAt    time.Time
	lastCreate          CreateRequestParams
	lastFinalize        FinalizeParams
	finalizeCtxErr      error
	finalizeHadDeadline bool
}

type priceKey struct {
	provider provider.Name
	model    string
}

type finalizeCall struct {
	params      FinalizeParams
	ctxErr      error
	hadDeadline bool
	err         error
}

type notifyingStore struct {
	base      *fakeStore
	finalized chan finalizeCall
}

func newNotifyingStore(base *fakeStore) *notifyingStore {
	return &notifyingStore{
		base:      base,
		finalized: make(chan finalizeCall, 1),
	}
}

func (s *notifyingStore) AuthenticateVirtualKey(ctx context.Context, prefix string, keyHash []byte) (AuthContext, error) {
	return s.base.AuthenticateVirtualKey(ctx, prefix, keyHash)
}

func (s *notifyingStore) ResolveProviderCredential(ctx context.Context, projectID string, name provider.Name) (ProviderCredential, error) {
	return s.base.ResolveProviderCredential(ctx, projectID, name)
}

func (s *notifyingStore) CreateGatewayRequest(ctx context.Context, params CreateRequestParams) (GatewayRequest, error) {
	return s.base.CreateGatewayRequest(ctx, params)
}

func (s *notifyingStore) FindModelPrice(ctx context.Context, name provider.Name, model string, at time.Time) (ModelPrice, error) {
	return s.base.FindModelPrice(ctx, name, model, at)
}

func (s *notifyingStore) FinalizeGatewayRequest(ctx context.Context, params FinalizeParams) error {
	err := s.base.FinalizeGatewayRequest(ctx, params)
	_, hadDeadline := ctx.Deadline()
	s.finalized <- finalizeCall{params: params, ctxErr: ctx.Err(), hadDeadline: hadDeadline, err: err}
	return err
}

func (s *notifyingStore) waitFinalize(t *testing.T) finalizeCall {
	t.Helper()
	select {
	case call := <-s.finalized:
		return call
	case <-time.After(time.Second):
		t.Fatal("gateway request was not finalized")
		return finalizeCall{}
	}
}

func (s *fakeStore) AuthenticateVirtualKey(_ context.Context, prefix string, keyHash []byte) (AuthContext, error) {
	s.authStartedAt = time.Now().UTC()
	s.authCalls++
	if s.onAuth != nil {
		s.onAuth()
	}
	if prefix != s.prefix || !bytes.Equal(keyHash, s.keyHash) {
		return AuthContext{}, ErrNotFound
	}
	return s.auth, nil
}

func (s *fakeStore) ResolveProviderCredential(_ context.Context, _ string, name provider.Name) (ProviderCredential, error) {
	s.resolveStartedAt = time.Now().UTC()
	s.resolveCalls++
	if s.credentials != nil {
		credential, ok := s.credentials[name]
		if !ok {
			return ProviderCredential{}, ErrNotFound
		}
		return credential, nil
	}
	if s.credential.ID == "" || s.credential.Provider != name {
		return ProviderCredential{}, ErrNotFound
	}
	return s.credential, nil
}

func (s *fakeStore) CreateGatewayRequest(_ context.Context, params CreateRequestParams) (GatewayRequest, error) {
	s.createCalls++
	s.lastCreate = params
	if s.createErr != nil {
		return GatewayRequest{}, s.createErr
	}
	return GatewayRequest{
		ID:                   testRequestID,
		ProjectID:            params.ProjectID,
		VirtualKeyID:         params.VirtualKeyID,
		ProviderCredentialID: params.ProviderCredentialID,
		Provider:             params.Provider,
		Model:                params.Model,
		IsStream:             params.IsStream,
		Status:               "in_progress",
		StartedAt:            params.StartedAt,
	}, nil
}

func (s *fakeStore) FindModelPrice(ctx context.Context, name provider.Name, model string, _ time.Time) (ModelPrice, error) {
	if s.priceLookupErr != nil {
		return ModelPrice{}, s.priceLookupErr
	}
	if s.priceLookupBlock != nil {
		s.priceLookupBlock(ctx)
	}
	if err := ctx.Err(); err != nil {
		return ModelPrice{}, err
	}
	price, ok := s.prices[priceKey{provider: name, model: model}]
	if !ok {
		return ModelPrice{}, ErrNotFound
	}
	return price, nil
}

func (s *fakeStore) FinalizeGatewayRequest(ctx context.Context, params FinalizeParams) error {
	s.finalizeCalls++
	s.lastFinalize = params
	s.finalizeCtxErr = ctx.Err()
	_, s.finalizeHadDeadline = ctx.Deadline()
	return s.finalizeErr
}

type fakeProviderClient struct {
	calls              int
	streamCalls        int
	lastChat           provider.ChatRequest
	lastCredential     provider.Credential
	result             provider.Result
	err                error
	streamResult       provider.StreamResult
	streamErr          error
	streamFactory      func(context.Context) (provider.StreamResult, error)
	observeContext     func(context.Context)
	sawCanceledContext bool
}

func (c *fakeProviderClient) CompleteChat(ctx context.Context, chat provider.ChatRequest, credential provider.Credential) (provider.Result, error) {
	c.calls++
	c.lastChat = chat
	c.lastCredential = credential
	c.lastCredential.APIKey = append([]byte(nil), credential.APIKey...)
	if c.observeContext != nil {
		c.observeContext(ctx)
		c.sawCanceledContext = ctx.Err() != nil
	}
	return c.result, c.err
}

func (c *fakeProviderClient) StreamChat(ctx context.Context, chat provider.ChatRequest, credential provider.Credential) (provider.StreamResult, error) {
	c.streamCalls++
	c.lastChat = chat
	c.lastCredential = credential
	c.lastCredential.APIKey = append([]byte(nil), credential.APIKey...)
	if c.streamFactory != nil {
		return c.streamFactory(ctx)
	}
	return c.streamResult, c.streamErr
}

type completeOnlyClient struct {
	calls int
}

func (c *completeOnlyClient) CompleteChat(context.Context, provider.ChatRequest, provider.Credential) (provider.Result, error) {
	c.calls++
	return provider.Result{}, nil
}

type fakeChatStream struct {
	events []provider.StreamEvent
	errs   []error
	usage  *provider.Usage
	onNext func(int)
	nexts  int
	closed bool
}

func (s *fakeChatStream) Next() (provider.StreamEvent, error) {
	s.nexts++
	if s.onNext != nil {
		s.onNext(s.nexts)
	}
	if s.nexts <= len(s.events) {
		return s.events[s.nexts-1], nil
	}
	errIndex := s.nexts - len(s.events) - 1
	if errIndex >= 0 && errIndex < len(s.errs) {
		return provider.StreamEvent{}, s.errs[errIndex]
	}
	return provider.StreamEvent{}, io.EOF
}

func (s *fakeChatStream) Close() error {
	s.closed = true
	return nil
}

func (s *fakeChatStream) Usage() *provider.Usage {
	return s.usage
}

type contextStream struct {
	ctx    context.Context
	closed bool
}

func (s *contextStream) Next() (provider.StreamEvent, error) {
	<-s.ctx.Done()
	return provider.StreamEvent{}, s.ctx.Err()
}

func (s *contextStream) Close() error {
	s.closed = true
	return nil
}

func (s *contextStream) Usage() *provider.Usage {
	return nil
}

type recordingStreamSink struct {
	prepared  bool
	committed bool
	events    []provider.StreamEvent
	err       error
}

func (s *recordingStreamSink) Prepare(GatewayRequest) error {
	s.prepared = true
	return nil
}

func (s *recordingStreamSink) WriteEvent(event provider.StreamEvent) error {
	s.committed = true
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, event)
	return nil
}

func (s *recordingStreamSink) Committed() bool {
	return s.committed
}

type blockingStreamSink struct {
	started   chan struct{}
	release   chan struct{}
	committed bool
	signaled  bool
}

func (s *blockingStreamSink) Prepare(GatewayRequest) error {
	return nil
}

func (s *blockingStreamSink) WriteEvent(provider.StreamEvent) error {
	s.committed = true
	if !s.signaled {
		s.signaled = true
		close(s.started)
		<-s.release
	}
	return nil
}

func (s *blockingStreamSink) Committed() bool {
	return s.committed
}

type noFlushResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *noFlushResponseWriter) Header() http.Header {
	return w.header
}

func (w *noFlushResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (w *noFlushResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var builder strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE event: %v", err)
		}
		builder.WriteString(line)
		if line == "\n" || line == "\r\n" {
			return builder.String()
		}
	}
}

func TestDeepSeekModelRequiresDeepSeekCredentialBeforeUpstream(t *testing.T) {
	store, rawKey := newAuthorizedStore(t) // openai credential only
	openAIClient := &fakeProviderClient{}
	deepSeekClient := &fakeProviderClient{}
	service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
		provider.OpenAI:   openAIClient,
		provider.DeepSeek: deepSeekClient,
	}, nil, time.Second, time.Second)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "deepseek/deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	gatewayErr, ok := err.(*GatewayError)
	if !ok || gatewayErr.Category != provider.ProviderNotConfigured {
		t.Fatalf("error = %#v, want ProviderNotConfigured", err)
	}
	if store.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0 (no paid upstream work without credential)", store.createCalls)
	}
	if deepSeekClient.calls != 0 || openAIClient.calls != 0 {
		t.Fatalf("provider calls: deepseek=%d openai=%d, want 0", deepSeekClient.calls, openAIClient.calls)
	}
}

func TestCrossProviderServiceRoutesDeepSeekToDeepSeekAdapter(t *testing.T) {
	store, rawKey := newMultiProviderAuthorizedStore(t, provider.OpenAI, provider.DeepSeek)
	openAIClient := &fakeProviderClient{
		result: provider.Result{
			Response: provider.ChatResponse{ID: "openai-result", Object: "chat.completion", Model: "gpt-test"},
			Usage:    &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	}
	deepSeekClient := &fakeProviderClient{
		result: provider.Result{
			Response: provider.ChatResponse{
				ID:      "deepseek-result",
				Object:  "chat.completion",
				Model:   "deepseek-chat",
				Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}}},
			},
			Usage:             &provider.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
			UpstreamStatus:    http.StatusOK,
			UpstreamRequestID: "req_deepseek",
		},
	}
	service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
		provider.OpenAI:   openAIClient,
		provider.DeepSeek: deepSeekClient,
	}, nil, time.Second, time.Second)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	result, record, err := service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "deepseek/deepseek-chat",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
	if record.Provider != provider.DeepSeek || record.Model != "deepseek-chat" || record.ID != testRequestID {
		t.Fatalf("record = %+v", record)
	}
	if result.Response.ID != "deepseek-result" {
		t.Fatalf("result = %+v", result)
	}
	if openAIClient.calls != 0 {
		t.Fatalf("openai provider calls = %d, want 0", openAIClient.calls)
	}
	if deepSeekClient.calls != 1 {
		t.Fatalf("deepseek provider calls = %d, want 1", deepSeekClient.calls)
	}
	if deepSeekClient.lastChat.Model != "deepseek-chat" {
		t.Fatalf("deepseek upstream model = %q", deepSeekClient.lastChat.Model)
	}
	if store.lastCreate.Provider != provider.DeepSeek || store.lastCreate.Model != "deepseek-chat" {
		t.Fatalf("create params = %+v", store.lastCreate)
	}
	if store.lastFinalize.Status != "succeeded" || store.lastFinalize.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
	if store.lastFinalize.TotalTokens == nil || *store.lastFinalize.TotalTokens != 10 {
		t.Fatalf("finalize usage = %+v", store.lastFinalize)
	}
}

func TestHandlerStreamsThroughDeepSeekClientOverHTTP(t *testing.T) {
	firstChunkFlushed := make(chan struct{})
	releaseRest := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("upstream Accept = %q", got)
		}
		var body struct {
			Model         string          `json:"model"`
			Stream        bool            `json:"stream"`
			StreamOptions json.RawMessage `json:"stream_options"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body.Model != "deepseek-chat" || !body.Stream {
			t.Fatalf("upstream body = %+v", body)
		}
		if len(body.StreamOptions) > 0 {
			t.Fatalf("stream_options present = %q, want absent", string(body.StreamOptions))
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("X-Request-ID", "req_deepseek_stream")

		_, _ = io.WriteString(response, `data: {"id":"ddchat_1","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\n\n")
		response.(http.Flusher).Flush()
		close(firstChunkFlushed)

		<-releaseRest
		_, _ = io.WriteString(response, `data: {"id":"ddchat_1","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`+"\n\n")
		_, _ = io.WriteString(response, `data: {"id":"ddchat_1","object":"chat.completion.chunk","created":123,"model":"deepseek-chat","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`+"\n\n")
		_, _ = io.WriteString(response, "data: [DONE]\n\n")
		response.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	baseStore, rawKey := newAuthorizedStoreForProvider(t, provider.DeepSeek, testCredentialID)
	baseStore.credential.BaseURLOverride = upstream.URL
	store := newNotifyingStore(baseStore)
	service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
		provider.DeepSeek: deepseek.New(upstream.Client()),
	}, nil, time.Second, time.Second)
	handler := NewHandler(service)
	mux := http.NewServeMux()
	handler.Register(mux)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
		"model":"deepseek/deepseek-chat",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header.Get("X-Gateway-Provider"); got != "deepseek" {
		t.Fatalf("X-Gateway-Provider = %q", got)
	}
	reader := bufio.NewReader(response.Body)
	firstEvent := readSSEEvent(t, reader)
	if !strings.Contains(firstEvent, `data: {"id":"ddchat_1"`) {
		t.Fatalf("first event = %q", firstEvent)
	}
	select {
	case <-firstChunkFlushed:
	case <-time.After(time.Second):
		t.Fatal("upstream did not flush first chunk")
	}

	close(releaseRest)
	events := firstEvent
	for !strings.Contains(events, "data: [DONE]\n\n") {
		events += readSSEEvent(t, reader)
	}
	if !strings.Contains(events, `"finish_reason":"stop"`) || !strings.Contains(events, `"usage":{"prompt_tokens":7`) {
		t.Fatalf("stream did not include DeepSeek final usage chunk: %s", events)
	}
	finalize := store.waitFinalize(t)
	if finalize.params.Status != "succeeded" || finalize.params.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", finalize.params)
	}
	if finalize.params.TotalTokens == nil || *finalize.params.TotalTokens != 10 {
		t.Fatalf("usage not finalized: %+v", finalize.params)
	}
	if finalize.params.UpstreamRequestID == nil || *finalize.params.UpstreamRequestID != "req_deepseek_stream" {
		t.Fatalf("upstream request ID not finalized: %+v", finalize.params)
	}
}

func TestHandlerCompleteChatThroughDeepSeekClientOverHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("upstream Accept = %q", got)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"id":"ddchat_1",
			"object":"chat.completion",
			"created":123,
			"model":"deepseek-chat",
			"choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}
		}`))
	}))
	defer upstream.Close()

	baseStore, rawKey := newAuthorizedStoreForProvider(t, provider.DeepSeek, testCredentialID)
	baseStore.credential.BaseURLOverride = upstream.URL
	store := newNotifyingStore(baseStore)
	service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
		provider.DeepSeek: deepseek.New(upstream.Client()),
	}, nil, time.Second, time.Second)
	handler := NewHandler(service)
	mux := http.NewServeMux()
	handler.Register(mux)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
		"model":"deepseek/deepseek-chat",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if got := response.Header.Get("X-Gateway-Provider"); got != "deepseek" {
		t.Fatalf("X-Gateway-Provider = %q", got)
	}
	var decoded struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode gateway response: %v", err)
	}
	if decoded.ID != "ddchat_1" || len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content != "world" {
		t.Fatalf("gateway response = %+v", decoded)
	}
	finalize := store.waitFinalize(t)
	if finalize.params.Status != "succeeded" || finalize.params.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", finalize.params)
	}
	if finalize.params.TotalTokens == nil || *finalize.params.TotalTokens != 10 {
		t.Fatalf("usage not finalized: %+v", finalize.params)
	}
}

// DeepSeek 401/402 mean the configured upstream provider access is not usable
// (invalid credential / insufficient balance). The gateway must surface these
// as provider_unavailable (HTTP 502), not provider_invalid_request (HTTP 400):
// the client request itself is valid; the upstream cannot be called right now.
// A 422 control case pins that real client-side provider errors still map to
// HTTP 400 provider_invalid_request.
func TestHandlerDeepSeekServerSideUpstreamStatusesAreNotClientErrors(t *testing.T) {
	for _, tt := range []struct {
		name           string
		upstreamStatus int
		stream         bool
		wantStatus     int
		wantType       string
	}{
		{"401 complete", http.StatusUnauthorized, false, http.StatusBadGateway, string(provider.ProviderUnavailable)},
		{"402 complete", http.StatusPaymentRequired, false, http.StatusBadGateway, string(provider.ProviderUnavailable)},
		{"402 stream", http.StatusPaymentRequired, true, http.StatusBadGateway, string(provider.ProviderUnavailable)},
		{"422 complete control", http.StatusUnprocessableEntity, false, http.StatusBadRequest, string(provider.ProviderInvalidReq)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.WriteHeader(tt.upstreamStatus)
				_, _ = response.Write([]byte(`{"error":{"message":"upstream said no","code":"upstream"},"extra":"secret-raw-upstream-body-xyz"}`))
			}))
			defer upstream.Close()

			baseStore, rawKey := newAuthorizedStoreForProvider(t, provider.DeepSeek, testCredentialID)
			baseStore.credential.BaseURLOverride = upstream.URL
			store := newNotifyingStore(baseStore)
			service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
				provider.DeepSeek: deepseek.New(upstream.Client()),
			}, nil, time.Second, time.Second)
			handler := NewHandler(service)
			mux := http.NewServeMux()
			handler.Register(mux)
			gateway := httptest.NewServer(mux)
			defer gateway.Close()

			body := `{"model":"deepseek/deepseek-chat","messages":[{"role":"user","content":"hello"}]}`
			if tt.stream {
				body = `{"model":"deepseek/deepseek-chat","messages":[{"role":"user","content":"hello"}],"stream":true}`
			}
			request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+rawKey)
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatalf("gateway request: %v", err)
			}
			defer response.Body.Close()
			responseBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read gateway response: %v", err)
			}

			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.StatusCode, tt.wantStatus, responseBody)
			}
			if !strings.Contains(string(responseBody), `"type":"`+tt.wantType+`"`) {
				t.Fatalf("error type = %s, want %q", responseBody, tt.wantType)
			}
			if strings.Contains(string(responseBody), "secret-raw-upstream-body-xyz") || strings.Contains(string(responseBody), "upstream said no") {
				t.Fatalf("raw upstream error leaked to client: %s", responseBody)
			}
			finalize := store.waitFinalize(t)
			if finalize.params.Status != "failed" || finalize.params.ErrorCategory == nil {
				t.Fatalf("finalize = %+v", finalize.params)
			}
			if *finalize.params.ErrorCategory != provider.ErrorCategory(tt.wantType) {
				t.Fatalf("finalized category = %q, want %q", *finalize.params.ErrorCategory, tt.wantType)
			}
		})
	}
}

func TestAnthropicModelRequiresAnthropicCredentialBeforeUpstream(t *testing.T) {
	store, rawKey := newAuthorizedStore(t) // openai credential only
	openAIClient := &fakeProviderClient{}
	anthropicClient := &fakeProviderClient{}
	service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
		provider.OpenAI:    openAIClient,
		provider.Anthropic: anthropicClient,
	}, nil, time.Second, time.Second)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, _, err = service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "anthropic/claude-sonnet-x",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	gatewayErr, ok := err.(*GatewayError)
	if !ok || gatewayErr.Category != provider.ProviderNotConfigured {
		t.Fatalf("error = %#v, want ProviderNotConfigured", err)
	}
	if store.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0 (no paid upstream work without credential)", store.createCalls)
	}
	if anthropicClient.calls != 0 || openAIClient.calls != 0 {
		t.Fatalf("provider calls: anthropic=%d openai=%d, want 0", anthropicClient.calls, openAIClient.calls)
	}
}

func TestCrossProviderServiceRoutesAnthropicToAnthropicAdapter(t *testing.T) {
	store, rawKey := newMultiProviderAuthorizedStore(t, provider.OpenAI, provider.Anthropic)
	openAIClient := &fakeProviderClient{
		result: provider.Result{
			Response: provider.ChatResponse{ID: "openai-result", Object: "chat.completion", Model: "gpt-test"},
			Usage:    &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
	}
	anthropicClient := &fakeProviderClient{
		result: provider.Result{
			Response: provider.ChatResponse{
				ID:      "msg_anthropic_result",
				Object:  "chat.completion",
				Model:   "claude-sonnet-x",
				Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}}},
			},
			Usage:             &provider.Usage{PromptTokens: 17, CompletionTokens: 9, TotalTokens: 26},
			UpstreamStatus:    http.StatusOK,
			UpstreamRequestID: "req_anthropic",
		},
	}
	service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
		provider.OpenAI:    openAIClient,
		provider.Anthropic: anthropicClient,
	}, nil, time.Second, time.Second)

	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	result, record, err := service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "anthropic/claude-sonnet-x",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("complete chat: %v", err)
	}
	if record.Provider != provider.Anthropic || record.Model != "claude-sonnet-x" || record.ID != testRequestID {
		t.Fatalf("record = %+v", record)
	}
	if result.Response.ID != "msg_anthropic_result" {
		t.Fatalf("result = %+v", result)
	}
	if openAIClient.calls != 0 {
		t.Fatalf("openai provider calls = %d, want 0", openAIClient.calls)
	}
	if anthropicClient.calls != 1 {
		t.Fatalf("anthropic provider calls = %d, want 1", anthropicClient.calls)
	}
	if anthropicClient.lastChat.Model != "claude-sonnet-x" {
		t.Fatalf("anthropic upstream model = %q", anthropicClient.lastChat.Model)
	}
	if store.lastCreate.Provider != provider.Anthropic || store.lastCreate.Model != "claude-sonnet-x" {
		t.Fatalf("create params = %+v", store.lastCreate)
	}
	if store.lastFinalize.Status != "succeeded" || store.lastFinalize.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", store.lastFinalize)
	}
	if store.lastFinalize.TotalTokens == nil || *store.lastFinalize.TotalTokens != 26 {
		t.Fatalf("finalize usage = %+v", store.lastFinalize)
	}
}

func TestHandlerCompleteChatThroughAnthropicClientOverHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get("x-api-key"); got != "sk-test" {
			t.Fatalf("upstream x-api-key = %q", got)
		}
		if got := request.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("upstream anthropic-version = %q", got)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("upstream Accept = %q", got)
		}
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int64  `json:"max_tokens"`
			System    string `json:"system"`
			Stream    bool   `json:"stream"`
			Messages  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(rawBody, &body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body.Model != "claude-sonnet-x" || body.Stream {
			t.Fatalf("upstream body = %+v", body)
		}
		if body.System != "You are helpful." {
			t.Fatalf("upstream system = %q", body.System)
		}
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
			t.Fatalf("upstream messages = %+v", body.Messages)
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("request-id", "req_anthropic_complete")
		_, _ = response.Write([]byte(`{
			"id":"msg_01ABC",
			"type":"message",
			"role":"assistant",
			"model":"claude-sonnet-x",
			"content":[{"type":"text","text":"world"}],
			"stop_reason":"end_turn",
			"stop_sequence":null,
			"usage":{"input_tokens":17,"output_tokens":9}
		}`))
	}))
	defer upstream.Close()

	baseStore, rawKey := newAuthorizedStoreForProvider(t, provider.Anthropic, testCredentialID)
	baseStore.credential.BaseURLOverride = upstream.URL
	store := newNotifyingStore(baseStore)
	service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
		provider.Anthropic: anthropic.New(upstream.Client()),
	}, nil, time.Second, time.Second)
	handler := NewHandler(service)
	mux := http.NewServeMux()
	handler.Register(mux)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
		"model":"anthropic/claude-sonnet-x",
		"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"hello"}]
	}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if got := response.Header.Get("X-Gateway-Provider"); got != "anthropic" {
		t.Fatalf("X-Gateway-Provider = %q", got)
	}
	var decoded struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode gateway response: %v", err)
	}
	// The gateway returns an OpenAI-compatible envelope regardless of the
	// upstream Anthropic wire format.
	if decoded.ID != "msg_01ABC" || decoded.Object != "chat.completion" || decoded.Model != "claude-sonnet-x" {
		t.Fatalf("gateway response = %+v", decoded)
	}
	if len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content != "world" {
		t.Fatalf("gateway choices = %+v", decoded.Choices)
	}
	if decoded.Usage == nil || decoded.Usage.PromptTokens != 17 || decoded.Usage.CompletionTokens != 9 || decoded.Usage.TotalTokens != 26 {
		t.Fatalf("gateway usage = %+v", decoded.Usage)
	}
	finalize := store.waitFinalize(t)
	if finalize.params.Status != "succeeded" || finalize.params.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", finalize.params)
	}
	if finalize.params.TotalTokens == nil || *finalize.params.TotalTokens != 26 {
		t.Fatalf("usage not finalized: %+v", finalize.params)
	}
	if finalize.params.UpstreamRequestID == nil || *finalize.params.UpstreamRequestID != "req_anthropic_complete" {
		t.Fatalf("upstream request ID not finalized: %+v", finalize.params)
	}
}

func TestHandlerStreamsThroughAnthropicClientOverHTTP(t *testing.T) {
	firstContentFlushed := make(chan struct{})
	releaseRest := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("upstream Accept = %q", got)
		}
		rawBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.Unmarshal(rawBody, &body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body.Model != "claude-sonnet-x" || !body.Stream {
			t.Fatalf("upstream body = %+v", body)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("request-id", "req_anthropic_stream")

		_, _ = io.WriteString(response, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-x\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":17,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(response, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n")
		response.(http.Flusher).Flush()
		close(firstContentFlushed)

		<-releaseRest
		_, _ = io.WriteString(response, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":9}}\n\n")
		_, _ = io.WriteString(response, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		response.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	baseStore, rawKey := newAuthorizedStoreForProvider(t, provider.Anthropic, testCredentialID)
	baseStore.credential.BaseURLOverride = upstream.URL
	store := newNotifyingStore(baseStore)
	service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
		provider.Anthropic: anthropic.New(upstream.Client()),
	}, nil, time.Second, time.Second)
	handler := NewHandler(service)
	mux := http.NewServeMux()
	handler.Register(mux)
	gateway := httptest.NewServer(mux)
	defer gateway.Close()

	request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
		"model":"anthropic/claude-sonnet-x",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d; body=%s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header.Get("X-Gateway-Provider"); got != "anthropic" {
		t.Fatalf("X-Gateway-Provider = %q", got)
	}
	reader := bufio.NewReader(response.Body)
	firstEvent := readSSEEvent(t, reader)
	if !strings.Contains(firstEvent, `data: {"id":"msg_stream"`) || !strings.Contains(firstEvent, `"content":"Hello"`) {
		t.Fatalf("first event = %q", firstEvent)
	}
	select {
	case <-firstContentFlushed:
	case <-time.After(time.Second):
		t.Fatal("upstream did not flush first content delta")
	}

	close(releaseRest)
	events := firstEvent
	for !strings.Contains(events, "data: [DONE]\n\n") {
		events += readSSEEvent(t, reader)
	}
	if !strings.Contains(events, `"finish_reason":"stop"`) {
		t.Fatalf("stream did not include the synthesized finish chunk: %s", events)
	}
	finalize := store.waitFinalize(t)
	if finalize.params.Status != "succeeded" || finalize.params.ErrorCategory != nil {
		t.Fatalf("finalize = %+v", finalize.params)
	}
	if finalize.params.TotalTokens == nil || *finalize.params.TotalTokens != 26 {
		t.Fatalf("usage not finalized: %+v", finalize.params)
	}
	if finalize.params.UpstreamRequestID == nil || *finalize.params.UpstreamRequestID != "req_anthropic_stream" {
		t.Fatalf("upstream request ID not finalized: %+v", finalize.params)
	}
	if finalize.params.TTFTMS == nil {
		t.Fatalf("TTFT not recorded for the anthropic stream: %+v", finalize.params)
	}
}

// The Week 6 public subset gains an optional max_tokens. It is forwarded to
// OpenAI-compatible providers as-is and omitted from the wire body when the
// client did not send it.
func TestHandlerForwardsMaxTokensToOpenAICompatibleProvider(t *testing.T) {
	for _, tt := range []struct {
		name       string
		request    string
		wantBody   string
		wantAbsent bool
	}{
		{
			name:     "explicit max_tokens is forwarded",
			request:  `{"model":"openai/gpt-test","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`,
			wantBody: `"max_tokens":128`,
		},
		{
			name:       "absent max_tokens stays off the wire body",
			request:    `{"model":"openai/gpt-test","messages":[{"role":"user","content":"hi"}]}`,
			wantAbsent: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				rawBody, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatalf("read upstream body: %v", err)
				}
				if tt.wantAbsent {
					if strings.Contains(string(rawBody), "max_tokens") {
						t.Fatalf("upstream body contains max_tokens: %s", rawBody)
					}
				} else if !strings.Contains(string(rawBody), tt.wantBody) {
					t.Fatalf("upstream body = %s, want %s", rawBody, tt.wantBody)
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{"id":"chat_1","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer upstream.Close()

			baseStore, rawKey := newAuthorizedStoreForProvider(t, provider.OpenAI, testCredentialID)
			baseStore.credential.BaseURLOverride = upstream.URL
			store := newNotifyingStore(baseStore)
			service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
				provider.OpenAI: openai.New(upstream.Client()),
			}, nil, time.Second, time.Second)
			handler := NewHandler(service)
			mux := http.NewServeMux()
			handler.Register(mux)
			gateway := httptest.NewServer(mux)
			defer gateway.Close()

			request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(tt.request))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+rawKey)
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatalf("gateway request: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d; body=%s", response.StatusCode, body)
			}
			store.waitFinalize(t)
		})
	}
}

func TestHandlerRejectsNonPositiveMaxTokensBeforeUpstream(t *testing.T) {
	for _, raw := range []string{
		`{"model":"openai/gpt-test","messages":[{"role":"user","content":"hi"}],"max_tokens":0}`,
		`{"model":"openai/gpt-test","messages":[{"role":"user","content":"hi"}],"max_tokens":-1}`,
	} {
		store, rawKey := newAuthorizedStore(t)
		client := &fakeProviderClient{}
		service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
			provider.OpenAI: client,
		}, nil, time.Second, time.Second)
		handler := NewHandler(service)
		mux := http.NewServeMux()
		handler.Register(mux)
		gateway := httptest.NewServer(mux)

		request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(raw))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+rawKey)
		response, err := gateway.Client().Do(request)
		gateway.Close()
		if err != nil {
			t.Fatalf("gateway request: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, body)
		}
		if client.calls != 0 {
			t.Fatalf("provider calls = %d, want 0", client.calls)
		}
		if store.createCalls != 0 {
			t.Fatalf("create calls = %d, want 0", store.createCalls)
		}
	}
}

// A mid-conversation system message is not translated by the Week 6 gateway
// subset (Anthropic supports it on some models only and the gateway does not
// detect per-model capability). The adapter rejects it before any upstream
// HTTP work; the gateway surfaces a stable 400 unsupported_feature and still
// records the failed attempt for attribution.
func TestHandlerRejectsAnthropicMidConversationSystemBeforeUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			upstreamCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				upstreamCalls++
				t.Fatalf("upstream must not be called for a rejected request")
			}))
			defer upstream.Close()

			baseStore, rawKey := newAuthorizedStoreForProvider(t, provider.Anthropic, testCredentialID)
			baseStore.credential.BaseURLOverride = upstream.URL
			store := newNotifyingStore(baseStore)
			service := newServiceForStoreWithClientsAndTimeouts(t, store, map[provider.Name]provider.Client{
				provider.Anthropic: anthropic.New(upstream.Client()),
			}, nil, time.Second, time.Second)
			handler := NewHandler(service)
			mux := http.NewServeMux()
			handler.Register(mux)
			gateway := httptest.NewServer(mux)
			defer gateway.Close()

			request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
				"model":"anthropic/claude-sonnet-x",
				"messages":[
					{"role":"system","content":"lead rule"},
					{"role":"user","content":"hello"},
					{"role":"system","content":"late rule"}
				],
				"stream":`+strconv.FormatBool(stream)+`
			}`))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+rawKey)
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatalf("gateway request: %v", err)
			}
			defer response.Body.Close()

			if response.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, body)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read response body: %v", err)
			}
			if !strings.Contains(string(body), `"code":"unsupported_feature"`) ||
				!strings.Contains(string(body), "request includes a feature not supported by the gateway for the selected provider") {
				t.Fatalf("response body = %s", body)
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
			}
			finalize := store.waitFinalize(t)
			if finalize.params.Status != "failed" || finalize.params.ErrorCategory == nil ||
				*finalize.params.ErrorCategory != provider.UnsupportedFeature {
				t.Fatalf("finalize = %+v", finalize.params)
			}
		})
	}
}

func pricedOpenAIResult() provider.Result {
	usage := &provider.Usage{PromptTokens: 1_000, CompletionTokens: 2_000, TotalTokens: 3_000}
	return provider.Result{
		Response: provider.ChatResponse{
			ID:      "chatcmpl_price",
			Object:  "chat.completion",
			Created: 123,
			Model:   "gpt-test",
			Choices: []provider.Choice{{Index: 0, Message: provider.ResponseMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
			Usage:   usage,
		},
		Usage:             usage,
		UpstreamStatus:    http.StatusOK,
		UpstreamRequestID: "req_price",
	}
}

// terraEquivalent is 2e9/12e9 nano-USD per million, matching the real
// openai/gpt-5.6-terra seed so the assertion doubles as a documented example.
const (
	terraInputNanoPerMillion  = int64(2_000_000_000)
	terraOutputNanoPerMillion = int64(12_000_000_000)
	terraPriceID              = "price-terra"
)

func addTerraPrice(store *fakeStore) {
	if store.prices == nil {
		store.prices = make(map[priceKey]ModelPrice)
	}
	store.prices[priceKey{provider: provider.OpenAI, model: "gpt-test"}] = ModelPrice{
		ID:                      terraPriceID,
		InputNanoUSDPerMillion:  terraInputNanoPerMillion,
		OutputNanoUSDPerMillion: terraOutputNanoPerMillion,
	}
}

func runSuccessChat(t *testing.T, store *fakeStore, rawKey string, client *fakeProviderClient) {
	t.Helper()
	service := newTestService(t, store, client)
	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, _, err := service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatalf("complete chat: %v", err)
	}
}

func TestCompleteChatRecordsPricingIDAndEstimatedCost(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	addTerraPrice(store)
	runSuccessChat(t, store, rawKey, &fakeProviderClient{result: pricedOpenAIResult()})

	if store.lastFinalize.Status != "succeeded" {
		t.Fatalf("finalize status = %q", store.lastFinalize.Status)
	}
	if store.lastFinalize.PricingID == nil || *store.lastFinalize.PricingID != terraPriceID {
		t.Fatalf("pricing id = %v, want %s", store.lastFinalize.PricingID, terraPriceID)
	}
	if store.lastFinalize.EstimatedCostNanoUSD == nil {
		t.Fatalf("estimated cost is nil, want calculated value")
	}
	// 1000*2e9/1e6 + 2000*12e9/1e6 = 2_000_000 + 24_000_000 nano-USD.
	if got := *store.lastFinalize.EstimatedCostNanoUSD; got != 26_000_000 {
		t.Fatalf("estimated cost = %d, want 26000000", got)
	}
}

func TestCompleteChatWithoutPriceVersionLeavesCostAttributionNull(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	runSuccessChat(t, store, rawKey, &fakeProviderClient{result: pricedOpenAIResult()})

	if store.lastFinalize.PricingID != nil {
		t.Fatalf("pricing id = %v, want nil when no price version is effective", store.lastFinalize.PricingID)
	}
	if store.lastFinalize.EstimatedCostNanoUSD != nil {
		t.Fatalf("estimated cost = %v, want nil", store.lastFinalize.EstimatedCostNanoUSD)
	}
}

func TestCompleteChatKeepsPricingIDWhenUsageMissing(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	addTerraPrice(store)
	client := &fakeProviderClient{result: pricedOpenAIResult()}
	client.result.Usage = nil
	client.result.Response.Usage = nil
	runSuccessChat(t, store, rawKey, client)

	if store.lastFinalize.PricingID == nil || *store.lastFinalize.PricingID != terraPriceID {
		t.Fatalf("pricing id = %v, want %s (provenance survives missing usage)", store.lastFinalize.PricingID, terraPriceID)
	}
	if store.lastFinalize.EstimatedCostNanoUSD != nil {
		t.Fatalf("estimated cost = %v, want nil when usage is missing", store.lastFinalize.EstimatedCostNanoUSD)
	}
}

func TestCompleteChatKeepsPricingIDWhenCalculationOverflows(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	addTerraPrice(store)
	client := &fakeProviderClient{result: pricedOpenAIResult()}
	overflowUsage := &provider.Usage{PromptTokens: 1 << 62, CompletionTokens: 0, TotalTokens: 1 << 62}
	client.result.Usage = overflowUsage
	client.result.Response.Usage = overflowUsage
	runSuccessChat(t, store, rawKey, client)

	if store.lastFinalize.PricingID == nil || *store.lastFinalize.PricingID != terraPriceID {
		t.Fatalf("pricing id = %v, want %s", store.lastFinalize.PricingID, terraPriceID)
	}
	if store.lastFinalize.EstimatedCostNanoUSD != nil {
		t.Fatalf("estimated cost = %v, want nil on calculation failure", store.lastFinalize.EstimatedCostNanoUSD)
	}
}

func TestFailedRequestNeverLooksUpPrice(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	addTerraPrice(store)
	client := &fakeProviderClient{err: &provider.Error{Category: provider.ProviderUnavailable, StatusCode: http.StatusServiceUnavailable, Message: "boom"}}
	service := newTestService(t, store, client)
	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, _, err := service.CompleteChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}); err == nil {
		t.Fatalf("complete chat succeeded, want provider error")
	}
	if store.lastFinalize.Status != "failed" {
		t.Fatalf("finalize status = %q, want failed", store.lastFinalize.Status)
	}
	if store.lastFinalize.PricingID != nil || store.lastFinalize.EstimatedCostNanoUSD != nil {
		t.Fatalf("failed request recorded cost attribution: %+v", store.lastFinalize)
	}
}

func TestPriceLookupErrorStillFinalizesSuccessfullyWithoutCost(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	addTerraPrice(store)
	store.priceLookupErr = errors.New("db unavailable")
	runSuccessChat(t, store, rawKey, &fakeProviderClient{result: pricedOpenAIResult()})

	if store.lastFinalize.Status != "succeeded" {
		t.Fatalf("finalize status = %q, want succeeded (pricing must never fail the request)", store.lastFinalize.Status)
	}
	if store.lastFinalize.PricingID != nil || store.lastFinalize.EstimatedCostNanoUSD != nil {
		t.Fatalf("cost attribution recorded despite price lookup error: %+v", store.lastFinalize)
	}
}

func TestStreamChatRecordsPricingIDAndEstimatedCostAtDone(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	addTerraPrice(store)
	usage := &provider.Usage{PromptTokens: 500, CompletionTokens: 250, TotalTokens: 750}
	client := &fakeProviderClient{streamFactory: func(ctx context.Context) (provider.StreamResult, error) {
		return provider.StreamResult{Stream: &fakeChatStream{
			events: []provider.StreamEvent{{Data: []byte(`chunk`), Event: "chat.completion.chunk"}, {Done: true}},
			usage:  usage,
		}, UpstreamStatus: http.StatusOK}, nil
	}}
	service := newTestService(t, store, client)
	auth, err := service.Authenticate(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	_, err = service.StreamChat(context.Background(), auth, "", provider.ChatRequest{
		Model:    "openai/gpt-test",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}, &recordingStreamSink{})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	if store.lastFinalize.PricingID == nil || *store.lastFinalize.PricingID != terraPriceID {
		t.Fatalf("pricing id = %v, want %s", store.lastFinalize.PricingID, terraPriceID)
	}
	// 500*2e9/1e6 + 250*12e9/1e6 = 1_000_000 + 3_000_000 nano-USD.
	if got := *store.lastFinalize.EstimatedCostNanoUSD; got != 4_000_000 {
		t.Fatalf("estimated cost = %d, want 4000000", got)
	}
}

func TestPricingLookupTimeoutStillFinalizesDurableRow(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	addTerraPrice(store)
	// The pricing lookup blocks until its own child context expires. This
	// deterministically proves the lookup budget cannot starve the durable
	// finalization that runs afterwards (ADR-016).
	store.priceLookupBlock = func(ctx context.Context) {
		<-ctx.Done()
	}
	runSuccessChat(t, store, rawKey, &fakeProviderClient{result: pricedOpenAIResult()})

	if store.lastFinalize.Status != "succeeded" {
		t.Fatalf("finalize status = %q, want succeeded (pricing timeout must not fail the request)", store.lastFinalize.Status)
	}
	if store.finalizeCalls != 1 {
		t.Fatalf("finalize calls = %d, want 1", store.finalizeCalls)
	}
	if store.finalizeCtxErr != nil {
		t.Fatalf("finalize received an expired context (%v); durable write must get a live context", store.finalizeCtxErr)
	}
	if !store.finalizeHadDeadline {
		t.Fatal("finalize context is not bounded")
	}
	if store.lastFinalize.PricingID != nil || store.lastFinalize.EstimatedCostNanoUSD != nil {
		t.Fatalf("cost attribution recorded despite pricing timeout: %+v", store.lastFinalize)
	}
	if store.lastFinalize.PromptTokens == nil || *store.lastFinalize.PromptTokens != 1000 {
		t.Fatalf("reported usage should still be persisted: %+v", store.lastFinalize)
	}
}
