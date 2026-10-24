package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
)

type ApplicationHandler struct {
	service    *application.Service
	middleware *auth.Middleware
}

func NewApplicationHandler(service *application.Service, middleware *auth.Middleware) *ApplicationHandler {
	return &ApplicationHandler{service: service, middleware: middleware}
}

func (h *ApplicationHandler) Register(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleClient))
		r.Post("/api/v1/applications", h.create)
		r.Get("/api/v1/applications", h.list)
		r.Get("/api/v1/applications/{applicationId}", h.get)
		r.Patch("/api/v1/applications/{applicationId}", h.patch)
		r.Get("/api/v1/applications/{applicationId}/timeline", h.timeline)
	})
}

func applicationScope(r *http.Request) application.Scope {
	u, _ := auth.CurrentUser(r.Context())
	scope := application.Scope{UserID: u.ID}
	if u.CompanyID != nil {
		scope.CompanyID = *u.CompanyID
	}

	return scope
}

func applicationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := parseID(chi.URLParam(r, "applicationId"))
	if !ok {
		writeApplicationError(w, application.ErrInvalidInput)
	}
	return id, ok
}

func (h *ApplicationHandler) create(w http.ResponseWriter, r *http.Request) {
	var input *struct {
		application.Fields
		CompanyID application.Optional[string] `json:"companyId"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input == nil {
		writeApplicationError(w, application.ErrInvalidInput)
		return
	}
	scope := applicationScope(r)
	if input.CompanyID.Present {
		if input.CompanyID.Value == nil {
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
		id, ok := parseID(*input.CompanyID.Value)
		if !ok {
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
		scope.CompanyID = id
	}
	result, err := h.service.Create(r.Context(), scope, input.Fields)
	if err != nil {
		if errors.Is(err, application.ErrNotFound) {
			writeAPIError(w, 404, "company_not_found", "Компания не найдена.")
			return
		}
		writeApplicationError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/applications/"+result.ApplicationID)
	writeJSON(w, http.StatusCreated, result)
}

func (h *ApplicationHandler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := applicationID(w, r)
	if !ok {
		return
	}
	result, err := h.service.Get(r.Context(), applicationScope(r), id)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ApplicationHandler) patch(w http.ResponseWriter, r *http.Request) {
	id, ok := applicationID(w, r)
	if !ok {
		return
	}
	var input struct {
		Version *int64 `json:"version"`
		application.Changes
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Version == nil {
		writeApplicationError(w, application.ErrInvalidInput)
		return
	}
	result, err := h.service.Patch(r.Context(), applicationScope(r), id, *input.Version, input.Changes)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ApplicationHandler) list(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeApplicationError(w, application.ErrInvalidInput)
		return
	}
	filter := application.ListFilter{Limit: 20}
	scope := applicationScope(r)
	for key, values := range query {
		if len(values) != 1 {
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
		if key == "companyId" {
			id, ok := parseID(values[0])
			if !ok {
				writeApplicationError(w, application.ErrInvalidInput)
				return
			}
			scope.CompanyID = id
			continue
		}
		if key == "status" {
			status := application.Status(values[0])
			filter.Status = &status
			continue
		}
		if key != "limit" && key != "offset" {
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
		n, err := strconv.ParseInt(values[0], 10, 32)
		if err != nil {
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
		if key == "limit" {
			filter.Limit = int(n)
		} else {
			filter.Offset = int(n)
		}
	}
	result, err := h.service.List(r.Context(), scope, filter)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *ApplicationHandler) timeline(w http.ResponseWriter, r *http.Request) {
	id, ok := applicationID(w, r)
	if !ok {
		return
	}
	result, err := h.service.Timeline(r.Context(), applicationScope(r), id)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items []application.Event `json:"items"`
	}{result})
}

func writeApplicationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrCompanyRequired):
		writeAPIError(w, 409, "company_required", "Выберите компанию явно через companyId.")
	case errors.Is(err, application.ErrInvalidInput):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Проверьте поля черновика, версию и параметры пагинации.")
	case errors.Is(err, application.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "application_not_found", "Заявка не найдена.")
	case errors.Is(err, application.ErrProductNotFound):
		writeAPIError(w, http.StatusNotFound, "product_not_found", "Кредитный продукт не найден.")
	case errors.Is(err, application.ErrVersionConflict):
		writeAPIError(w, http.StatusConflict, "version_conflict", "Заявка изменена. Загрузите актуальную версию.")
	case errors.Is(err, application.ErrInvalidStatus):
		writeAPIError(w, http.StatusConflict, "invalid_status", "Редактирование доступно только для черновика.")
	default:
		writeAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "Сервис заявок временно недоступен.")
	}
}
