// Package pipeline submits applications and coordinates durable preliminary checks.
package pipeline

import (
	"context"
	"errors"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/document"
)

var (
	ErrLeaseLost = errors.New("processing lease no longer owned")
	ErrStorage   = errors.New("pipeline storage unavailable")
)

const JobType = "prechecks"
const AwaitingScoring = "awaiting_scoring"
const TrustedFinancialSource = "demo_bank_feed"

type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type ValidationError struct{ Details []FieldError }

func (e *ValidationError) Error() string { return "application submission validation failed" }

type CompanySnapshot struct {
	Profile             company.Company `json:"profile"`
	FinancialDataSource string          `json:"financialDataSource"`
	RegistrationStatus  string          `json:"registrationStatus"`
}

type FinancialSnapshot struct {
	SchemaVersion int                 `json:"schemaVersion"`
	ApplicationID string              `json:"applicationId"`
	UserID        string              `json:"userId"`
	CompanyID     string              `json:"companyId"`
	Fields        application.Fields  `json:"fields"`
	Company       CompanySnapshot     `json:"company"`
	Documents     []document.Metadata `json:"documents"`
}

type SubmitResult struct {
	ApplicationID string             `json:"applicationId"`
	Status        application.Status `json:"status"`
	SubmittedAt   time.Time          `json:"submittedAt"`
	NextStage     string             `json:"nextStage"`
}

type SubmissionRepository interface {
	Submit(context.Context, application.Scope, string, int64, []string) (SubmitResult, error)
}
type ApplicationReader interface {
	Get(context.Context, application.Scope, string) (application.Application, error)
}
type DocumentReader interface {
	List(context.Context, document.Scope, string) ([]document.Metadata, error)
}

type ValidationInput struct {
	Fields    application.Fields
	Product   *credit.Product
	Company   CompanySnapshot
	Documents []document.Metadata
}

type Job struct {
	ID            string
	ApplicationID string
	LeaseToken    string
	Attempt       int
	Snapshot      FinancialSnapshot
}
type Check struct {
	Code   string `json:"code"`
	Passed bool   `json:"passed"`
}
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type CompanyCheck struct {
	Status     string    `json:"status"`
	Checks     []Check   `json:"checks"`
	Problems   []Problem `json:"problems"`
	CheckedAt  time.Time `json:"checkedAt"`
	DurationMS int64     `json:"durationMs"`
	Simulation bool      `json:"simulation"`
}
type CreditHistory struct {
	ExistingDebtRub            int64     `json:"existingDebtRub"`
	ExistingMonthlyPaymentsRub int64     `json:"existingMonthlyPaymentsRub"`
	OverdueCount               int       `json:"overdueCount"`
	CheckedAt                  time.Time `json:"checkedAt"`
	Simulation                 bool      `json:"simulation"`
}
type Results struct {
	SchemaVersion int           `json:"schemaVersion"`
	CompanyCheck  CompanyCheck  `json:"companyCheck"`
	CreditHistory CreditHistory `json:"creditHistory"`
	CompletedAt   time.Time     `json:"completedAt"`
}
type Queue interface {
	Claim(context.Context, time.Duration) (*Job, error)
	Complete(context.Context, Job, Results) error
	Fail(context.Context, Job, string, time.Duration) error
}
type Processor interface {
	Run(context.Context, FinancialSnapshot) (Results, error)
}
