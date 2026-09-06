package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lllmml/production-go-llm-gateway/internal/pprof"
)

// TestOpsPlaneServesPprofOnlyWhenWiredAndOnlyWithToken locks the ADR-019 D8
// contract at the app boundary: /debug/pprof/ exists on the ops server ONLY
// when New() received a (token-protected) PprofHandler, and the handler
// rejects unauthenticated requests with 401.
func TestOpsPlaneServesPprofOnlyWhenWired(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := pprof.NewHandler("ops-token")
	if err != nil {
		t.Fatalf("new pprof handler: %v", err)
	}
	options := testOptions(t)
	options.PprofHandler = handler
	application := New(options, &fakeDatabase{}, logger)
	opsHandler := application.opsServer.Handler

	unauthenticated := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil)
	unauthenticatedResponse := httptest.NewRecorder()
	opsHandler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("ops /debug/pprof without token = %d, want 401", unauthenticatedResponse.Code)
	}

	authenticated := httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil)
	authenticated.Header.Set("Authorization", "Bearer ops-token")
	authenticatedResponse := httptest.NewRecorder()
	opsHandler.ServeHTTP(authenticatedResponse, authenticated)
	if authenticatedResponse.Code != http.StatusOK {
		t.Fatalf("ops /debug/pprof with token = %d, want 200", authenticatedResponse.Code)
	}
}

func TestOpsPlaneWithoutPprofHandlerRejectsPprof(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := New(testOptions(t), &fakeDatabase{}, logger)
	opsHandler := application.opsServer.Handler

	request := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	response := httptest.NewRecorder()
	opsHandler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("ops /debug/pprof without handler = %d, want 404", response.Code)
	}
}
