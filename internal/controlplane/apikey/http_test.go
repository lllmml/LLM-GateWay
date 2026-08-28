package apikey

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

func TestCreateReturnsRawKeyOnceAndNoStoreCaching(t *testing.T) {
	store := &httpStore{}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-1", true })

	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-1/keys", strings.NewReader(`{"name":" local-dev "}`))
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
	rawKey, ok := body["key"].(string)
	if !ok || !strings.HasPrefix(rawKey, "pgw_") {
		t.Fatal("creation response did not contain the shown-once key")
	}
	if strings.Count(response.Body.String(), rawKey) != 1 {
		t.Fatal("creation response did not contain the raw key exactly once")
	}
	if _, exists := body["key_hash"]; exists {
		t.Fatal("creation response exposed key_hash")
	}
	if store.created.OwnerUserID != "owner-1" || store.created.ProjectID != "project-1" || store.created.Name != "local-dev" {
		t.Fatalf("create params did not preserve authenticated ownership and normalized input")
	}
	if len(store.created.KeyHash) != 32 {
		t.Fatalf("stored digest length = %d, want 32", len(store.created.KeyHash))
	}
}

func TestListReturnsMetadataOnly(t *testing.T) {
	now := time.Now().UTC()
	store := &httpStore{keys: []Key{{
		ID: "key-1", Name: "local-dev", Prefix: "safe1234", Status: StatusActive, CreatedAt: now,
	}}}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-1", true })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/keys", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var body struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Keys) != 1 {
		t.Fatalf("keys length = %d, want 1", len(body.Keys))
	}
	for _, forbidden := range []string{"key", "raw_key", "key_hash", "project_id", "owner_user_id"} {
		if _, exists := body.Keys[0][forbidden]; exists {
			t.Fatalf("list response exposed %s", forbidden)
		}
	}
	for _, required := range []string{"id", "name", "prefix", "status", "created_at", "last_used_at", "revoked_at"} {
		if _, exists := body.Keys[0][required]; !exists {
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

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/keys", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if store.calls != 0 {
		t.Fatal("unauthenticated request reached store")
	}
}

func TestMutationMapsCrossOwnerToNotFoundWithoutLeak(t *testing.T) {
	store := &httpStore{err: ErrNotFound}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-2", true })

	for _, test := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/v1/projects/project-1/keys/key-1/disable"},
		{method: http.MethodDelete, path: "/api/v1/projects/project-1/keys/key-1"},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want %d", test.method, response.Code, http.StatusNotFound)
		}
		if strings.Contains(strings.ToLower(response.Body.String()), "owner") {
			t.Fatalf("response leaked ownership detail: %s", response.Body.String())
		}
	}
}

func TestDisableAndRevokeResponsesNeverContainRawKey(t *testing.T) {
	store := &httpStore{}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-1", true })

	for _, test := range []struct {
		method     string
		path       string
		wantStatus Status
	}{
		{method: http.MethodPost, path: "/api/v1/projects/project-1/keys/key-1/disable", wantStatus: StatusDisabled},
		{method: http.MethodDelete, path: "/api/v1/projects/project-1/keys/key-1", wantStatus: StatusRevoked},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d; body=%s", test.method, response.Code, http.StatusOK, response.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["status"] != string(test.wantStatus) {
			t.Fatalf("status = %v, want %s", body["status"], test.wantStatus)
		}
		for _, forbidden := range []string{"key", "raw_key", "key_hash"} {
			if _, exists := body[forbidden]; exists {
				t.Fatalf("mutation response exposed %s", forbidden)
			}
		}
	}
}

func TestCreateRejectsUnknownFields(t *testing.T) {
	store := &httpStore{}
	handler := newHTTPTestHandler(t, store, func(*http.Request) (string, bool) { return "owner-1", true })

	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-1/keys", bytes.NewBufferString(`{"name":"local","owner_user_id":"attacker"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if store.calls != 0 {
		t.Fatal("invalid request reached store")
	}
}

func newHTTPTestHandler(t *testing.T, store Store, currentUserID CurrentUserID) http.Handler {
	t.Helper()
	service, err := NewService(store, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	mux := http.NewServeMux()
	NewHandler(service, currentUserID).Register(mux)
	return mux
}

type httpStore struct {
	created     CreateParams
	keys        []Key
	err         error
	calls       int
	lastOwner   string
	lastProject string
}

func (s *httpStore) CreateKey(_ context.Context, params CreateParams) (Key, error) {
	s.calls++
	s.created = params
	if s.err != nil {
		return Key{}, s.err
	}
	return Key{ID: "key-1", ProjectID: params.ProjectID, Name: params.Name, Prefix: params.Prefix, Status: StatusActive, CreatedAt: time.Now().UTC()}, nil
}

func (s *httpStore) ListKeys(_ context.Context, ownerUserID, projectID string) ([]Key, error) {
	s.calls++
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	return s.keys, s.err
}

func (s *httpStore) DisableKey(_ context.Context, ownerUserID, projectID, _ string) (Key, error) {
	s.calls++
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	if s.err != nil {
		return Key{}, s.err
	}
	return Key{ID: "key-1", Name: "local", Prefix: "safe1234", Status: StatusDisabled, CreatedAt: time.Now().UTC()}, nil
}

func (s *httpStore) RevokeKey(_ context.Context, ownerUserID, projectID, _ string) (Key, error) {
	s.calls++
	s.lastOwner = ownerUserID
	s.lastProject = projectID
	if s.err != nil {
		return Key{}, s.err
	}
	now := time.Now().UTC()
	return Key{ID: "key-1", Name: "local", Prefix: "safe1234", Status: StatusRevoked, CreatedAt: now, RevokedAt: &now}, nil
}

var _ Store = (*httpStore)(nil)
