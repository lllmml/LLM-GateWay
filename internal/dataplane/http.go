package dataplane

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/lllmml/production-go-llm-gateway/internal/provider"
)

const maxChatRequestBodyBytes = 1 << 20

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
}

func (h *Handler) chatCompletions(response http.ResponseWriter, request *http.Request) {
	requestStartedAt := time.Now().UTC()
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeError(response, http.StatusUnsupportedMediaType, provider.InvalidRequest, "invalid_request", "content type must be application/json")
		return
	}
	rawKey, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writeError(response, http.StatusUnauthorized, provider.AuthenticationFailed, "authentication_failed", "authentication failed")
		return
	}
	auth, err := h.service.Authenticate(request.Context(), rawKey)
	if err != nil {
		writeGatewayError(response, GatewayRequest{}, err)
		return
	}
	chat, err := decodeChatRequest(response, request)
	if err != nil {
		code := "invalid_request"
		var validationErr *requestValidationError
		if errors.As(err, &validationErr) {
			code = validationErr.Code
		}
		writeError(response, http.StatusBadRequest, provider.InvalidRequest, code, err.Error())
		return
	}
	traceID, err := requestTraceID(request)
	if err != nil {
		writeError(response, http.StatusInternalServerError, provider.InternalError, "internal_error", "request could not be completed")
		return
	}
	if chat.Stream {
		sink := newHTTPStreamSink(response)
		record, err := h.service.StreamChatStartedAt(request.Context(), auth, traceID, requestStartedAt, chat, sink)
		if err != nil && !sink.Committed() {
			writeGatewayError(response, record, err)
		}
		return
	}
	result, record, err := h.service.CompleteChatStartedAt(request.Context(), auth, traceID, requestStartedAt, chat)
	if err != nil {
		writeGatewayError(response, record, err)
		return
	}
	response.Header().Set("X-Gateway-Request-ID", record.ID)
	response.Header().Set("X-Gateway-Provider", string(record.Provider))
	response.Header().Set("X-Gateway-Retry-Count", "0")
	writeJSON(response, http.StatusOK, result.Response)
}

func decodeChatRequest(response http.ResponseWriter, request *http.Request) (provider.ChatRequest, error) {
	request.Body = http.MaxBytesReader(response, request.Body, maxChatRequestBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream    *bool  `json:"stream,omitempty"`
		MaxTokens *int64 `json:"max_tokens,omitempty"`
	}
	if err := decoder.Decode(&body); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return provider.ChatRequest{}, unsupportedParameter("request includes an unsupported parameter")
		}
		return provider.ChatRequest{}, errors.New("request body is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return provider.ChatRequest{}, errors.New("request body must contain one JSON value")
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		return provider.ChatRequest{}, errors.New("model is required")
	}
	if len(body.Messages) == 0 {
		return provider.ChatRequest{}, errors.New("messages is required")
	}
	messages := make([]provider.Message, 0, len(body.Messages))
	for _, current := range body.Messages {
		role := strings.TrimSpace(current.Role)
		if role != "system" && role != "user" && role != "assistant" {
			return provider.ChatRequest{}, errors.New("message role must be system, user, or assistant")
		}
		if current.Content == "" {
			return provider.ChatRequest{}, errors.New("message content is required")
		}
		messages = append(messages, provider.Message{Role: role, Content: current.Content})
	}
	stream := false
	if body.Stream != nil {
		stream = *body.Stream
	}
	if body.MaxTokens != nil && *body.MaxTokens <= 0 {
		return provider.ChatRequest{}, errors.New("max_tokens must be a positive integer")
	}
	return provider.ChatRequest{
		Model:     model,
		Messages:  messages,
		Stream:    stream,
		MaxTokens: body.MaxTokens,
	}, nil
}

func requestTraceID(request *http.Request) (string, error) {
	if traceID := strings.TrimSpace(request.Header.Get("X-Request-ID")); traceID != "" {
		return traceID, nil
	}
	return newUUIDV4()
}

func newUUIDV4() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4],
		raw[4:6],
		raw[6:8],
		raw[8:10],
		raw[10:16],
	), nil
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func writeGatewayError(response http.ResponseWriter, record GatewayRequest, err error) {
	var gatewayErr *GatewayError
	if !errors.As(err, &gatewayErr) {
		gatewayErr = NewError(provider.InternalError, "request could not be completed")
	}
	if record.ID != "" {
		response.Header().Set("X-Gateway-Request-ID", record.ID)
		response.Header().Set("X-Gateway-Provider", string(record.Provider))
		response.Header().Set("X-Gateway-Retry-Count", "0")
	}
	status := http.StatusInternalServerError
	switch gatewayErr.Category {
	case provider.InvalidRequest, provider.UnsupportedFeature, provider.ModelNotSupported, provider.ProviderNotConfigured:
		status = http.StatusBadRequest
	case provider.AuthenticationFailed:
		status = http.StatusUnauthorized
	case provider.AuthorizationFailed:
		status = http.StatusForbidden
	case provider.RateLimited, provider.ProviderRateLimited:
		status = http.StatusTooManyRequests
	case provider.ProviderTimeout:
		status = http.StatusGatewayTimeout
	case provider.ProviderUnavailable:
		status = http.StatusBadGateway
	case provider.ProviderInvalidReq:
		status = http.StatusBadRequest
	case provider.StreamInterrupted:
		status = http.StatusBadGateway
	case provider.UsagePersistenceFail:
		status = http.StatusInternalServerError
	}
	writeError(response, status, gatewayErr.Category, string(gatewayErr.Category), gatewayErr.Message)
}

type requestValidationError struct {
	Code    string
	Message string
}

func (e *requestValidationError) Error() string {
	return e.Message
}

func unsupportedParameter(message string) error {
	return &requestValidationError{Code: "unsupported_parameter", Message: message}
}

func isJSONContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

func writeError(response http.ResponseWriter, status int, category provider.ErrorCategory, code, message string) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(struct {
		Error struct {
			Message string                 `json:"message"`
			Type    provider.ErrorCategory `json:"type"`
			Code    string                 `json:"code"`
		} `json:"error"`
	}{Error: struct {
		Message string                 `json:"message"`
		Type    provider.ErrorCategory `json:"type"`
		Code    string                 `json:"code"`
	}{Message: message, Type: category, Code: code}})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

type httpStreamSink struct {
	response   http.ResponseWriter
	controller *http.ResponseController
	committed  bool
}

func newHTTPStreamSink(response http.ResponseWriter) *httpStreamSink {
	return &httpStreamSink{
		response:   response,
		controller: http.NewResponseController(response),
	}
}

func (s *httpStreamSink) Prepare(record GatewayRequest) error {
	header := s.response.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-store")
	header.Set("X-Gateway-Request-ID", record.ID)
	header.Set("X-Gateway-Provider", string(record.Provider))
	header.Set("X-Gateway-Retry-Count", "0")
	return nil
}

func (s *httpStreamSink) WriteEvent(event provider.StreamEvent) error {
	if !s.committed {
		s.response.WriteHeader(http.StatusOK)
		s.committed = true
	}
	if name := cleanSSEEventName(event.Event); name != "" {
		if _, err := fmt.Fprintf(s.response, "event: %s\n", name); err != nil {
			return err
		}
	}
	if err := writeSSEData(s.response, event.Data); err != nil {
		return err
	}
	if _, err := io.WriteString(s.response, "\n"); err != nil {
		return err
	}
	return s.controller.Flush()
}

func (s *httpStreamSink) Committed() bool {
	return s.committed
}

func cleanSSEEventName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "message" || strings.ContainsAny(name, "\r\n") {
		return ""
	}
	return name
}

func writeSSEData(writer io.Writer, data []byte) error {
	lines := bytes.Split(data, []byte{'\n'})
	for _, line := range lines {
		if _, err := io.WriteString(writer, "data: "); err != nil {
			return err
		}
		if _, err := writer.Write(line); err != nil {
			return err
		}
		if _, err := io.WriteString(writer, "\n"); err != nil {
			return err
		}
	}
	return nil
}
