package main

import (
	"strings"
	"testing"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/config"
)

func testCfgWithRedis(url string, commandTimeout time.Duration) config.Config {
	return config.Config{RateLimiterMode: "distributed", RedisURL: url, RedisCommandTimeout: commandTimeout}
}

// TestRedisClientOptionsPinProductionPosture pins the safety-critical client
// posture (P2-3): MaxRetries must be -1 (0 would normalize to the go-redis
// default of 3 retries and silently re-enable retransmission of ambiguous
// mutating admissions), ContextTimeoutEnabled must be true, and the timeouts
// must be the intended values. No Redis server is needed.
func TestRedisClientOptionsPinProductionPosture(t *testing.T) {
	options, err := redisClientOptions(testCfgWithRedis("redis://user:secret@127.0.0.1:6379/2", 250*time.Millisecond))
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if options.MaxRetries != -1 {
		t.Fatalf("MaxRetries = %d, want -1 (0 would mean the default 3 retries)", options.MaxRetries)
	}
	if !options.ContextTimeoutEnabled {
		t.Fatal("ContextTimeoutEnabled = false, want true")
	}
	if options.DialTimeout != time.Second {
		t.Fatalf("DialTimeout = %v, want 1s", options.DialTimeout)
	}
	if options.ReadTimeout != 250*time.Millisecond || options.WriteTimeout != 250*time.Millisecond {
		t.Fatalf("read/write timeouts = %v/%v, want the RedisCommandTimeout", options.ReadTimeout, options.WriteTimeout)
	}
	// DB and auth parsed from the URL.
	if options.DB != 2 {
		t.Fatalf("DB = %d, want 2", options.DB)
	}
	if options.Username != "user" || options.Password != "secret" {
		t.Fatalf("auth = %q/%q, want user/secret", options.Username, options.Password)
	}
}

// TestNewRedisClientConstructionDoesNotRequireLiveRedis pins P1-1: creating the
// process client (and the distributed limiter options) must succeed even when
// nothing listens at the address - go-redis connects lazily and Redis is not a
// startup/readiness dependency.
func TestNewRedisClientConstructionDoesNotRequireLiveRedis(t *testing.T) {
	cfg := testCfgWithRedis("redis://127.0.0.1:1/0", time.Second) // closed port
	client, err := newRedisClient(cfg)
	if err != nil {
		t.Fatalf("client construction failed against a closed port: %v", err)
	}
	defer client.Close()
	// go-redis normalizes MaxRetries inside NewClient: the -1 input becomes the
	// effective 0 (retries disabled); an explicit 0 input would become the
	// default 3. The client must show the disabled effective value.
	if got := client.Options().MaxRetries; got != 0 {
		t.Fatalf("constructed client effective MaxRetries = %d, want 0 (retries disabled)", got)
	}
}

// TestRedisClientOptionsSanitizesMalformedURLError pins P1-2: a malformed,
// credential-bearing REDIS_URL must produce the stable sanitized error with no
// raw URL, username, or password in the error/loggable text.
func TestRedisClientOptionsSanitizesMalformedURLError(t *testing.T) {
	const (
		raw      = "redis://alice:hunter2secret@127.0.0.1:%zz/0"
		username = "alice"
		password = "hunter2secret"
	)
	cfg := testCfgWithRedis(raw, time.Second)
	_, err := redisClientOptions(cfg)
	if err == nil {
		t.Fatal("malformed REDIS_URL accepted")
	}
	if got := err.Error(); got != "invalid REDIS_URL" {
		t.Fatalf("error = %q, want the stable sanitized %q", got, "invalid REDIS_URL")
	}
	if strings.Contains(err.Error(), username) || strings.Contains(err.Error(), password) {
		t.Fatalf("error leaks credential material: %q", err.Error())
	}
}
