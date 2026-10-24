package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
)

const ownCompanyID = "10000000-0000-4000-8000-000000000001"
const otherCompanyID = "10000000-0000-4000-8000-000000000002"

type creditRepoStub struct {
	company      company.Company
	products     []credit.Product
	err          error
	companyReads int
}

func (r *creditRepoStub) CompanyByID(_ context.Context, id string) (company.Company, error) {
	r.companyReads++
	if r.err != nil {
		return company.Company{}, r.err
	}
	if id != r.company.ID {
		return company.Company{}, company.ErrNotFound
	}
	return r.company, nil
}
func (r *creditRepoStub) ListProducts(context.Context) ([]credit.Product, error) {
	return r.products, r.err
}
func (r *creditRepoStub) ProductByID(_ context.Context, id string) (credit.Product, error) {
	if r.err != nil {
		return credit.Product{}, r.err
	}
	for _, p := range r.products {
		if p.ID == id {
			return p, nil
		}
	}
	return credit.Product{}, credit.ErrProductNotFound
}

func creditFixture() *creditRepoStub {
	return &creditRepoStub{
		company: company.Company{ID: ownCompanyID, Name: "ИП «Лавка у дома»", BankRelationship: "alfa_account", OrganizationType: "ИП", INN: "000000000001", RegistrationNumber: "000000000000001", RegistrationDate: "2020-01-15", OKVED: "47.11", DirectorName: "Иван Лавкин", Contact: company.Contact{Phone: "+79990000001", Email: "shop@example.test", Address: "Учебный адрес"}, Financials: company.Financials{MonthlyRevenueRub: 2400000, MonthlyExpensesRub: 1500000, ExistingDebtRub: 500000, ExistingMonthlyPaymentsRub: 90000}, Simulation: true},
		products: []credit.Product{
			{ID: "turnover", Name: "Оборотный кредит", Purposes: []string{"stock", "expenses", "seasonal"}, MinAmountRub: 100000, MaxAmountRub: 3000000, MinTermMonths: 6, MaxTermMonths: 24, AnnualRatePercent: json.Number("22.0000"), MinBusinessAgeMonths: 6, Version: 1, Illustrative: true},
			{ID: "investment", Name: "Инвестиционный кредит", Purposes: []string{"equipment", "upgrade"}, MinAmountRub: 300000, MaxAmountRub: 10000000, MinTermMonths: 12, MaxTermMonths: 60, AnnualRatePercent: json.Number("20.0000"), MinBusinessAgeMonths: 12, Version: 1, Illustrative: true},
		},
	}
}

func creditTestHandler(t *testing.T, repo *creditRepoStub, role auth.Role) (http.Handler, string) {
	t.Helper()
	user := demoUser()
	user.Role = role
	if role == auth.RoleBankEmployee {
		user.CompanyID = nil
	}
	authRepo := &authRepoStub{user: user}
	tokens, err := auth.NewTokens(strings.Repeat("t", 32), "http-test", "http-test")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	creditHandler := NewCreditHandler(company.NewService(repo), credit.NewService(repo, repo), auth.NewMiddleware(tokens, authRepo, time.Second))
	return NewHandler(nil, testLogger(), time.Second, nil, creditHandler.Register), token
}

func decodeCreditResult(t *testing.T, wBody []byte) credit.Result {
	t.Helper()
	var result credit.Result
	if err := json.Unmarshal(wBody, &result); err != nil {
		t.Fatal("invalid recommendation JSON")
	}
	return result
}

func TestCompanyHTTPOnlyOwnCompany(t *testing.T) {
	repo := creditFixture()
	h, token := creditTestHandler(t, repo, auth.RoleClient)
	w := authRequest(h, http.MethodGet, "/api/v1/companies/"+ownCompanyID, "", token)
	var result company.Company
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ID != ownCompanyID || result.BankRelationship != "alfa_account" || result.OrganizationType != "ИП" || result.Contact.Email == "" || result.Financials.ExistingMonthlyPaymentsRub != 90000 {
		t.Fatal("own company details failed")
	}
	reads := repo.companyReads
	w = authRequest(h, http.MethodGet, "/api/v1/companies/"+otherCompanyID, "", token)
	if w.Code != 403 || responseJSON(t, w)["error"] != "forbidden" || repo.companyReads != reads {
		t.Fatal("foreign company should be forbidden before reading")
	}
	w = authRequest(h, http.MethodGet, "/api/v1/companies/not-a-uuid", "", token)
	if w.Code != 400 || responseJSON(t, w)["error"] != "invalid_request" {
		t.Fatal("invalid company ID accepted")
	}
	repo.company.ID = otherCompanyID
	w = authRequest(h, http.MethodGet, "/api/v1/companies/"+ownCompanyID, "", token)
	if w.Code != 404 || responseJSON(t, w)["error"] != "company_not_found" {
		t.Fatal("missing own company should return 404")
	}
}

func TestProductsHTTPUsesPublicCodesAndDatabaseConditions(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleClient, auth.RoleBankEmployee} {
		t.Run(string(role), func(t *testing.T) {
			repo := creditFixture()
			h, token := creditTestHandler(t, repo, role)
			w := authRequest(h, http.MethodGet, "/api/v1/credit-products", "", token)
			var result struct {
				Items []credit.Product `json:"items"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Items) != 2 {
				t.Fatal("catalog failed")
			}
			for _, product := range repo.products {
				w = authRequest(h, http.MethodGet, "/api/v1/credit-products/"+product.ID, "", token)
				var got credit.Product
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ID != product.ID || got.AnnualRatePercent != product.AnnualRatePercent || got.MinAmountRub != product.MinAmountRub {
					t.Fatal("product detail did not use repository")
				}
			}
			w = authRequest(h, http.MethodGet, "/api/v1/credit-products/missing", "", token)
			if w.Code != 404 || responseJSON(t, w)["error"] != "product_not_found" {
				t.Fatal("missing product should return 404")
			}
		})
	}
}

func TestRecommendationHTTPPositiveAndExpectedNoMatch(t *testing.T) {
	for _, tc := range []struct {
		name, purpose     string
		amount            int64
		term              int
		revenue, expenses int64
		recommended       string
	}{
		{"turnover", "stock", 1500000, 12, 2400000, 1500000, "turnover"},
		{"investment", "equipment", 3000000, 36, 4200000, 2800000, "investment"},
		{"amount below all minima", "stock", 50000, 12, 2400000, 1500000, ""},
		{"amount exceeds product", "stock", 3500000, 12, 2400000, 1500000, ""},
		{"term below product", "stock", 1500000, 3, 2400000, 1500000, ""},
		{"insufficient budget", "stock", 1500000, 12, 100000, 90000, ""},
		{"zero revenue", "stock", 1500000, 12, 0, 0, ""},
		{"negative surplus", "stock", 1500000, 12, 100000, 200000, ""},
		{"unmatched valid purpose", "other", 1500000, 12, 2400000, 1500000, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := creditFixture()
			h, token := creditTestHandler(t, repo, auth.RoleClient)
			input := credit.Input{Purpose: tc.purpose, RequestedAmountRub: tc.amount, TermMonths: tc.term, MonthlyRevenueRub: tc.revenue, MonthlyExpensesRub: tc.expenses}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			w := authRequest(h, http.MethodPost, "/api/v1/recommendations", string(body), token)
			if w.Code != 200 {
				t.Fatalf("valid selection should return 200, got %d", w.Code)
			}
			result := decodeCreditResult(t, w.Body.Bytes())
			if result.CompanyID != ownCompanyID || len(result.Products) != 2 || !result.Simulation || result.Disclaimer == "" {
				t.Fatal("recommendation metadata missing")
			}
			if tc.recommended == "" {
				if result.RecommendedProductID != nil {
					t.Fatal("unsuitable input must not be recommended")
				}
			} else if result.RecommendedProductID == nil || *result.RecommendedProductID != tc.recommended {
				t.Fatal("wrong recommended product")
			}
			if tc.name == "turnover" && (result.SurplusRub != 900000 || result.PaymentBudgetRub != 360000 || result.ExistingMonthlyPaymentsRub != 90000) {
				t.Fatal("budget must use request cash flow and stored loan payments")
			}
		})
	}
}

func TestRecommendationHTTPRejectsInvalidOrInjectedFields(t *testing.T) {
	repo := creditFixture()
	h, token := creditTestHandler(t, repo, auth.RoleClient)
	valid := `{"purpose":"stock","requestedAmountRub":1500000,"termMonths":12,"monthlyRevenueRub":2400000,"monthlyExpensesRub":1500000}`
	for _, body := range []string{
		`{}`, `null`, `[]`,
		strings.Replace(valid, `"stock"`, `"unknown"`, 1),
		strings.Replace(valid, `1500000`, `-1`, 1),
		strings.Replace(valid, `1500000`, `1.5`, 1),
		strings.Replace(valid, `"termMonths":12`, `"termMonths":0`, 1),
		strings.Replace(valid, `"monthlyRevenueRub":2400000`, `"monthlyRevenueRub":-1`, 1),
		strings.Replace(valid, `"monthlyExpensesRub":1500000`, `"monthlyExpensesRub":null`, 1),
		strings.TrimSuffix(valid, "}") + `,"companyId":"` + otherCompanyID + `"}`,
	} {
		w := authRequest(h, http.MethodPost, "/api/v1/recommendations", body, token)
		if w.Code != 400 || responseJSON(t, w)["error"] != "invalid_request" {
			t.Fatalf("invalid recommendation body returned %d", w.Code)
		}
	}
	if repo.companyReads != 0 {
		t.Fatal("invalid input should be rejected before company lookup")
	}
}

func TestCreditHTTPRejectsMissingJWTAndWrongRole(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleClient, auth.RoleBankEmployee} {
		repo := creditFixture()
		h, token := creditTestHandler(t, repo, role)
		for _, tc := range []struct{ method, path string }{{"GET", "/api/v1/companies/" + ownCompanyID}, {"GET", "/api/v1/credit-products"}, {"GET", "/api/v1/credit-products/turnover"}, {"POST", "/api/v1/recommendations"}} {
			if w := authRequest(h, tc.method, tc.path, "", ""); w.Code != 401 {
				t.Fatal("missing JWT must be unauthorized")
			}
			if role == auth.RoleBankEmployee && (strings.Contains(tc.path, "companies") || strings.Contains(tc.path, "recommendations")) {
				w := authRequest(h, tc.method, tc.path, "", token)
				if w.Code != 403 || responseJSON(t, w)["error"] != "forbidden" {
					t.Fatal("bank employee reached client-only API")
				}
			}
		}
	}
}

func TestCreditHTTPDoesNotExposeDatabaseErrors(t *testing.T) {
	repo := creditFixture()
	repo.err = errors.New("postgres://private-user:private-password@private-db")
	h, token := creditTestHandler(t, repo, auth.RoleClient)
	for _, path := range []string{"/api/v1/companies/" + ownCompanyID, "/api/v1/credit-products", "/api/v1/credit-products/turnover"} {
		w := authRequest(h, "GET", path, "", token)
		if w.Code != 503 || responseJSON(t, w)["error"] != "service_unavailable" || strings.Contains(w.Body.String(), "private-") {
			t.Fatal("database error was exposed")
		}
	}
}
