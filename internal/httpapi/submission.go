package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/pipeline"
)

type SubmissionHandler struct {
	service    *pipeline.SubmissionService
	middleware *auth.Middleware
}

func NewSubmissionHandler(service *pipeline.SubmissionService, middleware *auth.Middleware) *SubmissionHandler {
	return &SubmissionHandler{service: service, middleware: middleware}
}

func (h *SubmissionHandler) Register(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleClient))
		r.Post("/api/v1/applications/{applicationId}/submit", h.submit)
	})
}

func (h *SubmissionHandler) submit(w http.ResponseWriter, r *http.Request) {
	id, ok := applicationID(w, r)
	if !ok {
		return
	}
	scope := applicationScope(r)
	if err := h.service.Authorize(r.Context(), scope, id); err != nil {
		writeApplicationError(w, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Для отправки заявки тело запроса не требуется.")
		return
	}
	result, err := h.service.Submit(r.Context(), scope, id)
	if err != nil {
		var validation *pipeline.ValidationError
		if errors.As(err, &validation) {
			writeJSON(w, http.StatusUnprocessableEntity, struct {
				Error   string                `json:"error"`
				Message string                `json:"message"`
				Details []pipeline.FieldError `json:"details"`
			}{"validation_failed", "Проверьте обязательные поля и документы заявки.", validation.Details})
		} else if errors.Is(err, application.ErrInvalidStatus) {
			writeAPIError(w, http.StatusConflict, "invalid_status", "Отправка недоступна в текущем состоянии заявки.")
		} else {
			writeApplicationError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
