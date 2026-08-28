package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesDefaults(t *testing.T) {
	values := map[string]string{
		"DATABASE_URL":          "postgres://example",
		"CREDENTIAL_MASTER_KEY": "development-key",
	}

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
}

func TestLoadRejectsMissingRequiredValues(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{
			name:   "database URL",
			values: map[string]string{"CREDENTIAL_MASTER_KEY": "development-key"},
			want:   "DATABASE_URL is required",
		},
		{
			name:   "credential master key",
			values: map[string]string{"DATABASE_URL": "postgres://example"},
			want:   "CREDENTIAL_MASTER_KEY is required",
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
	values := map[string]string{
		"DATABASE_URL":          "postgres://example",
		"CREDENTIAL_MASTER_KEY": "development-key",
		"DATA_PLANE_ADDR":       ":8080",
		"CONTROL_PLANE_ADDR":    ":8080",
	}

	_, err := load(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("load error = %v, want distinct-address error", err)
	}
}

func TestLoadParsesOverrides(t *testing.T) {
	values := map[string]string{
		"DATABASE_URL":             "postgres://example",
		"CREDENTIAL_MASTER_KEY":    "development-key",
		"LOG_LEVEL":                "debug",
		"DATABASE_CONNECT_TIMEOUT": "7s",
		"READINESS_TIMEOUT":        "3s",
		"SHUTDOWN_TIMEOUT":         "11s",
	}

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
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
