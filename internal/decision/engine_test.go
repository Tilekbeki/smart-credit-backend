package decision

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/document"
	"smart-credit-backend/internal/scoring"
)

type fixture struct {
	snapshot financialSnapshot
	product  credit.Product
	checks   prechecks
	response scoring.Response
	submit   time.Time
}

func pointer[T any](value T) *T { return &value }

func newFixture() fixture {
	stamp := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	f := fixture{submit: stamp}
	f.snapshot.SchemaVersion = 1
	f.snapshot.ApplicationID = "40000000-0000-4000-8000-000000000001"
	f.snapshot.UserID = "20000000-0000-4000-8000-000000000001"
	f.snapshot.CompanyID = "10000000-0000-4000-8000-000000000001"
	f.snapshot.Fields = application.Fields{ProductID: pointer("turnover"), Purpose: pointer("stock"), RequestedAmountRub: pointer(int64(1_000_000)),
		TermMonths: pointer(12), MonthlyRevenueRub: pointer(int64(1_000_000)), MonthlyExpensesRub: pointer(int64(500_000))}
	f.snapshot.Company.Profile = company.Company{ID: f.snapshot.CompanyID, Name: "Демонстрационная компания", OrganizationType: "ООО", INN: "7701234567",
		RegistrationNumber: "1237700000001", RegistrationDate: "2020-01-01", OKVED: "47.11", DirectorName: "Тестовый руководитель",
		Contact:    company.Contact{Phone: "+79990000001", Email: "test@example.test", Address: "Тестовый адрес"},
		Financials: company.Financials{MonthlyRevenueRub: 1_000_000, MonthlyExpensesRub: 500_000, ExistingDebtRub: 100_000, ExistingMonthlyPaymentsRub: 10_000}, Simulation: true}
	f.snapshot.Company.FinancialDataSource, f.snapshot.Company.RegistrationStatus = "demo_bank_feed", "active"
	f.snapshot.Documents = []document.Metadata{}
	f.product = credit.Product{ID: "turnover", Name: "Оборотный кредит", Purposes: []string{"stock", "expenses", "seasonal"},
		MinAmountRub: 100_000, MaxAmountRub: 3_000_000, MinTermMonths: 6, MaxTermMonths: 24, AnnualRatePercent: "22", MinBusinessAgeMonths: 6, Version: 1, Illustrative: true}
	f.checks.SchemaVersion, f.checks.CompletedAt = 1, stamp
	f.checks.CompanyCheck = companyCheck{Status: "passed", Checks: []check{}, Problems: []Reason{}, CheckedAt: stamp, DurationMS: 2, Simulation: true}
	for _, code := range []string{"company_name", "organization_type", "inn_format", "registration_number_format", "registration_date", "okved_format", "director_name", "registration_active"} {
		f.checks.CompanyCheck.Checks = append(f.checks.CompanyCheck.Checks, check{code, true})
	}
	f.checks.CreditHistory.ExistingDebtRub, f.checks.CreditHistory.ExistingMonthlyPaymentsRub = 100_000, 10_000
	f.checks.CreditHistory.CheckedAt, f.checks.CreditHistory.Simulation = stamp, true
	f.response = scoring.Response{Decision: "approved", Score: 90, ApprovedAmount: 1_000_000, Reasons: []string{"Синтетическая оценка"},
		Factors: scoring.Factors{OperatingCashFlow: 500_000, OperatingMargin: .5, EstimatedNewMonthlyPayment: 99_999, DebtServiceShare: pointer(.22), ExistingDebtToAnnualRevenue: pointer(.01)}}
	return f
}

func (f *fixture) input(t *testing.T) Input {
	t.Helper()
	marshal := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	fields, history := f.snapshot.Fields, f.checks.CreditHistory
	request := scoring.Request{ApplicationID: f.snapshot.ApplicationID, MonthlyRevenue: *fields.MonthlyRevenueRub,
		MonthlyExpenses: *fields.MonthlyExpensesRub, ExistingDebt: history.ExistingDebtRub,
		MonthlyDebtPayments: history.ExistingMonthlyPaymentsRub, OverdueCount: history.OverdueCount,
		RequestedAmount: *fields.RequestedAmountRub, TermMonths: *fields.TermMonths}
	return Input{ApplicationID: f.snapshot.ApplicationID, SubmittedAt: f.submit, FinancialSnapshot: marshal(f.snapshot), ProductSnapshot: marshal(f.product),
		PrecheckResults: marshal(f.checks), ScoringRequest: marshal(request), ScoringResponse: marshal(f.response)}
}

func (f *fixture) counterOfferBudget() {
	f.snapshot.Fields.MonthlyExpensesRub = pointer(int64(880_000))
	f.response.Factors.OperatingCashFlow = 120_000
}

func (f *fixture) failCheck(code string) {
	f.checks.CompanyCheck.Status = "failed"
	for i := range f.checks.CompanyCheck.Checks {
		if f.checks.CompanyCheck.Checks[i].Code == code {
			f.checks.CompanyCheck.Checks[i].Passed = false
		}
	}
	f.checks.CompanyCheck.Problems = append(f.checks.CompanyCheck.Problems, Reason{code, "Синтетический отрицательный результат"})
}

func evaluateFixture(t *testing.T, f fixture, want application.Status) Result {
	t.Helper()
	input := f.input(t)
	result, err := NewEngine().Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != want {
		t.Fatalf("status = %s, want %s; result = %+v", result.Status, want, result)
	}
	if err := ValidateResult(result); err != nil {
		t.Fatalf("engine returned invalid result: %v", err)
	}
	repeated, err := NewEngine().Evaluate(input)
	if err != nil || !reflect.DeepEqual(result, repeated) {
		t.Fatal("identical immutable input did not produce identical result")
	}
	return result
}

func TestRiskBoundariesAndAllFourOutcomes(t *testing.T) {
	for _, tc := range []struct {
		score  int
		risk   string
		status application.Status
	}{{0, "HIGH", application.Rejected}, {49, "HIGH", application.Rejected}, {50, "MEDIUM", application.ManualReview},
		{79, "MEDIUM", application.ManualReview}, {80, "LOW", application.Approved}, {100, "LOW", application.Approved}} {
		t.Run(string(tc.status)+"/"+strconv.Itoa(tc.score), func(t *testing.T) {
			f := newFixture()
			f.response.Score = tc.score
			result := evaluateFixture(t, f, tc.status)
			if result.RiskLevel != tc.risk || result.Score != tc.score {
				t.Fatalf("wrong score or risk: %+v", result)
			}
		})
	}
	f := newFixture()
	f.counterOfferBudget()
	result := evaluateFixture(t, f, application.CounterOffer)
	if result.Offer == nil || result.Offer.AmountRub != 534_220 || result.Offer.TermMonths != 12 || result.Offer.AnnualRatePercent != "22" || result.Offer.MonthlyPaymentRub != 50_000 || result.Offer.TotalPaymentRub != 600_000 || result.Offer.OverpaymentRub != 65_780 {
		t.Fatalf("incorrect rational/whole-ruble alternative: %+v", result.Offer)
	}
}

func TestApprovedOfferUsesFrozenProductRateAndCeiling(t *testing.T) {
	f := newFixture()
	result := evaluateFixture(t, f, application.Approved)
	if result.Offer.AmountRub != 1_000_000 || result.Offer.MonthlyPaymentRub != 93_595 || result.Offer.TotalPaymentRub != 1_123_140 || result.Offer.OverpaymentRub != 123_140 {
		t.Fatalf("incorrect approved offer: %+v", result.Offer)
	}
	// Scoring's estimated payment is advisory and cannot replace product terms.
	f.response.Factors.EstimatedNewMonthlyPayment = 1
	result = evaluateFixture(t, f, application.Approved)
	if result.Offer.MonthlyPaymentRub != 93_595 || result.Offer.AnnualRatePercent != "22" {
		t.Fatal("model changed frozen product terms")
	}
	f.product.AnnualRatePercent = "0"
	result = evaluateFixture(t, f, application.Approved)
	if result.Offer.MonthlyPaymentRub != 83_334 || result.Offer.OverpaymentRub != 8 {
		t.Fatal("zero-rate rounded payment did not use shared math")
	}
}

func TestCriticalBusinessRulesTakePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fixture)
	}{
		{"inactive despite passed check", func(f *fixture) { f.snapshot.Company.RegistrationStatus = "suspended" }},
		{"invalid inn despite passed check", func(f *fixture) { f.snapshot.Company.Profile.INN = "invalid" }},
		{"invalid registration number despite passed check", func(f *fixture) { f.snapshot.Company.Profile.RegistrationNumber = "invalid" }},
		{"failed registration", func(f *fixture) { f.failCheck("registration_active") }},
		{"failed inn", func(f *fixture) { f.failCheck("inn_format") }},
		{"failed registration number", func(f *fixture) { f.failCheck("registration_number_format") }},
		{"young business", func(f *fixture) { f.snapshot.Company.Profile.RegistrationDate = "2026-05-25" }},
		{"purpose mismatch", func(f *fixture) { f.snapshot.Fields.Purpose = pointer("equipment") }},
		{"below product amount", func(f *fixture) { f.snapshot.Fields.RequestedAmountRub = pointer(int64(99_999)) }},
		{"above product amount", func(f *fixture) { f.snapshot.Fields.RequestedAmountRub = pointer(int64(3_000_001)) }},
		{"below product term", func(f *fixture) { f.snapshot.Fields.TermMonths = pointer(5) }},
		{"above product term", func(f *fixture) { f.snapshot.Fields.TermMonths = pointer(25) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.response.Score = 60
			f.response.Decision = "rejected"
			f.response.ApprovedAmount = 100 // Contradictory advisory data never overrides a critical policy rule.
			tc.change(&f)
			result := evaluateFixture(t, f, application.Rejected)
			if result.Offer != nil || len(result.ManualReviewReasons) != 0 {
				t.Fatal("critical refusal fabricated an offer or manual decision")
			}
		})
	}
}

func TestNoncriticalAndAmbiguousChecksRequireManualReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fixture)
	}{
		{"failed director", func(f *fixture) { f.failCheck("director_name") }},
		{"missing director despite passed check", func(f *fixture) { f.snapshot.Company.Profile.DirectorName = "" }},
		{"invalid company name despite passed check", func(f *fixture) { f.snapshot.Company.Profile.Name = " " }},
		{"invalid organization despite passed check", func(f *fixture) { f.snapshot.Company.Profile.OrganizationType = "unknown" }},
		{"invalid okved despite passed check", func(f *fixture) { f.snapshot.Company.Profile.OKVED = "invalid" }},
		{"failed date", func(f *fixture) { f.failCheck("registration_date") }},
		{"missing required check", func(f *fixture) { f.checks.CompanyCheck.Checks = f.checks.CompanyCheck.Checks[:7] }},
		{"duplicate check", func(f *fixture) {
			f.checks.CompanyCheck.Checks = append(f.checks.CompanyCheck.Checks, check{"company_name", true})
		}},
		{"unexpected check", func(f *fixture) {
			f.checks.CompanyCheck.Checks = append(f.checks.CompanyCheck.Checks, check{"unknown_check", true})
		}},
		{"failed status without failed check", func(f *fixture) { f.checks.CompanyCheck.Status = "failed" }},
		{"passed status with problems", func(f *fixture) { f.checks.CompanyCheck.Problems = []Reason{{"director_name", "Problem"}} }},
		{"future registration date", func(f *fixture) { f.snapshot.Company.Profile.RegistrationDate = "2026-10-25" }},
		{"invalid registration date", func(f *fixture) { f.snapshot.Company.Profile.RegistrationDate = "unknown" }},
		{"credit overdue", func(f *fixture) { f.checks.CreditHistory.OverdueCount = 1; f.response.Factors.OverdueCount = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(&f)
			result := evaluateFixture(t, f, application.ManualReview)
			if len(result.ManualReviewReasons) == 0 || result.Offer != nil {
				t.Fatal("manual review needs persisted reasons and no final offer")
			}
		})
	}
}

func TestLowRiskCannotOverrideInvalidFrozenRegistrationFacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fixture)
	}{
		{"invalid inn", func(f *fixture) { f.snapshot.Company.Profile.INN = "invalid" }},
		{"invalid registration number", func(f *fixture) { f.snapshot.Company.Profile.RegistrationNumber = "invalid" }},
		{"closed company", func(f *fixture) { f.snapshot.Company.RegistrationStatus = "closed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture() // LOW, full scoring approval and all saved checks passed.
			tc.change(&f)
			evaluateFixture(t, f, application.Rejected)
		})
	}
}

func TestScoringConflictsCannotAutomaticallyApprove(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fixture)
	}{
		{"approved zero amount", func(f *fixture) { f.response.ApprovedAmount = 0 }},
		{"approved lesser amount", func(f *fixture) { f.response.ApprovedAmount = 900_000 }},
		{"approved greater amount", func(f *fixture) { f.response.ApprovedAmount = 1_000_001 }},
		{"counter same amount", func(f *fixture) { f.response.Decision = "counter_offer" }},
		{"counter zero", func(f *fixture) { f.response.Decision = "counter_offer"; f.response.ApprovedAmount = 0 }},
		{"rejection nonzero amount", func(f *fixture) { f.response.Decision = "rejected"; f.response.ApprovedAmount = 1 }},
		{"rejection with low risk", func(f *fixture) { f.response.Decision = "rejected"; f.response.ApprovedAmount = 0 }},
		{"counter despite affordable requested amount", func(f *fixture) { f.response.Decision = "counter_offer"; f.response.ApprovedAmount = 800_000 }},
		{"model cash flow disagrees", func(f *fixture) { f.response.Factors.OperatingCashFlow += 10 }},
		{"model overdue disagrees", func(f *fixture) { f.response.Factors.OverdueCount = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(&f)
			evaluateFixture(t, f, application.ManualReview)
		})
	}
	f := newFixture()
	f.response.Factors.OperatingCashFlow += .99
	evaluateFixture(t, f, application.Approved)
	// Unspecified model ratios are not a duplicated credit-risk algorithm.
	f.response.Factors.OperatingMargin, f.response.Factors.DebtServiceShare, f.response.Factors.ExistingDebtToAnnualRevenue = -42, nil, nil
	evaluateFixture(t, f, application.Approved)
}

func TestAlternativeBoundsAndUnaffordableMinimum(t *testing.T) {
	f := newFixture()
	f.counterOfferBudget()
	f.response.Decision, f.response.ApprovedAmount = "counter_offer", 500_000
	result := evaluateFixture(t, f, application.CounterOffer)
	if result.Offer.AmountRub != 500_000 {
		t.Fatal("alternative exceeded scoring's advisory cap")
	}
	f.response.ApprovedAmount = 99_999
	evaluateFixture(t, f, application.ManualReview)
	f.response.ApprovedAmount = 100_000
	result = evaluateFixture(t, f, application.CounterOffer)
	if result.Offer.AmountRub != f.product.MinAmountRub {
		t.Fatal("valid minimum alternative rejected")
	}
	f = newFixture()
	f.snapshot.Fields.MonthlyExpensesRub = pointer(int64(1_000_000))
	f.response.Factors.OperatingCashFlow = 0
	evaluateFixture(t, f, application.Rejected)
	f.snapshot.Fields.MonthlyExpensesRub = pointer(int64(1_100_000))
	f.response.Factors.OperatingCashFlow = -100_000
	evaluateFixture(t, f, application.Rejected)
	f = newFixture()
	f.counterOfferBudget()
	f.response.Decision, f.response.ApprovedAmount = "rejected", 0
	evaluateFixture(t, f, application.ManualReview)
}

func TestProductBoundaryAmountsTermsAndSubmittedBusinessAge(t *testing.T) {
	for _, amount := range []int64{100_000, 3_000_000} {
		for _, term := range []int{6, 24} {
			f := newFixture()
			f.snapshot.Fields.RequestedAmountRub = pointer(amount)
			f.snapshot.Fields.TermMonths = pointer(term)
			f.snapshot.Fields.MonthlyRevenueRub = pointer(int64(3_000_000))
			f.response.ApprovedAmount, f.response.Factors.OperatingCashFlow = amount, 2_500_000
			evaluateFixture(t, f, application.Approved)
		}
	}
	f := newFixture()
	f.snapshot.Company.Profile.RegistrationDate = "2026-04-24"
	evaluateFixture(t, f, application.Approved)
	f.submit = f.submit.AddDate(0, 0, -1)
	evaluateFixture(t, f, application.Rejected)
}

func mutateJSON(t *testing.T, raw []byte, path []string, replacement json.RawMessage, remove bool) json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if len(path) > 1 {
		object[path[0]] = mutateJSON(t, object[path[0]], path[1:], replacement, remove)
	} else if remove {
		delete(object, path[0])
	} else {
		object[path[0]] = replacement
	}
	updated, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestCorruptOrMissingDataIsTechnicalErrorEvenForHighRisk(t *testing.T) {
	targets := []struct {
		name string
		get  func(*Input) *json.RawMessage
	}{
		{"financial", func(input *Input) *json.RawMessage { return &input.FinancialSnapshot }},
		{"product", func(input *Input) *json.RawMessage { return &input.ProductSnapshot }},
		{"prechecks", func(input *Input) *json.RawMessage { return &input.PrecheckResults }},
		{"scoring request", func(input *Input) *json.RawMessage { return &input.ScoringRequest }},
		{"scoring response", func(input *Input) *json.RawMessage { return &input.ScoringResponse }},
	}
	for _, target := range targets {
		for _, broken := range []string{`null`, `{}`, `[]`, `not json`, `{"score":0,"score":90}`} {
			t.Run(target.name+"/"+broken, func(t *testing.T) {
				f := newFixture()
				f.response.Score = 0
				input := f.input(t)
				*target.get(&input) = []byte(broken)
				if result, err := NewEngine().Evaluate(input); !errors.Is(err, ErrInvalidData) || result.Status != "" {
					t.Fatalf("corrupt data became a business outcome: %+v %v", result, err)
				}
			})
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
	}{
		{"missing revenue", func(in *Input) {
			in.FinancialSnapshot = mutateJSON(t, in.FinancialSnapshot, []string{"fields", "monthlyRevenueRub"}, nil, true)
		}},
		{"invalid purpose enum", func(in *Input) {
			in.FinancialSnapshot = mutateJSON(t, in.FinancialSnapshot, []string{"fields", "purpose"}, []byte(`"unknown"`), false)
		}},
		{"invalid product code", func(in *Input) {
			in.FinancialSnapshot = mutateJSON(t, in.FinancialSnapshot, []string{"fields", "productId"}, []byte(`"not a product code"`), false)
		}},
		{"null revenue", func(in *Input) {
			in.FinancialSnapshot = mutateJSON(t, in.FinancialSnapshot, []string{"fields", "monthlyRevenueRub"}, []byte(`null`), false)
		}},
		{"null existing payments", func(in *Input) {
			in.PrecheckResults = mutateJSON(t, in.PrecheckResults, []string{"creditHistory", "existingMonthlyPaymentsRub"}, []byte(`null`), false)
		}},
		{"missing existing zero payments", func(in *Input) {
			in.PrecheckResults = mutateJSON(t, in.PrecheckResults, []string{"creditHistory", "existingMonthlyPaymentsRub"}, nil, true)
		}},
		{"missing check boolean", func(in *Input) {
			in.PrecheckResults = []byte(strings.Replace(string(in.PrecheckResults), `"passed":true`, `"missing":true`, 1))
		}},
		{"missing product minimum", func(in *Input) {
			in.ProductSnapshot = mutateJSON(t, in.ProductSnapshot, []string{"minAmountRub"}, nil, true)
		}},
		{"string product rate", func(in *Input) {
			in.ProductSnapshot = mutateJSON(t, in.ProductSnapshot, []string{"annualRatePercent"}, []byte(`"22"`), false)
		}},
		{"unknown product field", func(in *Input) {
			in.ProductSnapshot = mutateJSON(t, in.ProductSnapshot, []string{"unknown"}, []byte(`0`), false)
		}},
		{"changed scoring request", func(in *Input) {
			in.ScoringRequest = mutateJSON(t, in.ScoringRequest, []string{"monthly_revenue"}, []byte(`1`), false)
		}},
		{"missing scoring score", func(in *Input) { in.ScoringResponse = mutateJSON(t, in.ScoringResponse, []string{"score"}, nil, true) }},
		{"score null", func(in *Input) {
			in.ScoringResponse = mutateJSON(t, in.ScoringResponse, []string{"score"}, []byte(`null`), false)
		}},
		{"score 101", func(in *Input) {
			in.ScoringResponse = mutateJSON(t, in.ScoringResponse, []string{"score"}, []byte(`101`), false)
		}},
		{"score minus one", func(in *Input) {
			in.ScoringResponse = mutateJSON(t, in.ScoringResponse, []string{"score"}, []byte(`-1`), false)
		}},
		{"zero submission time", func(in *Input) { in.SubmittedAt = time.Time{} }},
		{"wrong application", func(in *Input) { in.ApplicationID = "40000000-0000-4000-8000-000000000002" }},
		{"missing company finance", func(in *Input) {
			in.FinancialSnapshot = mutateJSON(t, in.FinancialSnapshot, []string{"company", "profile", "financials", "existingDebtRub"}, nil, true)
		}},
		{"invalid registration state", func(in *Input) {
			in.FinancialSnapshot = mutateJSON(t, in.FinancialSnapshot, []string{"company", "registrationStatus"}, []byte(`"liquidated"`), false)
		}},
		{"empty registration state", func(in *Input) {
			in.FinancialSnapshot = mutateJSON(t, in.FinancialSnapshot, []string{"company", "registrationStatus"}, []byte(`""`), false)
		}},
		{"invalid financial data source", func(in *Input) {
			in.FinancialSnapshot = mutateJSON(t, in.FinancialSnapshot, []string{"company", "financialDataSource"}, []byte(`"invented"`), false)
		}},
		{"duplicate nested field", func(in *Input) {
			in.PrecheckResults = []byte(strings.Replace(string(in.PrecheckResults), `"overdueCount":0`, `"overdueCount":0,"overdueCount":1`, 1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			input := f.input(t)
			tc.mutate(&input)
			if result, err := NewEngine().Evaluate(input); !errors.Is(err, ErrInvalidData) || result.Status != "" {
				t.Fatalf("malformed input accepted: %+v %v", result, err)
			}
		})
	}
}

func TestLegitimateZeroFinancialValuesRemainPresent(t *testing.T) {
	f := newFixture()
	f.snapshot.Fields.MonthlyRevenueRub = pointer(int64(0))
	f.snapshot.Fields.MonthlyExpensesRub = pointer(int64(0))
	f.snapshot.Company.Profile.Financials = company.Financials{}
	f.checks.CreditHistory.ExistingDebtRub, f.checks.CreditHistory.ExistingMonthlyPaymentsRub = 0, 0
	f.response.Factors.OperatingCashFlow = 0
	evaluateFixture(t, f, application.Rejected)
}

func TestValidateResultRejectsFabricatedOutcomes(t *testing.T) {
	f := newFixture()
	valid, err := NewEngine().Evaluate(f.input(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Result){
		func(r *Result) { r.Status = application.ManualReview },
		func(r *Result) { r.RiskLevel = "HIGH" },
		func(r *Result) { r.Score = 101 },
		func(r *Result) { r.PolicyVersion = "unknown" },
		func(r *Result) { r.Offer = nil },
		func(r *Result) { r.Offer.MonthlyPaymentRub-- },
		func(r *Result) { r.Offer.TotalPaymentRub-- },
		func(r *Result) { r.Offer.OverpaymentRub-- },
	} {
		result := valid
		offer := *valid.Offer
		result.Offer = &offer
		change(&result)
		if err := ValidateResult(result); !errors.Is(err, ErrInvalidData) {
			t.Fatal("fabricated result accepted")
		}
	}
}
