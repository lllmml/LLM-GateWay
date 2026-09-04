package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lllmml/production-go-llm-gateway/internal/app"
	"github.com/lllmml/production-go-llm-gateway/internal/config"
	"github.com/lllmml/production-go-llm-gateway/internal/store/postgres"
	"github.com/lllmml/production-go-llm-gateway/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	logger := telemetry.NewLogger(os.Stdout, cfg.LogLevel)

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), cfg.DatabaseConnectTime)
	database, err := postgres.Open(connectCtx, cfg.DatabaseURL)
	cancelConnect()
	if err != nil {
		return err
	}

	application := app.New(app.Options{
		DataPlaneAddr:    cfg.DataPlaneAddr,
		ControlPlaneAddr: cfg.ControlPlaneAddr,
		OpsAddr:          cfg.OpsAddr,
		ReadinessTimeout: cfg.ReadinessTimeout,
		ShutdownTimeout:  cfg.ShutdownTimeout,
	}, database, logger)

	runCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return application.Run(runCtx)
}
