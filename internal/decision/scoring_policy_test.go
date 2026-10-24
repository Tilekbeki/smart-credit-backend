package decision

import (
	"errors"
	"testing"

	"smart-credit-backend/internal/application"
)

func TestFinancialRefusalPrecedesLowRiskScoringConflict(t *testing.T) {
	f := newFixture()
	f.snapshot.Fields.MonthlyRevenueRub = pointer(int64(600_000))
	f.snapshot.Fields.MonthlyExpensesRub = pointer(int64(450_000))
	f.checks.CreditHistory.ExistingMonthlyPaymentsRub = 90_000
	f.response.Score, f.response.Decision, f.response.ApprovedAmount = 85, "rejected", 0
	f.response.Factors.OperatingCashFlow = 150_000
	result := evaluateFixture(t, f, application.Rejected)
	if result.RiskLevel != "LOW" || result.Offer != nil || len(result.ManualReviewReasons) != 0 {
		t.Fatal("financial refusal became a manual or approved decision")
	}
}

func TestMandatoryFinancialLimitsCannotBeOverriddenByScoreOrEmployee(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fixture)
	}{
		{"margin below ten percent", func(f *fixture) {
			f.snapshot.Fields.MonthlyExpensesRub = pointer(int64(900_001))
			f.response.Factors.OperatingCashFlow = 99_999
		}},
		{"three overdue events", func(f *fixture) { f.checks.CreditHistory.OverdueCount = 3; f.response.Factors.OverdueCount = 3 }},
		{"debt at 150 percent annual revenue", func(f *fixture) { f.checks.CreditHistory.ExistingDebtRub = 18_000_000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.mutate(&f)
			evaluateFixture(t, f, application.Rejected)
			if _, err := PrepareManual(f.input(t), "approved", 1_000_000, 12); !errors.Is(err, ErrManualTerms) {
				t.Fatalf("manual approval bypassed mandatory limit: %v", err)
			}
		})
	}
	f := newFixture()
	f.snapshot.Fields.MonthlyExpensesRub = pointer(int64(900_000))
	f.response.Factors.OperatingCashFlow = 100_000
	// Exact 10% margin and debt just below 150% do not trigger critical rules.
	f.checks.CreditHistory.ExistingDebtRub = 17_999_999
	f.response.Score = 65
	evaluateFixture(t, f, application.ManualReview)
}

func TestHistoricalPolicyResultsRemainReadable(t *testing.T) {
	f := newFixture()
	result := evaluateFixture(t, f, application.Approved)
	result.PolicyVersion = "go-demo-v1"
	if err := ValidateResult(result); err != nil {
		t.Fatalf("historical persisted assessment rejected: %v", err)
	}
}
