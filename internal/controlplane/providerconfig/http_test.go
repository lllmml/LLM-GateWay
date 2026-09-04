package providerconfig

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpsertOpenAIRejectsBaseURLOverride(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(store)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	handler := NewHandler(service, func(*http.Request) (string, bool) {
		return "11111111-1111-4111-8111-111111111111", true
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/projects/project-1/provider-configs/openai", strings.NewReader(`{
		"credential_id":"33333333-3333-4333-8333-333333333333",
		"enabled":true,
		"base_url_override":"http://127.0.0.1:1234"
	}`))
	request.SetPathValue("projectID", "project-1")
	response := httptest.NewRecorder()

	handler.upsertOpenAI(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if store.calls != 0 {
		t.Fatalf("store calls = %d, want 0", store.calls)
	}
}

func TestUpsertDeepSeekConfig(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(store)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	handler := NewHandler(service, func(*http.Request) (string, bool) {
		return "11111111-1111-4111-8111-111111111111", true
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/projects/project-1/provider-configs/deepseek", strings.NewReader(`{
		"credential_id":"33333333-3333-4333-8333-333333333333",
		"enabled":true
	}`))
	request.SetPathValue("projectID", "project-1")
	response := httptest.NewRecorder()

	handler.upsertDeepSeek(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.params.Provider != "deepseek" || store.params.CredentialID != "33333333-3333-4333-8333-333333333333" {
		t.Fatalf("store params = %+v", store.params)
	}
}

// Week 6 brings the Anthropic adapter online, so the provider-config boundary
// accepts anthropic the same way it already accepts openai and deepseek.
func TestUpsertAnthropicConfig(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(store)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	handler := NewHandler(service, func(*http.Request) (string, bool) {
		return "11111111-1111-4111-8111-111111111111", true
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/projects/project-1/provider-configs/anthropic", strings.NewReader(`{
		"credential_id":"33333333-3333-4333-8333-333333333333",
		"enabled":true
	}`))
	request.SetPathValue("projectID", "project-1")
	response := httptest.NewRecorder()

	handler.upsertAnthropic(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if store.params.Provider != "anthropic" || store.params.CredentialID != "33333333-3333-4333-8333-333333333333" {
		t.Fatalf("store params = %+v", store.params)
	}
}

func TestServiceRejectsProvidersWithoutAdapters(t *testing.T) {
	service, err := NewService(&fakeStore{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	_, err = service.upsert(context.Background(), "11111111-1111-4111-8111-111111111111", "project-1", "gemini", "33333333-3333-4333-8333-333333333333", true)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "provider" {
		t.Fatalf("error = %#v, want provider validation error", err)
	}
}

type fakeStore struct {
	calls  int
	params UpsertParams
}

func (s *fakeStore) UpsertProviderConfig(_ context.Context, params UpsertParams) (Config, error) {
	s.calls++
	s.params = params
	return Config{
		ProjectID:    params.ProjectID,
		Provider:     params.Provider,
		CredentialID: params.CredentialID,
		Enabled:      params.Enabled,
		UpdatedAt:    time.Now().UTC(),
	}, nil
}
