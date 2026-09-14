package pprof

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	handler, err := NewHandler("secret-token")
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func authedRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Authorization", "Bearer secret-token")
	return request
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestNewHandlerRequiresToken(t *testing.T) {
	if handler, err := NewHandler("  "); err == nil || handler != nil {
		t.Fatal("NewHandler with a blank token must fail")
	}
	if _, err := NewHandler("some-token"); err != nil {
		t.Fatalf("NewHandler with a token: %v", err)
	}
}

func TestPprofRequiresToken(t *testing.T) {
	handler := newTestHandler(t)

	for _, target := range []string{"/debug/pprof/", "/debug/pprof/heap?debug=1", "/debug/pprof/profile?seconds=1"} {
		missing := serve(handler, httptest.NewRequest(http.MethodGet, target, nil))
		if missing.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token = %d, want 401", target, missing.Code)
		}
		wrong := httptest.NewRequest(http.MethodGet, target, nil)
		wrong.Header.Set("Authorization", "Bearer wrong-token")
		if got := serve(handler, wrong); got.Code != http.StatusUnauthorized {
			t.Fatalf("%s with wrong token = %d, want 401", target, got.Code)
		}
	}
}

func TestIndexAndNamedProfiles(t *testing.T) {
	handler := newTestHandler(t)

	index := serve(handler, authedRequest(t, http.MethodGet, "/debug/pprof/"))
	if index.Code != http.StatusOK {
		t.Fatalf("index = %d, want 200", index.Code)
	}
	for _, endpoint := range []string{"goroutine", "heap", "allocs", "mutex"} {
		if !strings.Contains(index.Body.String(), endpoint) {
			t.Fatalf("index does not list %s", endpoint)
		}
	}

	goroutine := serve(handler, authedRequest(t, http.MethodGet, "/debug/pprof/goroutine?debug=1"))
	if goroutine.Code != http.StatusOK || !strings.Contains(goroutine.Body.String(), "goroutine profile: total") {
		t.Fatalf("goroutine profile = %d, want 200 with profile header", goroutine.Code)
	}

	unknown := serve(handler, authedRequest(t, http.MethodGet, "/debug/pprof/not-a-profile"))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown profile = %d, want 404", unknown.Code)
	}

	badDebug := serve(handler, authedRequest(t, http.MethodGet, "/debug/pprof/heap?debug=9"))
	if badDebug.Code != http.StatusBadRequest {
		t.Fatalf("bad debug param = %d, want 400", badDebug.Code)
	}

	methodNotAllowed := serve(handler, authedRequest(t, http.MethodPost, "/debug/pprof/heap"))
	if methodNotAllowed.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", methodNotAllowed.Code)
	}
}

func TestCPUProfileBoundsAndConcurrency(t *testing.T) {
	handler := newTestHandler(t)

	for _, seconds := range []string{"abc", "-3", "0", "10000"} {
		request := authedRequest(t, http.MethodGet, "/debug/pprof/profile?seconds="+seconds)
		if got := serve(handler, request); got.Code != http.StatusBadRequest {
			t.Fatalf("seconds=%s = %d, want 400", seconds, got.Code)
		}
	}

	// Deterministic concurrency check: while cpuLock is held, a profile request
	// must be rejected with 409 (and never panic) instead of fighting over the
	// process-wide CPU profile.
	handler.cpuLock.Lock()
	conflict := serve(handler, authedRequest(t, http.MethodGet, "/debug/pprof/profile?seconds=1"))
	handler.cpuLock.Unlock()
	if conflict.Code != http.StatusConflict {
		t.Fatalf("concurrent CPU profile = %d, want 409", conflict.Code)
	}

	// A real short CPU profile round trip.
	recorder := httptest.NewRecorder()
	request := authedRequest(t, http.MethodGet, "/debug/pprof/profile?seconds=1")
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CPU profile did not finish within 5s")
	}
	if recorder.Code != http.StatusOK || recorder.Body.Len() == 0 {
		t.Fatalf("CPU profile = %d len=%d, want 200 with body", recorder.Code, recorder.Body.Len())
	}
}
