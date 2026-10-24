package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"smart-credit-backend/internal/company"
)

var ErrInvalidSnapshot = errors.New("invalid preliminary check snapshot")

// Simulator uses only the immutable synthetic company profile saved at submit.
// Failed registration checks are data for a later decision engine, not a refusal.
type Simulator struct{}

func NewSimulator() *Simulator { return &Simulator{} }

func (s *Simulator) Run(ctx context.Context, snapshot FinancialSnapshot) (Results, error) {
	if err := ctx.Err(); err != nil {
		return Results{}, err
	}
	if !validPrecheckSnapshot(snapshot) {
		return Results{}, ErrInvalidSnapshot
	}
	started := time.Now()
	profile := snapshot.Company.Profile
	financials := profile.Financials
	// Corrupt financial data is a technical failure. Do not manufacture a BKI
	// result or interpret invalid numbers as a negative credit decision.
	for _, value := range []int64{financials.MonthlyRevenueRub, financials.MonthlyExpensesRub,
		financials.ExistingDebtRub, financials.ExistingMonthlyPaymentsRub} {
		if value < 0 || value > 1_000_000_000_000 {
			return Results{}, ErrInvalidSnapshot
		}
	}
	if financials.OverdueCount < 0 || financials.OverdueCount > 100 {
		return Results{}, ErrInvalidSnapshot
	}
	check := CompanyCheck{Status: "passed", Checks: []Check{}, Problems: []Problem{},
		CheckedAt: started.UTC(), Simulation: true}
	add := func(code string, passed bool, message string) {
		check.Checks = append(check.Checks, Check{Code: code, Passed: passed})
		if !passed {
			check.Status = "failed"
			check.Problems = append(check.Problems, Problem{Code: code, Message: message})
		}
	}
	for _, registration := range company.RegistrationChecks(profile, snapshot.Company.RegistrationStatus, started) {
		add(registration.Code, registration.Passed, registration.Message)
	}
	check.DurationMS = time.Since(started).Milliseconds()
	if err := ctx.Err(); err != nil {
		return Results{}, err
	}
	return Results{SchemaVersion: 1, CompanyCheck: check, CreditHistory: CreditHistory{
		ExistingDebtRub:            financials.ExistingDebtRub,
		ExistingMonthlyPaymentsRub: financials.ExistingMonthlyPaymentsRub,
		OverdueCount:               financials.OverdueCount, CheckedAt: time.Now().UTC(), Simulation: true,
	}, CompletedAt: time.Now().UTC()}, nil
}

func validPrecheckSnapshot(snapshot FinancialSnapshot) bool {
	validID := func(value string) bool {
		id, err := uuid.Parse(value)
		return err == nil && len(value) == 36 && id != uuid.Nil
	}
	if snapshot.SchemaVersion != 1 || !validID(snapshot.ApplicationID) || !validID(snapshot.UserID) ||
		!validID(snapshot.CompanyID) || snapshot.Company.Profile.ID != snapshot.CompanyID ||
		len(requiredFields(snapshot.Fields)) != 0 {
		return false
	}
	fields := snapshot.Fields
	return *fields.RequestedAmountRub > 0 && *fields.RequestedAmountRub <= 1_000_000_000_000 &&
		*fields.TermMonths > 0 && *fields.TermMonths <= 120 &&
		*fields.MonthlyRevenueRub >= 0 && *fields.MonthlyRevenueRub <= 1_000_000_000_000 &&
		*fields.MonthlyExpensesRub >= 0 && *fields.MonthlyExpensesRub <= 1_000_000_000_000
}
