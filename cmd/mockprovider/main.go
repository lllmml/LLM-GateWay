package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	chatCompletionsPath = "/v1/chat/completions"
	maxRequestBodyBytes = 1 << 20
	oversizedEventBytes = maxRequestBodyBytes + 1
)

type config struct {
	addr                  string
	status                int
	headerDelay           time.Duration
	malformed             bool
	streamFirstTokenDelay time.Duration
	streamChunkInterval   time.Duration
	streamChunks          int
	streamUsage           bool
	streamMalformed       bool
	streamOversized       bool
	streamAbrupt          bool
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
	if cfg.streamChunks < 0 {
		return fmt.Errorf("stream-chunks must be non-negative")
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
	flags.DurationVar(&cfg.streamFirstTokenDelay, "stream-first-token-delay", 0, "delay before writing the first stream event")
	flags.DurationVar(&cfg.streamChunkInterval, "stream-chunk-interval", 0, "delay between stream content events")
	flags.IntVar(&cfg.streamChunks, "stream-chunks", 3, "number of stream content events")
	flags.BoolVar(&cfg.streamUsage, "stream-usage", true, "include a final stream usage event before [DONE]")
	flags.BoolVar(&cfg.streamMalformed, "stream-malformed", false, "write a malformed SSE data event")
	flags.BoolVar(&cfg.streamOversized, "stream-oversized", false, "write an oversized SSE data event")
	flags.BoolVar(&cfg.streamAbrupt, "stream-abrupt", false, "close the stream before [DONE]")
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
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(response, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	stream := requestWantsStream(body)

	if h.cfg.headerDelay > 0 {
		timer := time.NewTimer(h.cfg.headerDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-request.Context().Done():
			return
		}
	}
	if stream {
		h.streamChatCompletions(response, request)
		return
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

func requestWantsStream(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return false
	}
	return request.Stream
}

func (h *mockHandler) streamChatCompletions(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("X-Request-ID", "req_mock_123")
	if h.cfg.status < 200 || h.cfg.status >= 300 {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(h.cfg.status)
		_, _ = response.Write([]byte(`{"error":{"message":"mock provider error","type":"mock_error","code":"mock_error"}}`))
		return
	}

	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.WriteHeader(h.cfg.status)
	flush(response)

	if h.cfg.streamFirstTokenDelay > 0 && !wait(request.Context(), h.cfg.streamFirstTokenDelay) {
		return
	}
	if h.cfg.streamMalformed {
		_, _ = response.Write([]byte("data: {\"id\":\n\n"))
		flush(response)
		return
	}
	if h.cfg.streamOversized {
		_, _ = response.Write([]byte("data: {\"padding\":\"" + strings.Repeat("x", oversizedEventBytes) + "\"}\n\n"))
		flush(response)
		return
	}

	chunkCount := h.cfg.streamChunks
	if chunkCount == 0 {
		chunkCount = 3
	}
	for i := 1; i <= chunkCount; i++ {
		if i > 1 && h.cfg.streamChunkInterval > 0 && !wait(request.Context(), h.cfg.streamChunkInterval) {
			return
		}
		if _, err := fmt.Fprintf(response, "data: {\"id\":\"chatcmpl_mock_123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-mock\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"mock chunk %d\"},\"finish_reason\":null}]}\n\n", i); err != nil {
			return
		}
		flush(response)
	}
	if h.cfg.streamAbrupt {
		return
	}
	if h.cfg.streamUsage {
		if _, err := response.Write([]byte("data: {\"id\":\"chatcmpl_mock_123\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-mock\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")); err != nil {
			return
		}
		flush(response)
	}
	_, _ = response.Write([]byte("data: [DONE]\n\n"))
	flush(response)
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func flush(response http.ResponseWriter) {
	if flusher, ok := response.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *mockHandler) recordHit() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits++
}
