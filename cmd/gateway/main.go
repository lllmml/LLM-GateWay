package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"

	"github.com/lllmml/production-go-llm-gateway/internal/app"
	"github.com/lllmml/production-go-llm-gateway/internal/config"
	"github.com/lllmml/production-go-llm-gateway/internal/controlplane"
	"github.com/lllmml/production-go-llm-gateway/internal/dataplane"
	"github.com/lllmml/production-go-llm-gateway/internal/provider"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/anthropic"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/deepseek"
	"github.com/lllmml/production-go-llm-gateway/internal/provider/openai"
	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit"
	"github.com/lllmml/production-go-llm-gateway/internal/ratelimit/distributed"
	"github.com/lllmml/production-go-llm-gateway/internal/security"
	"github.com/lllmml/production-go-llm-gateway/internal/store/postgres"
	"github.com/lllmml/production-go-llm-gateway/internal/telemetry"
)

// redisClientOptions returns the go-redis options for the single long-lived
// process Redis client with the pinned Week 9 posture (ADR-018 implementation
// notes): MaxRetries=-1 (0 would normalize to the default 3 retries) so an
// ambiguous mutating admission is never automatically retransmitted,
// ContextTimeoutEnabled so the per-command CommandTimeout context bounds reads,
// and short dial/write/read timeouts. Parsing is the only validation; a
// malformed URL yields the stable sanitized error "invalid REDIS_URL" because
// the underlying net/url error can embed the raw URL including credentials.
func redisClientOptions(cfg config.Config) (*redis.Options, error) {
	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, errors.New("invalid REDIS_URL")
	}
	options.MaxRetries = -1
	options.ContextTimeoutEnabled = true
	options.DialTimeout = time.Second
	options.WriteTimeout = cfg.RedisCommandTimeout
	options.ReadTimeout = cfg.RedisCommandTimeout
	return options, nil
}

// newRedisClient creates the process Redis client WITHOUT a startup availability
// probe: go-redis connects lazily, and Redis is intentionally not a startup or
// readiness dependency (ADR-018 failure policy) - if Redis is temporarily
// unavailable the first real admission observes the dependency failure and the
// distributed wrapper degrades to the bounded emergency limiter.
func newRedisClient(cfg config.Config) (*redis.Client, error) {
	options, err := redisClientOptions(cfg)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(options), nil
}

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
	// transport or client is ever created per request. Automatic redirect
	// following is disabled (same policy as every adapter default) so a
	// provider POST is never transparently replayed to a redirect target and a
	// redirect-chain dial/DNS failure can never masquerade as a safe
	// pre-provider transport error eligible for retry.
	providerHTTPClient := &http.Client{
		Transport: openAITransport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	providerRegistry, err := provider.NewRegistry(map[provider.Name]provider.Client{
		provider.OpenAI:    openai.New(providerHTTPClient),
		provider.DeepSeek:  deepseek.New(providerHTTPClient),
		provider.Anthropic: anthropic.New(providerHTTPClient),
	})
	if err != nil {
		database.Close()
		return fmt.Errorf("configure provider registry: %w", err)
	}
	// Week 8/9 rate limiting. Local mode keeps the in-memory registry (only
	// created when at least one scope is enabled). Distributed mode is opt-in
	// (config-validated): one long-lived go-redis client per process with the
	// pinned B1 posture (MaxRetries=-1 disables retransmission of ambiguous
	// mutating commands; ContextTimeoutEnabled makes the per-command context
	// bound reads), wrapped by the degraded/emergency/recovery Limiter. Both
	// enter the data plane through ratelimit.Limiter.
	var rateLimiter ratelimit.Limiter
	var closeLimiter func()
	switch cfg.RateLimiterMode {
	case "local":
		if cfg.RateLimitKeyRequestsPerMinute > 0 || cfg.RateLimitProjectRequestsPerMinute > 0 {
			var localRegistry *ratelimit.Registry
			localRegistry, err = ratelimit.NewRegistry(ratelimit.Config{
				KeyRPM:        float64(cfg.RateLimitKeyRequestsPerMinute),
				ProjectRPM:    float64(cfg.RateLimitProjectRequestsPerMinute),
				EntryCap:      cfg.RateLimiterEntryCap,
				IdleTTL:       cfg.RateLimiterIdleTTL,
				SweepInterval: cfg.RateLimiterSweepInterval,
			})
			if err != nil {
				database.Close()
				return fmt.Errorf("configure rate limiter: %w", err)
			}
			rateLimiter = localRegistry
			closeLimiter = localRegistry.Close
		}
	case "distributed":
		redisClient, err := newRedisClient(cfg)
		if err != nil {
			database.Close()
			return fmt.Errorf("configure redis client: %w", err)
		}
		// Shutdown ordering: the limiter (probe loop + emergency registry)
		// must close before its redis.Client.
		defer redisClient.Close()
		rateLimiter, err = distributed.NewLimiter(redisClient, distributed.WrapperConfig{
			KeyRPM:         cfg.RateLimitKeyRequestsPerMinute,
			ProjectRPM:     cfg.RateLimitProjectRequestsPerMinute,
			IdleTTL:        cfg.RateLimiterIdleTTL,
			CommandTimeout: cfg.RedisCommandTimeout,
			ReplicaFactor:  cfg.RateLimiterReplicaFactor,
			ProbeInterval:  cfg.RedisProbeInterval,
			ProbeThreshold: cfg.RedisProbeThreshold,
			Logger:         logger,
		})
		if err != nil {
			database.Close()
			return fmt.Errorf("configure distributed limiter: %w", err)
		}
		closeLimiter = rateLimiter.(*distributed.Limiter).Close
	default:
		database.Close()
		return fmt.Errorf("unsupported rate limiter mode %q", cfg.RateLimiterMode)
	}
	if closeLimiter != nil {
		defer closeLimiter()
	}
	dataPlaneService, err := dataplane.NewService(dataplane.Options{
		Store:                     database,
		VirtualKeyPepper:          cfg.VirtualKeyPepper,
		CredentialCipher:          credentialCipher,
		UpstreamRequestTimeout:    cfg.UpstreamRequestTimeout,
		UpstreamStreamMaxDuration: cfg.UpstreamStreamMaxDuration,
		ProviderRegistry:          providerRegistry,
		Logger:                    logger,
		RateLimiter:               rateLimiter,
		MaxConcurrentRequests:     cfg.MaxConcurrentRequests,
		MaxConcurrentStreams:      cfg.MaxConcurrentStreams,
		RetryMaxRetries:           cfg.RetryMaxRetries,
		RetryBackoffMax:           cfg.RetryBackoffMax,
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
