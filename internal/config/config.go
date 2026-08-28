package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultDataPlaneAddr       = ":8080"
	defaultControlPlaneAddr    = ":8081"
	defaultOpsAddr             = ":9090"
	defaultDatabaseConnectTime = 5 * time.Second
	defaultReadinessTimeout    = 2 * time.Second
	defaultShutdownTimeout     = 10 * time.Second
)

type Config struct {
	DataPlaneAddr       string
	ControlPlaneAddr    string
	OpsAddr             string
	DatabaseURL         string
	CredentialMasterKey string
	PublicConsoleURL    string
	GitHubClientID      string
	GitHubClientSecret  string
	SessionTokenPepper  []byte
	VirtualKeyPepper    []byte
	SecureCookies       bool
	LogLevel            slog.Level
	DatabaseConnectTime time.Duration
	ReadinessTimeout    time.Duration
	ShutdownTimeout     time.Duration
}

func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	dataPlaneAddr := valueOrDefault(lookup, "DATA_PLANE_ADDR", defaultDataPlaneAddr)
	controlPlaneAddr := valueOrDefault(lookup, "CONTROL_PLANE_ADDR", defaultControlPlaneAddr)
	opsAddr := valueOrDefault(lookup, "OPS_ADDR", defaultOpsAddr)

	if dataPlaneAddr == controlPlaneAddr || dataPlaneAddr == opsAddr || controlPlaneAddr == opsAddr {
		return Config{}, errors.New("DATA_PLANE_ADDR, CONTROL_PLANE_ADDR, and OPS_ADDR must be distinct")
	}

	databaseURL, err := required(lookup, "DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	credentialMasterKey, err := required(lookup, "CREDENTIAL_MASTER_KEY")
	if err != nil {
		return Config{}, err
	}

	publicConsoleURL, secureCookies, err := parsePublicConsoleURL(lookup)
	if err != nil {
		return Config{}, err
	}

	githubClientID, err := required(lookup, "GITHUB_CLIENT_ID")
	if err != nil {
		return Config{}, err
	}

	githubClientSecret, err := required(lookup, "GITHUB_CLIENT_SECRET")
	if err != nil {
		return Config{}, err
	}

	sessionTokenPepper, err := base64Key(lookup, "SESSION_TOKEN_PEPPER", 32)
	if err != nil {
		return Config{}, err
	}
	virtualKeyPepper, err := base64Key(lookup, "VIRTUAL_KEY_PEPPER", 32)
	if err != nil {
		return Config{}, err
	}
	if bytes.Equal(sessionTokenPepper, virtualKeyPepper) {
		return Config{}, errors.New("SESSION_TOKEN_PEPPER and VIRTUAL_KEY_PEPPER must use different key material")
	}

	logLevel, err := parseLogLevel(valueOrDefault(lookup, "LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}

	databaseConnectTime, err := positiveDuration(lookup, "DATABASE_CONNECT_TIMEOUT", defaultDatabaseConnectTime)
	if err != nil {
		return Config{}, err
	}

	readinessTimeout, err := positiveDuration(lookup, "READINESS_TIMEOUT", defaultReadinessTimeout)
	if err != nil {
		return Config{}, err
	}

	shutdownTimeout, err := positiveDuration(lookup, "SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return Config{}, err
	}

	return Config{
		DataPlaneAddr:       dataPlaneAddr,
		ControlPlaneAddr:    controlPlaneAddr,
		OpsAddr:             opsAddr,
		DatabaseURL:         databaseURL,
		CredentialMasterKey: credentialMasterKey,
		PublicConsoleURL:    publicConsoleURL,
		GitHubClientID:      githubClientID,
		GitHubClientSecret:  githubClientSecret,
		SessionTokenPepper:  sessionTokenPepper,
		VirtualKeyPepper:    virtualKeyPepper,
		SecureCookies:       secureCookies,
		LogLevel:            logLevel,
		DatabaseConnectTime: databaseConnectTime,
		ReadinessTimeout:    readinessTimeout,
		ShutdownTimeout:     shutdownTimeout,
	}, nil
}

func parsePublicConsoleURL(lookup func(string) (string, bool)) (string, bool, error) {
	rawURL, err := required(lookup, "PUBLIC_CONSOLE_URL")
	if err != nil {
		return "", false, err
	}

	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false, errors.New("PUBLIC_CONSOLE_URL must be an origin URL without credentials, path, query, or fragment")
	}
	if parsed.Scheme == "https" {
		return strings.TrimRight(parsed.String(), "/"), true, nil
	}
	if parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname()) {
		return "", false, errors.New("PUBLIC_CONSOLE_URL must use https except for loopback development")
	}
	return strings.TrimRight(parsed.String(), "/"), false, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func base64Key(lookup func(string) (string, bool), name string, size int) ([]byte, error) {
	encoded, err := required(lookup, name)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != size {
		return nil, fmt.Errorf("%s must be standard base64 encoding of exactly %d bytes", name, size)
	}
	return decoded, nil
}

func valueOrDefault(lookup func(string) (string, bool), key, fallback string) string {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func required(lookup func(string) (string, bool), key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return strings.TrimSpace(value), nil
}

func positiveDuration(lookup func(string) (string, bool), key string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}

	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return duration, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(strings.TrimSpace(value)))); err != nil {
		return 0, fmt.Errorf("parse LOG_LEVEL: %w", err)
	}
	return level, nil
}
