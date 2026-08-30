package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateReturnsMetadataOnly(t *testing.T) {
	store := &httpStore{}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-1", true })

	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-1/provider-credentials", strings.NewReader(`{"provider":"openai","label":" local ","secret":"provider-secret"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, forbidden := range []string{"secret", "secret_ciphertext", "secret_nonce", "key_version", "project_id", "owner_user_id"} {
		if _, exists := body[forbidden]; exists {
			t.Fatalf("create response exposed %s", forbidden)
		}
	}
	if strings.Contains(response.Body.String(), "provider-secret") {
		t.Fatal("create response exposed raw provider secret")
	}
	if store.created.OwnerUserID != "owner-1" || store.created.ProjectID != "project-1" {
		t.Fatalf("create scope = (%q, %q), want owner/project", store.created.OwnerUserID, store.created.ProjectID)
	}
	if store.created.Provider != ProviderOpenAI || store.created.Label != "local" {
		t.Fatalf("created metadata = %+v", store.created)
	}
}

func TestListReturnsMetadataOnly(t *testing.T) {
	now := time.Now().UTC()
	store := &httpStore{credentials: []Credential{{
		ID: "credential-1", Provider: ProviderAnthropic, Label: "prod", Status: StatusActive, KeyVersion: 1, CreatedAt: now,
	}}}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-1", true })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/provider-credentials", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var body struct {
		Credentials []map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Credentials) != 1 {
		t.Fatalf("credentials length = %d, want 1", len(body.Credentials))
	}
	for _, forbidden := range []string{"secret", "secret_ciphertext", "secret_nonce", "key_version", "project_id", "owner_user_id"} {
		if _, exists := body.Credentials[0][forbidden]; exists {
			t.Fatalf("list response exposed %s", forbidden)
		}
	}
	for _, required := range []string{"id", "provider", "label", "status", "created_at", "rotated_at"} {
		if _, exists := body.Credentials[0][required]; !exists {
			t.Fatalf("list response omitted %s", required)
		}
	}
	if store.lastOwner != "owner-1" || store.lastProject != "project-1" {
		t.Fatal("list did not use authenticated owner and path project")
	}
}

func TestHandlerRequiresAuthenticationBeforeStore(t *testing.T) {
	store := &httpStore{}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "", false })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/provider-credentials", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if store.calls != 0 {
		t.Fatal("unauthenticated request reached store")
	}
}

func TestCreateAndRotateRejectUnknownFields(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{name: "create", path: "/api/v1/projects/project-1/provider-credentials", body: `{"provider":"openai","label":"local","secret":"secret","owner_user_id":"attacker"}`},
		{name: "rotate", path: "/api/v1/projects/project-1/provider-credentials/credential-1/rotate", body: `{"secret":"secret","provider":"openai"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &httpStore{}
			handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-1", true })

			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
			}
			if store.calls != 0 {
				t.Fatal("invalid request reached store")
			}
		})
	}
}

func TestCollectionMapsMissingOrCrossOwnerProjectToProjectNotFound(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		body   string
	}{
		{name: "create", method: http.MethodPost, body: `{"provider":"openai","label":"local","secret":"secret"}`},
		{name: "list", method: http.MethodGet},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &httpStore{err: ErrNotFound}
			handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-2", true })

			request := httptest.NewRequest(test.method, "/api/v1/projects/project-1/provider-credentials", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			assertHTTPError(t, response, http.StatusNotFound, "project_not_found")
			if strings.Contains(strings.ToLower(response.Body.String()), "owner") {
				t.Fatalf("response leaked ownership detail: %s", response.Body.String())
			}
		})
	}
}

func TestRotateAndDisableResponsesNeverContainSecrets(t *testing.T) {
	store := &httpStore{}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-1", true })

	for _, test := range []struct {
		name       string
		path       string
		body       string
		wantStatus Status
	}{
		{name: "rotate", path: "/api/v1/projects/project-1/provider-credentials/credential-1/rotate", body: `{"secret":"rotated-secret"}`, wantStatus: StatusActive},
		{name: "disable", path: "/api/v1/projects/project-1/provider-credentials/credential-1/disable", wantStatus: StatusDisabled},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body["status"] != string(test.wantStatus) {
				t.Fatalf("status = %v, want %s", body["status"], test.wantStatus)
			}
			for _, forbidden := range []string{"secret", "secret_ciphertext", "secret_nonce", "key_version"} {
				if _, exists := body[forbidden]; exists {
					t.Fatalf("response exposed %s", forbidden)
				}
			}
			if strings.Contains(response.Body.String(), "rotated-secret") {
				t.Fatal("response exposed rotated secret")
			}
		})
	}
}

func TestMutationMapsCrossOwnerToNotFoundWithoutLeak(t *testing.T) {
	store := &httpStore{err: ErrNotFound}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-2", true })

	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{name: "rotate", path: "/api/v1/projects/project-1/provider-credentials/credential-1/rotate", body: `{"secret":"secret"}`},
		{name: "disable", path: "/api/v1/projects/project-1/provider-credentials/credential-1/disable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			assertHTTPError(t, response, http.StatusNotFound, "provider_credential_not_found")
			if strings.Contains(strings.ToLower(response.Body.String()), "owner") {
				t.Fatalf("response leaked ownership detail: %s", response.Body.String())
			}
		})
	}
}

func newHTTPTestHandler(t *testing.T, store Store, currentUserID CurrentUserID) http.Handler {
	t.Helper()
	service, err := NewService(store, testSeal)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	mux := http.NewServeMux()
	NewHandler(service, currentUserID).Register(mux)
	return mux
}

func assertHTTPError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, wantStatus, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q", body.Error.Code, wantCode)
	}
}

type httpStore struct {
	created     CreateParams
	credentials []Credential
	err         error
	calls       int
	lastOwner   string
	lastProject string
}

func (s *httpStore) CreateCredential(_ context.Context, params CreateParams) (Credential, error) {
	s.calls++
	s.created = params
	if s.err != nil {
		return Credential{}, s.err
	}
	return Credential{ID: params.ID, ProjectID: params.ProjectID, Provider: params.Provider, Label: params.Label, Status: StatusActive, KeyVersion: params.KeyVersion, CreatedAt: time.Now().UTC()}, nil
}

func (s *httpStore) GetCredential(_ context.Context, ownerUserID, projectID, credentialID string) (Credential, error) {
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	if s.err != nil {
		return Credential{}, s.err
	}
	return Credential{ID: credentialID, ProjectID: projectID, Provider: ProviderOpenAI}, nil
}

func (s *httpStore) ListCredentials(_ context.Context, ownerUserID, projectID string) ([]Credential, error) {
	s.calls++
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	return s.credentials, s.err
}

func (s *httpStore) RotateCredential(_ context.Context, params RotateParams) (Credential, error) {
	s.calls++
	s.lastOwner = params.OwnerUserID
	s.lastProject = params.ProjectID
	if s.err != nil {
		return Credential{}, s.err
	}
	now := time.Now().UTC()
	return Credential{ID: params.CredentialID, ProjectID: params.ProjectID, Provider: ProviderOpenAI, Label: "local", Status: StatusActive, KeyVersion: params.KeyVersion, CreatedAt: now, RotatedAt: &now}, nil
}

func (s *httpStore) DisableCredential(_ context.Context, ownerUserID, projectID, credentialID string) (Credential, error) {
	s.calls++
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	if s.err != nil {
		return Credential{}, s.err
	}
	return Credential{ID: credentialID, ProjectID: projectID, Provider: ProviderOpenAI, Label: "local", Status: StatusDisabled, KeyVersion: 1, CreatedAt: time.Now().UTC()}, nil
}

var _ Store = (*httpStore)(nil)
