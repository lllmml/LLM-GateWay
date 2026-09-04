package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	chatCompletionsPath = "/v1/chat/completions"
	maxRequestBodyBytes = 1 << 20
)

type config struct {
	addr        string
	status      int
	headerDelay time.Duration
	malformed   bool
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mockprovider:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseFlags(os.Args[1:])
	if cfg.status < 100 || cfg.status > 599 {
		return fmt.Errorf("status must be a valid HTTP status code")
	}

	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           newHandler(cfg),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if err == http.ErrServerClosed {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errc
	}
}

func parseFlags(args []string) config {
	flags := flag.NewFlagSet("mockprovider", flag.ExitOnError)
	cfg := config{}
	flags.StringVar(&cfg.addr, "addr", "127.0.0.1:18080", "listen address")
	flags.IntVar(&cfg.status, "status", http.StatusOK, "response HTTP status")
	flags.DurationVar(&cfg.headerDelay, "header-delay", 0, "delay before writing response headers")
	flags.BoolVar(&cfg.malformed, "malformed", false, "write malformed JSON")
	_ = flags.Parse(args)
	return cfg
}

func newHandler(cfg config) http.Handler {
	return &mockHandler{cfg: cfg}
}

type mockHandler struct {
	cfg  config
	mu   sync.Mutex
	hits int
}

func (h *mockHandler) Hits() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

func (h *mockHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc(chatCompletionsPath, h.chatCompletions)
	mux.ServeHTTP(response, request)
}

func (h *mockHandler) chatCompletions(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.recordHit()
	request.Body = http.MaxBytesReader(response, request.Body, maxRequestBodyBytes)
	defer request.Body.Close()
	if _, err := io.Copy(io.Discard, request.Body); err != nil {
		http.Error(response, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if h.cfg.headerDelay > 0 {
		timer := time.NewTimer(h.cfg.headerDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-request.Context().Done():
			return
		}
	}

	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("X-Request-ID", "req_mock_123")
	response.WriteHeader(h.cfg.status)
	if h.cfg.malformed {
		_, _ = response.Write([]byte(`{"id":`))
		return
	}
	if h.cfg.status < 200 || h.cfg.status >= 300 {
		_, _ = response.Write([]byte(`{"error":{"message":"mock provider error","type":"mock_error","code":"mock_error"}}`))
		return
	}
	_, _ = response.Write([]byte(`{"id":"chatcmpl_mock_123","object":"chat.completion","created":1700000000,"model":"gpt-mock","choices":[{"index":0,"message":{"role":"assistant","content":"mock response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
}

func (h *mockHandler) recordHit() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits++
}
