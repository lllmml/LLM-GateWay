//go:build integration

// Week 10 A3c primary metrics evidence: a REAL request through the running
// gateway binary produces a counted durable metric that is visible end to end:
//
//	request -> gateway_requests_total -> Prometheus query -> Grafana datasource
//	(and the auto-provisioned Gateway Overview dashboard is importable).
//
// Requires the orchestrated environment (see
// scripts/observability-metrics-evidence.sh / `make observability-metrics-
// evidence`): postgres + migrations, the observability stack (collector,
// tempo, prometheus, grafana), and the gateway binary running with the same
// .env (DATABASE_URL / CREDENTIAL_MASTER_KEY / VIRTUAL_KEY_PEPPER) and the
// data plane on 18080. When the gateway/Prometheus/Grafana endpoints are
// unreachable the test skips (plain `make integration` stays unaffected).
package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/apikey"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/credential"
	projectdomain "github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

func evidenceEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func parseUUIDOrNil(value string) pgtype.UUID {
	if parsed, err := parseUUID(value); err == nil {
		return parsed
	}
	return pgtype.UUID{}
}

func evidenceReachable(client *http.Client, url string) bool {
	response, err := client.Get(url)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// prometheusSumFromBody sums the instant-vector values in a Prometheus query
// response body.
func prometheusSumFromBody(body []byte) (float64, error) {
	var payload struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, err
	}
	var total float64
	for _, result := range payload.Data.Result {
		if len(result.Value) == 2 {
			if value, err := strconv.ParseFloat(result.Value[1].(string), 64); err == nil {
				total += value
			}
		}
	}
	return total, nil
}

func prometheusSum(client *http.Client, base, expression string) (float64, error) {
	response, err := client.Get(base + "/api/v1/query?query=" + expression)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, err
	}
	return prometheusSumFromBody(body)
}

func TestObservabilityMetricsEvidence(t *testing.T) {
	dataURL := evidenceEnv("EVIDENCE_GATEWAY_DATA_URL", "http://127.0.0.1:18080")
	opsURL := evidenceEnv("EVIDENCE_GATEWAY_OPS_URL", "http://127.0.0.1:9090")
	prometheusURL := evidenceEnv("EVIDENCE_PROMETHEUS_URL", "http://127.0.0.1:9091")
	grafanaURL := evidenceEnv("EVIDENCE_GRAFANA_URL", "http://127.0.0.1:3001")

	client := &http.Client{Timeout: 2 * time.Second}
	// Liveness/health endpoints live on the private Ops plane; chat requests go
	// to the data plane.
	if !evidenceReachable(client, opsURL+"/health/live") ||
		!evidenceReachable(client, prometheusURL+"/-/ready") ||
		!evidenceReachable(client, grafanaURL+"/api/health") {
		t.Skipf("observability/gateway stack not running (run `make observability-metrics-evidence`): ops=%s prometheus=%s grafana=%s", opsURL, prometheusURL, grafanaURL)
	}

	masterKey, err := base64.StdEncoding.DecodeString(os.Getenv("CREDENTIAL_MASTER_KEY"))
	if err != nil || len(masterKey) != 32 {
		t.Skip("CREDENTIAL_MASTER_KEY is not a 32-byte base64 key in the environment (run through the evidence make target)")
	}
	pepper, err := base64.StdEncoding.DecodeString(os.Getenv("VIRTUAL_KEY_PEPPER"))
	if err != nil || len(pepper) != 32 {
		t.Skip("VIRTUAL_KEY_PEPPER is not a 32-byte base64 key in the environment (run through the evidence make target)")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set (run through the evidence make target)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	unique := time.Now().UnixNano()
	owner, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{
		GitHubID:    1_000_000_000_000 + unique%1_000_000_000,
		GitHubLogin: fmt.Sprintf("obs-evidence-owner-%d", unique%1_000_000),
	})
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	project, err := store.CreateProject(ctx, projectdomain.CreateParams{
		OwnerUserID: owner.ID,
		Name:        "ObservabilityEvidence",
		Slug:        fmt.Sprintf("obs-evidence-%d", unique),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectID := parseUUIDOrNil(project.ID)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		for _, query := range []string{
			"DELETE FROM gateway_requests WHERE project_id = $1",
			"DELETE FROM project_provider_configs WHERE project_id = $1",
			"DELETE FROM virtual_api_keys WHERE project_id = $1",
			"DELETE FROM provider_credentials WHERE project_id = $1",
			"DELETE FROM projects WHERE id = $1",
		} {
			if projectID.Valid {
				_, _ = store.pool.Exec(cleanupCtx, query, projectID)
			}
		}
		_, _ = store.pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", parseUUIDOrNil(owner.ID))
	}()

	keyService, err := apikey.NewService(store, pepper)
	if err != nil {
		t.Fatalf("new key service: %v", err)
	}
	keyResult, err := keyService.Create(ctx, owner.ID, project.ID, "obs-evidence-client")
	if err != nil {
		t.Fatalf("create virtual key: %v", err)
	}

	cipher, err := security.NewCredentialCipher(masterKey)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	credentialService, err := credential.NewService(store, func(secret []byte, sealContext credential.SealContext) (credential.SealedSecret, error) {
		encrypted, err := cipher.Encrypt(secret, security.CredentialIdentity{
			CredentialID: sealContext.CredentialID,
			ProjectID:    sealContext.ProjectID,
			Provider:     string(sealContext.Provider),
		})
		if err != nil {
			return credential.SealedSecret{}, err
		}
		return credential.SealedSecret{Ciphertext: encrypted.Ciphertext, Nonce: encrypted.Nonce, KeyVersion: encrypted.KeyVersion}, nil
	})
	if err != nil {
		t.Fatalf("new credential service: %v", err)
	}
	cred, err := credentialService.Create(ctx, owner.ID, project.ID, "openai", "obs-evidence-upstream", "sk-mock-not-logged")
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}

	var providerCalls atomic.Int64
	mockServer := httptest.NewServer(mockChatHandler(&providerCalls))
	defer mockServer.Close()
	if _, err := store.queries.UpsertProjectProviderConfigForOwner(ctx, UpsertProjectProviderConfigForOwnerParams{
		Provider:        "openai",
		Enabled:         true,
		BaseUrlOverride: pgtype.Text{String: mockServer.URL, Valid: true},
		CredentialID:    parseUUIDOrNil(cred.ID),
		ProjectID:       projectID,
		OwnerUserID:     parseUUIDOrNil(owner.ID),
	}); err != nil {
		t.Fatalf("upsert provider config with mock base URL: %v", err)
	}

	expression := "sum(gateway_requests_total)"
	before, err := prometheusSum(client, prometheusURL, expression)
	if err != nil {
		t.Fatalf("prometheus baseline query: %v", err)
	}

	requestBody := []byte(`{"model":"openai/gpt-test","messages":[{"role":"user","content":"hello"}]}`)
	request, err := http.NewRequest(http.MethodPost, dataURL+"/v1/chat/completions", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+keyResult.RawKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("gateway request status = %d, body=%s", response.StatusCode, body)
	}
	if providerCalls.Load() == 0 {
		t.Fatal("mock provider was never called; the request did not reach upstream")
	}

	// Poll Prometheus until the scrape reflects the counted durable metric.
	var after float64
	scrapeDeadline := time.Now().Add(60 * time.Second)
	for {
		after, err = prometheusSum(client, prometheusURL, expression)
		if err == nil && after > before {
			break
		}
		if time.Now().After(scrapeDeadline) {
			t.Fatalf("gateway_requests_total never exceeded its baseline (before=%v after=%v): scrape or counting broken", before, after)
		}
		time.Sleep(2 * time.Second)
	}

	// The metric is also visible through the provisioned Grafana datasource.
	succeededExpr := `sum(gateway_requests_total{status="succeeded",provider="openai",model_family="gpt",stream="false"})`
	grafanaClient := &http.Client{Timeout: 3 * time.Second}
	grafanaQueryURL := grafanaURL + "/api/datasources/proxy/uid/prometheus/api/v1/query?query=" + succeededExpr
	grafanaDeadline := time.Now().Add(30 * time.Second)
	var grafanaValue float64
	for {
		req, _ := http.NewRequest(http.MethodGet, grafanaQueryURL, nil)
		req.SetBasicAuth(evidenceEnv("GRAFANA_ADMIN_USER", "admin"), evidenceEnv("GRAFANA_ADMIN_PASSWORD", "admin"))
		if resp, requestErr := grafanaClient.Do(req); requestErr == nil {
			proxyBody, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				if grafanaValue, err = prometheusSumFromBody(proxyBody); err == nil && grafanaValue > 0 {
					break
				}
			}
		}
		if time.Now().After(grafanaDeadline) {
			t.Fatalf("Grafana Prometheus datasource did not expose gateway_requests_total (want > 0, got %v)", grafanaValue)
		}
		time.Sleep(2 * time.Second)
	}

	// The auto-provisioned dashboard is importable (Grafana visualization).
	dashboardReq, _ := http.NewRequest(http.MethodGet, grafanaURL+"/api/dashboards/uid/gateway-overview", nil)
	dashboardReq.SetBasicAuth(evidenceEnv("GRAFANA_ADMIN_USER", "admin"), evidenceEnv("GRAFANA_ADMIN_PASSWORD", "admin"))
	dashboardDeadline := time.Now().Add(30 * time.Second)
	for {
		dashboardResp, requestErr := grafanaClient.Do(dashboardReq)
		if requestErr == nil {
			dashboardBody, readErr := io.ReadAll(dashboardResp.Body)
			_ = dashboardResp.Body.Close()
			if readErr == nil && dashboardResp.StatusCode == http.StatusOK &&
				bytes.Contains(dashboardBody, []byte("Gateway Overview (minimal)")) {
				break
			}
		}
		if time.Now().After(dashboardDeadline) {
			t.Fatal("Gateway Overview dashboard was not auto-provisioned")
		}
		time.Sleep(2 * time.Second)
	}

	t.Logf("metrics evidence ok: gateway_requests_total baseline=%v after=%v, Grafana datasource=%v, dashboard imported", before, after, grafanaValue)
}
