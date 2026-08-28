package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/oauth2"

	"github.com/lllmml/production-go-llm-gateway/internal/app"
	"github.com/lllmml/production-go-llm-gateway/internal/config"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
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

	authHandler, err := controlplane.NewAuthHandler(controlplane.AuthConfig{
		OAuth: oauth2.Config{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
			RedirectURL:  cfg.PublicConsoleURL + "/auth/github/callback",
		},
		PublicConsoleURL:   cfg.PublicConsoleURL,
		SessionTokenPepper: cfg.SessionTokenPepper,
		SecureCookies:      cfg.SecureCookies,
	}, database, database)
	if err != nil {
		database.Close()
		return fmt.Errorf("configure control-plane authentication: %w", err)
	}
	controlPlaneHandler := controlplane.NewHandler(authHandler, database)

	application := app.New(app.Options{
		DataPlaneAddr:       cfg.DataPlaneAddr,
		ControlPlaneAddr:    cfg.ControlPlaneAddr,
		OpsAddr:             cfg.OpsAddr,
		ReadinessTimeout:    cfg.ReadinessTimeout,
		ShutdownTimeout:     cfg.ShutdownTimeout,
		ControlPlaneHandler: controlPlaneHandler,
	}, database, logger)

	runCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return application.Run(runCtx)
}
