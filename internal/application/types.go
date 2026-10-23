// Package application manages client-owned credit drafts and their history.
package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"smart-credit-backend/internal/company"
)

type Status string

const (
	Draft        Status = "draft"
	Submitted    Status = "submitted"
	Processing   Status = "processing"
	ManualReview Status = "manual_review"
	Approved     Status = "approved"
	Rejected     Status = "rejected"
	CounterOffer Status = "counter_offer"
	Signed       Status = "signed"
	Closed       Status = "closed"
)

var (
	ErrInvalidInput    = errors.New("invalid application input")
	ErrNotFound        = errors.New("application not found")
	ErrVersionConflict = errors.New("application version conflict")
	ErrInvalidStatus   = errors.New("application is not editable")
	ErrProductNotFound = errors.New("application product not found")
	ErrStorage         = errors.New("application storage unavailable")
)

type Scope struct {
	UserID    string
	CompanyID string
}

type Fields struct {
	ProductID          *string `json:"productId"`
	Purpose            *string `json:"purpose"`
	PurposeDescription *string `json:"purposeDescription"`
	RequestedAmountRub *int64  `json:"requestedAmountRub"`
	TermMonths         *int    `json:"termMonths"`
	MonthlyRevenueRub  *int64  `json:"monthlyRevenueRub"`
	MonthlyExpensesRub *int64  `json:"monthlyExpensesRub"`
}

// Optional distinguishes an absent JSON field from an explicit null.
type Optional[T any] struct {
	Present bool
	Value   *T
}

func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	o.Present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		o.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

type Changes struct {
	ProductID          Optional[string] `json:"productId"`
	Purpose            Optional[string] `json:"purpose"`
	PurposeDescription Optional[string] `json:"purposeDescription"`
	RequestedAmountRub Optional[int64]  `json:"requestedAmountRub"`
	TermMonths         Optional[int]    `json:"termMonths"`
	MonthlyRevenueRub  Optional[int64]  `json:"monthlyRevenueRub"`
	MonthlyExpensesRub Optional[int64]  `json:"monthlyExpensesRub"`
}

type Application struct {
	ApplicationID string `json:"applicationId"`
	UserID        string `json:"userId"`
	CompanyID     string `json:"companyId"`
	Fields
	ProductName      *string    `json:"productName"`
	Status           Status     `json:"status"`
	Version          int64      `json:"version"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	SubmittedAt      *time.Time `json:"submittedAt"`
	AvailableActions []string   `json:"availableActions"`
}

type ProductSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Detail struct {
	Application
	Company company.Company `json:"company"`
	Product *ProductSummary `json:"product"`
}

type Summary struct {
	ApplicationID      string    `json:"applicationId"`
	ProductID          *string   `json:"productId"`
	ProductName        *string   `json:"productName"`
	RequestedAmountRub *int64    `json:"requestedAmountRub"`
	TermMonths         *int      `json:"termMonths"`
	Status             Status    `json:"status"`
	Version            int64     `json:"version"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

type ListFilter struct {
	Limit  int
	Offset int
	Status *Status
}

type Page struct {
	Items  []Summary `json:"items"`
	Total  int64     `json:"total"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

type Event struct {
	EventID   string    `json:"eventId"`
	EventType string    `json:"eventType"`
	Sequence  int64     `json:"sequence"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	ActorID   *string   `json:"actorId"`
	Title     string    `json:"title"`
}

type Repository interface {
	Create(context.Context, Scope, Fields) (Application, error)
	Get(context.Context, Scope, string) (Application, error)
	List(context.Context, Scope, ListFilter) (Page, error)
	UpdateDraft(context.Context, Scope, string, int64, Fields) (Application, error)
	Timeline(context.Context, Scope, string) ([]Event, error)
}

type CompanyReader interface {
	CompanyByID(context.Context, string) (company.Company, error)
}
