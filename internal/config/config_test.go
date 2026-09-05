package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesDefaults(t *testing.T) {
	values := requiredTestValues()

	cfg, err := load(mapLookup(values))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.DataPlaneAddr != defaultDataPlaneAddr {
		t.Fatalf("DataPlaneAddr = %q, want %q", cfg.DataPlaneAddr, defaultDataPlaneAddr)
	}
	if cfg.ControlPlaneAddr != defaultControlPlaneAddr {
		t.Fatalf("ControlPlaneAddr = %q, want %q", cfg.ControlPlaneAddr, defaultControlPlaneAddr)
	}
	if cfg.OpsAddr != defaultOpsAddr {
		t.Fatalf("OpsAddr = %q, want %q", cfg.OpsAddr, defaultOpsAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
	if cfg.DatabaseConnectTime != defaultDatabaseConnectTime {
		t.Fatalf("DatabaseConnectTime = %v, want %v", cfg.DatabaseConnectTime, defaultDatabaseConnectTime)
	}
	if cfg.UpstreamRequestTimeout != defaultUpstreamRequestTimeout {
		t.Fatalf("UpstreamRequestTimeout = %v, want %v", cfg.UpstreamRequestTimeout, defaultUpstreamRequestTimeout)
	}
	if cfg.UpstreamStreamMaxDuration != defaultUpstreamStreamMaxDuration {
		t.Fatalf("UpstreamStreamMaxDuration = %v, want %v", cfg.UpstreamStreamMaxDuration, defaultUpstreamStreamMaxDuration)
	}
	if cfg.RateLimitKeyRequestsPerMinute != 0 {
		t.Fatalf("RateLimitKeyRequestsPerMinute = %d, want 0 (disabled)", cfg.RateLimitKeyRequestsPerMinute)
	}
	if cfg.RateLimitProjectRequestsPerMinute != 0 {
		t.Fatalf("RateLimitProjectRequestsPerMinute = %d, want 0 (disabled)", cfg.RateLimitProjectRequestsPerMinute)
	}
	if cfg.RateLimiterEntryCap != defaultRateLimiterEntryCap {
		t.Fatalf("RateLimiterEntryCap = %d, want %d", cfg.RateLimiterEntryCap, defaultRateLimiterEntryCap)
	}
	if cfg.RateLimiterIdleTTL != defaultRateLimiterIdleTTL {
		t.Fatalf("RateLimiterIdleTTL = %v, want %v", cfg.RateLimiterIdleTTL, defaultRateLimiterIdleTTL)
	}
	if cfg.MaxConcurrentRequests != 0 {
		t.Fatalf("MaxConcurrentRequests = %d, want 0 (disabled)", cfg.MaxConcurrentRequests)
	}
	if cfg.MaxConcurrentStreams != 0 {
		t.Fatalf("MaxConcurrentStreams = %d, want 0 (disabled)", cfg.MaxConcurrentStreams)
	}
	if cfg.RetryMaxRetries != defaultRetryMaxRetries {
		t.Fatalf("RetryMaxRetries = %d, want %d", cfg.RetryMaxRetries, defaultRetryMaxRetries)
	}
	if cfg.RetryBackoffMax != defaultRetryBackoffMax {
		t.Fatalf("RetryBackoffMax = %v, want %v", cfg.RetryBackoffMax, defaultRetryBackoffMax)
	}
	if len(cfg.CredentialMasterKey) != 32 {
		t.Fatalf("CredentialMasterKey length = %d, want 32", len(cfg.CredentialMasterKey))
	}
}

func TestLoadRejectsMissingRequiredValues(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{
			name:   "database URL",
			values: withoutKey(requiredTestValues(), "DATABASE_URL"),
			want:   "DATABASE_URL is required",
		},
		{
			name:   "credential master key",
			values: withoutKey(requiredTestValues(), "CREDENTIAL_MASTER_KEY"),
			want:   "CREDENTIAL_MASTER_KEY is required",
		},
		{
			name:   "GitHub client secret",
			values: withoutKey(requiredTestValues(), "GITHUB_CLIENT_SECRET"),
			want:   "GITHUB_CLIENT_SECRET is required",
		},
		{
			name:   "session token pepper",
			values: withoutKey(requiredTestValues(), "SESSION_TOKEN_PEPPER"),
			want:   "SESSION_TOKEN_PEPPER is required",
		},
		{
			name:   "virtual key pepper",
			values: withoutKey(requiredTestValues(), "VIRTUAL_KEY_PEPPER"),
			want:   "VIRTUAL_KEY_PEPPER is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(mapLookup(tt.values))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("load error = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsDuplicateAddresses(t *testing.T) {
	values := requiredTestValues()
	values["DATA_PLANE_ADDR"] = ":8080"
	values["CONTROL_PLANE_ADDR"] = ":8080"

	_, err := load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("load error = %v, want distinct-address error", err)
	}
}

func TestLoadParsesOverrides(t *testing.T) {
	values := requiredTestValues()
	values["LOG_LEVEL"] = "debug"
	values["DATABASE_CONNECT_TIMEOUT"] = "7s"
	values["READINESS_TIMEOUT"] = "3s"
	values["SHUTDOWN_TIMEOUT"] = "11s"
	values["UPSTREAM_REQUEST_TIMEOUT"] = "13s"
	values["UPSTREAM_STREAM_MAX_DURATION"] = "17m"
	values["RATE_LIMIT_KEY_REQUESTS_PER_MINUTE"] = "60"
	values["RATE_LIMIT_PROJECT_REQUESTS_PER_MINUTE"] = "600"
	values["RATE_LIMITER_ENTRY_CAP"] = "1234"
	values["RATE_LIMITER_IDLE_TTL"] = "9m"
	values["RATE_LIMITER_SWEEP_INTERVAL"] = "45s"
	values["DATA_PLANE_MAX_CONCURRENT_REQUESTS"] = "8"
	values["DATA_PLANE_MAX_CONCURRENT_STREAMS"] = "3"
	values["RETRY_MAX_RETRIES"] = "2"
	values["RETRY_BACKOFF_MAX"] = "250ms"

	cfg, err := load(mapLookup(values))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
	if cfg.DatabaseConnectTime != 7*time.Second {
		t.Fatalf("DatabaseConnectTime = %v, want 7s", cfg.DatabaseConnectTime)
	}
	if cfg.ReadinessTimeout != 3*time.Second {
		t.Fatalf("ReadinessTimeout = %v, want 3s", cfg.ReadinessTimeout)
	}
	if cfg.ShutdownTimeout != 11*time.Second {
		t.Fatalf("ShutdownTimeout = %v, want 11s", cfg.ShutdownTimeout)
	}
	if cfg.UpstreamRequestTimeout != 13*time.Second {
		t.Fatalf("UpstreamRequestTimeout = %v, want 13s", cfg.UpstreamRequestTimeout)
	}
	if cfg.UpstreamStreamMaxDuration != 17*time.Minute {
		t.Fatalf("UpstreamStreamMaxDuration = %v, want 17m", cfg.UpstreamStreamMaxDuration)
	}
	if cfg.RateLimitKeyRequestsPerMinute != 60 {
		t.Fatalf("RateLimitKeyRequestsPerMinute = %d, want 60", cfg.RateLimitKeyRequestsPerMinute)
	}
	if cfg.RateLimitProjectRequestsPerMinute != 600 {
		t.Fatalf("RateLimitProjectRequestsPerMinute = %d, want 600", cfg.RateLimitProjectRequestsPerMinute)
	}
	if cfg.RateLimiterEntryCap != 1234 {
		t.Fatalf("RateLimiterEntryCap = %d, want 1234", cfg.RateLimiterEntryCap)
	}
	if cfg.RateLimiterIdleTTL != 9*time.Minute {
		t.Fatalf("RateLimiterIdleTTL = %v, want 9m", cfg.RateLimiterIdleTTL)
	}
	if cfg.RateLimiterSweepInterval != 45*time.Second {
		t.Fatalf("RateLimiterSweepInterval = %v, want 45s", cfg.RateLimiterSweepInterval)
	}
	if cfg.MaxConcurrentRequests != 8 {
		t.Fatalf("MaxConcurrentRequests = %d, want 8", cfg.MaxConcurrentRequests)
	}
	if cfg.MaxConcurrentStreams != 3 {
		t.Fatalf("MaxConcurrentStreams = %d, want 3", cfg.MaxConcurrentStreams)
	}
	if cfg.RetryMaxRetries != 2 {
		t.Fatalf("RetryMaxRetries = %d, want 2", cfg.RetryMaxRetries)
	}
	if cfg.RetryBackoffMax != 250*time.Millisecond {
		t.Fatalf("RetryBackoffMax = %v, want 250ms", cfg.RetryBackoffMax)
	}
}

func TestLoadRejectsInvalidTimeoutOverrides(t *testing.T) {
	for _, key := range []string{
		"DATABASE_CONNECT_TIMEOUT",
		"READINESS_TIMEOUT",
		"SHUTDOWN_TIMEOUT",
		"UPSTREAM_REQUEST_TIMEOUT",
		"UPSTREAM_STREAM_MAX_DURATION",
	} {
		t.Run(key, func(t *testing.T) {
			values := requiredTestValues()
			values[key] = "0s"

			_, err := load(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), key+" must be positive") {
				t.Fatalf("load error = %v, want positive-duration error for %s", err, key)
			}
		})
	}
}

func TestLoadValidatesPublicConsoleURL(t *testing.T) {
	tests := []struct {
		name       string
		consoleURL string
		wantSecure bool
		wantError  bool
	}{
		{name: "production HTTPS", consoleURL: "https://console.example.com/", wantSecure: true},
		{name: "IPv4 loopback development", consoleURL: "http://127.0.0.1:8081", wantSecure: false},
		{name: "localhost development", consoleURL: "http://localhost:8081", wantSecure: false},
		{name: "public HTTP rejected", consoleURL: "http://console.example.com", wantError: true},
		{name: "credentials rejected", consoleURL: "https://user:pass@console.example.com", wantError: true},
		{name: "path rejected", consoleURL: "https://console.example.com/app", wantError: true},
		{name: "query rejected", consoleURL: "https://console.example.com?next=elsewhere", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := requiredTestValues()
			values["PUBLIC_CONSOLE_URL"] = tt.consoleURL

			cfg, err := load(mapLookup(values))
			if tt.wantError {
				if err == nil {
					t.Fatal("load returned nil error")
				}
				return
			}
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.SecureCookies != tt.wantSecure {
				t.Fatalf("SecureCookies = %t, want %t", cfg.SecureCookies, tt.wantSecure)
			}
		})
	}
}

func TestLoadRejectsInvalidSessionTokenPepper(t *testing.T) {
	values := requiredTestValues()
	values["SESSION_TOKEN_PEPPER"] = base64.StdEncoding.EncodeToString(make([]byte, 31))

	_, err := load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Fatalf("load error = %v, want 32-byte validation error", err)
	}
}

func TestLoadRejectsInvalidCredentialMasterKey(t *testing.T) {
	values := requiredTestValues()
	values["CREDENTIAL_MASTER_KEY"] = base64.StdEncoding.EncodeToString(make([]byte, 31))

	_, err := load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Fatalf("load error = %v, want 32-byte validation error", err)
	}
}

func TestLoadRejectsInvalidVirtualKeyPepper(t *testing.T) {
	values := requiredTestValues()
	values["VIRTUAL_KEY_PEPPER"] = base64.StdEncoding.EncodeToString(make([]byte, 31))

	_, err := load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Fatalf("load error = %v, want 32-byte validation error", err)
	}
}

func TestLoadKeepsSessionAndVirtualKeyPeppersSeparate(t *testing.T) {
	values := requiredTestValues()
	values["SESSION_TOKEN_PEPPER"] = base64.StdEncoding.EncodeToString(bytesOf(1, 32))
	values["VIRTUAL_KEY_PEPPER"] = base64.StdEncoding.EncodeToString(bytesOf(2, 32))

	cfg, err := load(mapLookup(values))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if bytes.Equal(cfg.SessionTokenPepper, cfg.VirtualKeyPepper) {
		t.Fatal("session and virtual key peppers unexpectedly share key material")
	}
}

func TestLoadRejectsReusedSecurityKeyMaterial(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
	}{
		{name: "credential and session", first: "CREDENTIAL_MASTER_KEY", second: "SESSION_TOKEN_PEPPER"},
		{name: "credential and virtual", first: "CREDENTIAL_MASTER_KEY", second: "VIRTUAL_KEY_PEPPER"},
		{name: "session and virtual", first: "SESSION_TOKEN_PEPPER", second: "VIRTUAL_KEY_PEPPER"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := requiredTestValues()
			sharedKey := base64.StdEncoding.EncodeToString(bytesOf(3, 32))
			values[test.first] = sharedKey
			values[test.second] = sharedKey

			_, err := load(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), test.first) || !strings.Contains(err.Error(), test.second) {
				t.Fatalf("load error = %v, want key-reuse error naming %s and %s", err, test.first, test.second)
			}
			if strings.Contains(err.Error(), sharedKey) {
				t.Fatalf("load error leaked key material: %v", err)
			}
		})
	}
}

func TestLoadRejectsInvalidReliabilityOverrides(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "key rpm negative", key: "RATE_LIMIT_KEY_REQUESTS_PER_MINUTE", value: "-1", want: "must be a non-negative integer"},
		{name: "project rpm negative", key: "RATE_LIMIT_PROJECT_REQUESTS_PER_MINUTE", value: "-5", want: "must be a non-negative integer"},
		{name: "entry cap zero", key: "RATE_LIMITER_ENTRY_CAP", value: "0", want: "must be a positive integer"},
		{name: "entry cap not an integer", key: "RATE_LIMITER_ENTRY_CAP", value: "many", want: "must be a positive integer"},
		{name: "idle ttl zero", key: "RATE_LIMITER_IDLE_TTL", value: "0s", want: "must be positive"},
		{name: "sweep interval zero", key: "RATE_LIMITER_SWEEP_INTERVAL", value: "0s", want: "must be positive"},
		{name: "concurrent requests negative", key: "DATA_PLANE_MAX_CONCURRENT_REQUESTS", value: "-2", want: "must be a non-negative integer"},
		{name: "concurrent streams negative", key: "DATA_PLANE_MAX_CONCURRENT_STREAMS", value: "-1", want: "must be a non-negative integer"},
		{name: "retry max retries above bound", key: "RETRY_MAX_RETRIES", value: "6", want: "RETRY_MAX_RETRIES must be between 0 and 5"},
		{name: "retry max retries negative", key: "RETRY_MAX_RETRIES", value: "-1", want: "RETRY_MAX_RETRIES must be between 0 and 5"},
		{name: "retry backoff zero", key: "RETRY_BACKOFF_MAX", value: "0s", want: "must be positive"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := requiredTestValues()
			values[test.key] = test.value

			_, err := load(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("load error = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func testCredentialMasterKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func requiredTestValues() map[string]string {
	return map[string]string{
		"DATABASE_URL":          "postgres://example",
		"CREDENTIAL_MASTER_KEY": testCredentialMasterKey(),
		"PUBLIC_CONSOLE_URL":    "http://127.0.0.1:8081",
		"GITHUB_CLIENT_ID":      "test-client-id",
		"GITHUB_CLIENT_SECRET":  "test-client-secret",
		"SESSION_TOKEN_PEPPER":  base64.StdEncoding.EncodeToString(bytesOf(1, 32)),
		"VIRTUAL_KEY_PEPPER":    base64.StdEncoding.EncodeToString(bytesOf(2, 32)),
	}
}

func bytesOf(value byte, size int) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = value
	}
	return result
}

func withoutKey(values map[string]string, key string) map[string]string {
	delete(values, key)
	return values
}

func TestDistributedRateLimiterConfigDefaultsAreLocal(t *testing.T) {
	base := map[string]string{}
	for key, value := range map[string]string{
		"DATA_PLANE_ADDR": ":8080", "CONTROL_PLANE_ADDR": ":8081", "OPS_ADDR": ":9090",
		"DATABASE_URL": "postgres://x", "CREDENTIAL_MASTER_KEY": rawKey32("master"),
		"PUBLIC_CONSOLE_URL": "http://127.0.0.1:5173", "GITHUB_CLIENT_ID": "id",
		"GITHUB_CLIENT_SECRET": "secret", "SESSION_TOKEN_PEPPER": rawKey32("session"),
		"VIRTUAL_KEY_PEPPER": rawKey32("vk"),
	} {
		base[key] = value
	}
	cfg, err := load(func(key string) (string, bool) { value, ok := base[key]; return value, ok })
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RateLimiterMode != "local" {
		t.Fatalf("RateLimiterMode = %q, want local", cfg.RateLimiterMode)
	}
	if cfg.RedisURL != "" || cfg.RedisCommandTimeout != defaultRedisCommandTimeout || cfg.RedisProbeInterval != defaultRedisProbeInterval ||
		cfg.RedisProbeThreshold != defaultRedisProbeThreshold || cfg.RateLimiterReplicaFactor != defaultRateLimiterReplicaFactor {
		t.Fatalf("distributed defaults not applied: %+v", cfg)
	}
}

func rawKey32(prefix string) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte(prefix), 32/len(prefix)+1)[:32])
}

func loadWithDistributed(t *testing.T, overrides map[string]string, keyRPM, projectRPM int) (Config, error) {
	t.Helper()
	base := map[string]string{
		"DATA_PLANE_ADDR": ":8080", "CONTROL_PLANE_ADDR": ":8081", "OPS_ADDR": ":9090",
		"DATABASE_URL": "postgres://x", "CREDENTIAL_MASTER_KEY": rawKey32("master"),
		"PUBLIC_CONSOLE_URL": "http://127.0.0.1:5173", "GITHUB_CLIENT_ID": "id",
		"GITHUB_CLIENT_SECRET": "secret", "SESSION_TOKEN_PEPPER": rawKey32("session"),
		"VIRTUAL_KEY_PEPPER": rawKey32("vk"),
		"RATE_LIMITER_MODE":  "distributed", "REDIS_URL": "redis://127.0.0.1:6379/0",
		"RATE_LIMIT_KEY_REQUESTS_PER_MINUTE": fmt.Sprint(keyRPM), "RATE_LIMIT_PROJECT_REQUESTS_PER_MINUTE": fmt.Sprint(projectRPM),
	}
	for key, value := range overrides {
		base[key] = value
	}
	return load(func(key string) (string, bool) { value, ok := base[key]; return value, ok })
}

func TestDistributedConfigParses(t *testing.T) {
	cfg, err := loadWithDistributed(t, map[string]string{
		"REDIS_COMMAND_TIMEOUT": "250ms", "REDIS_PROBE_INTERVAL": "2s",
		"REDIS_PROBE_THRESHOLD": "5", "RATE_LIMITER_REPLICA_FACTOR": "3",
	}, 20, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RedisCommandTimeout != 250*time.Millisecond || cfg.RedisProbeInterval != 2*time.Second ||
		cfg.RedisProbeThreshold != 5 || cfg.RateLimiterReplicaFactor != 3 {
		t.Fatalf("distributed values = %+v", cfg)
	}
}

func TestDistributedConfigValidation(t *testing.T) {
	cases := []struct {
		name       string
		overrides  map[string]string
		keyRPM     int
		projectRPM int
	}{
		{"missing redis url", map[string]string{"REDIS_URL": ""}, 20, 0},
		{"no scope enabled", nil, 0, 0},
		{"factor zero", map[string]string{"RATE_LIMITER_REPLICA_FACTOR": "0"}, 20, 0},
		{"threshold zero", map[string]string{"REDIS_PROBE_THRESHOLD": "0"}, 20, 0},
		{"command timeout zero", map[string]string{"REDIS_COMMAND_TIMEOUT": "0"}, 20, 0},
		{"probe interval zero", map[string]string{"REDIS_PROBE_INTERVAL": "0"}, 20, 0},
		{"bad mode", map[string]string{"RATE_LIMITER_MODE": "hybrid"}, 20, 0},
	}
	for _, tc := range cases {
		if _, err := loadWithDistributed(t, tc.overrides, tc.keyRPM, tc.projectRPM); err == nil {
			t.Fatalf("config %q accepted", tc.name)
		}
	}
}
