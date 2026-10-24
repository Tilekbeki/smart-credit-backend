package lending

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"smart-credit-backend/internal/application"
)

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

// Execute hashes typed, normalized input, so JSON whitespace and field order do
// not change the identity of a request. Authorization and replay are checked by
// the repository in the same transaction as the mutation.
func (s *Service) Execute(ctx context.Context, command Command) (Response, error) {
	var ok bool
	if command.ApplicationID, ok = canonicalID(command.ApplicationID); !ok {
		return Response{}, application.ErrInvalidInput
	}
	if command.ActorID, ok = canonicalID(command.ActorID); !ok {
		return Response{}, application.ErrInvalidInput
	}
	if command.Key, ok = canonicalID(command.Key); !ok || command.Version < 1 {
		return Response{}, application.ErrInvalidInput
	}
	var body any
	switch command.Operation {
	case Manual:
		if command.CompanyID != "" || command.OfferID != "" {
			return Response{}, application.ErrInvalidInput
		}
		if command.Decision != "approved" && command.Decision != "rejected" && command.Decision != "counter_offer" {
			return Response{}, application.ErrInvalidInput
		}
		if (command.Decision == "rejected") != (command.Offer == nil) {
			return Response{}, application.ErrInvalidInput
		}
		command.Comment = strings.TrimSpace(command.Comment)
		if command.Comment == "" {
			return Response{}, invalidField("comment", "required", "Добавьте внутренний комментарий сотрудника банка.")
		}
		if !utf8.ValidString(command.Comment) || strings.ContainsRune(command.Comment, 0) {
			return Response{}, invalidField("comment", "invalid_value", "Комментарий содержит недопустимые символы.")
		}
		if utf8.RuneCountInString(command.Comment) > 1000 {
			return Response{}, invalidField("comment", "out_of_range", "Комментарий должен содержать не более 1000 символов.")
		}
		command.CustomerExplanation = strings.TrimSpace(command.CustomerExplanation)
		if !utf8.ValidString(command.CustomerExplanation) || strings.ContainsRune(command.CustomerExplanation, 0) || utf8.RuneCountInString(command.CustomerExplanation) > 1000 {
			return Response{}, invalidField("customerExplanation", "invalid_value", "Объяснение должно содержать до 1000 символов.")
		}
		if command.Offer != nil && (command.Offer.AmountRub < 1 || command.Offer.AmountRub > 1_000_000_000_000) {
			return Response{}, invalidField("offer.amountRub", "out_of_range", "Сумма должна быть от 1 до 1000000000000 рублей.")
		}
		if command.Offer != nil && (command.Offer.TermMonths < 1 || command.Offer.TermMonths > 120) {
			return Response{}, invalidField("offer.termMonths", "out_of_range", "Срок должен быть от 1 до 120 месяцев.")
		}
		body = struct {
			Version             int64        `json:"version"`
			Decision            string       `json:"decision"`
			Comment             string       `json:"comment"`
			Offer               *ManualOffer `json:"offer"`
			CustomerExplanation string       `json:"customerExplanation,omitempty"`
		}{command.Version, command.Decision, command.Comment, command.Offer, command.CustomerExplanation}
	case Accept, Decline:
		if command.CompanyID != "" {
			command.CompanyID, ok = canonicalID(command.CompanyID)
		}
		if command.CompanyID != "" && !ok {
			return Response{}, application.ErrInvalidInput
		}
		if command.OfferID, ok = canonicalID(command.OfferID); !ok || command.Offer != nil || command.Decision != "" || command.Comment != "" || command.CustomerExplanation != "" {
			return Response{}, application.ErrInvalidInput
		}
		body = struct {
			Version int64  `json:"version"`
			OfferID string `json:"offerId"`
		}{command.Version, command.OfferID}
	default:
		return Response{}, application.ErrInvalidInput
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Response{}, application.ErrInvalidInput
	}
	hash := sha256.Sum256(data)
	command.RequestHash = hex.EncodeToString(hash[:])
	return s.repo.Execute(ctx, command)
}

func invalidField(field, code, message string) *ValidationError {
	return &ValidationError{Details: []FieldError{{Field: field, Code: code, Message: message}}}
}

func canonicalID(value string) (string, bool) {
	id, err := uuid.Parse(value)
	return id.String(), err == nil && len(value) == 36 && id != uuid.Nil
}
