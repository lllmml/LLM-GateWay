package credential

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const maxRequestBodyBytes = 128 << 10

type CurrentUserID func(*http.Request) (string, bool)

type Handler struct {
	service       *Service
	currentUserID CurrentUserID
}

func NewHandler(service *Service, currentUserID CurrentUserID) *Handler {
	return &Handler{service: service, currentUserID: currentUserID}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/projects/{projectID}/provider-credentials", h.create)
	mux.HandleFunc("GET /api/v1/projects/{projectID}/provider-credentials", h.list)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/provider-credentials/{credentialID}/rotate", h.rotate)
	mux.HandleFunc("POST /api/v1/projects/{projectID}/provider-credentials/{credentialID}/disable", h.disable)
}

func (h *Handler) create(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Label    string `json:"label"`
		Secret   string `json:"secret"`
	}
	if err := decodeJSON(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	created, err := h.service.Create(request.Context(), ownerUserID, request.PathValue("projectID"), body.Provider, body.Label, body.Secret)
	if err != nil {
		writeCollectionServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, responseCredential(created))
}

func (h *Handler) list(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	credentials, err := h.service.List(request.Context(), ownerUserID, request.PathValue("projectID"))
	if err != nil {
		writeCollectionServiceError(response, err)
		return
	}
	items := make([]credentialResponse, 0, len(credentials))
	for _, current := range credentials {
		items = append(items, responseCredential(current))
	}
	writeJSON(response, http.StatusOK, struct {
		Credentials []credentialResponse `json:"credentials"`
	}{Credentials: items})
}

func (h *Handler) rotate(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	var body struct {
		Secret string `json:"secret"`
	}
	if err := decodeJSON(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	rotated, err := h.service.Rotate(
		request.Context(),
		ownerUserID,
		request.PathValue("projectID"),
		request.PathValue("credentialID"),
		body.Secret,
	)
	if err != nil {
		writeCredentialServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, responseCredential(rotated))
}

func (h *Handler) disable(response http.ResponseWriter, request *http.Request) {
	ownerUserID, ok := h.requireUser(response, request)
	if !ok {
		return
	}
	disabled, err := h.service.Disable(request.Context(), ownerUserID, request.PathValue("projectID"), request.PathValue("credentialID"))
	if err != nil {
		writeCredentialServiceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, responseCredential(disabled))
}

type credentialResponse struct {
	ID        string     `json:"id"`
	Provider  Provider   `json:"provider"`
	Label     string     `json:"label"`
	Status    Status     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	RotatedAt *time.Time `json:"rotated_at"`
}

func responseCredential(credential Credential) credentialResponse {
	return credentialResponse{
		ID:        credential.ID,
		Provider:  credential.Provider,
		Label:     credential.Label,
		Status:    credential.Status,
		CreatedAt: credential.CreatedAt,
		RotatedAt: credential.RotatedAt,
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

func writeCollectionServiceError(response http.ResponseWriter, err error) {
	writeServiceError(response, err, "project_not_found", "project was not found")
}

func writeCredentialServiceError(response http.ResponseWriter, err error) {
	writeServiceError(response, err, "provider_credential_not_found", "provider credential was not found")
}

func writeServiceError(response http.ResponseWriter, err error, notFoundCode, notFoundMessage string) {
	var validationErr *ValidationError
	switch {
	case errors.As(err, &validationErr):
		writeError(response, http.StatusBadRequest, "invalid_request", validationErr.Error())
	case errors.Is(err, ErrNotFound):
		writeError(response, http.StatusNotFound, notFoundCode, notFoundMessage)
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
