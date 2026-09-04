package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/oauth2"

	"github.com/lllmml/production-go-llm-gateway/internal/app"
	"github.com/lllmml/production-go-llm-gateway/internal/config"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
	"github.com/lllmml/production-go-llm-gateway/internal/dataplane"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/anthropic"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/deepseek"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/openai"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
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
	credentialCipher, err := security.NewCredentialCipher(cfg.CredentialMasterKey)
	clear(cfg.CredentialMasterKey)
	if err != nil {
		database.Close()
		return fmt.Errorf("configure provider credential encryption: %w", err)
	}
	controlPlaneHandler, err := controlplane.NewHandler(
		authHandler,
		database,
		database,
		cfg.VirtualKeyPepper,
		database,
		credentialCipher,
		database,
		database,
		database,
	)
	if err != nil {
		database.Close()
		return fmt.Errorf("configure control plane: %w", err)
	}
	openAITransport := openai.NewTransport()
	defer openAITransport.CloseIdleConnections()
	// One long-lived transport is shared by every provider adapter; no
	// transport or client is ever created per request.
	providerHTTPClient := &http.Client{Transport: openAITransport}
	providerRegistry, err := provider.NewRegistry(map[provider.Name]provider.Client{
		provider.OpenAI:    openai.New(providerHTTPClient),
		provider.DeepSeek:  deepseek.New(providerHTTPClient),
		provider.Anthropic: anthropic.New(providerHTTPClient),
	})
	if err != nil {
		database.Close()
		return fmt.Errorf("configure provider registry: %w", err)
	}
	dataPlaneService, err := dataplane.NewService(dataplane.Options{
		Store:                     database,
		VirtualKeyPepper:          cfg.VirtualKeyPepper,
		CredentialCipher:          credentialCipher,
		UpstreamRequestTimeout:    cfg.UpstreamRequestTimeout,
		UpstreamStreamMaxDuration: cfg.UpstreamStreamMaxDuration,
		ProviderRegistry:          providerRegistry,
		Logger:                    logger,
	})
	if err != nil {
		database.Close()
		return fmt.Errorf("configure data plane: %w", err)
	}
	dataPlaneMux := http.NewServeMux()
	dataplane.NewHandler(dataPlaneService).Register(dataPlaneMux)

	application := app.New(app.Options{
		DataPlaneAddr:       cfg.DataPlaneAddr,
		ControlPlaneAddr:    cfg.ControlPlaneAddr,
		OpsAddr:             cfg.OpsAddr,
		ReadinessTimeout:    cfg.ReadinessTimeout,
		ShutdownTimeout:     cfg.ShutdownTimeout,
		DataPlaneHandler:    dataPlaneMux,
		ControlPlaneHandler: controlPlaneHandler,
	}, database, logger)

	runCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return application.Run(runCtx)
}
