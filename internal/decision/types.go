// Package decision applies credit policy to immutable, persisted assessment data.
// It does not calculate a risk score or call the external scoring service.
package decision

import (
	"encoding/json"
	"errors"
	"time"

	"smart-credit-backend/internal/application"
)

const PolicyVersion = "go-demo-v1"

var ErrInvalidData = errors.New("invalid persisted decision data")

type Input struct {
	ApplicationID     string
	FinancialSnapshot json.RawMessage
	ProductSnapshot   json.RawMessage
	PrecheckResults   json.RawMessage
	ScoringRequest    json.RawMessage
	ScoringResponse   json.RawMessage
	SubmittedAt       time.Time
}

type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Offer struct {
	ProductID         string      `json:"productId"`
	AmountRub         int64       `json:"amountRub"`
	TermMonths        int         `json:"termMonths"`
	AnnualRatePercent json.Number `json:"annualRatePercent"`
	MonthlyPaymentRub int64       `json:"monthlyPaymentRub"`
	TotalPaymentRub   int64       `json:"totalPaymentRub"`
	OverpaymentRub    int64       `json:"overpaymentRub"`
}

// ManualReview has no final credit outcome: its reasons are persisted in the
// job assessment until a bank employee makes a decision in a later stage.
type Result struct {
	Status              application.Status `json:"status"`
	Score               int                `json:"score"`
	RiskLevel           string             `json:"riskLevel"`
	Reasons             []string           `json:"reasons"`
	ManualReviewReasons []Reason           `json:"manualReviewReasons"`
	Offer               *Offer             `json:"offer"`
	PolicyVersion       string             `json:"policyVersion"`
}
