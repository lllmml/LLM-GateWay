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

func TestProjectJSONResponsesAreNotCacheable(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{name: "list success", method: http.MethodGet, path: "/api/v1/projects", wantStatus: http.StatusOK},
		{name: "create success", method: http.MethodPost, path: "/api/v1/projects", body: `{"name":"Gateway","slug":"gateway"}`, wantStatus: http.StatusCreated},
		{name: "validation error", method: http.MethodPost, path: "/api/v1/projects", body: `{"name":"Gateway","slug":"invalid--slug"}`, wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			handler := NewHandler(NewService(store), func(*http.Request) (string, bool) { return "owner-from-session", true })
			mux := http.NewServeMux()
			handler.Register(mux)

			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
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
