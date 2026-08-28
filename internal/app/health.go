package app

import (
	"context"
	"io"
	"net/http"
)

func (a *App) opsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", a.handleLiveness)
	mux.HandleFunc("GET /health/ready", a.handleReadiness)
	return mux
}

func (a *App) handleLiveness(response http.ResponseWriter, _ *http.Request) {
	writeHealth(response, http.StatusOK, `{"status":"ok"}`)
}

func (a *App) handleReadiness(response http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), a.readinessTimeout)
	defer cancel()

	if err := a.database.Ping(ctx); err != nil {
		a.logger.Warn("readiness check failed", "dependency", "postgres")
		writeHealth(response, http.StatusServiceUnavailable, `{"status":"not_ready","checks":{"postgres":"unavailable"}}`)
		return
	}

	writeHealth(response, http.StatusOK, `{"status":"ready","checks":{"postgres":"ok"}}`)
}

func writeHealth(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, body+"\n")
}
