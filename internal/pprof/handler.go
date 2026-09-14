// Package pprof provides the protected pprof endpoints for the private
// Operations Plane (ADR-019 D8/N2).
//
// Security model:
//   - pprof is DISABLED by default (wiring only mounts a handler when
//     PPROF_ENABLED=true).
//   - The token is compared as fixed-length SHA-256 digests with
//     subtle.ConstantTimeCompare (no length timing, no raw token handling).
//   - The token is defense-in-depth only: the Ops plane lives on a private
//     network and is never publicly routed; the token never replaces network
//     isolation.
//   - Handlers are built from runtime/pprof directly. The net/http/pprof
//     package is NOT imported, so http.DefaultServeMux is never touched.
//
// Available endpoints (all GET, all behind the token):
//
//	/debug/pprof/            index listing the supported profiles
//	/debug/pprof/{name}      goroutine|heap|allocs|block|mutex|threadcreate
//	                         (optional ?debug=0|1|2; default 1 for text)
//	/debug/pprof/profile     CPU profile (?seconds=1..120, default 30)
//
// The CPU profile endpoint rejects concurrent profile requests (409) so two
// callers cannot fight over runtime/pprof.StartCPUProfile, and the seconds
// parameter is bounded.
package pprof

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	runtimepprof "runtime/pprof"
)

const (
	tokenHeader        = "Authorization"
	tokenPrefix        = "Bearer "
	minProfileSeconds  = 1
	defaultProfileSecs = 30
	maxProfileSeconds  = 120
)

// Handler is the protected pprof handler: a self-contained mux whose routes
// all require the token.
type Handler struct {
	tokenDigest []byte
	mux         *http.ServeMux

	// cpuLock prevents concurrent CPU profile requests (StartCPUProfile is
	// global to the process).
	cpuLock sync.Mutex
}

// NewHandler builds the protected pprof handler. token must be non-empty;
// the raw token is hashed once and never stored in plain text.
func NewHandler(token string) (*Handler, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("pprof token must be non-empty")
	}
	digest := sha256.Sum256([]byte(token))
	handler := &Handler{tokenDigest: digest[:]}

	mux := http.NewServeMux()
	handler.mux = mux

	mux.HandleFunc("GET /debug/pprof/", handler.requireAuth(handler.index))
	mux.HandleFunc("GET /debug/pprof", handler.requireAuth(handler.index))
	mux.HandleFunc("GET /debug/pprof/profile", handler.requireAuth(handler.cpuProfile))
	mux.HandleFunc("GET /debug/pprof/{name}", handler.requireAuth(handler.namedProfile))
	return handler, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	h.mux.ServeHTTP(response, request)
}

func (h *Handler) authorized(request *http.Request) bool {
	header := request.Header.Get(tokenHeader)
	if !strings.HasPrefix(header, tokenPrefix) {
		return false
	}
	presented := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(header, tokenPrefix))))
	return subtle.ConstantTimeCompare(presented[:], h.tokenDigest) == 1
}

func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if !h.authorized(request) {
			response.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(response, request)
	}
}

func (h *Handler) index(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(response, "available profiles:")
	for _, name := range []string{"goroutine", "heap", "allocs", "block", "mutex", "threadcreate", "profile"} {
		_, _ = fmt.Fprintln(response, "/debug/pprof/"+name)
	}
}

func (h *Handler) namedProfile(response http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	switch name {
	case "goroutine", "heap", "allocs", "block", "mutex", "threadcreate":
	default:
		http.NotFound(response, request)
		return
	}
	debug := 1 // human-readable text by default (matches net/http/pprof style)
	if raw := request.URL.Query().Get("debug"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 2 {
			http.Error(response, "debug must be 0, 1, or 2", http.StatusBadRequest)
			return
		}
		debug = parsed
	}
	profile := runtimepprof.Lookup(name)
	if profile == nil {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Content-Type", profileContentType(debug))
	_ = profile.WriteTo(response, debug)
}

func (h *Handler) cpuProfile(response http.ResponseWriter, request *http.Request) {
	seconds := defaultProfileSecs
	if raw := request.URL.Query().Get("seconds"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < minProfileSeconds || parsed > maxProfileSeconds {
			http.Error(response, fmt.Sprintf("seconds must be an integer between %d and %d", minProfileSeconds, maxProfileSeconds), http.StatusBadRequest)
			return
		}
		seconds = parsed
	}
	if !h.cpuLock.TryLock() {
		http.Error(response, "a CPU profile is already running", http.StatusConflict)
		return
	}
	defer h.cpuLock.Unlock()
	if err := runtimepprof.StartCPUProfile(response); err != nil {
		http.Error(response, "failed to start CPU profile", http.StatusInternalServerError)
		return
	}
	defer runtimepprof.StopCPUProfile()
	time.Sleep(time.Duration(seconds) * time.Second)
}

func profileContentType(debug int) string {
	if debug == 0 {
		return "application/octet-stream"
	}
	return "text/plain; charset=utf-8"
}
