package credit

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"smart-credit-backend/internal/company"
)

type fakeProducts struct {
	items []Product
	err   error
}

func (f *fakeProducts) ListProducts(context.Context) ([]Product, error) {
	return append([]Product(nil), f.items...), f.err
}

func (f *fakeProducts) ProductByID(_ context.Context, id string) (Product, error) {
	if f.err != nil {
		return Product{}, f.err
	}
	for _, item := range f.items {
		if item.ID == id {
			return item, nil
		}
	}
	return Product{}, ErrProductNotFound
}

type fakeCompanies struct {
	item        company.Company
	err         error
	requestedID string
}

func (f *fakeCompanies) CompanyByID(_ context.Context, id string) (company.Company, error) {
	f.requestedID = id
	return f.item, f.err
}

func demoProducts() []Product {
	return []Product{
		{ID: "turnover", Name: "Оборотный кредит", Purposes: []string{"stock", "expenses", "seasonal"}, MinAmountRub: 100_000, MaxAmountRub: 3_000_000, MinTermMonths: 6, MaxTermMonths: 24, AnnualRatePercent: "22", MinBusinessAgeMonths: 6, Version: 1, Illustrative: true},
		{ID: "investment", Name: "Инвестиционный кредит", Purposes: []string{"equipment", "upgrade"}, MinAmountRub: 300_000, MaxAmountRub: 10_000_000, MinTermMonths: 12, MaxTermMonths: 60, AnnualRatePercent: "20", MinBusinessAgeMonths: 12, Version: 1, Illustrative: true},
	}
}

func serviceFixture() (*Service, *fakeProducts, *fakeCompanies) {
	products := &fakeProducts{items: demoProducts()}
	companies := &fakeCompanies{item: company.Company{ID: "own-company", RegistrationDate: "2020-01-01", Financials: company.Financials{MonthlyRevenueRub: 99, MonthlyExpensesRub: 99, ExistingMonthlyPaymentsRub: 100_000}}}
	service := NewService(products, companies)
	service.now = func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }
	return service, products, companies
}

func demoInput() Input {
	return Input{Purpose: "stock", RequestedAmountRub: 1_000_000, TermMonths: 12, MonthlyRevenueRub: 2_000_000, MonthlyExpensesRub: 1_000_000}
}

func productResult(t *testing.T, result Result, id string) ProductRecommendation {
	t.Helper()
	for _, product := range result.Products {
		if product.Product.ID == id {
			return product
		}
	}
	t.Fatalf("product %q not in result", id)
	return ProductRecommendation{}
}

func TestRecommendTurnoverAndInvestment(t *testing.T) {
	for _, tt := range []struct {
		purpose, product string
		amount, payment  int64
	}{
		{"stock", "turnover", 1_000_000, 93_595},
		{"expenses", "turnover", 1_000_000, 93_595},
		{"seasonal", "turnover", 1_000_000, 93_595},
		{"equipment", "investment", 2_000_000, 185_270},
		{"upgrade", "investment", 2_000_000, 185_270},
	} {
		t.Run(tt.purpose, func(t *testing.T) {
			service, _, companies := serviceFixture()
			input := demoInput()
			input.Purpose = tt.purpose
			input.RequestedAmountRub = tt.amount
			result, err := service.Recommend(context.Background(), "own-company", input)
			if err != nil {
				t.Fatal(err)
			}
			if companies.requestedID != "own-company" {
				t.Fatal("company not loaded using authenticated company ID")
			}
			if result.RecommendedProductID == nil || *result.RecommendedProductID != tt.product {
				t.Fatalf("unexpected recommended product: %v", result.RecommendedProductID)
			}
			if result.SurplusRub != 1_000_000 || result.PaymentBudgetRub != 400_000 || result.ExistingMonthlyPaymentsRub != 100_000 {
				t.Fatalf("unexpected financing data: %+v", result)
			}
			product := productResult(t, result, tt.product)
			if !product.Eligible || product.MonthlyPaymentRub == nil || *product.MonthlyPaymentRub != tt.payment {
				t.Fatalf("unexpected recommendation: %+v", product)
			}
			if !reflect.DeepEqual(product.Matches, Matches{true, true, true, true, true}) {
				t.Fatalf("matches = %+v", product.Matches)
			}
			if product.Limitations == nil || len(product.Limitations) != 0 || product.Adjustments == nil || len(product.Adjustments) != 0 || len(product.Reasons) == 0 {
				t.Fatalf("invalid explanation arrays: %+v", product)
			}
			if !result.Simulation || result.Disclaimer == "" {
				t.Fatal("missing preliminary/demo disclaimer")
			}
		})
	}
}

func TestRecommendationProductBoundaries(t *testing.T) {
	tests := []struct {
		name                                   string
		amount                                 int64
		term                                   int
		registered                             string
		amountMatches, termMatches, ageMatches bool
	}{
		{"all lower boundaries", 100_000, 6, "2026-04-23", true, true, true},
		{"all upper boundaries", 3_000_000, 24, "2020-01-01", true, true, true},
		{"amount below minimum", 99_999, 12, "2020-01-01", false, true, true},
		{"amount above maximum", 3_000_001, 12, "2020-01-01", false, true, true},
		{"term below minimum", 100_000, 5, "2020-01-01", true, false, true},
		{"term above maximum", 100_000, 25, "2020-01-01", true, false, true},
		{"business one day too young", 100_000, 6, "2026-04-24", true, true, false},
		{"future company", 100_000, 6, "2027-01-01", true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, _, companies := serviceFixture()
			companies.item.RegistrationDate = tt.registered
			input := demoInput()
			input.RequestedAmountRub = tt.amount
			input.TermMonths = tt.term
			result, err := service.Recommend(context.Background(), "own-company", input)
			if err != nil {
				t.Fatal(err)
			}
			product := productResult(t, result, "turnover")
			if product.Matches.Amount != tt.amountMatches || product.Matches.Term != tt.termMatches || product.Matches.BusinessAge != tt.ageMatches {
				t.Fatalf("unexpected matches: %+v", product.Matches)
			}
			wantEligible := tt.amountMatches && tt.termMatches && tt.ageMatches
			if product.Eligible != wantEligible {
				t.Fatalf("eligible = %v, want %v", product.Eligible, wantEligible)
			}
			if !wantEligible && result.RecommendedProductID != nil {
				t.Fatal("nonmatching original request was recommended")
			}
			if !tt.termMatches && (product.MonthlyPaymentRub != nil || product.PreliminaryAvailableAmountRub != 0) {
				t.Fatal("invalid term must not silently use another term")
			}
			if !tt.ageMatches && len(product.Adjustments) != 0 {
				t.Fatal("adjustment cannot fix company age")
			}
			if !wantEligible && len(product.Limitations) == 0 {
				t.Fatal("missing limitation")
			}
		})
	}
}

func TestRecommendBudgetAndNoSuitableProduct(t *testing.T) {
	for _, tt := range []struct {
		name                                         string
		revenue, expenses, existing, surplus, budget int64
	}{
		{"zero revenue", 0, 0, 0, 0, 0},
		{"negative operating result", 100_000, 200_000, 10_000, -100_000, 0},
		{"debt exceeds surplus share", 200_000, 100_000, 100_000, 100_000, 0},
		{"odd surplus rounds budget down", 200_001, 100_000, 1, 100_001, 49_999},
		{"insufficient minimum credit budget", 100_010, 100_000, 0, 10, 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service, _, companies := serviceFixture()
			companies.item.Financials.ExistingMonthlyPaymentsRub = tt.existing
			input := demoInput()
			input.MonthlyRevenueRub = tt.revenue
			input.MonthlyExpensesRub = tt.expenses
			result, err := service.Recommend(context.Background(), "own-company", input)
			if err != nil {
				t.Fatal(err)
			}
			if result.SurplusRub != tt.surplus || result.PaymentBudgetRub != tt.budget {
				t.Fatalf("surplus/budget = %d/%d, want %d/%d", result.SurplusRub, result.PaymentBudgetRub, tt.surplus, tt.budget)
			}
			if result.RecommendedProductID != nil {
				t.Fatal("unaffordable request recommended")
			}
			for _, product := range result.Products {
				if product.Eligible || len(product.Limitations) == 0 {
					t.Fatal("missing affordability limitation")
				}
			}
		})
	}
	t.Run("unsupported purpose is expected outcome", func(t *testing.T) {
		service, _, _ := serviceFixture()
		input := demoInput()
		input.Purpose = "other"
		result, err := service.Recommend(context.Background(), "own-company", input)
		if err != nil {
			t.Fatal(err)
		}
		if result.RecommendedProductID != nil || len(result.Products) != 2 {
			t.Fatal("expected products without recommendation")
		}
		for _, product := range result.Products {
			if product.Matches.Purpose || len(product.Adjustments) != 0 || len(product.Limitations) == 0 {
				t.Fatal("invalid purpose mismatch")
			}
		}
	})
	t.Run("no active products", func(t *testing.T) {
		service, products, _ := serviceFixture()
		products.items = nil
		result, err := service.Recommend(context.Background(), "own-company", demoInput())
		if err != nil {
			t.Fatal(err)
		}
		if result.Products == nil || len(result.Products) != 0 || result.RecommendedProductID != nil {
			t.Fatal("empty catalog should give JSON array and null recommendation")
		}
	})
}

func TestAffordableAmountBoundary(t *testing.T) {
	service, _, companies := serviceFixture()
	companies.item.Financials.ExistingMonthlyPaymentsRub = 0
	input := demoInput()
	input.MonthlyRevenueRub = 100_000
	input.MonthlyExpensesRub = 0
	input.RequestedAmountRub = 534_220
	result, err := service.Recommend(context.Background(), "own-company", input)
	if err != nil {
		t.Fatal(err)
	}
	product := productResult(t, result, "turnover")
	if !product.Eligible || product.PreliminaryAvailableAmountRub != 534_220 || *product.MonthlyPaymentRub != 50_000 {
		t.Fatalf("incorrect affordable boundary: %+v", product)
	}
	input.RequestedAmountRub++
	result, err = service.Recommend(context.Background(), "own-company", input)
	if err != nil {
		t.Fatal(err)
	}
	if productResult(t, result, "turnover").Matches.Affordability || result.RecommendedProductID != nil {
		t.Fatal("one ruble above available principal accepted")
	}
}

func TestAffordabilityIndependentOfProductAmountCap(t *testing.T) {
	service, _, companies := serviceFixture()
	companies.item.Financials.ExistingMonthlyPaymentsRub = 0
	input := demoInput()
	input.RequestedAmountRub = 3_500_000
	input.MonthlyRevenueRub = 4_000_000
	input.MonthlyExpensesRub = 0
	result, err := service.Recommend(context.Background(), "own-company", input)
	if err != nil {
		t.Fatal(err)
	}
	product := productResult(t, result, "turnover")
	if product.Matches.Amount || !product.Matches.Affordability || product.Eligible {
		t.Fatalf("payment budget and product amount cap must be separate checks: %+v", product.Matches)
	}
	if product.PreliminaryAvailableAmountRub != product.Product.MaxAmountRub {
		t.Fatal("preliminary amount must still be capped by product limit")
	}
}

func TestPossibleAdjustmentsAreFeasibleAndNotRecommendations(t *testing.T) {
	for _, tt := range []struct {
		name           string
		amount, budget int64
		term           int
	}{
		{"extend term", 1_000_000, 60_000, 12},
		{"reduce amount at longest term", 1_000_000, 20_000, 12},
		{"fix product amount", 4_000_000, 400_000, 12},
		{"fix product term", 1_000_000, 400_000, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service, _, companies := serviceFixture()
			companies.item.Financials.ExistingMonthlyPaymentsRub = 0
			input := demoInput()
			input.RequestedAmountRub = tt.amount
			input.TermMonths = tt.term
			input.MonthlyRevenueRub = tt.budget * 2
			input.MonthlyExpensesRub = 0
			result, err := service.Recommend(context.Background(), "own-company", input)
			if err != nil {
				t.Fatal(err)
			}
			if result.RecommendedProductID != nil {
				t.Fatal("adjusted conditions must not make original request eligible")
			}
			product := productResult(t, result, "turnover")
			if len(product.Adjustments) != 1 {
				t.Fatalf("expected one deterministic adjustment: %+v", product)
			}
			adjustment := product.Adjustments[0]
			if adjustment.AmountRub < product.Product.MinAmountRub || adjustment.AmountRub > product.Product.MaxAmountRub || adjustment.TermMonths < product.Product.MinTermMonths || adjustment.TermMonths > product.Product.MaxTermMonths || adjustment.MonthlyPaymentRub > tt.budget || adjustment.Explanation == "" {
				t.Fatalf("infeasible adjustment: %+v", adjustment)
			}
		})
	}
}

func TestRecommendationTieIsDeterministic(t *testing.T) {
	service, products, _ := serviceFixture()
	first := products.items[0]
	second := first
	first.ID = "z-product"
	second.ID = "a-product"
	products.items = []Product{first, second}
	result, err := service.Recommend(context.Background(), "own-company", demoInput())
	if err != nil {
		t.Fatal(err)
	}
	if result.RecommendedProductID == nil || *result.RecommendedProductID != "a-product" {
		t.Fatal("equal payments should choose stable lowest ID")
	}
	first.AnnualRatePercent = "10"
	products.items = []Product{second, first}
	result, err = service.Recommend(context.Background(), "own-company", demoInput())
	if err != nil {
		t.Fatal(err)
	}
	if result.RecommendedProductID == nil || *result.RecommendedProductID != "z-product" {
		t.Fatal("lower monthly payment should take priority over ID")
	}
}

func TestInvalidRecommendationInput(t *testing.T) {
	mutations := []func(*Input){
		func(i *Input) { i.Purpose = "" }, func(i *Input) { i.Purpose = "unknown" },
		func(i *Input) { i.RequestedAmountRub = 0 }, func(i *Input) { i.RequestedAmountRub = -1 }, func(i *Input) { i.RequestedAmountRub = maxInputAmount + 1 },
		func(i *Input) { i.TermMonths = 0 }, func(i *Input) { i.TermMonths = 121 },
		func(i *Input) { i.MonthlyRevenueRub = -1 }, func(i *Input) { i.MonthlyRevenueRub = maxInputAmount + 1 },
		func(i *Input) { i.MonthlyExpensesRub = -1 }, func(i *Input) { i.MonthlyExpensesRub = maxInputAmount + 1 },
	}
	for index, mutate := range mutations {
		service, _, companies := serviceFixture()
		input := demoInput()
		mutate(&input)
		_, err := service.Recommend(context.Background(), "own-company", input)
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d: error = %v", index, err)
		}
		if companies.requestedID != "" {
			t.Fatalf("case %d: invalid input reached repository", index)
		}
	}
	t.Run("maximum valid integers", func(t *testing.T) {
		service, _, _ := serviceFixture()
		input := demoInput()
		input.RequestedAmountRub = maxInputAmount
		input.MonthlyRevenueRub = maxInputAmount
		input.MonthlyExpensesRub = maxInputAmount
		input.TermMonths = 120
		if _, err := service.Recommend(context.Background(), "own-company", input); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCatalogAndStorageErrors(t *testing.T) {
	service, products, companies := serviceFixture()
	items, err := service.ListProducts(context.Background())
	if err != nil || len(items) != 2 || items[0].ID != "investment" {
		t.Fatalf("catalog: %+v, %v", items, err)
	}
	if product, err := service.Product(context.Background(), "turnover"); err != nil || product.ID != "turnover" {
		t.Fatalf("product lookup: %+v, %v", product, err)
	}
	if _, err := service.Product(context.Background(), "missing"); !errors.Is(err, ErrProductNotFound) {
		t.Fatalf("missing product error = %v", err)
	}
	secretError := errors.New("postgres://private:secret@host")
	products.err = secretError
	if _, err := service.ListProducts(context.Background()); err != ErrStorage {
		t.Fatalf("unsanitized error: %v", err)
	}
	if _, err := service.Product(context.Background(), "turnover"); err != ErrStorage {
		t.Fatalf("unsanitized error: %v", err)
	}
	products.err = nil
	companies.err = secretError
	if _, err := service.Recommend(context.Background(), "own-company", demoInput()); err != ErrStorage {
		t.Fatalf("unsanitized error: %v", err)
	}
	companies.err = nil
	companies.item.RegistrationDate = "invalid"
	if _, err := service.Recommend(context.Background(), "own-company", demoInput()); err != ErrStorage {
		t.Fatalf("invalid registration date error: %v", err)
	}
}

func TestInvalidProductConfiguration(t *testing.T) {
	mutations := []func(*Product){
		func(p *Product) { p.AnnualRatePercent = "NaN" }, func(p *Product) { p.AnnualRatePercent = "Inf" },
		func(p *Product) { p.Purposes = nil }, func(p *Product) { p.Purposes = []string{"unknown"} },
		func(p *Product) { p.MinAmountRub = 0 }, func(p *Product) { p.MaxAmountRub = p.MinAmountRub - 1 },
		func(p *Product) { p.MaxTermMonths = 121 }, func(p *Product) { p.MinTermMonths = 0 },
		func(p *Product) { p.MinBusinessAgeMonths = -1 }, func(p *Product) { p.Version = 0 },
	}
	for index, mutate := range mutations {
		service, products, _ := serviceFixture()
		mutate(&products.items[0])
		if _, err := service.ListProducts(context.Background()); err != ErrStorage {
			t.Fatalf("case %d accepted malformed product: %v", index, err)
		}
		if _, err := service.Recommend(context.Background(), "own-company", demoInput()); err != ErrStorage {
			t.Fatalf("case %d recommendation accepted malformed product: %v", index, err)
		}
	}
}
