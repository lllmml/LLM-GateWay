package project

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerRequiresAuthenticationBeforeStore(t *testing.T) {
	store := &fakeStore{}
	handler := NewHandler(NewService(store), func(*http.Request) (string, bool) { return "", false })
	mux := http.NewServeMux()
	handler.Register(mux)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if len(store.owners) != 0 {
		t.Fatal("unauthenticated request reached store")
	}
}

func TestHandlerUsesAuthenticatedOwnerNotRequestData(t *testing.T) {
	store := &fakeStore{}
	handler := NewHandler(NewService(store), func(*http.Request) (string, bool) { return "owner-from-session", true })
	mux := http.NewServeMux()
	handler.Register(mux)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewBufferString(`{"name":"Gateway","slug":"gateway","owner_user_id":"attacker"}`))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want unknown owner field rejected with %d", response.Code, http.StatusBadRequest)
	}
	if store.createCalls != 0 {
		t.Fatal("invalid request reached store")
	}
}

func TestHandlerMapsCrossOwnerLookupToNotFound(t *testing.T) {
	store := &errorStore{err: ErrNotFound}
	handler := NewHandler(NewService(store), func(*http.Request) (string, bool) { return "owner-2", true })
	mux := http.NewServeMux()
	handler.Register(mux)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-owned-by-user-1", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if strings.Contains(response.Body.String(), "owner") {
		t.Fatalf("response leaked ownership detail: %s", response.Body.String())
	}
}

type errorStore struct {
	err error
}

func (s *errorStore) CreateProject(context.Context, CreateParams) (Project, error) {
	return Project{}, s.err
}

func (s *errorStore) ListProjects(context.Context, string) ([]Project, error) {
	return nil, s.err
}

func (s *errorStore) GetProject(context.Context, string, string) (Project, error) {
	return Project{}, s.err
}

func (s *errorStore) UpdateProject(context.Context, string, string, UpdateParams) (Project, error) {
	return Project{}, s.err
}
