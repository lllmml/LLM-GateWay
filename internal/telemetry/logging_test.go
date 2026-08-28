package telemetry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewLoggerWritesJSONAtConfiguredLevel(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, slog.LevelInfo)

	logger.Debug("hidden")
	logger.Info("started", "plane", "data")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode log record: %v", err)
	}
	if record["msg"] != "started" {
		t.Fatalf("msg = %v, want started", record["msg"])
	}
	if record["plane"] != "data" {
		t.Fatalf("plane = %v, want data", record["plane"])
	}
}
