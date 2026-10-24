package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
)

func ptr[T any](value T) *T { return &value }

type repositoryStub struct {
	record Application
	err    error
	writes int
}

func (r *repositoryStub) Create(_ context.Context, scope Scope, f Fields) (Application, error) {
	r.writes++
	r.record = Application{UserID: scope.UserID, CompanyID: scope.CompanyID, Fields: f, Status: Draft, Version: 1}
	return r.record, r.err
}
func (r *repositoryStub) Get(context.Context, Scope, string) (Application, error) {
	return r.record, r.err
}
func (r *repositoryStub) List(_ context.Context, _ Scope, f ListFilter) (Page, error) {
	return Page{Limit: f.Limit, Offset: f.Offset}, r.err
}
func (r *repositoryStub) UpdateDraft(_ context.Context, _ Scope, _ string, v int64, f Fields) (Application, error) {
	r.writes++
	r.record.Fields = f
	r.record.Version = v + 1
	return r.record, r.err
}
func (r *repositoryStub) Timeline(context.Context, Scope, string) ([]Event, error) {
	return []Event{}, r.err
}

type catalogStub struct{ err error }

func (c catalogStub) ListProducts(context.Context) ([]credit.Product, error) { return nil, c.err }
func (c catalogStub) ProductByID(_ context.Context, id string) (credit.Product, error) {
	if c.err != nil {
		return credit.Product{}, c.err
	}
	if id != "turnover" {
		return credit.Product{}, credit.ErrProductNotFound
	}
	return credit.Product{ID: id, Name: "Оборотный", MinAmountRub: 100000, MaxAmountRub: 3000000, MinTermMonths: 6, MaxTermMonths: 24, Purposes: []string{"stock", "expenses", "seasonal"}}, nil
}
func (c catalogStub) CompanyByID(context.Context, string) (company.Company, error) {
	return company.Company{}, c.err
}

func TestCreatePartialDraftValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields Fields
		want   error
	}{
		{"empty", Fields{}, nil},
		{"product only", Fields{ProductID: ptr("turnover")}, nil},
		{"purpose only", Fields{Purpose: ptr("other")}, nil},
		{"zero cash flow", Fields{MonthlyRevenueRub: ptr(int64(0)), MonthlyExpensesRub: ptr(int64(0))}, nil},
		{"maximum global amount", Fields{RequestedAmountRub: ptr(int64(1e12))}, nil},
		{"minimum global amount", Fields{RequestedAmountRub: ptr(int64(1))}, nil},
		{"unknown product", Fields{ProductID: ptr("missing")}, ErrProductNotFound},
		{"empty product", Fields{ProductID: ptr("")}, ErrInvalidInput},
		{"malformed product", Fields{ProductID: ptr("' OR true--")}, ErrInvalidInput},
		{"purpose whitespace", Fields{Purpose: ptr(" stock ")}, ErrInvalidInput},
		{"unknown purpose", Fields{Purpose: ptr("unknown")}, ErrInvalidInput},
		{"negative amount", Fields{RequestedAmountRub: ptr(int64(-1))}, ErrInvalidInput},
		{"zero amount", Fields{RequestedAmountRub: ptr(int64(0))}, ErrInvalidInput},
		{"large amount", Fields{RequestedAmountRub: ptr(int64(1e12 + 1))}, ErrInvalidInput},
		{"zero term", Fields{TermMonths: ptr(0)}, ErrInvalidInput},
		{"large term", Fields{TermMonths: ptr(121)}, ErrInvalidInput},
		{"negative revenue", Fields{MonthlyRevenueRub: ptr(int64(-1))}, ErrInvalidInput},
		{"large revenue", Fields{MonthlyRevenueRub: ptr(int64(1e12 + 1))}, ErrInvalidInput},
		{"negative expenses", Fields{MonthlyExpensesRub: ptr(int64(-1))}, ErrInvalidInput},
		{"blank description", Fields{PurposeDescription: ptr(" \n ")}, ErrInvalidInput},
		{"NUL description", Fields{PurposeDescription: ptr("a\x00b")}, ErrInvalidInput},
		{"description too long", Fields{PurposeDescription: ptr(strings.Repeat("я", 301))}, ErrInvalidInput},
		{"Unicode limit", Fields{PurposeDescription: ptr(strings.Repeat("я", 300))}, nil},
		{"product minimum amount", Fields{ProductID: ptr("turnover"), RequestedAmountRub: ptr(int64(100000))}, nil},
		{"product below minimum", Fields{ProductID: ptr("turnover"), RequestedAmountRub: ptr(int64(99999))}, ErrInvalidInput},
		{"product above maximum", Fields{ProductID: ptr("turnover"), RequestedAmountRub: ptr(int64(3000001))}, ErrInvalidInput},
		{"product short term", Fields{ProductID: ptr("turnover"), TermMonths: ptr(5)}, ErrInvalidInput},
		{"product long term", Fields{ProductID: ptr("turnover"), TermMonths: ptr(25)}, ErrInvalidInput},
		{"product wrong purpose", Fields{ProductID: ptr("turnover"), Purpose: ptr("equipment")}, ErrInvalidInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &repositoryStub{}
			s := NewService(r, catalogStub{}, catalogStub{})
			a, err := s.Create(context.Background(), Scope{UserID: "owner", CompanyID: "company"}, tc.fields)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want %v", err, tc.want)
			}
			if tc.want != nil && r.writes != 0 {
				t.Fatal("invalid draft written")
			}
			if tc.want == nil && (a.Status != Draft || a.Version != 1 || a.UserID != "owner" || a.CompanyID != "company") {
				t.Fatal("draft identity/status")
			}
		})
	}
}

func TestPatchMergeNullVersionAndState(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     Status
		version    int64
		want       error
	}{
		{"absent unchanged", `{"monthlyExpensesRub":0}`, Draft, 4, nil},
		{"clear", `{"productId":null,"requestedAmountRub":null,"purposeDescription":null}`, Draft, 4, nil},
		{"multiple fields", `{"purpose":"expenses","requestedAmountRub":150000,"termMonths":12}`, Draft, 4, nil},
		{"merged product limits", `{"requestedAmountRub":1}`, Draft, 4, ErrInvalidInput},
		{"clear product removes limits", `{"productId":null,"requestedAmountRub":1}`, Draft, 4, nil},
		{"empty patch", `{}`, Draft, 4, ErrInvalidInput},
		{"stale", `{"purpose":null}`, Draft, 3, ErrVersionConflict},
		{"zero version", `{"purpose":null}`, Draft, 0, ErrInvalidInput},
		{"submitted", `{"purpose":null}`, Submitted, 4, ErrInvalidStatus},
		{"signed", `{"purpose":null}`, Signed, 4, ErrInvalidStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &repositoryStub{record: Application{Fields: Fields{ProductID: ptr("turnover"), RequestedAmountRub: ptr(int64(100000)), Purpose: ptr("stock"), PurposeDescription: ptr("Описание")}, Version: 4, Status: tc.status}}
			s := NewService(r, catalogStub{}, catalogStub{})
			var change Changes
			if err := json.Unmarshal([]byte(tc.body), &change); err != nil {
				t.Fatal(err)
			}
			a, err := s.Patch(context.Background(), Scope{}, "id", tc.version, change)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want%v", err, tc.want)
			}
			if err != nil && r.writes != 0 {
				t.Fatal("invalid patch written")
			}
			if err == nil && a.Version != 5 {
				t.Fatal("version not incremented")
			}
			if tc.name == "absent unchanged" && (*a.RequestedAmountRub != 100000 || a.MonthlyExpensesRub == nil || *a.MonthlyExpensesRub != 0) {
				t.Fatal("absent/zero corrupted")
			}
			if tc.name == "clear" && (a.ProductID != nil || a.RequestedAmountRub != nil || a.PurposeDescription != nil || *a.Purpose != "stock") {
				t.Fatal("null did not clear selectively")
			}
		})
	}
}

func TestDescriptionTrimAndStorageFailures(t *testing.T) {
	r := &repositoryStub{}
	s := NewService(r, catalogStub{}, catalogStub{})
	a, err := s.Create(context.Background(), Scope{}, Fields{PurposeDescription: ptr("  Описание \n")})
	if err != nil || *a.PurposeDescription != "Описание" {
		t.Fatal("description not normalized")
	}
	s = NewService(r, catalogStub{err: errors.New("internal credentials")}, catalogStub{})
	if _, err = s.Create(context.Background(), Scope{}, Fields{ProductID: ptr("turnover")}); err != ErrStorage {
		t.Fatal("storage error not sanitized")
	}
	r.err = ErrNotFound
	if _, err = s.Patch(context.Background(), Scope{}, "id", 1, Changes{}); err != ErrNotFound {
		t.Fatal("missing draft should stay hidden")
	}
}

func TestPaginationValidation(t *testing.T) {
	s := NewService(&repositoryStub{}, catalogStub{}, catalogStub{})
	for _, f := range []ListFilter{{}, {Limit: 101}, {Limit: 1, Offset: -1}, {Limit: 1, Status: ptr(Status("confirmed"))}, {Limit: 1, Status: ptr(Status("processing_failed"))}} {
		if _, err := s.List(context.Background(), Scope{}, f); err != ErrInvalidInput {
			t.Fatal("invalid filter accepted")
		}
	}
	for _, status := range []Status{Draft, Submitted, Processing, ManualReview, Approved, Rejected, CounterOffer, Signed, Closed} {
		if _, err := s.List(context.Background(), Scope{}, ListFilter{Limit: 20, Status: &status}); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *repositoryStub) DeleteDraft(_ context.Context, _ Scope, _ string, _ int64) error {
	return r.err
}
