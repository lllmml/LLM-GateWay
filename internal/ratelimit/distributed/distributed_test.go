package distributed

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
)

func TestMaxSafeRPMLuaExactIntegerBoundary(t *testing.T) {
	if got, want := MaxSafeRPM(), 150119987579; got != want {
		t.Fatalf("MaxSafeRPM() = %d, want %d", got, want)
	}
	// Capacity at the boundary must stay below 2^53 so every Lua integer is
	// exactly representable (mandatory invariant #3).
	if capacity := int64(MaxSafeRPM()) * UnitsPerToken; capacity >= int64(1)<<53 {
		t.Fatalf("capacity at max RPM %d exceeds the Lua exact-integer range", capacity)
	}
	if err := ValidateScopeRPM(0); err != nil {
		t.Fatalf("rpm 0 (disabled) rejected: %v", err)
	}
	if err := ValidateScopeRPM(1); err != nil {
		t.Fatalf("rpm 1 rejected: %v", err)
	}
	if err := ValidateScopeRPM(MaxSafeRPM()); err != nil {
		t.Fatalf("max safe rpm rejected: %v", err)
	}
	if err := ValidateScopeRPM(MaxSafeRPM() + 1); err == nil {
		t.Fatal("one above max safe rpm accepted")
	}
	if err := ValidateScopeRPM(-1); err == nil {
		t.Fatal("negative rpm accepted")
	}
}

// TestCoreAllScopesDisabledAdmitsWithoutRedis pins two properties: a
// fully-disabled limiter never depends on Redis (AdmitCore succeeds although
// nothing listens on the client address), and cancellation is checked BEFORE
// the all-disabled fast path, so an already-cancelled context returns
// context.Canceled and issues no Redis command (P1-2 ordering).
func TestCoreAllScopesDisabledAdmitsWithoutRedis(t *testing.T) {
	deadClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	defer deadClient.Close()
	cfg := Config{IdleTTL: time.Minute, CommandTimeout: time.Second}

	core, err := New(deadClient, cfg)
	if err != nil {
		t.Fatalf("new core: %v", err)
	}
	decision, err := core.AdmitCore(t.Context(), "key-1", "proj-1")
	if err != nil {
		t.Fatalf("admit with no enabled scopes: %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("no enabled scopes must admit, got %+v", decision)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := core.AdmitCore(cancelled, "key-1", "proj-1"); err == nil {
		t.Fatal("all-disabled core admitted a cancelled request")
	} else if err != context.Canceled {
		t.Fatalf("all-disabled cancelled error = %v, want context.Canceled", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer client.Close()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"rpm above max", Config{KeyRPM: MaxSafeRPM() + 1, IdleTTL: time.Minute, CommandTimeout: time.Second}},
		{"negative rpm", Config{KeyRPM: -5, IdleTTL: time.Minute, CommandTimeout: time.Second}},
		{"zero idle ttl", Config{KeyRPM: 20, IdleTTL: 0, CommandTimeout: time.Second}},
		{"sub-millisecond idle ttl", Config{KeyRPM: 20, IdleTTL: 500 * time.Microsecond, CommandTimeout: time.Second}},
		{"zero command timeout", Config{KeyRPM: 20, IdleTTL: time.Minute, CommandTimeout: 0}},
	}
	for _, tc := range cases {
		if _, err := New(client, tc.cfg); err == nil {
			t.Fatalf("config %q accepted", tc.name)
		}
	}
	// The minimum IdleTTL (1ms) is accepted so PEXPIRE is never skipped by an
	// integer-millisecond truncation to zero.
	if _, err := New(client, Config{KeyRPM: 20, IdleTTL: time.Millisecond, CommandTimeout: time.Second}); err != nil {
		t.Fatalf("1ms idle TTL rejected: %v", err)
	}
}

func TestParseDecision(t *testing.T) {
	allowed, err := parseDecision([]any{int64(1), int64(0), ""})
	if err != nil || !allowed.Allowed || allowed.RetryAfter != 0 || allowed.BlockingScope != "" {
		t.Fatalf("allow parse = %+v, %v", allowed, err)
	}
	rejected, err := parseDecision([]any{int64(0), int64(3000), "vk"})
	if err != nil || rejected.Allowed || rejected.RetryAfter != 3*time.Second || rejected.BlockingScope != ratelimit.ScopeVirtualKey {
		t.Fatalf("vk reject parse = %+v, %v", rejected, err)
	}
	proj, err := parseDecision([]any{int64(0), int64(6000), "proj"})
	if err != nil || proj.Allowed || proj.RetryAfter != 6*time.Second || proj.BlockingScope != ratelimit.ScopeProject {
		t.Fatalf("proj reject parse = %+v, %v", proj, err)
	}
	// Inconsistent replies are rejected (P2-5): allowed must carry retry 0 and
	// empty scope; rejected must carry retry > 0 and a concrete scope.
	bad := []any{
		[]any{},
		[]any{int64(1)},
		[]any{int64(7), int64(0), ""},
		[]any{int64(0), int64(-1), "vk"},
		[]any{int64(0), int64(100), "unknown"},
		[]any{int64(1), int64(5), ""},
		[]any{int64(1), int64(0), "vk"},
		[]any{int64(0), int64(0), "vk"},
		[]any{int64(0), int64(0), ""},
	}
	for _, raw := range bad {
		if _, err := parseDecision(raw); err == nil {
			t.Fatalf("malformed reply %#v accepted", raw)
		}
	}
}

func TestArgsLayout(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer client.Close()
	core, err := New(client, Config{KeyRPM: 20, ProjectRPM: 40, IdleTTL: 90 * time.Second, CommandTimeout: time.Second})
	if err != nil {
		t.Fatalf("new core: %v", err)
	}
	args := core.args()
	want := []string{"60000", "1", "20", "90000", "1", "40", "90000"}
	if len(args) != len(want) {
		t.Fatalf("args len = %d, want %d", len(args), len(want))
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %v, want %s", i, args[i], want[i])
		}
	}
}

func TestScopeKeyNamespaceAndHashTag(t *testing.T) {
	const (
		project = "proj-11111111-1111-4111-8111-111111111111"
		key     = "key-22222222-2222-4222-8222-222222222222"
	)
	keyScope := ScopeKey(project, key)
	projectScope := ProjectScopeKey(project)
	if want := "gwrl:v1:{" + project + "}:vk:" + key; keyScope != want {
		t.Fatalf("scope key = %q, want %q", keyScope, want)
	}
	if want := "gwrl:v1:{" + project + "}:project"; projectScope != want {
		t.Fatalf("project key = %q, want %q", projectScope, want)
	}
	split := func(s string) string {
		start, end := -1, -1
		for i := 0; i < len(s); i++ {
			if s[i] == '{' {
				start = i
			}
			if s[i] == '}' && start >= 0 {
				end = i
				break
			}
		}
		return s[start+1 : end]
	}
	if split(keyScope) != project || split(projectScope) != project {
		t.Fatalf("hash tags differ: %q vs %q", keyScope, projectScope)
	}
}
