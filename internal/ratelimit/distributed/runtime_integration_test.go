//go:build integration

// Runtime-assumption verification for the pinned go-redis v9.22.0 + Redis
// 7.4.11-alpine stack (ADR-018 D8 mandatory acceptance requirement). These
// tests pin observable behavior instead of assuming it from configuration:
//
//   - Script.Run EVALSHA -> NOSCRIPT -> EVAL fallback executes exactly once;
//   - MaxRetries normalization: 0 means the DEFAULT 3 retries in go-redis
//     v9.22.0, -1 disables retries - so the production posture must set -1;
//   - with retries disabled, a mutating command whose outcome is ambiguous
//     (server closed after receiving the bytes, no reply) is never
//     retransmitted: exactly one connection attempt is observed;
//   - context cancellation surfaces as a context error and is never treated
//     as a dependency failure by the limiter core (ADR-018 D8);
//   - a genuinely unreachable Redis with a live parent context surfaces as a
//     DependencyError for the future degraded wrapper to handle.
package distributed

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestScriptRunEvalshaFallsBackToEvalAndExecutesOnce(t *testing.T) {
	client := testRedisClient(t)
	ctx := context.Background()
	counterKey := "runtime:" + t.Name() + ":counter"
	t.Cleanup(func() { _ = client.Del(ctx, counterKey).Err() })

	script := redis.NewScript(`return redis.call('INCR', KEYS[1])`)
	// Script has never been loaded: EVALSHA must answer NOSCRIPT and go-redis
	// falls back to EVAL. NOSCRIPT proves the script never executed, so the
	// fallback is a provably pre-execution path and must execute exactly once.
	first, err := script.Run(ctx, client, []string{counterKey}).Int()
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first != 1 {
		t.Fatalf("first Run result = %d, want 1 (EVAL executed exactly once)", first)
	}
	// Second Run hits the now-cached script via EVALSHA: still one execution.
	second, err := script.Run(ctx, client, []string{counterKey}).Int()
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second != 2 {
		t.Fatalf("second Run result = %d, want 2", second)
	}
}

func TestGoRedisMaxRetriesNormalization(t *testing.T) {
	client := testRedisClient(t)
	_ = client
	// go-redis v9.22.0 normalizes MaxRetries in NewClient: 0 (unset) becomes
	// the default 3; -1 disables retries (documented in options.go and pinned
	// by these assertions so a config change cannot silently reintroduce
	// retransmission of ambiguous mutating scripts).
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

// swallowServer accepts connections, reads whatever bytes arrive, then closes
// without ever replying - simulating a Redis that consumed a command whose
// outcome is ambiguous. It counts accepted connections so retransmission is
// observable as additional dials.
type swallowServer struct {
	listener net.Listener
	accepted atomic.Int64
}

func newSwallowServer(t *testing.T) *swallowServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &swallowServer{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server.accepted.Add(1)
			go func(conn net.Conn) {
				buffer := make([]byte, 4096)
				_, _ = conn.Read(buffer) // wait for at least the command bytes
				_ = conn.Close()
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return server
}

func TestZeroRetryNeverRetransmitsAmbiguousCommand(t *testing.T) {
	server := newSwallowServer(t)
	client := redis.NewClient(&redis.Options{
		Addr:         server.listener.Addr().String(),
		MaxRetries:   -1, // the production posture (0 would mean 3 retries!)
		DialTimeout:  2 * time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := client.Ping(ctx).Err()
	if err == nil {
		t.Fatal("ping against a swallowing server unexpectedly succeeded")
	}
	// Give any (forbidden) reconnect a moment to appear before asserting.
	time.Sleep(200 * time.Millisecond)
	if got := server.accepted.Load(); got != 1 {
		t.Fatalf("zero-retry client opened %d connections, want exactly 1 (no retransmission)", got)
	}
}

func TestDefaultRetryWouldRetransmit(t *testing.T) {
	server := newSwallowServer(t)
	client := redis.NewClient(&redis.Options{
		Addr:         server.listener.Addr().String(),
		DialTimeout:  2 * time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		// MaxRetries unset -> normalized to the go-redis default of 3.
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = client.Ping(ctx).Err() // error expected; we only observe the retransmission
	deadline := time.Now().Add(3 * time.Second)
	for server.accepted.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := server.accepted.Load(); got <= 1 {
		t.Fatalf("default-retry client opened %d connections, want > 1 (demonstrating why -1 is mandatory)", got)
	}
}

func TestAdmitPropagatesParentCancellationNotDependency(t *testing.T) {
	limiter, _ := newTestLimiter(t, cfgWith(20, 0))
	projectID, keyID := uniqueIDs(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := limiter.Admit(ctx, keyID, projectID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admit error = %v, want context.Canceled", err)
	}
	var dependency *DependencyError
	if errors.As(err, &dependency) {
		t.Fatal("cancellation was misclassified as a dependency failure")
	}
}

func TestAdmitSurfacesDependencyErrorWhenRedisUnreachable(t *testing.T) {
	// Point at a closed port: command fails while the parent context is live.
	deadClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 300 * time.Millisecond})
	defer deadClient.Close()
	limiter, err := New(deadClient, Config{KeyRPM: 20, IdleTTL: time.Minute, CommandTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("new limiter: %v", err)
	}
	projectID, keyID := uniqueIDs(t)
	_, err = limiter.Admit(context.Background(), keyID, projectID)
	if err == nil {
		t.Fatal("admit against unreachable redis succeeded")
	}
	if !errors.Is(err, ErrDependency) {
		t.Fatalf("error = %v, want ErrDependency for the degraded wrapper", err)
	}
}

func TestCommandReadBoundedByReadTimeoutNotContext(t *testing.T) {
	// Empirical finding pinned against go-redis v9.22.0: a context deadline does
	// NOT interrupt an in-flight blocking server reply (DEBUG SLEEP / BLPop).
	// The effective in-flight bound is the connection ReadTimeout: with a 250ms
	// ReadTimeout the command fails as a net i/o timeout after ~250ms, never as
	// context.DeadlineExceeded and never retransmitted with the -1 posture.
	// Consequence for the production client: ReadTimeout must be set tight
	// (comparable to the per-command budget), because per-command contexts alone
	// do not bound hung reads. Recorded in ADR-018's implementation note.
	options, err := redis.ParseURL(os.Getenv("REDIS_URL"))
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	options.MaxRetries = -1
	options.ReadTimeout = 250 * time.Millisecond
	options.WriteTimeout = time.Second
	options.DialTimeout = time.Second
	client := redis.NewClient(options)
	defer client.Close()

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = client.Do(ctx, "DEBUG", "SLEEP", "2").Err()
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("blocked command unexpectedly succeeded")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v: go-redis v9.22.0 read is not interrupted by ctx (documented)", err)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("read was not bounded by ReadTimeout: %v after %v", err, elapsed)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("error = %v, want a net timeout from ReadTimeout", err)
	}
}
