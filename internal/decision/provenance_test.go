package decision

import (
	"encoding/json"
	"errors"
	"smart-credit-backend/internal/application"
	"testing"
)

func TestUnverifiedFinancialOverridesRequireSeparateConfirmation(t *testing.T) {
	f := newFixture()
	input := f.input(t)
	var frozen financialSnapshot
	if json.Unmarshal(input.FinancialSnapshot, &frozen) != nil {
		t.Fatal("snapshot")
	}
	// Inflate the saved source inconsistency after constructing a legitimate model
	// result; high score and bank-statement presence must not replace verification.
	frozen.Company.Profile.Financials.MonthlyRevenueRub = 500000
	input.FinancialSnapshot, _ = json.Marshal(frozen)
	result, err := NewEngine().Evaluate(input)
	if err != nil || result.Status != application.ManualReview || result.Offer != nil {
		t.Fatalf("unsafe auto result: %+v %v", result, err)
	}
	found := false
	for _, reason := range result.ManualReviewReasons {
		found = found || reason.Code == "financial_verification_required"
	}
	if !found {
		t.Fatal("missing verification reason")
	}
	if _, err := PrepareManual(input, "approved", 1000000, 12); !errors.Is(err, ErrManualTerms) {
		t.Fatal("manual decision bypassed verification")
	}
	input.FinancialVerified = true
	if _, err := PrepareManual(input, "approved", 1000000, 12); err != nil {
		t.Fatal("confirmed financials rejected", err)
	}
}
