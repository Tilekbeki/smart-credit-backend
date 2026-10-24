package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
)

type CreditHandler struct {
	companies  *company.Service
	credit     *credit.Service
	middleware *auth.Middleware
}

func NewCreditHandler(companies *company.Service, creditService *credit.Service, middleware *auth.Middleware) *CreditHandler {
	return &CreditHandler{companies: companies, credit: creditService, middleware: middleware}
}

func (h *CreditHandler) Register(r chi.Router) {
	r.Group(func(protected chi.Router) {
		protected.Use(h.middleware.Authentication)
		protected.With(h.middleware.RequireRole(auth.RoleClient)).Get("/api/v1/companies/{companyId}", h.company)
		protected.With(h.middleware.RequireRole(auth.RoleClient, auth.RoleBankEmployee)).Get("/api/v1/credit-products", h.products)
		protected.With(h.middleware.RequireRole(auth.RoleClient, auth.RoleBankEmployee)).Get("/api/v1/credit-products/{productId}", h.product)
		protected.With(h.middleware.RequireRole(auth.RoleClient)).Post("/api/v1/recommendations", h.recommend)
	})
}

func (h *CreditHandler) company(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(chi.URLParam(r, "companyId"))
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Поле companyId должно содержать UUID компании.")
		return
	}
	user, _ := auth.CurrentUser(r.Context())
	result, err := h.companies.Get(r.Context(), user.ID, id)
	if err != nil {
		writeCreditError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *CreditHandler) products(w http.ResponseWriter, r *http.Request) {
	products, err := h.credit.ListProducts(r.Context())
	if err != nil {
		writeCreditError(w, err)
		return
	}
	if products == nil {
		products = []credit.Product{}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []credit.Product `json:"items"`
	}{products})
}

func (h *CreditHandler) product(w http.ResponseWriter, r *http.Request) {
	result, err := h.credit.Product(r.Context(), chi.URLParam(r, "productId"))
	if err != nil {
		writeCreditError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *CreditHandler) recommend(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CompanyID          application.Optional[string] `json:"companyId"`
		Purpose            *string                      `json:"purpose"`
		RequestedAmountRub *int64                       `json:"requestedAmountRub"`
		TermMonths         *int                         `json:"termMonths"`
		MonthlyRevenueRub  *int64                       `json:"monthlyRevenueRub"`
		MonthlyExpensesRub *int64                       `json:"monthlyExpensesRub"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Purpose == nil || input.RequestedAmountRub == nil || input.TermMonths == nil || input.MonthlyRevenueRub == nil || input.MonthlyExpensesRub == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Укажите purpose, requestedAmountRub, termMonths, monthlyRevenueRub и monthlyExpensesRub.")
		return
	}
	user, _ := auth.CurrentUser(r.Context())
	selected := ""
	if input.CompanyID.Present {
		if input.CompanyID.Value == nil {
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
		var ok bool
		selected, ok = parseID(*input.CompanyID.Value)
		if !ok {
			writeApplicationError(w, application.ErrInvalidInput)
			return
		}
	}
	selected, err := h.companies.ResolveCompany(r.Context(), user.ID, selected, false)
	if err != nil {
		if errors.Is(err, company.ErrForbidden) {
			writeAPIError(w, http.StatusNotFound, "company_not_found", "Компания не найдена.")
		} else {
			writeApplicationError(w, err)
		}
		return
	}
	result, err := h.credit.Recommend(r.Context(), selected, credit.Input{
		Purpose: *input.Purpose, RequestedAmountRub: *input.RequestedAmountRub, TermMonths: *input.TermMonths,
		MonthlyRevenueRub: *input.MonthlyRevenueRub, MonthlyExpensesRub: *input.MonthlyExpensesRub,
	})
	if err != nil {
		writeCreditError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeCreditError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, company.ErrForbidden):
		writeAPIError(w, http.StatusForbidden, "forbidden", "Доступ к компании запрещён.")
	case errors.Is(err, company.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "company_not_found", "Компания не найдена.")
	case errors.Is(err, credit.ErrProductNotFound):
		writeAPIError(w, http.StatusNotFound, "product_not_found", "Кредитный продукт не найден.")
	case errors.Is(err, credit.ErrInvalidInput):
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "Проверьте цель, сумму, срок, выручку и расходы. Суммы указываются целыми рублями.")
	default:
		writeAPIError(w, http.StatusServiceUnavailable, "service_unavailable", "Сервис подбора финансирования временно недоступен.")
	}
}
