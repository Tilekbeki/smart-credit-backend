package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/review"
)

type ReviewHandler struct {
	service    *review.Service
	middleware *auth.Middleware
}

func NewReviewHandler(service *review.Service, middleware *auth.Middleware) *ReviewHandler {
	return &ReviewHandler{service: service, middleware: middleware}
}
func (h *ReviewHandler) Register(r chi.Router) {
	r.With(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleClient)).Get("/api/v1/applications/{applicationId}/decision", h.decision)
	r.Group(func(r chi.Router) {
		r.Use(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleBankEmployee))
		r.Get("/api/v1/bank/applications", h.list)
		r.Get("/api/v1/bank/applications/{applicationId}", h.detail)
		r.Get("/api/v1/bank/applications/{applicationId}/assessment", h.assessment)
	})
}
func (h *ReviewHandler) decision(w http.ResponseWriter, r *http.Request) {
	id, ok := applicationID(w, r)
	if !ok {
		return
	}
	result, err := h.service.Decision(r.Context(), applicationScope(r), id)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (h *ReviewHandler) detail(w http.ResponseWriter, r *http.Request) {
	id, ok := applicationID(w, r)
	if !ok {
		return
	}
	result, err := h.service.BankDetail(r.Context(), id)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (h *ReviewHandler) assessment(w http.ResponseWriter, r *http.Request) {
	id, ok := applicationID(w, r)
	if !ok {
		return
	}
	result, err := h.service.BankAssessment(r.Context(), id)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (h *ReviewHandler) list(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeApplicationError(w, application.ErrInvalidInput)
		return
	}
	f := review.Filter{Limit: 20}
	for key, values := range query {
		if len(values) != 1 {
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
		switch key {
		case "status":
			status := application.Status(values[0])
			f.Status = &status
		case "companyId":
			id, ok := parseID(values[0])
			if !ok {
				writeApplicationError(w, application.ErrInvalidInput)
				return
			}
			f.CompanyID = &id
		case "limit", "offset":
			n, err := strconv.ParseInt(values[0], 10, 32)
			if err != nil {
				writeApplicationError(w, application.ErrInvalidInput)
				return
			}
			if key == "limit" {
				f.Limit = int(n)
			} else {
				f.Offset = int(n)
			}
		default:
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
	}
	result, err := h.service.BankList(r.Context(), f)
	if err != nil {
		writeApplicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
