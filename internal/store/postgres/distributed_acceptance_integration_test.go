//go:build integration

// Week 9 Slice B2b acceptance: the distributed limiter wired through the real
// data plane at the HTTP level, against the pinned Redis (7.4.11), real
// PostgreSQL, and the shared deterministic mock provider.
//
//  1. Two replicas share Redis: the exact motivating cluster-quota case
//     (Slice A allowed 24; shared distributed quota must allow exactly 20).
//  2. Redis becomes unavailable: the replica degrades, the gateway stays
//     alive behind a bounded local emergency limiter, and no Redis error ever
//     leaks to the HTTP client.
//  3. Redis recovers: non-mutating probes qualify, a real admission returns
//     the replica to normal, and the exact distributed quota resumes.
package postgres

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/openai"
	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit/distributed"
)

const replicaExperimentKeyName = "replica-experiment-client"

// redisRealAddr returns the pinned Redis host:port from REDIS_URL.
func redisRealAddr(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Fatal("REDIS_URL is required for distributed acceptance tests")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	return options.Addr
}

// redisTestClient builds a go-redis client with the pinned production posture.
func redisTestClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:                  addr,
		MaxRetries:            -1, // never retransmit an ambiguous mutating admission
		ContextTimeoutEnabled: true,
		DialTimeout:           time.Second,
		ReadTimeout:           time.Second,
		WriteTimeout:          time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Fatalf("ping redis %s: %v", addr, err)
	}
	info, err := client.Info(ctx, "server").Result()
	if err != nil || !strings.Contains(info, "redis_version:7.4.11") {
		client.Close()
		t.Fatalf("acceptance tests pin Redis 7.4.11; got info=%q err=%v", info, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// distributedWrapper builds a wrapper over the given client and registers its
// Close (which runs before the client's cleanup, preserving shutdown order).
func distributedWrapper(t *testing.T, client *redis.Client, keyRPM int, probeInterval time.Duration, probeThreshold int) *distributed.Limiter {
	t.Helper()
	limiter, err := distributed.NewLimiter(client, distributed.WrapperConfig{
		KeyRPM:         keyRPM,
		IdleTTL:        time.Minute,
		CommandTimeout: 500 * time.Millisecond,
		ReplicaFactor:  2,
		ProbeInterval:  probeInterval,
		ProbeThreshold: probeThreshold,
	})
	if err != nil {
		t.Fatalf("new distributed limiter: %v", err)
	}
	t.Cleanup(limiter.Close)
	return limiter
}

// deleteOwnedLimiterKeys removes only this fixture's limiter keys (never
// FLUSHDB), keyed off the DB-owned UUIDs so tests stay isolated.
func deleteOwnedLimiterKeys(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	var keyID, projectID string
	err := store.pool.QueryRow(ctx, `
		SELECT k.id::text, p.id::text
		FROM virtual_api_keys k
		JOIN projects p ON p.id = k.project_id
		WHERE k.name = $1 LIMIT 1`, replicaExperimentKeyName).Scan(&keyID, &projectID)
	if err != nil {
		return // schema may not exist yet (caller cleans up); nothing to delete
	}
	client := redisTestClient(t, redisRealAddr(t))
	_ = client.Del(ctx, distributed.ScopeKey(projectID, keyID), distributed.ProjectScopeKey(projectID)).Err()
}

// TestDistributedAcceptanceTwoReplicasSharedRedisExactClusterQuota runs the
// Week 9 motivating case over shared Redis: KeyRPM 20, 24 requests split 12/12
// across two independent distributed.Limiter instances sharing one Redis. The
// cluster must allow exactly 20 and reject 4 - the Slice A local-registry
// over-admission (24) is gone.
func TestDistributedAcceptanceTwoReplicasSharedRedisExactClusterQuota(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	var providerCalls atomic.Int64
	upstream := httptest.NewServer(mockChatHandler(&providerCalls))
	defer upstream.Close()
	fixture := seedExperimentFixture(t, ctx, store, upstream.URL)
	t.Cleanup(func() { deleteOwnedLimiterKeys(t, ctx, store) })

	redisClient := redisTestClient(t, redisRealAddr(t))
	limiterA := distributedWrapper(t, redisClient, 20, time.Minute, 3)
	limiterB := distributedWrapper(t, redisClient, 20, time.Minute, 3)

	providerRegistry, err := provider.NewRegistry(map[provider.Name]provider.Client{
		provider.OpenAI: openai.New(upstream.Client()),
	})
	if err != nil {
		t.Fatalf("new provider registry: %v", err)
	}
	serviceA := experimentService(t, store, fixture.cipher, providerRegistry, limiterA)
	serviceB := experimentService(t, store, fixture.cipher, providerRegistry, limiterB)
	gatewayA := experimentGateway(t, serviceA)
	gatewayB := experimentGateway(t, serviceB)
	proxy := newRoundRobinProxy(t, []string{gatewayA.URL, gatewayB.URL})

	client := &http.Client{Timeout: 15 * time.Second}
	statusCounts := map[int]int{}
	for i := 0; i < 24; i++ {
		status, _, snippet := experimentPost(t, client, proxy.URL, fixture.rawKey)
		statusCounts[status]++
		if status != http.StatusOK && status != http.StatusTooManyRequests {
			t.Fatalf("request %d: unexpected status %d, body %s", i, status, snippet)
		}
	}

	rows := countExperimentRequests(t, ctx, store)
	t.Logf("two-replica shared Redis evidence (KeyRPM=20, 24 split 12/12):")
	t.Logf("  HTTP 200 = %d, HTTP 429 = %d", statusCounts[http.StatusOK], statusCounts[http.StatusTooManyRequests])
	t.Logf("  provider calls = %d", providerCalls.Load())
	t.Logf("  durable gateway_requests rows = %d", rows)

	if statusCounts[http.StatusOK] != 20 {
		t.Fatalf("cluster allowed = %d, want exactly 20 (shared distributed quota)", statusCounts[http.StatusOK])
	}
	if statusCounts[http.StatusTooManyRequests] != 4 {
		t.Fatalf("cluster rejected = %d, want exactly 4", statusCounts[http.StatusTooManyRequests])
	}
	if providerCalls.Load() != 20 {
		t.Fatalf("provider calls = %d, want 20", providerCalls.Load())
	}
	if rows != 20 {
		t.Fatalf("durable rows = %d, want 20", rows)
	}
}

// togglingRelay proxies client <-> real Redis and can be flipped down to make
// Redis deterministically unavailable (existing connections are severed so the
// go-redis pool cannot silently reuse a healthy one).
type togglingRelay struct {
	upstream string
	listener net.Listener
	down     atomic.Bool
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

func newTogglingRelay(t *testing.T, upstream string) *togglingRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	relay := &togglingRelay{upstream: upstream, listener: listener, conns: map[net.Conn]struct{}{}}
	go relay.acceptLoop()
	t.Cleanup(func() { _ = listener.Close() })
	return relay
}

func (r *togglingRelay) acceptLoop() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return
		}
		if r.down.Load() {
			_ = conn.Close()
			continue
		}
		upstream, err := net.Dial("tcp", r.upstream)
		if err != nil {
			_ = conn.Close()
			continue
		}
		r.mu.Lock()
		r.conns[conn] = struct{}{}
		r.mu.Unlock()
		go r.relay(conn, upstream)
	}
}

func (r *togglingRelay) relay(conn, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
	_ = conn.Close()
	_ = upstream.Close()
}

func (r *togglingRelay) setDown(down bool) {
	r.down.Store(down)
	if !down {
		return
	}
	r.mu.Lock()
	for conn := range r.conns {
		_ = conn.Close()
	}
	r.conns = map[net.Conn]struct{}{}
	r.mu.Unlock()
}

// TestDistributedAcceptanceDegradedThenRecoveryHTTP proves the gateway stays
// alive when Redis becomes unavailable (bounded emergency, no Redis leakage to
// clients) and returns to normal distributed quota after Redis recovers.
func TestDistributedAcceptanceDegradedThenRecoveryHTTP(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newMigratedStore(t, ctx)
	defer cleanup()

	var providerCalls atomic.Int64
	upstream := httptest.NewServer(mockChatHandler(&providerCalls))
	defer upstream.Close()
	fixture := seedExperimentFixture(t, ctx, store, upstream.URL)
	t.Cleanup(func() { deleteOwnedLimiterKeys(t, ctx, store) })

	relay := newTogglingRelay(t, redisRealAddr(t))
	redisClient := redisTestClient(t, relay.listener.Addr().String())
	limiter := distributedWrapper(t, redisClient, 6, 30*time.Millisecond, 2)

	providerRegistry, err := provider.NewRegistry(map[provider.Name]provider.Client{
		provider.OpenAI: openai.New(upstream.Client()),
	})
	if err != nil {
		t.Fatalf("new provider registry: %v", err)
	}
	service := experimentService(t, store, fixture.cipher, providerRegistry, limiter)
	gateway := experimentGateway(t, service)
	client := &http.Client{Timeout: 15 * time.Second}

	post := func() (int, string) {
		status, _, snippet := experimentPost(t, client, gateway.URL, fixture.rawKey)
		if status == http.StatusInternalServerError || status == http.StatusBadGateway {
			t.Fatalf("gateway leaked a raw error to the client: status %d body %s", status, snippet)
		}
		if status != http.StatusOK && status != http.StatusTooManyRequests {
			t.Fatalf("unexpected status %d body %s", status, snippet)
		}
		return status, snippet
	}

	// Healthy phase: distributed admission allowed.
	if status, _ := post(); status != http.StatusOK {
		t.Fatalf("healthy request status = %d", status)
	}

	// Redis becomes unavailable. The first genuine dependency failure degrades
	// the replica; the current request is served by the emergency limiter
	// (KeyRPM 6 / factor 2 -> emergency quota 3), then further requests beyond
	// the emergency quota get a normal rate_limited 429.
	relay.setDown(true)
	emergencyAllowed := 0
	for i := 0; i < 4; i++ {
		status, snippet := post()
		if status == http.StatusOK {
			emergencyAllowed++
		}
		if status == http.StatusTooManyRequests {
			if snippet == "" {
				t.Fatal("429 carried no body")
			}
		}
	}
	if emergencyAllowed != 3 {
		t.Fatalf("emergency allows while degraded = %d, want exactly 3 (bounded)", emergencyAllowed)
	}
	rowsDegraded := countExperimentRequests(t, ctx, store)
	t.Logf("degraded evidence: emergency allowed=3, durable rows=%d", rowsDegraded)
	if rowsDegraded != 4 { // 1 healthy + 3 emergency-allowed (the 4th was rejected)
		t.Fatalf("durable rows after degraded phase = %d, want 4", rowsDegraded)
	}

	// Redis recovers. Probes qualify (interval 30ms, threshold 2) -> recovering
	// -> the next real request performs one real admission -> normal. Poll for
	// the recovery 200 (requests before it are bounded 429s from emergency).
	relay.setDown(false)
	var statusAfterRecovery int
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, _ := post()
		if status == http.StatusOK {
			statusAfterRecovery = status
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica did not return to normal after Redis recovery")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if statusAfterRecovery != http.StatusOK {
		t.Fatalf("post-recovery request status = %d", statusAfterRecovery)
	}

	// Normal distributed quota resumes. KeyRPM 6 bucket: 1 consumed healthy +
	// 1 recovery admission -> 4 remain -> four 200s then one rate_limited 429.
	next := []int{http.StatusOK, http.StatusOK, http.StatusOK, http.StatusOK, http.StatusTooManyRequests}
	for i, want := range next {
		if got, _ := post(); got != want {
			t.Fatalf("post-recovery request %d status = %d, want %d (exact distributed quota)", i, got, want)
		}
	}
	// Provider calls: 1 healthy + 3 emergency + 1 recovery + 4 = 9; durable rows
	// match allowed admissions (the final 429 creates none).
	if providerCalls.Load() != 9 {
		t.Fatalf("provider calls = %d, want 9", providerCalls.Load())
	}
	if rows := countExperimentRequests(t, ctx, store); rows != 9 {
		t.Fatalf("durable rows = %d, want 9", rows)
	}
}
