package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/document"
)

type SubmissionService struct {
	applications ApplicationReader
	documents    DocumentReader
	repo         SubmissionRepository
}

func NewSubmissionService(applications ApplicationReader, documents DocumentReader, repo SubmissionRepository) *SubmissionService {
	return &SubmissionService{applications: applications, documents: documents, repo: repo}
}

func (s *SubmissionService) Authorize(ctx context.Context, scope application.Scope, id string) error {
	_, err := s.applications.Get(ctx, scope, id)
	return err
}

func requiredFields(fields application.Fields) []FieldError {
	problems := []FieldError{}
	add := func(field string, missing bool) {
		if missing {
			problems = append(problems, FieldError{field, "required", "Обязательное поле не заполнено."})
		}
	}
	add("productId", fields.ProductID == nil)
	add("purpose", fields.Purpose == nil)
	add("requestedAmountRub", fields.RequestedAmountRub == nil)
	add("termMonths", fields.TermMonths == nil)
	add("monthlyRevenueRub", fields.MonthlyRevenueRub == nil)
	add("monthlyExpensesRub", fields.MonthlyExpensesRub == nil)
	if fields.Purpose != nil && *fields.Purpose == "other" {
		add("purposeDescription", fields.PurposeDescription == nil)
	}
	for _, p := range application.ValidateFields(&fields) {
		problems = append(problems, FieldError{p.Field, p.Code, "Переданное значение недопустимо."})
	}
	return problems
}

// ValidateSubmission is also called under database locks using current product,
// company and ready-document rows. HTTP-side validation alone is insufficient.
func ValidateSubmission(input ValidationInput, now time.Time) []FieldError {
	problems := requiredFields(input.Fields)
	if input.Product == nil {
		if input.Fields.ProductID != nil {
			problems = append(problems, FieldError{"productId", "product_unavailable", "Выбранный продукт недоступен."})
		}
		return problems
	}
	for _, p := range application.ValidateProductFields(input.Fields, *input.Product) {
		message := "Значение не соответствует условиям выбранного продукта."
		switch p.Field {
		case "requestedAmountRub":
			message = fmt.Sprintf("Сумма должна быть от %d до %d рублей.", input.Product.MinAmountRub, input.Product.MaxAmountRub)
		case "termMonths":
			message = fmt.Sprintf("Срок должен быть от %d до %d месяцев.", input.Product.MinTermMonths, input.Product.MaxTermMonths)
		case "purpose":
			message = "Допустимые цели продукта: " + strings.Join(input.Product.Purposes, ", ") + "."
		}
		problems = append(problems, FieldError{p.Field, p.Code, message})
	}
	registered, err := time.Parse(time.DateOnly, input.Company.Profile.RegistrationDate)
	if err != nil || registered.After(now.UTC()) {
		problems = append(problems, FieldError{"company.registrationDate", "invalid_value", "Нет корректной даты регистрации компании."})
	} else if credit.BusinessAgeMonths(registered, now) < input.Product.MinBusinessAgeMonths {
		problems = append(problems, FieldError{"company.registrationDate", "business_too_young", fmt.Sprintf("Возраст компании должен быть не менее %d месяцев.", input.Product.MinBusinessAgeMonths)})
	}
	for _, kind := range document.RequiredTypes(input.Product.ID, input.Company.FinancialDataSource == TrustedFinancialSource) {
		present := false
		for _, meta := range input.Documents {
			if meta.DocumentType == kind && meta.Status == "ready" {
				present = true
				break
			}
		}
		if !present {
			problems = append(problems, FieldError{"documents." + string(kind), "required_document_missing", "Загрузите обязательный документ и дождитесь завершения загрузки."})
		}
	}
	// A global limit and a narrower product limit can report the same field.
	// Keep the more specific product explanation instead of duplicate messages.
	unique := make([]FieldError, 0, len(problems))
	positions := map[string]int{}
	for _, problem := range problems {
		key := problem.Field + ":" + problem.Code
		if pos, exists := positions[key]; exists {
			unique[pos] = problem
			continue
		}
		positions[key] = len(unique)
		unique = append(unique, problem)
	}
	return unique
}

func (s *SubmissionService) Submit(ctx context.Context, scope application.Scope, id string) (SubmitResult, error) {
	a, err := s.applications.Get(ctx, scope, id)
	if err != nil {
		return SubmitResult{}, err
	}
	// Replays do not revalidate live profiles or reread attachments: submission
	// snapshots and the unique processing job already define the operation.
	if a.Status != application.Draft {
		return s.repo.Submit(ctx, scope, id, a.Version, nil)
	}
	if details := requiredFields(a.Fields); len(details) != 0 {
		return SubmitResult{}, &ValidationError{Details: details}
	}
	docs, err := s.documents.List(ctx, document.Scope{UserID: scope.UserID, CompanyID: scope.CompanyID, Role: auth.RoleClient}, id)
	if err != nil {
		return SubmitResult{}, ErrStorage
	}
	ids := make([]string, 0, len(docs))
	for _, meta := range docs {
		ids = append(ids, meta.DocumentID)
	}
	return s.repo.Submit(ctx, scope, id, a.Version, ids)
}
