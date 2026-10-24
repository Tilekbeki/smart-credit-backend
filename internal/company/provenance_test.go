package company

import (
	"testing"
	"time"
)

func TestFinancialSourceNeverTransfersToOverrides(t *testing.T) {
	p := Company{Financials: Financials{MonthlyRevenueRub: 2400000, MonthlyExpensesRub: 1500000}}
	if !FinancialsVerified(p, "demo_bank_feed", 2400000, 1500000) || FinancialsVerified(p, "demo_bank_feed", 10000000, 1500000) || FinancialsVerified(p, "unverified", 2400000, 1500000) {
		t.Fatal("legacy evidence leakage")
	}
	stamp := time.Now()
	p.FinancialProvenance = &Provenance{Source: "client_declared", Status: "pending_review", MonthlyRevenueRub: 10000000, MonthlyExpensesRub: 1500000}
	if FinancialsVerified(p, "demo_bank_feed", 10000000, 1500000) {
		t.Fatal("pending declared amount became trusted")
	}
	p.FinancialProvenance.Status = "verified"
	p.FinancialProvenance.VerifiedAt = &stamp
	if !FinancialsVerified(p, "unverified", 10000000, 1500000) || FinancialsVerified(p, "demo_bank_feed", 10000001, 1500000) {
		t.Fatal("confirmed exact value mismatch")
	}
}

func TestCommandNormalizationDoesNotMutateCaller(t *testing.T) {
	name := "  Отображаемое имя  "
	c := Command{ActorID: "20000000-0000-4000-8000-000000000001", Action: "profile", Version: 1, DisplayName: &name}
	normalized := c
	if err := ValidateCommand(&normalized); err != nil {
		t.Fatal(err)
	}
	if name != "  Отображаемое имя  " || *normalized.DisplayName != "Отображаемое имя" {
		t.Fatal("normalization mutated caller-owned data")
	}
}
