package apikey

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
	mux.HandleFunc("POST /api/v1/projects/{projectID}/keys", h.create)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/keys", h.list)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/keys/{keyID}/disable", h.disable)
	mux.HandleFunc("DELETE /api/v1/projects/{projectID}/keys/{keyID}", h.revoke)
}

func (h *Handler) create(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	created, err := h.service.Create(request.Context(), ownerUserID, request.PathValue("projectID"), body.Name)
	if err != nil {
		writeServiceError(response, err)
		return
	}
	metadata := responseKey(created.Key)
	writeJSON(response, http.StatusCreated, struct {
		keyResponse
		Key string `json:"key"`
	}{keyResponse: metadata, Key: created.RawKey})
}

func (h *Handler) list(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	keys, err := h.service.List(request.Context(), ownerUserID, request.PathValue("projectID"))
	if err != nil {
		writeServiceError(response, err)
		return
	}
	items := make([]keyResponse, 0, len(keys))
	for _, key := range keys {
		items = append(items, responseKey(key))
	}
	writeJSON(response, http.StatusOK, struct {
		Keys []keyResponse `json:"keys"`
	}{Keys: items})
}

func (h *Handler) disable(response http.ResponseWriter, request *http.Request) {
	h.mutate(response, request, h.service.Disable)
}

func (h *Handler) revoke(response http.ResponseWriter, request *http.Request) {
	h.mutate(response, request, h.service.Revoke)
}

func (h *Handler) mutate(response http.ResponseWriter, request *http.Request, operation func(context.Context, string, string, string) (Key, error)) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	key, err := operation(request.Context(), ownerUserID, request.PathValue("projectID"), request.PathValue("keyID"))
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, responseKey(key))
}

type keyResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func responseKey(key Key) keyResponse {
	return keyResponse{
		ID:         key.ID,
		Name:       key.Name,
		Prefix:     key.Prefix,
		Status:     key.Status,
		CreatedAt:  key.CreatedAt,
		LastUsedAt: key.LastUsedAt,
		RevokedAt:  key.RevokedAt,
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
		writeError(response, http.StatusNotFound, "api_key_not_found", "API key was not found")
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
