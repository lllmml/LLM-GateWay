package controlplane

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
)

func TestControlPlaneProjectRoutesUseSessionOwnerAndCSRF(t *testing.T) {
	user := User{ID: "owner-from-session", GitHubLogin: "octo"}
	sessions := newFakeSessionStore(user)
	auth := newTestAuthHandler(t, testAuthDeps{sessionStore: sessions})
	sessionCookie, csrfCookie := seedSession(t, auth, sessions, user)
	projects := &handlerProjectStore{}
	handler := NewHandler(auth, projects)

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
