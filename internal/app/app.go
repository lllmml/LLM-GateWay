package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const (
	defaultReadHeaderTimeout = 5 * time.Second
	dataPlaneReadTimeout     = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	controlReadTimeout       = 15 * time.Second
	controlWriteTimeout      = 30 * time.Second
	opsReadTimeout           = 5 * time.Second
	opsWriteTimeout          = 5 * time.Second
	maxHeaderBytes           = 1 << 20

	// telemetryShutdownTimeout bounds the optional telemetry hook invocation
	// (ADR-019 D9). This is an orchestration budget owned by internal/app, so
	// internal/app never imports telemetry; the telemetry resource owns its
	// own shutdown internally (ADR-019 N1).
	telemetryShutdownTimeout = 5 * time.Second
)

type Database interface {
	Ping(context.Context) error
	Close()
}

type Options struct {
	DataPlaneAddr       string
	ControlPlaneAddr    string
	OpsAddr             string
	ReadinessTimeout    time.Duration
	ShutdownTimeout     time.Duration
	DataPlaneHandler    http.Handler
	ControlPlaneHandler http.Handler

	// MetricsHandler, when non-nil, is mounted at GET /metrics on the private
	// Operations Plane only (ADR-019 D2). The data and control planes never
	// serve it; wiring owns that isolation.
	MetricsHandler http.Handler

	// TelemetryShutdown, when non-nil, is invoked once by App.Run after HTTP
	// drain completes and before database.Close(), under a bounded context
	// (ADR-019 D9). It is a plain function type so internal/app never depends
	// on concrete telemetry/OTel types; wiring (cmd/gateway) supplies the real
	// closure.
	TelemetryShutdown func(context.Context) error

	// listeners, when non-empty, is a test seam: the three pre-bound
	// listeners are used instead of binding DataPlaneAddr / ControlPlaneAddr /
	// OpsAddr (in that order). Production callers never set it, so real
	// startup keeps using net.Listen. Same-package tests use it to remove the
	// bind-close-rebind TOCTOU from lifecycle tests. Unexported because it is
	// only meaningful inside this package.
	listeners []net.Listener
}

type App struct {
	database           Database
	logger             *slog.Logger
	readinessTimeout   time.Duration
	shutdownTimeout    time.Duration
	dataPlaneServer    *http.Server
	controlPlaneServer *http.Server
	opsServer          *http.Server
	listeners          []net.Listener // test seam, see Options.listeners
	telemetryShutdown  func(context.Context) error
}

type runningServer struct {
	name     string
	server   *http.Server
	listener net.Listener
}

func New(options Options, database Database, logger *slog.Logger) *App {
	if options.DataPlaneHandler == nil {
		options.DataPlaneHandler = http.NewServeMux()
	}
	if options.ControlPlaneHandler == nil {
		options.ControlPlaneHandler = http.NewServeMux()
	}

	application := &App{
		database:          database,
		logger:            logger,
		readinessTimeout:  options.ReadinessTimeout,
		shutdownTimeout:   options.ShutdownTimeout,
		listeners:         options.listeners,
		telemetryShutdown: options.TelemetryShutdown,
	}
	application.dataPlaneServer = newDataPlaneServer(options.DataPlaneAddr, options.DataPlaneHandler)
	application.controlPlaneServer = newControlPlaneServer(options.ControlPlaneAddr, options.ControlPlaneHandler)
	application.opsServer = newOpsServer(options.OpsAddr, application.opsHandler(options.MetricsHandler))
	return application
}

func (a *App) Run(ctx context.Context) error {
	defer a.database.Close()
	// ADR-019 D9: telemetry cleanup is registered AFTER database.Close, so the
	// LIFO order runs telemetry shutdown first on every Run exit path - the
	// normal path (servers drained above in a.shutdown) and the abnormal
	// partial-listen() path - keeping:
	//   HTTP drain -> telemetry shutdown -> database close.
	if a.telemetryShutdown != nil {
		defer a.runTelemetryShutdown()
	}

	servers, err := a.listen()
	if err != nil {
		return err
	}

	serveErrors := make(chan error, len(servers))
	for _, current := range servers {
		current := current
		a.logger.Info("http server started", "plane", current.name, "address", current.listener.Addr().String())
		go func() {
			if err := current.server.Serve(current.listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				serveErrors <- fmt.Errorf("serve %s plane: %w", current.name, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		a.logger.Info("shutdown requested")
	case runErr = <-serveErrors:
		a.logger.Error("http server failed", "error", runErr)
	}

	for _, current := range servers {
		if err := current.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("close %s listener: %w", current.name, err))
		}
	}

	if err := a.shutdown(); err != nil {
		runErr = errors.Join(runErr, err)
	}

	a.logger.Info("application stopped")
	return runErr
}

// runTelemetryShutdown invokes the optional telemetry hook exactly once per
// Run under a bounded orchestration context. A hook error is logged and never
// changes Run's returned error: telemetry flush is best-effort and must not
// alter the process exit status (ADR-019 D9).
func (a *App) runTelemetryShutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
	defer cancel()
	if err := a.telemetryShutdown(ctx); err != nil {
		a.logger.Error("telemetry shutdown failed", "error", err)
	}
}

func (a *App) listen() ([]runningServer, error) {
	definitions := []struct {
		name   string
		server *http.Server
	}{
		{name: "data", server: a.dataPlaneServer},
		{name: "control", server: a.controlPlaneServer},
		{name: "operations", server: a.opsServer},
	}

	servers := make([]runningServer, 0, len(definitions))
	for index, definition := range definitions {
		var (
			listener net.Listener
			err      error
		)
		if index < len(a.listeners) {
			// Test seam: a pre-bound listener is used verbatim (no
			// bind-close-rebind window). Production leaves a.listeners empty.
			listener = a.listeners[index]
		} else {
			listener, err = net.Listen("tcp", definition.server.Addr)
		}
		if err != nil {
			for _, current := range servers {
				_ = current.listener.Close()
			}
			return nil, fmt.Errorf("listen on %s plane %q: %w", definition.name, definition.server.Addr, err)
		}
		servers = append(servers, runningServer{
			name:     definition.name,
			server:   definition.server,
			listener: listener,
		})
	}
	return servers, nil
}

func (a *App) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()

	servers := []struct {
		name   string
		server *http.Server
	}{
		{name: "control", server: a.controlPlaneServer},
		{name: "data", server: a.dataPlaneServer},
		{name: "operations", server: a.opsServer},
	}

	errs := make(chan error, len(servers))
	for _, current := range servers {
		current := current
		go func() {
			if err := current.server.Shutdown(ctx); err != nil {
				if closeErr := current.server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) && !errors.Is(closeErr, net.ErrClosed) {
					err = errors.Join(err, fmt.Errorf("force close %s plane: %w", current.name, closeErr))
				}
				errs <- fmt.Errorf("shutdown %s plane: %w", current.name, err)
				return
			}
			errs <- nil
		}()
	}

	var shutdownErr error
	for range servers {
		if err := <-errs; err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	return shutdownErr
}

func newDataPlaneServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		// ReadTimeout bounds header-plus-body reads so a client cannot send
		// headers and then trickle a request body forever. WriteTimeout stays
		// zero because future SSE responses may be valid long-running streams.
		ReadTimeout:    dataPlaneReadTimeout,
		IdleTimeout:    defaultIdleTimeout,
		MaxHeaderBytes: maxHeaderBytes,
	}
}

func newControlPlaneServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       controlReadTimeout,
		WriteTimeout:      controlWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

func newOpsServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       opsReadTimeout,
		WriteTimeout:      opsWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}
