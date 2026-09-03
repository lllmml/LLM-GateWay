package app

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHealthHandlers(t *testing.T) {
	database := &fakeDatabase{}
	application := newTestApp(t, database, Options{})

	t.Run("liveness", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		response := httptest.NewRecorder()

		application.opsServer.Handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
		}
		if response.Body.String() != "{\"status\":\"ok\"}\n" {
			t.Fatalf("body = %q", response.Body.String())
		}
	})

	t.Run("ready", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
		response := httptest.NewRecorder()

		application.opsServer.Handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
		}
		if response.Body.String() != "{\"status\":\"ready\",\"checks\":{\"postgres\":\"ok\"}}\n" {
			t.Fatalf("body = %q", response.Body.String())
		}
	})

	t.Run("not ready", func(t *testing.T) {
		database.setPingError(errors.New("database unavailable"))
		request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
		response := httptest.NewRecorder()

		application.opsServer.Handler.ServeHTTP(response, request)

		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
		}
		if strings.Contains(response.Body.String(), "database unavailable") {
			t.Fatalf("body exposed dependency error: %q", response.Body.String())
		}
	})
}

func TestDataPlaneHasNoWriteTimeout(t *testing.T) {
	application := newTestApp(t, &fakeDatabase{}, Options{})

	if application.dataPlaneServer.WriteTimeout != 0 {
		t.Fatalf("data plane WriteTimeout = %v, want 0 for streaming", application.dataPlaneServer.WriteTimeout)
	}
	if application.dataPlaneServer.ReadTimeout != dataPlaneReadTimeout {
		t.Fatalf("data plane ReadTimeout = %v, want %v", application.dataPlaneServer.ReadTimeout, dataPlaneReadTimeout)
	}
	if application.controlPlaneServer.WriteTimeout != controlWriteTimeout {
		t.Fatalf("control plane WriteTimeout = %v, want %v", application.controlPlaneServer.WriteTimeout, controlWriteTimeout)
	}
}

func TestRunStartsAllPlanesAndStopsCleanly(t *testing.T) {
	database := &fakeDatabase{}
	options := testOptions(t)
	application := newTestApp(t, database, options)

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- application.Run(ctx)
	}()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	waitForStatus(t, client, "http://"+options.OpsAddr+"/health/live", http.StatusOK)
	waitForStatus(t, client, "http://"+options.OpsAddr+"/health/ready", http.StatusOK)
	waitForStatus(t, client, "http://"+options.DataPlaneAddr+"/", http.StatusNotFound)
	waitForStatus(t, client, "http://"+options.ControlPlaneAddr+"/", http.StatusNotFound)

	cancel()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}

	if !database.isClosed() {
		t.Fatal("database was not closed")
	}
}

func TestRunDrainsActiveDataPlaneRequest(t *testing.T) {
	database := &fakeDatabase{}
	options := testOptions(t)
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	options.DataPlaneHandler = http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		response.WriteHeader(http.StatusNoContent)
	})
	application := newTestApp(t, database, options)

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- application.Run(ctx)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	waitForStatus(t, client, "http://"+options.OpsAddr+"/health/live", http.StatusOK)
	requestResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + options.DataPlaneAddr + "/active")
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("data plane request did not start")
	}

	cancel()
	select {
	case err := <-runResult:
		t.Fatalf("Run returned before active request drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRequest)
	if err := <-requestResult; err != nil {
		t.Fatalf("active request failed: %v", err)
	}
	if err := <-runResult; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRunDrainsActiveFlushedDataPlaneStream(t *testing.T) {
	database := &fakeDatabase{}
	options := testOptions(t)
	streamFlushed := make(chan struct{})
	releaseStream := make(chan struct{})
	var streamFlushedOnce sync.Once
	options.DataPlaneHandler = http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(response, "data: first\n\n")
		if err := http.NewResponseController(response).Flush(); err != nil {
			t.Errorf("flush first event: %v", err)
			return
		}
		streamFlushedOnce.Do(func() { close(streamFlushed) })
		<-releaseStream
		_, _ = io.WriteString(response, "data: [DONE]\n\n")
		_ = http.NewResponseController(response).Flush()
	})
	application := newTestApp(t, database, options)

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- application.Run(ctx)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	waitForStatus(t, client, "http://"+options.OpsAddr+"/health/live", http.StatusOK)

	response, err := client.Get("http://" + options.DataPlaneAddr + "/stream")
	if err != nil {
		t.Fatalf("start stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	if event := readAppSSEEvent(t, reader); event != "data: first\n\n" {
		t.Fatalf("first event = %q", event)
	}
	select {
	case <-streamFlushed:
	case <-time.After(time.Second):
		t.Fatal("stream did not flush first event")
	}

	cancel()
	select {
	case err := <-runResult:
		t.Fatalf("Run returned before active stream drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseStream)
	if event := readAppSSEEvent(t, reader); event != "data: [DONE]\n\n" {
		t.Fatalf("done event = %q", event)
	}
	if err := <-runResult; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRunStartsDataPlaneShutdownWhileControlPlaneRequestIsBlocked(t *testing.T) {
	database := &fakeDatabase{}
	options := testOptions(t)
	options.ShutdownTimeout = 750 * time.Millisecond

	controlStarted := make(chan struct{})
	releaseControl := make(chan struct{})
	var controlStartedOnce sync.Once
	options.ControlPlaneHandler = http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		controlStartedOnce.Do(func() { close(controlStarted) })
		<-releaseControl
		response.WriteHeader(http.StatusNoContent)
	})

	dataStarted := make(chan struct{})
	dataShutdownStarted := make(chan struct{})
	var dataStartedOnce sync.Once
	options.DataPlaneHandler = http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		dataStartedOnce.Do(func() { close(dataStarted) })
		select {
		case <-dataShutdownStarted:
			response.WriteHeader(http.StatusNoContent)
		case <-request.Context().Done():
		}
	})

	application := newTestApp(t, database, options)
	application.dataPlaneServer.RegisterOnShutdown(func() {
		close(dataShutdownStarted)
	})

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- application.Run(ctx)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	waitForStatus(t, client, "http://"+options.OpsAddr+"/health/live", http.StatusOK)

	controlResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + options.ControlPlaneAddr + "/blocked")
		if response != nil {
			_ = response.Body.Close()
		}
		controlResult <- err
	}()

	dataResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + options.DataPlaneAddr + "/active")
		if response != nil {
			_ = response.Body.Close()
		}
		dataResult <- err
	}()

	select {
	case <-controlStarted:
	case <-time.After(time.Second):
		t.Fatal("control plane request did not start")
	}
	select {
	case <-dataStarted:
	case <-time.After(time.Second):
		t.Fatal("data plane request did not start")
	}

	cancel()

	select {
	case err := <-dataResult:
		if err != nil {
			t.Fatalf("data plane request failed before graceful drain: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		close(releaseControl)
		t.Fatal("data plane did not begin graceful shutdown while control plane was blocked")
	}

	close(releaseControl)
	if err := <-controlResult; err != nil {
		t.Fatalf("control plane request failed after release: %v", err)
	}
	if err := <-runResult; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRunForceClosesStuckRequestAfterShutdownTimeout(t *testing.T) {
	database := &fakeDatabase{}
	options := testOptions(t)
	options.ShutdownTimeout = 200 * time.Millisecond

	requestStarted := make(chan struct{})
	var requestStartedOnce sync.Once
	options.DataPlaneHandler = http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		requestStartedOnce.Do(func() { close(requestStarted) })
		<-request.Context().Done()
	})

	application := newTestApp(t, database, options)

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- application.Run(ctx)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	waitForStatus(t, client, "http://"+options.OpsAddr+"/health/live", http.StatusOK)

	requestResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + options.DataPlaneAddr + "/stuck")
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("data plane request did not start")
	}

	cancel()

	select {
	case err := <-runResult:
		if err == nil {
			t.Fatal("Run returned nil error, want shutdown timeout error")
		}
		if !strings.Contains(err.Error(), "shutdown data plane") {
			t.Fatalf("Run error = %v, want data-plane shutdown error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown timeout")
	}

	select {
	case <-requestResult:
	case <-time.After(time.Second):
		t.Fatal("stuck request was not force closed")
	}
}

func TestRunReturnsListenErrorAndClosesDatabase(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve occupied address: %v", err)
	}
	defer occupied.Close()

	database := &fakeDatabase{}
	options := testOptions(t)
	options.DataPlaneAddr = occupied.Addr().String()
	application := newTestApp(t, database, options)

	err = application.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listen on data plane") {
		t.Fatalf("Run error = %v, want data-plane listen error", err)
	}
	if !database.isClosed() {
		t.Fatal("database was not closed after listen failure")
	}
}

type fakeDatabase struct {
	mu      sync.Mutex
	pingErr error
	closed  bool
}

func (d *fakeDatabase) Ping(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pingErr
}

func (d *fakeDatabase) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
}

func (d *fakeDatabase) setPingError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pingErr = err
}

func (d *fakeDatabase) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

func newTestApp(t *testing.T, database Database, options Options) *App {
	t.Helper()
	if options.DataPlaneAddr == "" {
		options = testOptions(t)
	}
	return New(options, database, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		DataPlaneAddr:    freeAddress(t),
		ControlPlaneAddr: freeAddress(t),
		OpsAddr:          freeAddress(t),
		ReadinessTimeout: 100 * time.Millisecond,
		ShutdownTimeout:  time.Second,
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved address: %v", err)
	}
	return address
}

func waitForStatus(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not return status %d", url, want)
}

func readAppSSEEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var builder strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE event: %v", err)
		}
		builder.WriteString(line)
		if line == "\n" || line == "\r\n" {
			return builder.String()
		}
	}
}
