package httpapi

import (
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"net/http"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
)

type ManagementHandler struct {
	service    *company.ManagementService
	middleware *auth.Middleware
}

func NewManagementHandler(s *company.ManagementService, m *auth.Middleware) *ManagementHandler {
	return &ManagementHandler{s, m}
}
func (h *ManagementHandler) Register(r chi.Router) {
	r.With(h.middleware.Authentication).Patch("/api/v1/me", h.mutate("profile"))
	r.Group(func(r chi.Router) {
		r.Use(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleClient))
		r.Get("/api/v1/me/companies", h.memberships)
		r.Patch("/api/v1/companies/{companyId}", h.mutate("contact"))
		r.Post("/api/v1/companies/{companyId}/change-requests", h.mutate("registration"))
		r.Get("/api/v1/companies/{companyId}/change-requests", h.history("registration", false))
		r.Post("/api/v1/companies/{companyId}/change-requests/{requestId}/cancel", h.mutate("cancel_registration"))
		r.Post("/api/v1/companies/{companyId}/financial-declarations", h.mutate("financial"))
		r.Get("/api/v1/companies/{companyId}/financial-declarations", h.history("financial", false))
	})
	r.Group(func(r chi.Router) {
		r.Use(h.middleware.Authentication, h.middleware.RequireRole(auth.RoleBankEmployee))
		r.Get("/api/v1/bank/companies/{companyId}/memberships", h.memberships)
		r.Put("/api/v1/bank/companies/{companyId}/memberships/{userId}", h.mutate("membership"))
		r.Get("/api/v1/bank/companies/{companyId}/change-requests", h.history("registration", true))
		r.Post("/api/v1/bank/companies/{companyId}/change-requests/{requestId}/review", h.mutate("review_registration"))
		r.Get("/api/v1/bank/companies/{companyId}/financial-declarations", h.history("financial", true))
		r.Post("/api/v1/bank/companies/{companyId}/financial-declarations/{requestId}/review", h.mutate("review_financial"))
	})
}
func managementID(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	value := chi.URLParam(r, key)
	id, ok := parseID(value)
	if !ok {
		writeManagementError(w, company.ErrInvalidInput)
	}
	return id, ok
}
func (h *ManagementHandler) memberships(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.CurrentUser(r.Context())
	id := ""
	if chi.URLParam(r, "companyId") != "" {
		var ok bool
		id, ok = managementID(w, r, "companyId")
		if !ok {
			return
		}
	}
	items, err := h.service.Memberships(r.Context(), u.ID, id)
	if err != nil {
		writeManagementError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (h *ManagementHandler) history(kind string, bank bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := managementID(w, r, "companyId")
		if !ok {
			return
		}
		u, _ := auth.CurrentUser(r.Context())
		items, err := h.service.History(r.Context(), u.ID, id, kind, bank)
		if err != nil {
			writeManagementError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
	}
}
func (h *ManagementHandler) mutate(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, _ := auth.CurrentUser(r.Context())
		c := company.Command{ActorID: u.ID, Action: action}
		if action != "profile" {
			id, ok := managementID(w, r, "companyId")
			if !ok {
				return
			}
			c.CompanyID = id
		}
		if action == "membership" {
			id, ok := managementID(w, r, "userId")
			if !ok {
				return
			}
			c.TargetID = id
		}
		if action == "review_financial" || action == "review_registration" || action == "cancel_registration" {
			id, ok := managementID(w, r, "requestId")
			if !ok {
				return
			}
			c.TargetID = id
		}
		if action != "profile" && action != "contact" {
			keys := r.Header.Values("Idempotency-Key")
			if len(keys) != 1 {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			key, ok := parseID(keys[0])
			if !ok {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			c.Key = key
		}
		var raw json.RawMessage
		if !readJSON(w, r, &raw) {
			return
		}
		// A separate DTO per operation prevents universal arbitrary-column PATCH.
		switch action {
		case "profile":
			var v struct {
				Version         int64   `json:"version"`
				DisplayName     *string `json:"displayName"`
				AdditionalEmail *string `json:"additionalEmail"`
			}
			if !managementObject(raw, &v, []string{"version", "displayName", "additionalEmail"}, []string{"version"}) {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			c.Version = v.Version
			c.DisplayName = v.DisplayName
			c.AdditionalEmail = v.AdditionalEmail
		case "contact":
			var v struct {
				Version int64            `json:"version"`
				Contact *company.Contact `json:"contact"`
			}
			if !managementObject(raw, &v, []string{"version", "contact"}, []string{"version", "contact"}) {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			var obj map[string]json.RawMessage
			_ = json.Unmarshal(raw, &obj)
			if _, ok := strictLendingObject(obj["contact"], new(company.Contact), []string{"phone", "email", "address"}, ""); !ok {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			c.Version = v.Version
			c.Contact = v.Contact
		case "membership":
			var v struct {
				Version        int64  `json:"version"`
				MembershipRole string `json:"membershipRole"`
				Status         string `json:"status"`
				Reason         string `json:"reason"`
			}
			if !managementObject(raw, &v, []string{"version", "membershipRole", "status", "reason"}, []string{"version", "membershipRole", "status", "reason"}) {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			c.Version = v.Version
			c.MembershipRole = v.MembershipRole
			c.Status = v.Status
			c.Reason = v.Reason
		case "registration":
			var v struct {
				Version int64                        `json:"version"`
				Changes *company.RegistrationChanges `json:"changes"`
				Reason  string                       `json:"reason"`
			}
			if !managementObject(raw, &v, []string{"version", "changes", "reason"}, []string{"version", "changes", "reason"}) {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			var obj map[string]json.RawMessage
			_ = json.Unmarshal(raw, &obj)
			if !managementObject(obj["changes"], new(company.RegistrationChanges), []string{"name", "inn", "registrationNumber", "registrationDate", "okved", "directorName"}, nil) {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			c.Version = v.Version
			c.Changes = v.Changes
			c.Reason = v.Reason
		case "financial":
			var v struct {
				Version    int64   `json:"version"`
				Revenue    *int64  `json:"monthlyRevenueRub"`
				Expenses   *int64  `json:"monthlyExpensesRub"`
				Reason     string  `json:"reason"`
				DocumentID *string `json:"supportingDocumentId"`
			}
			if !managementObject(raw, &v, []string{"version", "monthlyRevenueRub", "monthlyExpensesRub", "reason", "supportingDocumentId"}, []string{"version", "monthlyRevenueRub", "monthlyExpensesRub", "reason"}) {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			c.Version = v.Version
			c.MonthlyRevenueRub = v.Revenue
			c.MonthlyExpensesRub = v.Expenses
			c.Reason = v.Reason
			c.SupportingDocumentID = v.DocumentID
		default:
			var v struct {
				Version int64  `json:"version"`
				Outcome string `json:"outcome"`
				Reason  string `json:"reason"`
			}
			allowed := []string{"version", "outcome", "reason"}
			if action == "cancel_registration" {
				allowed = []string{"version", "reason"}
			}
			if !managementObject(raw, &v, allowed, allowed) {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			c.Version = v.Version
			c.Outcome = v.Outcome
			c.Reason = v.Reason
		}
		result, err := h.service.Execute(r.Context(), c)
		if err != nil {
			writeManagementError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(result.Status)
		_, _ = w.Write(result.Body)
	}
}
func managementObject(raw []byte, target any, allowed, required []string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return false
	}
	for k, v := range object {
		found := false
		for _, key := range allowed {
			if key == k {
				found = true
			}
		}
		if !found || string(v) == "null" {
			return false
		}
	}
	for _, key := range required {
		if _, ok := object[key]; !ok {
			return false
		}
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	_, ok := strictLendingObject(raw, target, keys, "")
	return ok
}
func writeManagementError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, company.ErrInvalidInput):
		writeAPIError(w, 400, "invalid_request", "Проверьте поля, формат и версию данных.")
	case errors.Is(err, company.ErrNotFound):
		writeAPIError(w, 404, "company_not_found", "Компания или запись не найдена.")
	case errors.Is(err, company.ErrForbidden):
		writeAPIError(w, 403, "forbidden", "Недостаточно прав.")
	case errors.Is(err, company.ErrVersionConflict):
		writeAPIError(w, 409, "version_conflict", "Данные изменены. Загрузите актуальную версию.")
	case errors.Is(err, company.ErrInvalidStatus):
		writeAPIError(w, 409, "invalid_status", "Запрос уже рассмотрен или отменён.")
	case errors.Is(err, company.ErrIdempotencyConflict):
		writeAPIError(w, 409, "idempotency_conflict", "Ключ уже использован с другим запросом.")
	default:
		writeAPIError(w, 503, "service_unavailable", "Сервис управления данными временно недоступен.")
	}
}
