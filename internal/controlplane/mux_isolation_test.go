package controlplane

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

// TestControlPlaneMuxDoesNotExposeMetrics locks the ADR-019 D2/D8 invariant
// that Prometheus endpoints never exist on the Control Plane mux. Metrics are
// mounted only on the private Operations Plane by internal/app wiring.
func TestControlPlaneMuxDoesNotExposeMetrics(t *testing.T) {
	user := User{ID: "owner-from-session", GitHubLogin: "octo"}
	sessions := newFakeSessionStore(user)
	auth := newTestAuthHandler(t, testAuthDeps{sessionStore: sessions})
	cipher, err := security.NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatalf("new credential cipher: %v", err)
	}
	handler, err := NewHandler(
		auth,
		&handlerProjectStore{},
		&handlerKeyStore{},
		bytes.Repeat([]byte{9}, 32),
		&handlerCredentialStore{},
		cipher,
		&handlerProviderConfigStore{},
		&handlerRequestStore{},
		&handlerUsageStore{},
	)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics on control plane mux = %d, want 404", response.Code)
	}
}
