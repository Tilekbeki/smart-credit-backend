package httpapi

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"net/http"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/pipeline"
)

func RecoveryRoutes(repo pipeline.RecoveryRepository, m *auth.Middleware) func(chi.Router) {
	return func(r chi.Router) {
		r.With(m.Authentication, m.RequireRole(auth.RoleBankEmployee)).Post("/api/v1/bank/applications/{applicationId}/retry-processing", func(w http.ResponseWriter, r *http.Request) {
			id, ok := applicationID(w, r)
			if !ok {
				return
			}
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
			var raw json.RawMessage
			if !readJSON(w, r, &raw) {
				return
			}
			var input struct {
				Version int64  `json:"version"`
				Reason  string `json:"reason"`
			}
			if !managementObject(raw, &input, []string{"version", "reason"}, []string{"version", "reason"}) {
				writeManagementError(w, company.ErrInvalidInput)
				return
			}
			u, _ := auth.CurrentUser(r.Context())
			result, err := repo.RecoverJob(r.Context(), pipeline.RecoveryCommand{ActorID: u.ID, ApplicationID: id, Key: key, Reason: input.Reason, Version: input.Version})
			if err != nil {
				writeManagementError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(result.Status)
			_, _ = w.Write(result.Body)
		})
	}
}
