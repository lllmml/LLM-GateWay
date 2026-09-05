//go:build integration

// Week 9 Slice A primary evidence (HTTP / reverse-proxy level experiment).
//
// Topology under test:
//
//	test-only client (real HTTP requests, same virtual key)
//	        |
//	        v
//	test-only round-robin reverse proxy (stdlib httptest)
//	        |                 |
//	        v                 v
//	    Gateway A          Gateway B     (complete dataplane.NewHandler + NewService HTTP stacks)
//	        |                 |
//	        v                 v
//	independent ratelimit.Registry   independent ratelimit.Registry  (frozen clock, KeyRPM=20)
//	        |                 |
//	        +---- shared real PostgreSQL (durable rows, key/credential/config)
//	              +---- shared deterministic mock provider (single httptest OpenAI-compatible endpoint)
//
// This mirrors the Tech Design Phase B "two Gateway instances behind the
// reverse proxy" topology at the HTTP/state boundary. Each replica holds its
// own in-memory token bucket for the same virtual key, so quota state is
// duplicated per process. The experiment proves the claim:
//
//	distributed (T=24 split 12/12, per-key limit 20):
//	  A admits 12, B admits 12 -> HTTP 200 = 24, HTTP 429 = 0,
//	  mock provider calls = 24 > intended cluster limit 20
//
//	single-replica control (T=24 against one replica, fresh state):
//	  HTTP 200 = exactly 20, HTTP 429 = exactly 4,
//	  mock provider calls = exactly 20
//
// Determinism: both registries use one frozen clock (Registry.Config.Now), so
// x/time/rate refill is zero and outcomes are exact. Requests are sequential
// and deterministically interleaved A,B,A,B,...; provider calls are counted on
// the single shared mock endpoint; RetryMaxRetries=0 so one request == one
// provider call exactly. No production code/config/.env*/Compose is touched
// and no Redis code exists in this slice.
//
// Test location note: this experiment is a cross-layer Gateway integration
// test (HTTP -> dataplane -> rate limiter -> PostgreSQL -> provider), not a
// PostgreSQL repository behavior test. It currently lives in the postgres
// package because it deliberately reuses that package's existing integration
// harness (isolated schema + migration application + real store seeding of
// owner/project/virtual key/credential via the same package-private helpers
// the other integration tests use). Moving it to internal/dataplane would
// either force the test into an external package that must duplicate the
// isolated-schema/seeding harness or require exposing production API solely
// for test placement; neither is worth it in Week 9 Slice A. This location is
// accepted as a bounded trade-off and revisited if the harness ever moves.
package postgres

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane/apikey"
	projectdomain "github.com/lllmml/production-go-llm-gateway/internal/controlplane/project"
	"github.com/lllmml/production-go-llm-gateway/internal/dataplane"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/openai"
	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
)

const (
	experimentLimitRPM     = 20 // per-key configured limit, burst 20 (Week 8 policy)
	experimentTotal        = 24 // cluster requests
	experimentSplit        = 12 // requests routed to each replica in the distributed case
	experimentControlOK    = 20 // single-replica control: allowed requests
	experimentControlLimit = 4  // single-replica control: rejected requests (24 - 20)
)

var experimentFrozenClock = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// mockChatHandler is the single shared deterministic OpenAI-compatible
// non-stream endpoint used as the "provider". It counts every admission that
// reaches upstream.
func mockChatHandler(calls *atomic.Int64) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/v1/chat/completions" {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-Request-ID", "req_replica_experiment")
		_, _ = io.WriteString(response, `{
			"id":"chatcmpl_replica",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
		}`)
	}
}

// experimentFixture carries the seeded ownership chain (owner/project/virtual
// key/credential/provider config with the mock base URL) plus the raw key the
// test authenticates with and the cipher that decrypts the credential.
type experimentFixture struct {
	store  *Store
	rawKey string
	cipher *security.CredentialCipher
}

// seedExperimentFixture writes one owner/project/virtual-key/credential/
// provider-config chain into the isolated schema. The provider config's
// base_url_override points at the shared mock provider so the real OpenAI
// adapter is exercised end to end without any production base-URL change.
func seedExperimentFixture(t *testing.T, ctx context.Context, store *Store, upstreamURL string) *experimentFixture {
	t.Helper()
	owner, err := store.UpsertGitHubUser(ctx, controlplane.GitHubUser{GitHubID: 9101, GitHubLogin: "replica-experiment-owner"})
	if err != nil {
		t.Fatalf("upsert owner: %v", err)
	}
	project, err := store.CreateProject(ctx, projectdomain.CreateParams{OwnerUserID: owner.ID, Name: "ReplicaExperiment", Slug: "replica-experiment"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	pepper := bytes.Repeat([]byte{9}, 32)
	keyService, err := apikey.NewService(store, pepper)
	if err != nil {
		t.Fatalf("new key service: %v", err)
	}
	keyResult, err := keyService.Create(ctx, owner.ID, project.ID, "replica-experiment-client")
	if err != nil {
		t.Fatalf("create virtual key: %v", err)
	}
	cipher, credentialService := newCredentialServiceForIntegration(t, store)
	cred, err := credentialService.Create(ctx, owner.ID, project.ID, "openai", "replica-experiment-upstream", "sk-mock-not-logged")
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	ownerID, err := parseUUID(owner.ID)
	if err != nil {
		t.Fatalf("parse owner ID: %v", err)
	}
	projectID, err := parseUUID(project.ID)
	if err != nil {
		t.Fatalf("parse project ID: %v", err)
	}
	credentialID, err := parseUUID(cred.ID)
	if err != nil {
		t.Fatalf("parse credential ID: %v", err)
	}
	if _, err := store.queries.UpsertProjectProviderConfigForOwner(ctx, UpsertProjectProviderConfigForOwnerParams{
		Provider:        "openai",
		Enabled:         true,
		BaseUrlOverride: pgtype.Text{String: upstreamURL, Valid: true},
		CredentialID:    credentialID,
		ProjectID:       projectID,
		OwnerUserID:     ownerID,
	}); err != nil {
		t.Fatalf("upsert provider config with mock base URL: %v", err)
	}
	return &experimentFixture{store: store, rawKey: keyResult.RawKey, cipher: cipher}
}

// experimentRegistry builds a per-replica in-memory limiter with the key scope
// enabled at limitRPM, project scope disabled (so only the per-virtual-key
// claim is under test), and a frozen clock so refill is exactly zero and every
// outcome is deterministic.
func experimentRegistry(t *testing.T, limitRPM float64) *ratelimit.Registry {
	t.Helper()
	registry, err := ratelimit.NewRegistry(ratelimit.Config{
		KeyRPM:        limitRPM,
		ProjectRPM:    0,
		EntryCap:      1000,
		IdleTTL:       10 * time.Minute,
		SweepInterval: time.Minute,
		Now:           func() time.Time { return experimentFrozenClock },
	})
	if err != nil {
		t.Fatalf("new experiment registry: %v", err)
	}
	t.Cleanup(registry.Close)
	return registry
}

// experimentService builds one complete data plane Service wired to the real
// PostgreSQL store, the shared mock provider (through the real OpenAI adapter),
// the seed cipher/pepper, and the given per-replica limiter. Retries are
// disabled so one allowed request is exactly one provider call.
func experimentService(t *testing.T, store *Store, cipher *security.CredentialCipher, providerRegistry *provider.Registry, limiter *ratelimit.Registry) *dataplane.Service {
	t.Helper()
	service, err := dataplane.NewService(dataplane.Options{
		Store:                     store,
		VirtualKeyPepper:          bytes.Repeat([]byte{9}, 32),
		CredentialCipher:          cipher,
		UpstreamRequestTimeout:    5 * time.Second,
		UpstreamStreamMaxDuration: 5 * time.Second,
		ProviderRegistry:          providerRegistry,
		RateLimiter:               limiter,
		RetryMaxRetries:           0,
	})
	if err != nil {
		t.Fatalf("new data plane service: %v", err)
	}
	return service
}

// experimentGateway wraps a Service in a complete HTTP stack.
func experimentGateway(t *testing.T, service *dataplane.Service) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	dataplane.NewHandler(service).Register(mux)
	gateway := httptest.NewServer(mux)
	t.Cleanup(gateway.Close)
	return gateway
}

// roundRobinProxy is the test-only reverse proxy that deterministically
// alternates client requests between two gateway replicas and tags each
// response with the replica that served it (X-Backend).
type roundRobinProxy struct {
	backends []string
	mu       sync.Mutex
	next     int
	client   *http.Client
}

func newRoundRobinProxy(t *testing.T, backends []string) *httptest.Server {
	t.Helper()
	proxy := &roundRobinProxy{
		backends: backends,
		client:   &http.Client{},
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server
}

func (p *roundRobinProxy) pick() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	index := p.next % len(p.backends)
	p.next++
	return p.backends[index], index
}

func (p *roundRobinProxy) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	backend, index := p.pick()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(response, "read request body", http.StatusBadRequest)
		return
	}
	target := backend + request.URL.Path
	if request.URL.RawQuery != "" {
		target += "?" + request.URL.RawQuery
	}
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(response, "build upstream request", http.StatusBadGateway)
		return
	}
	upstream.Header = request.Header.Clone()
	upstreamResponse, err := p.client.Do(upstream)
	if err != nil {
		http.Error(response, "gateway unreachable", http.StatusBadGateway)
		return
	}
	defer upstreamResponse.Body.Close()
	upstreamBody, err := io.ReadAll(upstreamResponse.Body)
	if err != nil {
		http.Error(response, "read gateway response", http.StatusBadGateway)
		return
	}
	for name, values := range upstreamResponse.Header {
		for _, value := range values {
			response.Header().Add(name, value)
		}
	}
	if index%2 == 0 {
		response.Header().Set("X-Backend", "A")
	} else {
		response.Header().Set("X-Backend", "B")
	}
	response.WriteHeader(upstreamResponse.StatusCode)
	_, _ = response.Write(upstreamBody)
}

const experimentRequestBody = `{"model":"openai/gpt-test","messages":[{"role":"user","content":"hello"}],"stream":false}`

// experimentPost sends one real chat-completions request and returns status,
// serving backend ("" when absent), and a bounded error body snippet.
func experimentPost(t *testing.T, client *http.Client, targetURL, rawKey string) (int, string, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, targetURL+"/v1/chat/completions", bytes.NewBufferString(experimentRequestBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+rawKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return response.StatusCode, response.Header.Get("X-Backend"), string(body)
}

func countExperimentRequests(t *testing.T, ctx context.Context, store *Store) int64 {
	t.Helper()
	var count int64
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM gateway_requests").Scan(&count); err != nil {
		t.Fatalf("count gateway_requests: %v", err)
	}
	return count
}

// TestReplicaRateLimitInconsistencyHTTPLevel is the Week 9 Slice A primary
// evidence: two complete gateway replicas behind a test-only reverse proxy,
// each with its own in-memory limiter, sharing real PostgreSQL and one mock
// provider. The cluster admits all 24 requests (12 per replica) although the
// intended per-key cluster limit is 20.
func TestReplicaRateLimitInconsistencyHTTPLevel(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	var providerCalls atomic.Int64
	upstream := httptest.NewServer(mockChatHandler(&providerCalls))
	defer upstream.Close()

	fixture := seedExperimentFixture(t, ctx, store, upstream.URL)

	providerRegistry, err := provider.NewRegistry(map[provider.Name]provider.Client{
		provider.OpenAI: openai.New(upstream.Client()),
	})
	if err != nil {
		t.Fatalf("new provider registry: %v", err)
	}

	serviceA := experimentService(t, store, fixture.cipher, providerRegistry, experimentRegistry(t, experimentLimitRPM))
	serviceB := experimentService(t, store, fixture.cipher, providerRegistry, experimentRegistry(t, experimentLimitRPM))
	gatewayA := experimentGateway(t, serviceA)
	gatewayB := experimentGateway(t, serviceB)
	proxy := newRoundRobinProxy(t, []string{gatewayA.URL, gatewayB.URL})

	client := &http.Client{Timeout: 15 * time.Second}
	perBackendOK := map[string]int{}
	statusCounts := map[int]int{}
	for i := 0; i < experimentTotal; i++ {
		status, backend, snippet := experimentPost(t, client, proxy.URL, fixture.rawKey)
		statusCounts[status]++
		if status == http.StatusOK && backend != "" {
			perBackendOK[backend]++
		}
		if status != http.StatusOK {
			t.Fatalf("request %d: status = %d (want 200), body = %s", i, status, snippet)
		}
	}

	providerCallsTotal := providerCalls.Load()
	rows := countExperimentRequests(t, ctx, store)
	t.Logf("distributed evidence (limit = %d per key):", experimentLimitRPM)
	t.Logf("  replica A admitted = %d (HTTP 200), rejected = %d", perBackendOK["A"], 0)
	t.Logf("  replica B admitted = %d (HTTP 200), rejected = %d", perBackendOK["B"], 0)
	t.Logf("  total HTTP 200 = %d, total HTTP 429 = %d", statusCounts[http.StatusOK], statusCounts[http.StatusTooManyRequests])
	t.Logf("  mock provider calls = %d > intended cluster limit %d", providerCallsTotal, experimentLimitRPM)
	t.Logf("  durable gateway_requests rows = %d", rows)

	if statusCounts[http.StatusOK] != experimentTotal {
		t.Fatalf("total HTTP 200 = %d, want %d", statusCounts[http.StatusOK], experimentTotal)
	}
	if statusCounts[http.StatusTooManyRequests] != 0 {
		t.Fatalf("total HTTP 429 = %d, want 0", statusCounts[http.StatusTooManyRequests])
	}
	if perBackendOK["A"] != experimentSplit || perBackendOK["B"] != experimentSplit {
		t.Fatalf("per-replica admitted A=%d B=%d, want %d each", perBackendOK["A"], perBackendOK["B"], experimentSplit)
	}
	if providerCallsTotal != experimentTotal {
		t.Fatalf("mock provider calls = %d, want %d", providerCallsTotal, experimentTotal)
	}
	if providerCallsTotal <= experimentLimitRPM {
		t.Fatalf("mock provider calls = %d, want > %d to demonstrate cluster over-admission", providerCallsTotal, experimentLimitRPM)
	}
}

// TestReplicaRateLimitSingleReplicaControl is the Week 9 Slice A control: the
// exact same client load against ONE replica with a fresh limiter state is
// throttled (20 allowed, 4 rejected) and the mock provider sees exactly 20
// calls - proving the same gateway machinery does enforce the limit when quota
// state is not duplicated across processes.
func TestReplicaRateLimitSingleReplicaControl(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	var providerCalls atomic.Int64
	upstream := httptest.NewServer(mockChatHandler(&providerCalls))
	defer upstream.Close()

	fixture := seedExperimentFixture(t, ctx, store, upstream.URL)

	providerRegistry, err := provider.NewRegistry(map[provider.Name]provider.Client{
		provider.OpenAI: openai.New(upstream.Client()),
	})
	if err != nil {
		t.Fatalf("new provider registry: %v", err)
	}

	service := experimentService(t, store, fixture.cipher, providerRegistry, experimentRegistry(t, experimentLimitRPM))
	gateway := experimentGateway(t, service)

	client := &http.Client{Timeout: 15 * time.Second}
	ok, rejected := 0, 0
	for i := 0; i < experimentTotal; i++ {
		status, _, snippet := experimentPost(t, client, gateway.URL, fixture.rawKey)
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			rejected++
		default:
			t.Fatalf("request %d: unexpected status = %d, body = %s", i, status, snippet)
		}
	}

	providerCallsTotal := providerCalls.Load()
	rows := countExperimentRequests(t, ctx, store)
	t.Logf("single-replica control evidence (limit = %d per key):", experimentLimitRPM)
	t.Logf("  HTTP 200 = exactly %d, HTTP 429 = exactly %d", ok, rejected)
	t.Logf("  mock provider calls = exactly %d", providerCallsTotal)
	t.Logf("  durable gateway_requests rows = %d", rows)

	if ok != experimentControlOK {
		t.Fatalf("HTTP 200 = %d, want exactly %d", ok, experimentControlOK)
	}
	if rejected != experimentControlLimit {
		t.Fatalf("HTTP 429 = %d, want exactly %d", rejected, experimentControlLimit)
	}
	if providerCallsTotal != experimentControlOK {
		t.Fatalf("mock provider calls = %d, want exactly %d", providerCallsTotal, experimentControlOK)
	}
}
