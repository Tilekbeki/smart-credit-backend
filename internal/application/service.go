package application

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"smart-credit-backend/internal/credit"
)

type Service struct {
	repo      Repository
	products  credit.Repository
	companies CompanyReader
}

func NewService(repo Repository, products credit.Repository, companies CompanyReader) *Service {
	return &Service{repo: repo, products: products, companies: companies}
}

func ValidStatus(status Status) bool {
	return slices.Contains([]Status{Draft, Submitted, Processing, ManualReview, Approved, Rejected, CounterOffer, Signed, Closed}, status)
}

var productCode = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func (s *Service) validate(ctx context.Context, fields *Fields) error {
	if len(ValidateFields(fields)) != 0 {
		return ErrInvalidInput
	}
	if fields.ProductID == nil {
		return nil
	}
	product, err := s.products.ProductByID(ctx, *fields.ProductID)
	if errors.Is(err, credit.ErrProductNotFound) {
		return ErrProductNotFound
	}
	if err != nil {
		return ErrStorage
	}
	if len(ValidateProductFields(*fields, product)) != 0 {
		return ErrInvalidInput
	}
	return nil
}

// FieldProblem is shared by partial-draft and complete-submission validation.
type FieldProblem struct {
	Field string
	Code  string
}

// ValidateFields accepts absent draft fields and normalizes the description.
func ValidateFields(fields *Fields) []FieldProblem {
	problems := []FieldProblem{}
	add := func(field, code string) { problems = append(problems, FieldProblem{field, code}) }
	if fields.ProductID != nil && !productCode.MatchString(*fields.ProductID) {
		add("productId", "invalid_value")
	}
	if fields.Purpose != nil && !slices.Contains([]string{"stock", "expenses", "seasonal", "equipment", "upgrade", "other"}, *fields.Purpose) {
		add("purpose", "invalid_value")
	}
	if fields.PurposeDescription != nil {
		value := strings.TrimSpace(*fields.PurposeDescription)
		if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 300 || strings.ContainsRune(value, 0) {
			add("purposeDescription", "invalid_value")
		}
		fields.PurposeDescription = &value
	}
	if fields.RequestedAmountRub != nil && (*fields.RequestedAmountRub < 1 || *fields.RequestedAmountRub > 1_000_000_000_000) {
		add("requestedAmountRub", "out_of_range")
	}
	if fields.TermMonths != nil && (*fields.TermMonths < 1 || *fields.TermMonths > 120) {
		add("termMonths", "out_of_range")
	}
	if fields.MonthlyRevenueRub != nil && (*fields.MonthlyRevenueRub < 0 || *fields.MonthlyRevenueRub > 1_000_000_000_000) {
		add("monthlyRevenueRub", "out_of_range")
	}
	if fields.MonthlyExpensesRub != nil && (*fields.MonthlyExpensesRub < 0 || *fields.MonthlyExpensesRub > 1_000_000_000_000) {
		add("monthlyExpensesRub", "out_of_range")
	}
	return problems
}

// ValidateProductFields applies product conditions to values present in a draft.
func ValidateProductFields(fields Fields, product credit.Product) []FieldProblem {
	problems := []FieldProblem{}
	if fields.RequestedAmountRub != nil && (*fields.RequestedAmountRub < product.MinAmountRub || *fields.RequestedAmountRub > product.MaxAmountRub) {
		problems = append(problems, FieldProblem{"requestedAmountRub", "out_of_range"})
	}
	if fields.TermMonths != nil && (*fields.TermMonths < product.MinTermMonths || *fields.TermMonths > product.MaxTermMonths) {
		problems = append(problems, FieldProblem{"termMonths", "out_of_range"})
	}
	if fields.Purpose != nil && !slices.Contains(product.Purposes, *fields.Purpose) {
		problems = append(problems, FieldProblem{"purpose", "unsupported_purpose"})
	}
	return problems
}

func (s *Service) Create(ctx context.Context, scope Scope, fields Fields) (Application, error) {
	if err := s.validate(ctx, &fields); err != nil {
		return Application{}, err
	}
	return s.repo.Create(ctx, scope, fields)
}

func (s *Service) Get(ctx context.Context, scope Scope, id string) (Detail, error) {
	a, err := s.repo.Get(ctx, scope, id)
	if err != nil {
		return Detail{}, err
	}
	c, err := s.companies.CompanyByID(ctx, a.CompanyID)
	if err != nil {
		return Detail{}, ErrStorage
	}
	d := Detail{Application: a, Company: c}
	if a.ProductID != nil && a.ProductName != nil {
		d.Product = &ProductSummary{ID: *a.ProductID, Name: *a.ProductName}
	}
	return d, nil
}

func (s *Service) List(ctx context.Context, scope Scope, filter ListFilter) (Page, error) {
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 || filter.Offset > 2_147_483_647 || (filter.Status != nil && !ValidStatus(*filter.Status)) {
		return Page{}, ErrInvalidInput
	}
	return s.repo.List(ctx, scope, filter)
}

func merge[T any](target **T, change Optional[T]) bool {
	if change.Present {
		*target = change.Value
	}
	return change.Present
}

func (s *Service) Patch(ctx context.Context, scope Scope, id string, version int64, changes Changes) (Application, error) {
	if version < 1 {
		return Application{}, ErrInvalidInput
	}
	a, err := s.repo.Get(ctx, scope, id)
	if err != nil {
		return Application{}, err
	}
	if a.Status != Draft {
		return Application{}, ErrInvalidStatus
	}
	if a.Version != version {
		return Application{}, ErrVersionConflict
	}
	changed := false
	// Evaluate each merge separately: short circuiting would skip later fields.
	for _, present := range []bool{
		merge(&a.ProductID, changes.ProductID), merge(&a.Purpose, changes.Purpose),
		merge(&a.PurposeDescription, changes.PurposeDescription), merge(&a.RequestedAmountRub, changes.RequestedAmountRub),
		merge(&a.TermMonths, changes.TermMonths), merge(&a.MonthlyRevenueRub, changes.MonthlyRevenueRub),
		merge(&a.MonthlyExpensesRub, changes.MonthlyExpensesRub),
	} {
		changed = changed || present
	}
	if !changed {
		return Application{}, ErrInvalidInput
	}
	if err := s.validate(ctx, &a.Fields); err != nil {
		return Application{}, err
	}
	return s.repo.UpdateDraft(ctx, scope, id, version, a.Fields)
}

func (s *Service) Timeline(ctx context.Context, scope Scope, id string) ([]Event, error) {
	return s.repo.Timeline(ctx, scope, id)
}

func (s *Service) Delete(ctx context.Context, scope Scope, id string, version int64) error {
	if version < 1 {
		return ErrInvalidInput
	}
	return s.repo.DeleteDraft(ctx, scope, id, version)
}
