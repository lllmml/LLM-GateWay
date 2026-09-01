package dataplane

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

const (
	testProjectID    = "11111111-1111-4111-8111-111111111111"
	testVirtualKeyID = "22222222-2222-4222-8222-222222222222"
	testCredentialID = "33333333-3333-4333-8333-333333333333"
	testRequestID    = "44444444-4444-4444-8444-444444444444"
)

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
	for _, want := range []string{testRequestID, string(provider.OpenAI), string(provider.ProviderTimeout), "finalize write failed"} {
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

func TestHandlerRejectsStreamBeforeUpstream(t *testing.T) {
	store, rawKey := newAuthorizedStore(t)
	client := &fakeProviderClient{}
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
	if !strings.Contains(response.Body.String(), `"code":"unsupported_parameter"`) || !strings.Contains(response.Body.String(), `"type":"invalid_request"`) {
		t.Fatalf("body = %s", response.Body.String())
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
	request.Header.Set("Traceparent", "00-raw-traceparent-raw-traceparent-01")
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
	if strings.Contains(response.Body.String(), "raw-traceparent") || store.lastCreate.TraceID != "" {
		t.Fatalf("traceparent leaked body=%s trace=%q", response.Body.String(), store.lastCreate.TraceID)
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
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	service, err := NewService(Options{
		Store:            store,
		VirtualKeyPepper: bytes.Repeat([]byte{9}, 32),
		CredentialCipher: cipher,
		UpstreamTimeout:  time.Second,
		Providers:        map[provider.Name]provider.Client{provider.OpenAI: client},
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

func newAuthorizedStore(t *testing.T) (*fakeStore, string) {
	t.Helper()
	rawKey, prefix, err := apikey.GenerateRawKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyHash, err := apikey.HashKey(rawKey, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("hash key: %v", err)
	}
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	encrypted, err := cipher.Encrypt([]byte("sk-test"), security.CredentialIdentity{
		CredentialID: testCredentialID,
		ProjectID:    testProjectID,
		Provider:     string(provider.OpenAI),
	})
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}
	return &fakeStore{
		prefix:  prefix,
		keyHash: keyHash,
		auth:    AuthContext{ProjectID: testProjectID, VirtualKeyID: testVirtualKeyID, KeyPrefix: prefix},
		credential: ProviderCredential{
			ID:               testCredentialID,
			ProjectID:        testProjectID,
			Provider:         provider.OpenAI,
			SecretCiphertext: encrypted.Ciphertext,
			SecretNonce:      encrypted.Nonce,
			KeyVersion:       encrypted.KeyVersion,
		},
	}, rawKey
}

type fakeStore struct {
	prefix              string
	keyHash             []byte
	auth                AuthContext
	credential          ProviderCredential
	authCalls           int
	resolveCalls        int
	createCalls         int
	finalizeCalls       int
	createErr           error
	finalizeErr         error
	lastCreate          CreateRequestParams
	lastFinalize        FinalizeParams
	finalizeCtxErr      error
	finalizeHadDeadline bool
}

func (s *fakeStore) AuthenticateVirtualKey(_ context.Context, prefix string, keyHash []byte) (AuthContext, error) {
	s.authCalls++
	if prefix != s.prefix || !bytes.Equal(keyHash, s.keyHash) {
		return AuthContext{}, ErrNotFound
	}
	return s.auth, nil
}

func (s *fakeStore) ResolveProviderCredential(context.Context, string, provider.Name) (ProviderCredential, error) {
	s.resolveCalls++
	if s.credential.ID == "" {
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

func (s *fakeStore) FinalizeGatewayRequest(ctx context.Context, params FinalizeParams) error {
	s.finalizeCalls++
	s.lastFinalize = params
	s.finalizeCtxErr = ctx.Err()
	_, s.finalizeHadDeadline = ctx.Deadline()
	return s.finalizeErr
}

type fakeProviderClient struct {
	calls              int
	lastChat           provider.ChatRequest
	lastCredential     provider.Credential
	result             provider.Result
	err                error
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
