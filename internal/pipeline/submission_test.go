package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/document"
)

func submissionPtr[T any](value T) *T { return &value }
func submissionFields() application.Fields {
	return application.Fields{
		ProductID: submissionPtr("turnover"), Purpose: submissionPtr("stock"), RequestedAmountRub: submissionPtr(int64(150000)), TermMonths: submissionPtr(12), MonthlyRevenueRub: submissionPtr(int64(0)), MonthlyExpensesRub: submissionPtr(int64(1)),
	}
}
func submissionInput() ValidationInput {
	input := ValidationInput{Fields: submissionFields(), Product: &credit.Product{ID: "turnover", MinAmountRub: 100000, MaxAmountRub: 3000000, MinTermMonths: 6, MaxTermMonths: 24, Purposes: []string{"stock", "expenses", "seasonal"}, MinBusinessAgeMonths: 6}, Company: CompanySnapshot{FinancialDataSource: TrustedFinancialSource}}
	input.Company.Profile.RegistrationDate = "2020-01-01"
	return input
}

func TestSubmissionValidation(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		change      func(*ValidationInput)
		field, code string
	}{
		{"complete with zero revenue and negative cash flow", func(*ValidationInput) {}, "", ""},
		{"missing amount", func(i *ValidationInput) { i.Fields.RequestedAmountRub = nil }, "requestedAmountRub", "required"},
		{"missing expenses", func(i *ValidationInput) { i.Fields.MonthlyExpensesRub = nil }, "monthlyExpensesRub", "required"},
		{"missing product", func(i *ValidationInput) { i.Fields.ProductID = nil; i.Product = nil }, "productId", "required"},
		{"unavailable product", func(i *ValidationInput) { i.Product = nil }, "productId", "product_unavailable"},
		{"unsupported purpose", func(i *ValidationInput) { i.Fields.Purpose = submissionPtr("equipment") }, "purpose", "unsupported_purpose"},
		{"product amount low", func(i *ValidationInput) { i.Fields.RequestedAmountRub = submissionPtr(int64(99999)) }, "requestedAmountRub", "out_of_range"},
		{"product amount high", func(i *ValidationInput) { i.Fields.RequestedAmountRub = submissionPtr(int64(3000001)) }, "requestedAmountRub", "out_of_range"},
		{"product term", func(i *ValidationInput) { i.Fields.TermMonths = submissionPtr(25) }, "termMonths", "out_of_range"},
		{"negative revenue", func(i *ValidationInput) { i.Fields.MonthlyRevenueRub = submissionPtr(int64(-1)) }, "monthlyRevenueRub", "out_of_range"},
		{"missing date", func(i *ValidationInput) { i.Company.Profile.RegistrationDate = "" }, "company.registrationDate", "invalid_value"},
		{"future date", func(i *ValidationInput) { i.Company.Profile.RegistrationDate = "2027-01-01" }, "company.registrationDate", "invalid_value"},
		{"young company", func(i *ValidationInput) { i.Company.Profile.RegistrationDate = "2026-10-01" }, "company.registrationDate", "business_too_young"},
		{"exactly minimum age", func(i *ValidationInput) { i.Company.Profile.RegistrationDate = "2026-04-23" }, "", ""},
		{"untrusted turnover", func(i *ValidationInput) { i.Company.FinancialDataSource = "unverified" }, "documents.BANK_STATEMENT", "required_document_missing"},
		{"pending is insufficient", func(i *ValidationInput) {
			i.Company.FinancialDataSource = "unverified"
			i.Documents = []document.Metadata{{DocumentType: document.BankStatement, Status: "pending"}}
		}, "documents.BANK_STATEMENT", "required_document_missing"},
		{"failed is insufficient", func(i *ValidationInput) {
			i.Company.FinancialDataSource = "unverified"
			i.Documents = []document.Metadata{{DocumentType: document.BankStatement, Status: "failed"}}
		}, "documents.BANK_STATEMENT", "required_document_missing"},
		{"ready statement", func(i *ValidationInput) {
			i.Company.FinancialDataSource = "unverified"
			i.Documents = []document.Metadata{{DocumentType: document.BankStatement, Status: "ready"}}
		}, "", ""},
		{"negative registration is a later business check", func(i *ValidationInput) { i.Company.RegistrationStatus = "suspended" }, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := submissionInput()
			tc.change(&input)
			issues := ValidateSubmission(input, now)
			if tc.field == "" {
				if len(issues) != 0 {
					t.Fatalf("valid input: %+v", issues)
				}
				return
			}
			for _, issue := range issues {
				if issue.Field == tc.field && issue.Code == tc.code {
					return
				}
			}
			t.Fatalf("missing error %s:%s in %+v", tc.field, tc.code, issues)
		})
	}
}

func TestInvestmentRequiresInvoiceAndOtherPurposeDescription(t *testing.T) {
	input := submissionInput()
	input.Fields.ProductID = submissionPtr("investment")
	input.Product.ID = "investment"
	input.Company.FinancialDataSource = "unverified"
	now := time.Now()
	problems := ValidateSubmission(input, now)
	if len(problems) != 1 || problems[0].Field != "documents.EQUIPMENT_INVOICE" {
		t.Fatalf("invoice policy: %+v", problems)
	}
	input.Documents = []document.Metadata{{DocumentType: document.EquipmentInvoice, Status: "ready"}}
	if problems = ValidateSubmission(input, now); len(problems) != 0 {
		t.Fatalf("invoice should suffice: %+v", problems)
	}
	input.Product.Purposes = []string{"other"}
	input.Fields.Purpose = submissionPtr("other")
	if problems = ValidateSubmission(input, now); len(problems) != 1 || problems[0].Field != "purposeDescription" {
		t.Fatalf("other purpose: %+v", problems)
	}
	input.Fields.PurposeDescription = submissionPtr("Оборудование")
	if problems = ValidateSubmission(input, now); len(problems) != 0 {
		t.Fatal(problems)
	}
}

type submissionApplicationStub struct {
	record application.Application
	err    error
}

func (s *submissionApplicationStub) Get(context.Context, application.Scope, string) (application.Application, error) {
	return s.record, s.err
}

type submissionDocumentStub struct {
	records []document.Metadata
	err     error
	calls   int
}

func (s *submissionDocumentStub) List(context.Context, document.Scope, string) ([]document.Metadata, error) {
	s.calls++
	return s.records, s.err
}

type submissionRepositoryStub struct {
	calls   int
	version int64
	ids     []string
	err     error
}

func (s *submissionRepositoryStub) Submit(_ context.Context, _ application.Scope, id string, version int64, ids []string) (SubmitResult, error) {
	s.calls++
	s.version = version
	s.ids = ids
	return SubmitResult{ApplicationID: id, Status: application.Submitted, SubmittedAt: time.Now(), NextStage: "prechecks"}, s.err
}

func TestSubmissionServiceValidationReplayAndErrors(t *testing.T) {
	apps := &submissionApplicationStub{record: application.Application{Status: application.Draft, Version: 4, Fields: submissionFields()}}
	docs := &submissionDocumentStub{records: []document.Metadata{{DocumentID: "verified"}}}
	repo := &submissionRepositoryStub{}
	service := NewSubmissionService(apps, docs, repo)
	if _, err := service.Submit(t.Context(), application.Scope{}, "id"); err != nil || repo.version != 4 || len(repo.ids) != 1 || repo.ids[0] != "verified" {
		t.Fatal("verified identifiers not forwarded")
	}
	apps.record.Fields.MonthlyRevenueRub = nil
	if _, err := service.Submit(t.Context(), application.Scope{}, "id"); err == nil {
		t.Fatal("incomplete draft submitted")
	}
	if docs.calls != 1 || repo.calls != 1 {
		t.Fatal("invalid draft reached documents/repository")
	}
	apps.record.Status = application.Processing
	docs.err = errors.New("storage unavailable")
	if _, err := service.Submit(t.Context(), application.Scope{}, "id"); err != nil || repo.calls != 2 || docs.calls != 1 {
		t.Fatal("replay must use frozen data")
	}
	apps.record.Status = application.Draft
	apps.record.Fields = submissionFields()
	if _, err := service.Submit(t.Context(), application.Scope{}, "id"); err != ErrStorage {
		t.Fatal("document error not sanitized")
	}
	apps.err = application.ErrNotFound
	if _, err := service.Submit(t.Context(), application.Scope{}, "id"); err != application.ErrNotFound {
		t.Fatal("foreign/missing draft exposed")
	}
}
