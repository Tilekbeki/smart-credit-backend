// Package lending coordinates employee decisions and client offer actions.
package lending

import (
	"context"
	"encoding/json"
	"errors"
)

const (
	Manual  = "manual"
	Accept  = "accept"
	Decline = "decline"
)

var (
	ErrValidation          = errors.New("invalid manual decision conditions")
	ErrIdempotencyConflict = errors.New("idempotency key conflict")
	ErrOfferNotActive      = errors.New("offer is not active")
)

type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ValidationError struct{ Details []FieldError }

func (e *ValidationError) Error() string { return ErrValidation.Error() }
func (e *ValidationError) Unwrap() error { return ErrValidation }

type ManualOffer struct {
	AmountRub  int64 `json:"amountRub"`
	TermMonths int   `json:"termMonths"`
}

type Command struct {
	ApplicationID string
	ActorID       string
	CompanyID     string
	Key           string
	Operation     string
	RequestHash   string
	Version       int64
	Decision      string
	Comment       string
	OfferID       string
	Offer         *ManualOffer
}

type Response struct {
	Status int
	Body   json.RawMessage
}

type Repository interface {
	Execute(context.Context, Command) (Response, error)
}
