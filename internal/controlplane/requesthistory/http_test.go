package requesthistory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type handlerStore struct {
	requests []Request
}

func (s *handlerStore) ListRequests(_ context.Context, _ string, params ListParams) ([]Request, error) {
	limit := params.Limit
	if limit > len(s.requests) {
		limit = len(s.requests)
	}
	return s.requests[:limit], nil
}

func (s *handlerStore) GetRequest(_ context.Context, _ string, id string) (Request, error) {
	for _, item := range s.requests {
		if item.ID == id {
			return item, nil
		}
	}
	return Request{}, ErrNotFound
}

func signedInUser(_ *http.Request) (string, bool) { return "owner-user", true }
func noUser(_ *http.Request) (string, bool)       { return "", false }

func newRequestHandler(t *testing.T, store Store) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	NewHandler(NewService(store), signedInUser).Register(mux)
	return mux
}

func TestRequestHandlerRequiresAuthentication(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	NewHandler(NewService(&handlerStore{}), noUser).Register(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/requests", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestRequestHandlerRejectsBadQueryParameters(t *testing.T) {
	t.Parallel()
	mux := newRequestHandler(t, &handlerStore{})
	cases := []string{
		"/api/v1/requests?provider=gemini",
		"/api/v1/requests?status=exploded",
		"/api/v1/requests?stream=maybe",
		"/api/v1/requests?limit=99999",
		"/api/v1/requests?from=not-a-time",
		"/api/v1/requests?cursor=@@@",
	}
	for _, path := range cases {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, response.Code)
		}
	}
}

func TestRequestHandlerReturnsPageShape(t *testing.T) {
	t.Parallel()
	store := &handlerStore{requests: sortedRequests(2)}
	mux := newRequestHandler(t, store)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/requests?limit=1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	var page struct {
		Items      []Request `json:"items"`
		NextCursor *string   `json:"next_cursor"`
		HasMore    bool      `json:"has_more"`
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Items) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("page = %+v", page)
	}
}

func TestRequestHandlerDetailNotFoundAndFound(t *testing.T) {
	t.Parallel()
	store := &handlerStore{requests: sortedRequests(1)}
	knownID := store.requests[0].ID
	mux := newRequestHandler(t, store)

	missing := httptest.NewRecorder()
	mux.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/requests/"+"00000000-0000-4000-8000-000000000000", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", missing.Code)
	}
	found := httptest.NewRecorder()
	mux.ServeHTTP(found, httptest.NewRequest(http.MethodGet, "/api/v1/requests/"+knownID, nil))
	if found.Code != http.StatusOK {
		t.Fatalf("found status = %d, want 200", found.Code)
	}
}
