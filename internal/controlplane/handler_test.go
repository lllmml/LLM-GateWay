package controlplane

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/credential"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

func TestControlPlaneProjectKeyAndCredentialRoutesUseSessionOwnerAndCSRF(t *testing.T) {
	const credentialProjectID = "22222222-2222-4222-8222-222222222222"
	user := User{ID: "owner-from-session", GitHubLogin: "octo"}
	sessions := newFakeSessionStore(user)
	auth := newTestAuthHandler(t, testAuthDeps{sessionStore: sessions})
	sessionCookie, csrfCookie := seedSession(t, auth, sessions, user)
	projects := &handlerProjectStore{}
	keys := &handlerKeyStore{}
	credentials := &handlerCredentialStore{}
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new credential cipher: %v", err)
	}
	handler, err := NewHandler(auth, projects, keys, bytes.Repeat([]byte{9}, 32), credentials, cipher)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	unauthenticatedKeyRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/keys", nil)
	unauthenticatedKeyResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedKeyResponse, unauthenticatedKeyRequest)
	if unauthenticatedKeyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated key list status = %d, want %d", unauthenticatedKeyResponse.Code, http.StatusUnauthorized)
	}
	if keys.listCalls != 0 {
		t.Fatal("unauthenticated key list reached store")
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	listRequest.AddCookie(sessionCookie)
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", listResponse.Code, http.StatusOK)
	}
	if projects.lastOwner != user.ID {
		t.Fatalf("list owner = %q, want session owner %q", projects.lastOwner, user.ID)
	}

	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewBufferString(`{"name":"Gateway","slug":"gateway"}`))
	missingCSRF.AddCookie(sessionCookie)
	missingCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want %d", missingCSRFResponse.Code, http.StatusForbidden)
	}
	if projects.createCalls != 0 {
		t.Fatal("request without CSRF reached project store")
	}

	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewBufferString(`{"name":"Gateway","slug":"gateway"}`))
	createRequest.Header.Set("Origin", "http://127.0.0.1:8081")
	createRequest.Header.Set("X-CSRF-Token", csrfCookie.Value)
	createRequest.AddCookie(sessionCookie)
	createRequest.AddCookie(csrfCookie)
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body=%s", createResponse.Code, http.StatusCreated, createResponse.Body.String())
	}
	if projects.createCalls != 1 || projects.lastOwner != user.ID {
		t.Fatalf("create calls = %d, owner = %q", projects.createCalls, projects.lastOwner)
	}

	keyListRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/keys", nil)
	keyListRequest.AddCookie(sessionCookie)
	keyListResponse := httptest.NewRecorder()
	handler.ServeHTTP(keyListResponse, keyListRequest)
	if keyListResponse.Code != http.StatusOK {
		t.Fatalf("key list status = %d, want %d", keyListResponse.Code, http.StatusOK)
	}
	if keys.lastOwner != user.ID || keys.lastProject != "project-1" {
		t.Fatal("key list did not use session owner and path project")
	}

	missingKeyCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-1/keys", bytes.NewBufferString(`{"name":"local"}`))
	missingKeyCSRF.AddCookie(sessionCookie)
	missingKeyCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingKeyCSRFResponse, missingKeyCSRF)
	if missingKeyCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("key create without CSRF status = %d, want %d", missingKeyCSRFResponse.Code, http.StatusForbidden)
	}
	if keys.createCalls != 0 {
		t.Fatal("key create without CSRF reached store")
	}

	keyCreateRequest := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-1/keys", bytes.NewBufferString(`{"name":"local"}`))
	keyCreateRequest.Header.Set("Origin", "http://127.0.0.1:8081")
	keyCreateRequest.Header.Set("X-CSRF-Token", csrfCookie.Value)
	keyCreateRequest.AddCookie(sessionCookie)
	keyCreateRequest.AddCookie(csrfCookie)
	keyCreateResponse := httptest.NewRecorder()
	handler.ServeHTTP(keyCreateResponse, keyCreateRequest)
	if keyCreateResponse.Code != http.StatusCreated {
		t.Fatalf("key create status = %d, want %d", keyCreateResponse.Code, http.StatusCreated)
	}
	if keys.createCalls != 1 || keys.lastOwner != user.ID || keys.lastProject != "project-1" {
		t.Fatal("key create did not use session owner and path project")
	}

	missingRevokeCSRF := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/project-1/keys/key-1", nil)
	missingRevokeCSRF.AddCookie(sessionCookie)
	missingRevokeCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingRevokeCSRFResponse, missingRevokeCSRF)
	if missingRevokeCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("key revoke without CSRF status = %d, want %d", missingRevokeCSRFResponse.Code, http.StatusForbidden)
	}
	if keys.revokeCalls != 0 {
		t.Fatal("key revoke without CSRF reached store")
	}

	credentialListRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+credentialProjectID+"/provider-credentials", nil)
	credentialListRequest.AddCookie(sessionCookie)
	credentialListResponse := httptest.NewRecorder()
	handler.ServeHTTP(credentialListResponse, credentialListRequest)
	if credentialListResponse.Code != http.StatusOK {
		t.Fatalf("credential list status = %d, want %d", credentialListResponse.Code, http.StatusOK)
	}
	if credentials.lastOwner != user.ID || credentials.lastProject != credentialProjectID {
		t.Fatal("credential list did not use session owner and path project")
	}

	missingCredentialCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+credentialProjectID+"/provider-credentials", bytes.NewBufferString(`{"provider":"openai","label":"prod","secret":"sk-test"}`))
	missingCredentialCSRF.AddCookie(sessionCookie)
	missingCredentialCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingCredentialCSRFResponse, missingCredentialCSRF)
	if missingCredentialCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("credential create without CSRF status = %d, want %d", missingCredentialCSRFResponse.Code, http.StatusForbidden)
	}
	if credentials.createCalls != 0 {
		t.Fatal("credential create without CSRF reached store")
	}

	credentialCreateRequest := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+credentialProjectID+"/provider-credentials", bytes.NewBufferString(`{"provider":"openai","label":"prod","secret":"sk-test"}`))
	credentialCreateRequest.Header.Set("Origin", "http://127.0.0.1:8081")
	credentialCreateRequest.Header.Set("X-CSRF-Token", csrfCookie.Value)
	credentialCreateRequest.AddCookie(sessionCookie)
	credentialCreateRequest.AddCookie(csrfCookie)
	credentialCreateResponse := httptest.NewRecorder()
	handler.ServeHTTP(credentialCreateResponse, credentialCreateRequest)
	if credentialCreateResponse.Code != http.StatusCreated {
		t.Fatalf("credential create status = %d, want %d; body=%s", credentialCreateResponse.Code, http.StatusCreated, credentialCreateResponse.Body.String())
	}
	if credentials.createCalls != 1 || credentials.lastOwner != user.ID || credentials.lastProject != credentialProjectID {
		t.Fatal("credential create did not use session owner and path project")
	}
}

type handlerProjectStore struct {
	lastOwner   string
	createCalls int
}

func (s *handlerProjectStore) CreateProject(_ context.Context, params project.CreateParams) (project.Project, error) {
	s.createCalls++
	s.lastOwner = params.OwnerUserID
	return project.Project{ID: "project-1", OwnerUserID: params.OwnerUserID, Name: params.Name, Slug: params.Slug, Status: project.StatusActive}, nil
}

func (s *handlerProjectStore) ListProjects(_ context.Context, ownerUserID string) ([]project.Project, error) {
	s.lastOwner = ownerUserID
	return nil, nil
}

func (s *handlerProjectStore) GetProject(context.Context, string, string) (project.Project, error) {
	return project.Project{}, project.ErrNotFound
}

func (s *handlerProjectStore) UpdateProject(context.Context, string, string, project.UpdateParams) (project.Project, error) {
	return project.Project{}, project.ErrNotFound
}

type handlerKeyStore struct {
	createCalls int
	listCalls   int
	revokeCalls int
	lastOwner   string
	lastProject string
}

func (s *handlerKeyStore) CreateKey(_ context.Context, params apikey.CreateParams) (apikey.Key, error) {
	s.createCalls++
	s.lastOwner = params.OwnerUserID
	s.lastProject = params.ProjectID
	return apikey.Key{
		ID:        "key-1",
		ProjectID: params.ProjectID,
		Name:      params.Name,
		Prefix:    params.Prefix,
		Status:    apikey.StatusActive,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *handlerKeyStore) ListKeys(_ context.Context, ownerUserID, projectID string) ([]apikey.Key, error) {
	s.listCalls++
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	return nil, nil
}

func (s *handlerKeyStore) DisableKey(context.Context, string, string, string) (apikey.Key, error) {
	return apikey.Key{}, apikey.ErrNotFound
}

func (s *handlerKeyStore) RevokeKey(context.Context, string, string, string) (apikey.Key, error) {
	s.revokeCalls++
	return apikey.Key{}, apikey.ErrNotFound
}

type handlerCredentialStore struct {
	createCalls int
	lastOwner   string
	lastProject string
}

func (s *handlerCredentialStore) CreateCredential(_ context.Context, params credential.CreateParams) (credential.Credential, error) {
	s.createCalls++
	s.lastOwner = params.OwnerUserID
	s.lastProject = params.ProjectID
	return credential.Credential{
		ID:        params.ID,
		ProjectID: params.ProjectID,
		Provider:  params.Provider,
		Label:     params.Label,
		Status:    credential.StatusActive,
		CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *handlerCredentialStore) GetCredential(_ context.Context, ownerUserID, projectID, credentialID string) (credential.Credential, error) {
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	return credential.Credential{ID: credentialID, ProjectID: projectID, Provider: credential.ProviderOpenAI}, nil
}

func (s *handlerCredentialStore) ListCredentials(_ context.Context, ownerUserID, projectID string) ([]credential.Credential, error) {
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	return nil, nil
}

func (s *handlerCredentialStore) RotateCredential(context.Context, credential.RotateParams) (credential.Credential, error) {
	return credential.Credential{}, credential.ErrNotFound
}

func (s *handlerCredentialStore) DisableCredential(context.Context, string, string, string) (credential.Credential, error) {
	return credential.Credential{}, credential.ErrNotFound
}
