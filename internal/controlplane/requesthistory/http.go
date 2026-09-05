package requesthistory

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type CurrentUserID func(*http.Request) (string, bool)

type Handler struct {
	service       *Service
	currentUserID CurrentUserID
}

func NewHandler(service *Service, currentUserID CurrentUserID) *Handler {
	return &Handler{service: service, currentUserID: currentUserID}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/requests", h.list)
	mux.HandleFunc("GET /api/v1/requests/{requestID}", h.get)
}

func (h *Handler) list(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	params, err := parseListParams(request)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	page, err := h.service.List(request.Context(), ownerUserID, params)
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, page)
}

func (h *Handler) get(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	item, err := h.service.Get(request.Context(), ownerUserID, request.PathValue("requestID"))
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, item)
}

// parseListParams decodes query filters. A missing or malformed optional value
// is an invalid_request; a cursor value is decoded with the same rules the API
// guarantees (opaque token).
func parseListParams(request *http.Request) (ListParams, error) {
	query := request.URL.Query()
	params := ListParams{
		ProjectID: strings.TrimSpace(query.Get("project_id")),
		Provider:  query.Get("provider"),
		Model:     strings.TrimSpace(query.Get("model")),
		Status:    query.Get("status"),
	}
	if raw := query.Get("stream"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return ListParams{}, errors.New("stream must be true or false")
		}
		params.Stream = &value
	}
	if raw := query.Get("from"); raw != "" {
		from, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			return ListParams{}, errors.New("from must be an RFC3339 timestamp")
		}
		params.From = &from
	}
	if raw := query.Get("to"); raw != "" {
		to, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			return ListParams{}, errors.New("to must be an RFC3339 timestamp")
		}
		params.To = &to
	}
	if raw := query.Get("limit"); raw != "" {
		limit, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return ListParams{}, errors.New("limit must be an integer")
		}
		params.Limit = limit
	}
	if raw := query.Get("cursor"); raw != "" {
		cursor, parseErr := DecodeCursor(raw)
		if parseErr != nil {
			return ListParams{}, parseErr
		}
		params.Cursor = &cursor
	}
	return params, nil
}

func (h *Handler) requireUser(response http.ResponseWriter, request *http.Request) (string, bool) {
	userID, ok := h.currentUserID(request)
	if !ok || userID == "" {
		writeError(response, http.StatusUnauthorized, "authentication_required", "authentication is required")
		return "", false
	}
	return userID, true
}

func writeServiceError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(response, http.StatusNotFound, "request_not_found", "request was not found")
	case errors.Is(err, ErrInvalidParams):
		writeError(response, http.StatusBadRequest, "invalid_request", err.Error())
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
