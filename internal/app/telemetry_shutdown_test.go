package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// lifecycleRecordingDatabase records the order of the telemetry hook
// ("telemetry") and database.Close ("database") events so tests can assert the
// ADR-019 D9 ordering: HTTP drain -> telemetry shutdown -> database close.
type lifecycleRecordingDatabase struct {
	fakeDatabase
	mu     sync.Mutex
	events []string
}

func (d *lifecycleRecordingDatabase) Close() {
	d.fakeDatabase.Close()
	d.mu.Lock()
	d.events = append(d.events, "database")
	d.mu.Unlock()
}

func (d *lifecycleRecordingDatabase) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

func TestTelemetryShutdownRunsBeforeDatabaseCloseOnCleanShutdown(t *testing.T) {
	database := &lifecycleRecordingDatabase{}
	options := testOptions(t)
	var hookCalls int
	var hookHadDeadline bool
	options.TelemetryShutdown = func(ctx context.Context) error {
		database.mu.Lock()
		database.events = append(database.events, "telemetry")
		database.mu.Unlock()
		hookCalls++
		_, hookHadDeadline = ctx.Deadline()
		return nil
	}
	application := newTestApp(t, database, options)

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- application.Run(ctx)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	waitForReady(t, client, "http://"+options.OpsAddr+"/health/live", http.StatusOK, runResult)
	cancel()
	if err := <-runResult; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if events := database.snapshot(); len(events) != 2 || events[0] != "telemetry" || events[1] != "database" {
		t.Fatalf("lifecycle events = %v, want [telemetry database] (ADR-019 D9 order)", events)
	}
	if hookCalls != 1 {
		t.Fatalf("telemetry hook calls = %d, want exactly 1", hookCalls)
	}
	if !hookHadDeadline {
		t.Fatal("telemetry hook did not receive a bounded context")
	}
}

func TestTelemetryShutdownErrorIsLoggedNotFatal(t *testing.T) {
	database := &lifecycleRecordingDatabase{}
	options := testOptions(t)
	options.TelemetryShutdown = func(context.Context) error {
		database.mu.Lock()
		database.events = append(database.events, "telemetry")
		database.mu.Unlock()
		return errors.New("flush failed")
	}
	application := newTestApp(t, database, options)

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- application.Run(ctx)
	}()
	client := &http.Client{Timeout: 2 * time.Second}
	waitForReady(t, client, "http://"+options.OpsAddr+"/health/live", http.StatusOK, runResult)
	cancel()
	if err := <-runResult; err != nil {
		t.Fatalf("Run returned error = %v, want nil (telemetry flush is best-effort)", err)
	}
	if events := database.snapshot(); len(events) != 2 || events[0] != "telemetry" || events[1] != "database" {
		t.Fatalf("lifecycle events = %v, want [telemetry database]", events)
	}
}

func TestTelemetryShutdownRunsOnPartialListenFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve occupied address: %v", err)
	}
	defer occupied.Close()

	database := &lifecycleRecordingDatabase{}
	// No pre-bound listeners: the real net.Listen path fails on the occupied
	// data-plane port after earlier defers were already registered.
	options := Options{
		DataPlaneAddr:    occupied.Addr().String(),
		ControlPlaneAddr: "127.0.0.1:0",
		OpsAddr:          "127.0.0.1:0",
		ReadinessTimeout: 100 * time.Millisecond,
		ShutdownTimeout:  time.Second,
		TelemetryShutdown: func(ctx context.Context) error {
			database.mu.Lock()
			database.events = append(database.events, "telemetry")
			database.mu.Unlock()
			_, hasDeadline := ctx.Deadline()
			if !hasDeadline {
				t.Error("telemetry hook on listen failure did not receive a bounded context")
			}
			return nil
		},
	}
	application := newTestApp(t, database, options)

	err = application.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listen on data plane") {
		t.Fatalf("Run error = %v, want data-plane listen error", err)
	}
	events := database.snapshot()
	if len(events) != 2 || events[0] != "telemetry" || events[1] != "database" {
		t.Fatalf("lifecycle events on listen failure = %v, want [telemetry database]", events)
	}
}
