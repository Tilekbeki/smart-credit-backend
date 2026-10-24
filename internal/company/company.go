// Package company provides company details scoped to the authenticated client.
package company

import (
	"context"
	"errors"
)

var (
	ErrNotFound  = errors.New("company not found")
	ErrForbidden = errors.New("company access forbidden")
	ErrStorage   = errors.New("company storage unavailable")
)

type Contact struct {
	Phone   string `json:"phone"`
	Email   string `json:"email"`
	Address string `json:"address"`
}

type Financials struct {
	MonthlyRevenueRub          int64 `json:"monthlyRevenueRub"`
	MonthlyExpensesRub         int64 `json:"monthlyExpensesRub"`
	ExistingDebtRub            int64 `json:"existingDebtRub"`
	ExistingMonthlyPaymentsRub int64 `json:"existingMonthlyPaymentsRub"`
	OverdueCount               int   `json:"overdueCount"`
}

type Company struct {
	Version                int64       `json:"version,omitempty"`
	FinancialProvenance    *Provenance `json:"financialProvenance,omitempty"`
	RegistrationDataSource string      `json:"registrationDataSource,omitempty"`
	ID                     string      `json:"id"`
	Name                   string      `json:"name"`
	OrganizationType       string      `json:"organizationType"`
	INN                    string      `json:"inn"`
	RegistrationNumber     string      `json:"registrationNumber"`
	RegistrationDate       string      `json:"registrationDate"`
	OKVED                  string      `json:"okved"`
	DirectorName           string      `json:"directorName"`
	Contact                Contact     `json:"contact"`
	Financials             Financials  `json:"financials"`
	Simulation             bool        `json:"simulation"`
}

type Repository interface {
	CompanyByID(context.Context, string) (Company, error)
	ResolveCompany(context.Context, string, string, bool) (string, error)
}

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

// ResolveCompany is enforced by the PostgreSQL repository; the selector itself grants no rights.
func (s *Service) ResolveCompany(ctx context.Context, actor, selected string, write bool) (string, error) {
	return s.repo.ResolveCompany(ctx, actor, selected, write)
}
func (s *Service) Get(ctx context.Context, actor, requested string) (Company, error) {
	id, err := s.ResolveCompany(ctx, actor, requested, false)
	if err != nil {
		return Company{}, err
	}
	return s.repo.CompanyByID(ctx, id)
}
