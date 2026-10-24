// Package review provides read-only views of persisted lending results.
package review

import (
	"context"
	"encoding/json"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/scoring"
)

type Processing struct {
	Stage   string     `json:"stage"`
	State   string     `json:"state"`
	RetryAt *time.Time `json:"retryAt"`
	Message *string    `json:"message"`
}

type Offer struct {
	OfferID           string      `json:"offerId"`
	ApplicationID     string      `json:"applicationId"`
	ProductID         string      `json:"productId"`
	AmountRub         int64       `json:"amountRub"`
	TermMonths        int         `json:"termMonths"`
	AnnualRatePercent json.Number `json:"annualRatePercent"`
	MonthlyPaymentRub int64       `json:"monthlyPaymentRub"`
	ExpiresAt         *time.Time  `json:"expiresAt"`
	Status            string      `json:"status"`
}

type CreditDecision struct {
	ApplicationID string             `json:"applicationId"`
	Status        application.Status `json:"status"`
	Version       int64              `json:"version"`
	Decision      *string            `json:"decision"`
	Score         *int               `json:"score"`
	RiskLevel     *string            `json:"riskLevel"`
	Reasons       []string           `json:"reasons"`
	AssessedAt    *time.Time         `json:"assessedAt"`
	ReviewedBy    *string            `json:"reviewedBy"`
	Offer         *Offer             `json:"offer"`
	Processing    *Processing        `json:"processing"`
}

type CompanySummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	INN  string `json:"inn"`
}
type BankApplication struct {
	ApplicationID        string                     `json:"applicationId"`
	Company              CompanySummary             `json:"company"`
	Product              application.ProductSummary `json:"product"`
	RequestedAmountRub   int64                      `json:"requestedAmountRub"`
	TermMonths           int                        `json:"termMonths"`
	Status               application.Status         `json:"status"`
	Version              int64                      `json:"version"`
	CreatedAt            time.Time                  `json:"createdAt"`
	UpdatedAt            time.Time                  `json:"updatedAt"`
	SubmittedAt          time.Time                  `json:"submittedAt"`
	ManualReviewRequired bool                       `json:"manualReviewRequired"`
	AvailableActions     []string                   `json:"availableActions"`
}
type BankPage struct {
	Items  []BankApplication `json:"items"`
	Total  int64             `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}
type BankDetail struct {
	application.Application
	Company              company.Company            `json:"company"`
	Product              application.ProductSummary `json:"product"`
	ProductTerms         credit.Product             `json:"productTerms"`
	ManualReviewRequired bool                       `json:"manualReviewRequired"`
}
type ScoringAssessment struct {
	Score          int             `json:"score"`
	Model          *string         `json:"model"`
	ModelVersion   *string         `json:"modelVersion"`
	Decision       string          `json:"decision"`
	ApprovedAmount int64           `json:"approvedAmountRub"`
	Reasons        []string        `json:"reasons"`
	Factors        scoring.Factors `json:"factors"`
	AssessedAt     time.Time       `json:"assessedAt"`
}
type RecommendedOffer struct {
	AmountRub  int64 `json:"amountRub"`
	TermMonths int   `json:"termMonths"`
}
type Recommendation struct {
	Decision string            `json:"decision"`
	Reasons  []string          `json:"reasons"`
	Offer    *RecommendedOffer `json:"offer"`
}
type ManualDecision struct {
	Decision            string    `json:"decision"`
	Comment             string    `json:"comment"`
	InternalComment     string    `json:"internalComment"`
	CustomerExplanation string    `json:"customerExplanation"`
	ReviewedBy          string    `json:"reviewedBy"`
	ReviewedAt          time.Time `json:"reviewedAt"`
}
type ProcessingDetails struct {
	Stage         string `json:"stage"`
	AttemptCount  int    `json:"attemptCount"`
	MaxAttempts   int    `json:"maxAttempts"`
	RecoveryCount int    `json:"recoveryCount"`
	CanRetry      bool   `json:"canRetry"`
}
type BankAssessment struct {
	ProcessingDetails   *ProcessingDetails      `json:"processingDetails"`
	ApplicationID       string                  `json:"applicationId"`
	GoDecision          *decision.Result        `json:"goDecision"`
	Status              application.Status      `json:"status"`
	Version             int64                   `json:"version"`
	CompanyCheck        *pipeline.CompanyCheck  `json:"companyCheck"`
	CreditHistory       *pipeline.CreditHistory `json:"creditHistory"`
	Scoring             *ScoringAssessment      `json:"scoring"`
	RiskLevel           *string                 `json:"riskLevel"`
	ManualReviewReasons []decision.Reason       `json:"manualReviewReasons"`
	Recommendations     []Recommendation        `json:"recommendations"`
	ManualDecision      *ManualDecision         `json:"manualDecision"`
	Processing          *Processing             `json:"processing"`
}

type Filter struct {
	Limit, Offset int
	Status        *application.Status
	CompanyID     *string
}

// Record contains only the persisted inputs needed by the views. No lease token,
// storage key, raw HTTP failure or infrastructure address crosses this boundary.
type Record struct {
	Application       application.Application
	FinancialSnapshot json.RawMessage
	ProductSnapshot   json.RawMessage
	Job               *Job
	Final             *Final
}
type Job struct {
	AttemptCount  int               `json:"attemptCount"`
	MaxAttempts   int               `json:"maxAttempts"`
	RecoveryCount int               `json:"recoveryCount"`
	Status        string            `json:"status"`
	Stage         string            `json:"stage"`
	HasError      bool              `json:"hasError"`
	AvailableAt   time.Time         `json:"availableAt"`
	LeaseUntil    *time.Time        `json:"leaseUntil"`
	ReadAt        time.Time         `json:"readAt"`
	Prechecks     *pipeline.Results `json:"prechecks"`
	Scoring       *scoring.Response `json:"scoring"`
	ScoringAt     *time.Time        `json:"scoringAt"`
	Assessment    *Assessment       `json:"assessment"`
}
type Assessment struct {
	SchemaVersion int             `json:"schemaVersion"`
	Source        string          `json:"source"`
	Result        decision.Result `json:"result"`
	AssessedAt    time.Time       `json:"assessedAt"`
}
type Final struct {
	Outcome        string          `json:"outcome"`
	Reasons        []string        `json:"reasons"`
	RiskLevel      *string         `json:"riskLevel"`
	AssessedAt     time.Time       `json:"assessedAt"`
	ReviewedBy     *string         `json:"reviewedBy"`
	Offer          *Offer          `json:"offer"`
	ManualDecision *ManualDecision `json:"manualDecision"`
}
type Repository interface {
	Read(context.Context, *application.Scope, string) (Record, error)
	List(context.Context, Filter) ([]Record, int64, error)
}
