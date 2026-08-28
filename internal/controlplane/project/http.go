package project

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	mux.HandleFunc("POST /api/v1/projects", h.create)
	mux.HandleFunc("GET /api/v1/projects", h.list)
	mux.HandleFunc("GET /api/v1/projects/{projectID}", h.get)
	mux.HandleFunc("PATCH /api/v1/projects/{projectID}", h.update)
}

func (h *Handler) create(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if err := decodeJSON(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	created, err := h.service.Create(request.Context(), ownerUserID, body.Name, body.Slug)
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, created)
}

func (h *Handler) list(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	projects, err := h.service.List(request.Context(), ownerUserID)
	if err != nil {
		writeServiceError(response, err)
		return
	}
	if projects == nil {
		projects = []Project{}
	}
	writeJSON(response, http.StatusOK, struct {
		Projects []Project `json:"projects"`
	}{Projects: projects})
}

func (h *Handler) get(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	selected, err := h.service.Get(request.Context(), ownerUserID, request.PathValue("projectID"))
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, selected)
}

func (h *Handler) update(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	var body struct {
		Name   *string `json:"name"`
		Slug   *string `json:"slug"`
		Status *string `json:"status"`
	}
	if err := decodeJSON(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	updated, err := h.service.Update(request.Context(), ownerUserID, request.PathValue("projectID"), UpdateParams(body))
	if err != nil {
		writeServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, updated)
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
		writeError(response, http.StatusNotFound, "project_not_found", "project was not found")
	case errors.Is(err, ErrConflict):
		writeError(response, http.StatusConflict, "project_conflict", "a project with that slug already exists")
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
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
