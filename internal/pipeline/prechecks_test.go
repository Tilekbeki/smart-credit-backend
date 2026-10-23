package pipeline

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/company"
)

func simulatorSnapshot() FinancialSnapshot {
	return FinancialSnapshot{SchemaVersion: 1, ApplicationID: "30000000-0000-4000-8000-000000000001",
		UserID: "20000000-0000-4000-8000-000000000001", CompanyID: "10000000-0000-4000-8000-000000000001",
		Fields: application.Fields{ProductID: submissionPtr("turnover"), Purpose: submissionPtr("stock"),
			RequestedAmountRub: submissionPtr(int64(1_000_000)), TermMonths: submissionPtr(12),
			MonthlyRevenueRub: submissionPtr(int64(2_400_000)), MonthlyExpensesRub: submissionPtr(int64(1_500_000))},
		Company: CompanySnapshot{RegistrationStatus: "active", Profile: company.Company{
			ID:   "10000000-0000-4000-8000-000000000001",
			Name: "ИП «Демонстрационный магазин»", OrganizationType: "ИП", INN: "000000000001",
			RegistrationNumber: "000000000000001", RegistrationDate: "2020-01-10", OKVED: "47.11",
			DirectorName: "Иван Примеров", Simulation: true,
			Financials: company.Financials{MonthlyRevenueRub: 2_400_000, MonthlyExpensesRub: 1_500_000,
				ExistingDebtRub: 500_000, ExistingMonthlyPaymentsRub: 90_000, OverdueCount: 1},
		}}}
}

func TestSimulatorSyntheticProfileAndStableCreditHistory(t *testing.T) {
	snapshot := simulatorSnapshot()
	first, err := NewSimulator().Run(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSimulator().Run(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first.SchemaVersion != 1 || first.CompanyCheck.Status != "passed" || !first.CompanyCheck.Simulation ||
		len(first.CompanyCheck.Problems) != 0 || first.CompletedAt.IsZero() || first.CompanyCheck.DurationMS < 0 {
		t.Fatalf("unexpected simulation results: %+v", first)
	}
	for _, check := range first.CompanyCheck.Checks {
		if !check.Passed {
			t.Fatalf("synthetic identifiers must pass format checks: %+v", check)
		}
	}
	if first.CreditHistory.ExistingDebtRub != 500_000 || first.CreditHistory.ExistingMonthlyPaymentsRub != 90_000 ||
		first.CreditHistory.OverdueCount != 1 || !first.CreditHistory.Simulation {
		t.Fatalf("BKI must preserve the company profile exactly: %+v", first.CreditHistory)
	}
	first.CreditHistory.CheckedAt, second.CreditHistory.CheckedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(first.CreditHistory, second.CreditHistory) {
		t.Fatal("credit history changed between runs")
	}
}

func TestSimulatorBusinessProblemsRemainStructuredResults(t *testing.T) {
	snapshot := simulatorSnapshot()
	snapshot.Company.RegistrationStatus = "inactive"
	snapshot.Company.Profile.Name = " "
	snapshot.Company.Profile.INN = "not-an-inn"
	snapshot.Company.Profile.RegistrationDate = "2999-01-01"
	snapshot.Company.Profile.DirectorName = ""
	results, err := NewSimulator().Run(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("negative registration checks are not technical errors: %v", err)
	}
	if results.CompanyCheck.Status != "failed" || len(results.CompanyCheck.Problems) != 5 {
		t.Fatalf("missing business problems: %+v", results.CompanyCheck)
	}
	seen := map[string]bool{}
	for _, problem := range results.CompanyCheck.Problems {
		if problem.Code == "" || problem.Message == "" || seen[problem.Code] {
			t.Fatal("reason codes must be unique and include an explanation")
		}
		seen[problem.Code] = true
	}
	if results.CreditHistory.ExistingDebtRub != snapshot.Company.Profile.Financials.ExistingDebtRub {
		t.Fatal("business problems must not remove credit history")
	}
}

func TestSimulatorMalformedFinancialSnapshotAndCancellation(t *testing.T) {
	for _, mutate := range []func(*FinancialSnapshot){
		func(s *FinancialSnapshot) { s.SchemaVersion = 0 },
		func(s *FinancialSnapshot) { s.ApplicationID = "" },
		func(s *FinancialSnapshot) { s.UserID = "not-a-uuid" },
		func(s *FinancialSnapshot) { s.CompanyID = "00000000-0000-0000-0000-000000000000" },
		func(s *FinancialSnapshot) { s.Company.Profile.ID = "10000000-0000-4000-8000-000000000002" },
		func(s *FinancialSnapshot) { s.Fields.MonthlyRevenueRub = nil },
		func(s *FinancialSnapshot) { s.Company.Profile.Financials.ExistingDebtRub = -1 },
		func(s *FinancialSnapshot) { s.Company.Profile.Financials.ExistingMonthlyPaymentsRub = -1 },
		func(s *FinancialSnapshot) { s.Company.Profile.Financials.MonthlyRevenueRub = 1_000_000_000_001 },
		func(s *FinancialSnapshot) { s.Company.Profile.Financials.OverdueCount = -1 },
		func(s *FinancialSnapshot) { s.Company.Profile.Financials.OverdueCount = 101 },
	} {
		snapshot := simulatorSnapshot()
		mutate(&snapshot)
		if _, err := NewSimulator().Run(context.Background(), snapshot); !errors.Is(err, ErrInvalidSnapshot) {
			t.Fatalf("malformed financial snapshot must fail technically: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSimulator().Run(ctx, simulatorSnapshot()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled processor returned %v", err)
	}
}

func TestSimulatorCompanyFormats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*FinancialSnapshot)
		code   string
	}{
		{"organization", func(s *FinancialSnapshot) { s.Company.Profile.OrganizationType = "unknown" }, "organization_type"},
		{"number", func(s *FinancialSnapshot) { s.Company.Profile.RegistrationNumber = "123" }, "registration_number_format"},
		{"date", func(s *FinancialSnapshot) { s.Company.Profile.RegistrationDate = "2024-99-30" }, "registration_date"},
		{"okved", func(s *FinancialSnapshot) { s.Company.Profile.OKVED = "arbitrary" }, "okved_format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := simulatorSnapshot()
			tc.mutate(&snapshot)
			result, err := NewSimulator().Run(context.Background(), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			for _, problem := range result.CompanyCheck.Problems {
				if problem.Code == tc.code {
					return
				}
			}
			t.Fatalf("expected %s problem", tc.code)
		})
	}
	snapshot := simulatorSnapshot()
	snapshot.Company.Profile.OrganizationType = "ООО"
	snapshot.Company.Profile.INN = "0000000001"
	snapshot.Company.Profile.RegistrationNumber = "0000000000001"
	result, err := NewSimulator().Run(context.Background(), snapshot)
	if err != nil || result.CompanyCheck.Status != "passed" {
		t.Fatal("synthetic LLC profile must be accepted")
	}
}
