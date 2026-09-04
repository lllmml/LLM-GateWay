package providerconfig

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const maxRequestBodyBytes = 64 << 10

type CurrentUserID func(*http.Request) (string, bool)

type Handler struct {
	service       *Service
	currentUserID CurrentUserID
}

func NewHandler(service *Service, currentUserID CurrentUserID) *Handler {
	return &Handler{service: service, currentUserID: currentUserID}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/v1/projects/{projectID}/provider-configs/openai", h.upsertOpenAI)
	mux.HandleFunc("PUT /api/v1/projects/{projectID}/provider-configs/deepseek", h.upsertDeepSeek)
}

func (h *Handler) upsertOpenAI(response http.ResponseWriter, request *http.Request) {
	h.upsert(response, request, func(ctx context.Context, ownerUserID, projectID, credentialID string, enabled bool) (Config, error) {
		return h.service.UpsertOpenAI(ctx, ownerUserID, projectID, credentialID, enabled)
	})
}

func (h *Handler) upsertDeepSeek(response http.ResponseWriter, request *http.Request) {
	h.upsert(response, request, func(ctx context.Context, ownerUserID, projectID, credentialID string, enabled bool) (Config, error) {
		return h.service.UpsertDeepSeek(ctx, ownerUserID, projectID, credentialID, enabled)
	})
}

func (h *Handler) upsert(response http.ResponseWriter, request *http.Request, apply func(context.Context, string, string, string, bool) (Config, error)) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	var body struct {
		CredentialID string `json:"credential_id"`
		Enabled      *bool  `json:"enabled,omitempty"`
	}
	if err := decodeJSON(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	config, err := apply(
		request.Context(),
		ownerUserID,
		request.PathValue("projectID"),
		body.CredentialID,
		enabled,
	)
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, responseConfig(config))
}

type configResponse struct {
	ProjectID    string    `json:"project_id"`
	Provider     string    `json:"provider"`
	CredentialID string    `json:"credential_id"`
	Enabled      bool      `json:"enabled"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func responseConfig(config Config) configResponse {
	return configResponse{
		ProjectID:    config.ProjectID,
		Provider:     config.Provider,
		CredentialID: config.CredentialID,
		Enabled:      config.Enabled,
		UpdatedAt:    config.UpdatedAt,
	}
}

func (h *Handler) requireUser(response http.ResponseWriter, request *http.Request) (string, bool) {
	userID, ok := h.currentUserID(request)
	if !ok || userID == "" {
		writeError(response, http.StatusUnauthorized, "authentication_required", "authentication is required")
		return "", false
	}
	return userID, true
}

func decodeJSON(response http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func writeServiceError(response http.ResponseWriter, err error) {
	var validationErr *ValidationError
	switch {
	case errors.As(err, &validationErr):
		writeError(response, http.StatusBadRequest, "invalid_request", validationErr.Error())
	case errors.Is(err, ErrNotFound):
		writeError(response, http.StatusNotFound, "provider_config_target_not_found", "project or provider credential was not found")
	default:
		writeError(response, http.StatusInternalServerError, "internal_error", "request could not be completed")
	}
}

func writeError(response http.ResponseWriter, status int, code, message string) {
	writeJSON(response, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
