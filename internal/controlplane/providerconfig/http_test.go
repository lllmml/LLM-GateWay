package providerconfig

import (
	"context"
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

type fakeStore struct {
	calls int
}

func (s *fakeStore) UpsertProviderConfig(_ context.Context, params UpsertParams) (Config, error) {
	s.calls++
	return Config{
		ProjectID:    params.ProjectID,
		Provider:     params.Provider,
		CredentialID: params.CredentialID,
		Enabled:      params.Enabled,
		UpdatedAt:    time.Now().UTC(),
	}, nil
}
