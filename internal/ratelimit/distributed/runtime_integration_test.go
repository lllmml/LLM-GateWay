//go:build integration

// Runtime-assumption verification for the pinned go-redis v9.22.0 + Redis
// 7.4.11-alpine stack (ADR-018 D8 mandatory acceptance requirement). These
// tests pin observable behavior instead of assuming it from configuration:
//
//   - the integration helper refuses to run against any Redis whose version is
//     not the pinned 7.4.11;
//   - Script.Run EVALSHA -> NOSCRIPT -> EVAL fallback executes exactly once
//     (unique script body per run so the SHA cannot already be cached);
//   - MaxRetries normalization: 0 means the DEFAULT 3 retries in go-redis
//     v9.22.0, -1 disables retries - so the production posture must set -1;
//   - with retries disabled, an ambiguous mutating EVAL (the real Redis
//     executed it but the reply was dropped by a test-only proxy) is never
//     retransmitted: exactly one execution and one connection;
//   - read interruption semantics: with ContextTimeoutEnabled=false (default)
//     the read is bounded by ReadTimeout (net timeout); with it enabled the
//     per-command context deadline bounds the read (surfaced as a net timeout
//     whose source is the context).
package distributed

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestScriptRunEvalshaFallsBackToEvalAndExecutesOnce uses a per-run unique
// script body (a random nonce inside a Lua comment) so the SHA can never be
// pre-loaded on the persistent shared Redis: the first Run must take the
// EVALSHA -> NOSCRIPT -> EVAL path and mutate exactly once (P2-1).
func TestScriptRunEvalshaFallsBackToEvalAndExecutesOnce(t *testing.T) {
	client := testRedisClient(t)
	ctx := context.Background()
	counterKey := "runtime:" + t.Name() + ":counter"
	t.Cleanup(func() { _ = client.Del(ctx, counterKey).Err() })

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	body := "-- nonce " + hex.EncodeToString(nonce) + "\nreturn redis.call('INCR', KEYS[1])"
	script := redis.NewScript(body)

	sha := sha1.Sum([]byte(body))
	shaHex := hex.EncodeToString(sha[:])
	exists, err := client.ScriptExists(ctx, shaHex).Result()
	if err != nil {
		t.Fatalf("script exists: %v", err)
	}
	if len(exists) != 1 || exists[0] {
		t.Fatalf("unique script unexpectedly already cached: %v", exists)
	}

	// First Run: EVALSHA answers NOSCRIPT (script never executed) so go-redis
	// falls back to EVAL - a provably pre-execution path - executing once.
	first, err := script.Run(ctx, client, []string{counterKey}).Int()
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first != 1 {
		t.Fatalf("first Run result = %d, want 1 (EVAL executed exactly once)", first)
	}
	existsAfter, err := client.ScriptExists(ctx, shaHex).Result()
	if err != nil || len(existsAfter) != 1 || !existsAfter[0] {
		t.Fatalf("script not cached after Run: %v / %v", existsAfter, err)
	}
	// Second Run hits the cached script via EVALSHA: one more mutation.
	second, err := script.Run(ctx, client, []string{counterKey}).Int()
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second != 2 {
		t.Fatalf("second Run result = %d, want 2", second)
	}
}

func TestGoRedisMaxRetriesNormalization(t *testing.T) {
	testRedisClient(t) // also asserts the pinned server version (P2-2)
	for _, tc := range []struct {
		set  int
		want int
	}{
		{set: 0, want: 3},
		{set: -1, want: 0},
	} {
		raw, err := redis.ParseURL("redis://127.0.0.1:6379/0")
		if err != nil {
			t.Fatalf("parse url: %v", err)
		}
		raw.MaxRetries = tc.set
		probe := redis.NewClient(raw)
		if got := probe.Options().MaxRetries; got != tc.want {
			t.Fatalf("MaxRetries set to %d -> Options().MaxRetries = %d, want %d", tc.set, got, tc.want)
		}
		_ = probe.Close()
	}
}

func TestAdmitPropagatesParentCancellationNotDependency(t *testing.T) {
	core, _ := newTestLimiter(t, cfgWith(20, 0))
	projectID, keyID := uniqueIDs(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := core.AdmitCore(ctx, keyID, projectID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admit error = %v, want context.Canceled", err)
	}
	var dependency *DependencyError
	if errors.As(err, &dependency) {
		t.Fatal("cancellation was misclassified as a dependency failure")
	}
}

func TestAdmitSurfacesDependencyErrorWhenRedisUnreachable(t *testing.T) {
	deadClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 300 * time.Millisecond})
	defer deadClient.Close()
	core, err := New(deadClient, Config{KeyRPM: 20, IdleTTL: time.Minute, CommandTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("new core: %v", err)
	}
	projectID, keyID := uniqueIDs(t)
	_, err = core.AdmitCore(context.Background(), keyID, projectID)
	if err == nil {
		t.Fatal("admit against unreachable redis succeeded")
	}
	if !errors.Is(err, ErrDependency) {
		t.Fatalf("error = %v, want ErrDependency for the degraded wrapper", err)
	}
}

// TestContextDoesNotInterruptReadByDefault pins the go-redis default posture:
// with ContextTimeoutEnabled=false the per-command context does NOT bound an
// in-flight read - ReadTimeout does (net i/o timeout at ~ReadTimeout, never a
// context error). This is why the production client must enable
// ContextTimeoutEnabled (see the next test) OR keep ReadTimeout tight.
func TestContextDoesNotInterruptReadByDefault(t *testing.T) {
	client := pinnedRedisClient(t, func(options *redis.Options) {
		options.ContextTimeoutEnabled = false // default
		options.ReadTimeout = 300 * time.Millisecond
	})
	defer client.Close()

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Do(ctx, "DEBUG", "SLEEP", "2").Err()
	elapsed := time.Since(started)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v: ctx must NOT interrupt the read in the default mode", err)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("read not bounded by ReadTimeout: %v after %v", err, elapsed)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("error = %v, want a net timeout from ReadTimeout", err)
	}
}

// TestContextInterruptsReadWhenEnabled pins the chosen production posture:
// with ContextTimeoutEnabled=true the per-command context deadline DOES bound
// the in-flight read even when ReadTimeout is much longer (~ctx deadline here,
// 250ms, against a 5s ReadTimeout and a 2s server sleep). Empirically go-redis
// v9.22.0 surfaces that read abort as a net i/o timeout rather than
// context.DeadlineExceeded - the crucial observable is WHERE the deadline
// came from (ctx, not ReadTimeout), which is what the core's per-command
// CommandTimeout context relies on.
func TestContextInterruptsReadWhenEnabled(t *testing.T) {
	client := pinnedRedisClient(t, func(options *redis.Options) {
		options.ContextTimeoutEnabled = true
		options.ReadTimeout = 5 * time.Second
	})
	defer client.Close()

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := client.Do(ctx, "DEBUG", "SLEEP", "2").Err()
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("blocked command unexpectedly succeeded")
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("ctx deadline did not bound the read: %v after %v", err, elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("read aborted suspiciously early: %v after %v", err, elapsed)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("error = %v, want a timeout (deadline sourced from ctx, surfaced as net i/o timeout)", err)
	}
}

// pinnedRedisClient dials the pinned Redis with explicit options mutations,
// used by the read-semantics probes.
func pinnedRedisClient(t *testing.T, mutate func(*redis.Options)) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Fatal("REDIS_URL is required for distributed integration tests")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	options.MaxRetries = -1
	options.DialTimeout = time.Second
	options.WriteTimeout = time.Second
	if mutate != nil {
		mutate(options)
	}
	client := redis.NewClient(options)
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		client.Close()
		t.Fatalf("ping pinned Redis: %v", err)
	}
	return client
}
