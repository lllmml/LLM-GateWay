//go:build integration

package distributed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Integration tests run against the pinned Redis image (docker-compose.yml:
// redis:7.4.11-alpine), reached through REDIS_URL (Makefile default
// redis://127.0.0.1:6379/0). Redis is used as an ephemeral, shared local
// instance; tests use unique key namespaces and only DEL/UNLINK their own
// keys - never an unconditional FLUSHDB (ADR-018 D11).

// testRedisClient dials the pinned Redis, applies the production retry posture
// (MaxRetries: -1 disables command retransmission in go-redis v9.22.0 - 0
// would normalize to the default 3 retries), and closes on cleanup.
func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Fatal("REDIS_URL is required for distributed integration tests (make redis-up)")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	options.MaxRetries = -1
	options.DialTimeout = 2 * time.Second
	options.ReadTimeout = 2 * time.Second
	options.WriteTimeout = 2 * time.Second
	client := redis.NewClient(options)
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		client.Close()
		t.Fatalf("ping pinned Redis %s: %v (run `make redis-up`)", options.Addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// newTestLimiter builds a limiter on the shared test Redis with a command
// timeout generous enough for slow CI.
func newTestLimiter(t *testing.T, cfg Config) (*Limiter, *redis.Client) {
	t.Helper()
	client := testRedisClient(t)
	if cfg.CommandTimeout == 0 {
		cfg.CommandTimeout = 3 * time.Second
	}
	limiter, err := New(client, cfg)
	if err != nil {
		t.Fatalf("new limiter: %v", err)
	}
	return limiter, client
}

func uniqueIDs(t *testing.T) (projectID, keyID string) {
	t.Helper()
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatalf("random ids: %v", err)
	}
	suffix := hex.EncodeToString(buffer)
	return "proj-" + suffix, "key-" + suffix
}

func serverNowMS(t *testing.T, ctx context.Context, client *redis.Client) int64 {
	t.Helper()
	serverTime, err := client.Time(ctx).Result()
	if err != nil {
		t.Fatalf("read redis TIME: %v", err)
	}
	return serverTime.UnixMilli()
}

// setScopeState writes a scope's stored tokens/ts directly (white-box
// determinism for refill / monotonic-clock / clamp scenarios; no FLUSHDB).
func setScopeState(t *testing.T, ctx context.Context, client *redis.Client, scopeKey string, tokens, ts int64) {
	t.Helper()
	if err := client.HSet(ctx, scopeKey, "t", strconv.FormatInt(tokens, 10), "s", strconv.FormatInt(ts, 10)).Err(); err != nil {
		t.Fatalf("set scope state %s: %v", scopeKey, err)
	}
}

func readScopeState(t *testing.T, ctx context.Context, client *redis.Client, scopeKey string) (tokens, ts int64, present bool) {
	t.Helper()
	row, err := client.HMGet(ctx, scopeKey, "t", "s").Result()
	if err != nil {
		t.Fatalf("read scope state %s: %v", scopeKey, err)
	}
	if row[0] == nil || row[1] == nil {
		return 0, 0, false
	}
	tokens, err = strconv.ParseInt(fmt.Sprint(row[0]), 10, 64)
	if err != nil {
		t.Fatalf("parse tokens %v: %v", row[0], err)
	}
	ts, err = strconv.ParseInt(fmt.Sprint(row[1]), 10, 64)
	if err != nil {
		t.Fatalf("parse ts %v: %v", row[1], err)
	}
	return tokens, ts, true
}

func scopeTTL(t *testing.T, ctx context.Context, client *redis.Client, scopeKey string) time.Duration {
	t.Helper()
	ms, err := client.PTTL(ctx, scopeKey).Result()
	if err != nil {
		t.Fatalf("read TTL %s: %v", scopeKey, err)
	}
	return ms
}
