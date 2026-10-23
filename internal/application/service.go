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
	if fields.ProductID != nil && !productCode.MatchString(*fields.ProductID) {
		return ErrInvalidInput
	}
	if fields.Purpose != nil && !slices.Contains([]string{"stock", "expenses", "seasonal", "equipment", "upgrade", "other"}, *fields.Purpose) {
		return ErrInvalidInput
	}
	if fields.PurposeDescription != nil {
		value := strings.TrimSpace(*fields.PurposeDescription)
		if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 300 || strings.ContainsRune(value, 0) {
			return ErrInvalidInput
		}
		fields.PurposeDescription = &value
	}
	if fields.RequestedAmountRub != nil && (*fields.RequestedAmountRub < 1 || *fields.RequestedAmountRub > 1_000_000_000_000) {
		return ErrInvalidInput
	}
	if fields.TermMonths != nil && (*fields.TermMonths < 1 || *fields.TermMonths > 120) {
		return ErrInvalidInput
	}
	for _, value := range []*int64{fields.MonthlyRevenueRub, fields.MonthlyExpensesRub} {
		if value != nil && (*value < 0 || *value > 1_000_000_000_000) {
			return ErrInvalidInput
		}
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
	if fields.RequestedAmountRub != nil && (*fields.RequestedAmountRub < product.MinAmountRub || *fields.RequestedAmountRub > product.MaxAmountRub) {
		return ErrInvalidInput
	}
	if fields.TermMonths != nil && (*fields.TermMonths < product.MinTermMonths || *fields.TermMonths > product.MaxTermMonths) {
		return ErrInvalidInput
	}
	if fields.Purpose != nil && !slices.Contains(product.Purposes, *fields.Purpose) {
		return ErrInvalidInput
	}
	return nil
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
