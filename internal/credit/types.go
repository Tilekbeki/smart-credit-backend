// Package credit provides a preliminary financing selection, not a credit decision.
package credit

import (
	"context"
	"encoding/json"
	"errors"

	"smart-credit-backend/internal/company"
)

var (
	ErrProductNotFound = errors.New("credit product not found")
	ErrStorage         = errors.New("credit product storage unavailable")
	ErrInvalidInput    = errors.New("invalid recommendation input")
)

type Product struct {
	ID                   string      `json:"id"`
	Name                 string      `json:"name"`
	Purposes             []string    `json:"purposes"`
	MinAmountRub         int64       `json:"minAmountRub"`
	MaxAmountRub         int64       `json:"maxAmountRub"`
	MinTermMonths        int         `json:"minTermMonths"`
	MaxTermMonths        int         `json:"maxTermMonths"`
	AnnualRatePercent    json.Number `json:"annualRatePercent"`
	MinBusinessAgeMonths int         `json:"minBusinessAgeMonths"`
	Version              int64       `json:"version"`
	Illustrative         bool        `json:"illustrative"`
}

type Repository interface {
	ListProducts(context.Context) ([]Product, error)
	ProductByID(context.Context, string) (Product, error)
}

type Input struct {
	Purpose            string `json:"purpose"`
	RequestedAmountRub int64  `json:"requestedAmountRub"`
	TermMonths         int    `json:"termMonths"`
	MonthlyRevenueRub  int64  `json:"monthlyRevenueRub"`
	MonthlyExpensesRub int64  `json:"monthlyExpensesRub"`
}

type Matches struct {
	Purpose       bool `json:"purpose"`
	Amount        bool `json:"amount"`
	Term          bool `json:"term"`
	BusinessAge   bool `json:"businessAge"`
	Affordability bool `json:"affordability"`
}

type Adjustment struct {
	AmountRub         int64  `json:"amountRub"`
	TermMonths        int    `json:"termMonths"`
	MonthlyPaymentRub int64  `json:"monthlyPaymentRub"`
	Explanation       string `json:"explanation"`
}

type ProductRecommendation struct {
	Product                       Product      `json:"product"`
	Matches                       Matches      `json:"matches"`
	Eligible                      bool         `json:"eligible"`
	PreliminaryAvailableAmountRub int64        `json:"preliminaryAvailableAmountRub"`
	MonthlyPaymentRub             *int64       `json:"monthlyPaymentRub"`
	Reasons                       []string     `json:"reasons"`
	Limitations                   []string     `json:"limitations"`
	Adjustments                   []Adjustment `json:"adjustments"`
}

type Result struct {
	CompanyID                  string                  `json:"companyId"`
	BusinessAgeMonths          int                     `json:"businessAgeMonths"`
	SurplusRub                 int64                   `json:"surplusRub"`
	ExistingMonthlyPaymentsRub int64                   `json:"existingMonthlyPaymentsRub"`
	PaymentBudgetRub           int64                   `json:"paymentBudgetRub"`
	Products                   []ProductRecommendation `json:"products"`
	RecommendedProductID       *string                 `json:"recommendedProductId"`
	Disclaimer                 string                  `json:"disclaimer"`
	Simulation                 bool                    `json:"simulation"`
}

type CompanyReader interface {
	CompanyByID(context.Context, string) (company.Company, error)
}
