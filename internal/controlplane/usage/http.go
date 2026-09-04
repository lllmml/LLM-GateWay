package usage

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
	mux.HandleFunc("GET /api/v1/usage/summary", h.summary)
	mux.HandleFunc("GET /api/v1/usage/timeseries", h.timeseries)
	mux.HandleFunc("GET /api/v1/usage/breakdown", h.breakdown)
}

func (h *Handler) summary(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	projectID, from, to, ok := h.normalizedWindow(response, request)
	if !ok {
		return
	}
	summary, err := h.service.Summary(request.Context(), ownerUserID, projectID, &from, &to)
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, summary)
}

func (h *Handler) timeseries(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	bucket := strings.TrimSpace(request.URL.Query().Get("bucket"))
	if bucket == "" {
		bucket = "day"
	}
	if bucket != "hour" && bucket != "day" {
		writeError(response, http.StatusBadRequest, "invalid_request", "bucket must be hour or day")
		return
	}
	projectID, from, to, ok := h.normalizedWindow(response, request)
	if !ok {
		return
	}
	points, err := h.service.Timeseries(request.Context(), ownerUserID, projectID, bucket, &from, &to)
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, struct {
		From   time.Time `json:"from"`
		To     time.Time `json:"to"`
		Bucket string    `json:"bucket"`
		Items  []Point   `json:"items"`
	}{From: from, To: to, Bucket: bucket, Items: points})
}

func (h *Handler) breakdown(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	dimension := strings.TrimSpace(request.URL.Query().Get("dimension"))
	if dimension == "" {
		writeError(response, http.StatusBadRequest, "invalid_request", "dimension is required")
		return
	}
	limit := DefaultBreakdownLimit
	if raw := request.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeError(response, http.StatusBadRequest, "invalid_request", "limit must be an integer")
			return
		}
		limit = parsed
	}
	projectID, from, to, ok := h.normalizedWindow(response, request)
	if !ok {
		return
	}
	groups, err := h.service.Breakdown(request.Context(), ownerUserID, projectID, dimension, &from, &to, limit)
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, struct {
		Dimension string    `json:"dimension"`
		From      time.Time `json:"from"`
		To        time.Time `json:"to"`
		Items     []Group   `json:"items"`
	}{Dimension: dimension, From: from, To: to, Items: groups})
}

// normalizedWindow parses optional project_id/from/to and applies the shared
// default-window and 90-day-cap rules so the response envelope and the query
// always describe the same window. It writes a 400 and returns ok=false on
// invalid input.
func (h *Handler) normalizedWindow(response http.ResponseWriter, request *http.Request) (string, time.Time, time.Time, bool) {
	query := request.URL.Query()
	projectID := strings.TrimSpace(query.Get("project_id"))
	var from, to *time.Time
	if raw := query.Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(response, http.StatusBadRequest, "invalid_request", "from must be an RFC3339 timestamp")
			return "", time.Time{}, time.Time{}, false
		}
		from = &parsed
	}
	if raw := query.Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(response, http.StatusBadRequest, "invalid_request", "to must be an RFC3339 timestamp")
			return "", time.Time{}, time.Time{}, false
		}
		to = &parsed
	}
	fromTime, toTime, err := EffectiveWindow(from, to)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return "", time.Time{}, time.Time{}, false
	}
	return projectID, fromTime, toTime, true
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
	if errors.Is(err, ErrInvalidParams) {
		writeError(response, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeError(response, http.StatusInternalServerError, "internal_error", "request could not be completed")
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
