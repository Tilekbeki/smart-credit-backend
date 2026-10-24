package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/contact"
)

type ContactHandler struct {
	service    *contact.Service
	middleware *auth.Middleware
}

func NewContactHandler(service *contact.Service, middleware *auth.Middleware) *ContactHandler {
	return &ContactHandler{service: service, middleware: middleware}
}

func (h *ContactHandler) Register(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleClient))
		r.Patch("/api/v1/me", h.user)
		r.Patch("/api/v1/companies/{companyId}", h.company)
	})
}

func (h *ContactHandler) user(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if !readJSON(w, r, &raw) {
		return
	}
	var changes contact.UserChanges
	if _, ok := strictLendingObject(raw, &changes, []string{"version", "additionalEmail"}, "additionalEmail"); !ok {
		writeContactError(w, contact.ErrInvalidInput)
		return
	}
	u, _ := auth.CurrentUser(r.Context())
	result, err := h.service.UpdateUser(r.Context(), u, changes)
	if err != nil {
		writeContactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ContactHandler) company(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(chi.URLParam(r, "companyId"))
	if !ok {
		writeContactError(w, contact.ErrInvalidInput)
		return
	}
	var raw json.RawMessage
	if !readJSON(w, r, &raw) {
		return
	}
	var changes contact.CompanyChanges
	object, ok := strictLendingObject(raw, &changes, []string{"version", "contact"}, "")
	if !ok {
		writeContactError(w, contact.ErrInvalidInput)
		return
	}
	if _, ok = strictLendingObject(object["contact"], new(contact.CompanyFields), nil, "", "phone", "email", "address"); !ok {
		writeContactError(w, contact.ErrInvalidInput)
		return
	}
	u, _ := auth.CurrentUser(r.Context())
	result, err := h.service.UpdateCompany(r.Context(), u, id, changes)
	if err != nil {
		writeContactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeContactError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contact.ErrInvalidInput):
		writeAPIError(w, 400, "invalid_request", "Проверьте контактные данные и версию. Разрешены только контактные поля.")
	case errors.Is(err, contact.ErrForbidden):
		writeAPIError(w, 403, "forbidden", "Изменение контактных данных запрещено.")
	case errors.Is(err, contact.ErrVersionConflict):
		writeAPIError(w, 409, "version_conflict", "Контактные данные изменены. Загрузите актуальную версию.")
	default:
		writeAPIError(w, 503, "service_unavailable", "Сервис контактных данных временно недоступен.")
	}
}
