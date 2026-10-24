package company

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalidInput        = errors.New("invalid company management input")
	ErrVersionConflict     = errors.New("data version conflict")
	ErrInvalidStatus       = errors.New("data change status conflict")
	ErrIdempotencyConflict = errors.New("data idempotency conflict")
)

type Membership struct {
	ID               string   `json:"membershipId"`
	UserID           string   `json:"userId"`
	CompanyID        string   `json:"companyId"`
	CompanyName      string   `json:"companyName"`
	OrganizationType string   `json:"organizationType"`
	MembershipRole   string   `json:"membershipRole"`
	Status           string   `json:"status"`
	Version          int64    `json:"version"`
	AvailableActions []string `json:"availableActions"`
}
type UserProfile struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	LegalName       string  `json:"legalName"`
	Phone           string  `json:"phone"`
	Role            string  `json:"role"`
	CompanyID       *string `json:"companyId"`
	DisplayName     string  `json:"displayName"`
	AdditionalEmail *string `json:"additionalEmail"`
	Version         int64   `json:"version"`
}
type RegistrationChanges struct {
	Name               *string `json:"name,omitempty"`
	INN                *string `json:"inn,omitempty"`
	RegistrationNumber *string `json:"registrationNumber,omitempty"`
	RegistrationDate   *string `json:"registrationDate,omitempty"`
	OKVED              *string `json:"okved,omitempty"`
	DirectorName       *string `json:"directorName,omitempty"`
}
type Command struct {
	ActorID, CompanyID, TargetID, Action, Key, RequestHash string
	Version                                                int64                `json:"version"`
	DisplayName                                            *string              `json:"displayName,omitempty"`
	AdditionalEmail                                        *string              `json:"additionalEmail,omitempty"`
	Contact                                                *Contact             `json:"contact,omitempty"`
	Changes                                                *RegistrationChanges `json:"changes,omitempty"`
	MonthlyRevenueRub                                      *int64               `json:"monthlyRevenueRub,omitempty"`
	MonthlyExpensesRub                                     *int64               `json:"monthlyExpensesRub,omitempty"`
	SupportingDocumentID                                   *string              `json:"supportingDocumentId,omitempty"`
	MembershipRole                                         string               `json:"membershipRole,omitempty"`
	Status                                                 string               `json:"status,omitempty"`
	Reason                                                 string               `json:"reason,omitempty"`
	Outcome                                                string               `json:"outcome,omitempty"`
}
type Response struct {
	Status int
	Body   json.RawMessage
}
type ManagementRepository interface {
	Memberships(context.Context, string, string) ([]Membership, error)
	Profile(context.Context, string) (UserProfile, error)
	History(context.Context, string, string, string, bool) ([]json.RawMessage, error)
	Execute(context.Context, Command) (Response, error)
}
type ManagementService struct{ repo ManagementRepository }

func NewManagementService(repo ManagementRepository) *ManagementService {
	return &ManagementService{repo: repo}
}
func (s *ManagementService) Memberships(ctx context.Context, actor, company string) ([]Membership, error) {
	return s.repo.Memberships(ctx, actor, company)
}
func (s *ManagementService) Profile(ctx context.Context, actor string) (UserProfile, error) {
	return s.repo.Profile(ctx, actor)
}
func (s *ManagementService) History(ctx context.Context, actor, company, kind string, bank bool) ([]json.RawMessage, error) {
	return s.repo.History(ctx, actor, company, kind, bank)
}

var contactPhone = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
var innFormat = regexp.MustCompile(`^([0-9]{10}|[0-9]{12})$`)
var regFormat = regexp.MustCompile(`^([0-9]{13}|[0-9]{15})$`)
var okvedFormat = regexp.MustCompile(`^[0-9]{2}(\.[0-9]{1,2}){0,2}$`)

func normalize(value *string, max int, empty bool) bool {
	if value == nil {
		return false
	}
	*value = strings.TrimSpace(*value)
	return utf8.ValidString(*value) && utf8.RuneCountInString(*value) <= max && (empty || *value != "") && strings.IndexFunc(*value, unicode.IsControl) < 0
}
func validEmail(value string) bool {
	if value == "" {
		return true
	}
	a, err := mail.ParseAddress(value)
	return err == nil && a.Address == value && !strings.ContainsAny(value, "<>\r\n") && len(value) <= 254
}
func validID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && len(value) == 36 && id != uuid.Nil
}

// Validation owns its normalized copy, including nested pointers. Reusing one
// immutable command in concurrent service calls must not mutate the caller.
func ValidateCommand(c *Command) error {
	*c = copyCommand(*c)
	if !validID(c.ActorID) || c.Version < 0 {
		return ErrInvalidInput
	}
	if c.Action != "profile" && (!validID(c.CompanyID) || (c.Action != "contact" && !validID(c.Key))) {
		return ErrInvalidInput
	}
	if c.Action != "membership" && c.Version < 1 {
		return ErrInvalidInput
	}
	switch c.Action {
	case "profile":
		if c.DisplayName == nil && c.AdditionalEmail == nil {
			return ErrInvalidInput
		}
		if c.DisplayName != nil && !normalize(c.DisplayName, 160, false) {
			return ErrInvalidInput
		}
		if c.AdditionalEmail != nil && (!normalize(c.AdditionalEmail, 254, true) || !validEmail(*c.AdditionalEmail)) {
			return ErrInvalidInput
		}
	case "contact":
		if c.Contact == nil || !contactPhone.MatchString(c.Contact.Phone) || !normalize(&c.Contact.Email, 254, false) || !validEmail(c.Contact.Email) || !normalize(&c.Contact.Address, 300, false) {
			return ErrInvalidInput
		}
	case "membership":
		if !validID(c.TargetID) || (c.MembershipRole != "OWNER" && c.MembershipRole != "AUTHORIZED_REPRESENTATIVE" && c.MembershipRole != "VIEWER") || (c.Status != "active" && c.Status != "revoked") {
			return ErrInvalidInput
		}
	case "registration":
		if c.Changes == nil {
			return ErrInvalidInput
		}
		ch := c.Changes
		if ch.Name == nil && ch.INN == nil && ch.RegistrationNumber == nil && ch.RegistrationDate == nil && ch.OKVED == nil && ch.DirectorName == nil {
			return ErrInvalidInput
		}
		for _, f := range []*string{ch.Name, ch.DirectorName} {
			if f != nil && !normalize(f, 160, false) {
				return ErrInvalidInput
			}
		}
		if ch.INN != nil && !innFormat.MatchString(*ch.INN) {
			return ErrInvalidInput
		}
		if ch.RegistrationNumber != nil && !regFormat.MatchString(*ch.RegistrationNumber) {
			return ErrInvalidInput
		}
		if ch.OKVED != nil && !okvedFormat.MatchString(*ch.OKVED) {
			return ErrInvalidInput
		}
		if ch.RegistrationDate != nil {
			date, err := time.Parse(time.DateOnly, *ch.RegistrationDate)
			if err != nil || date.After(time.Now().UTC()) || date.Year() < 1800 {
				return ErrInvalidInput
			}
		}
	case "financial":
		if c.MonthlyRevenueRub == nil || c.MonthlyExpensesRub == nil {
			return ErrInvalidInput
		}
		for _, v := range []int64{*c.MonthlyRevenueRub, *c.MonthlyExpensesRub} {
			if v < 0 || v > 1_000_000_000_000 {
				return ErrInvalidInput
			}
		}
		if c.SupportingDocumentID != nil && !validID(*c.SupportingDocumentID) {
			return ErrInvalidInput
		}
	case "review_registration", "review_financial":
		if !validID(c.TargetID) || (c.Outcome != "approved" && c.Outcome != "rejected") {
			return ErrInvalidInput
		}
	case "cancel_registration":
		if !validID(c.TargetID) {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	if c.Action != "profile" && c.Action != "contact" && !normalize(&c.Reason, 1000, false) {
		return ErrInvalidInput
	}
	return nil
}
func (s *ManagementService) Execute(ctx context.Context, c Command) (Response, error) {
	if err := ValidateCommand(&c); err != nil {
		return Response{}, err
	}
	// Scope, target and operation are included, so one key cannot be rebound.
	data, err := json.Marshal(c)
	if err != nil {
		return Response{}, ErrInvalidInput
	}
	h := sha256.Sum256(data)
	c.RequestHash = hex.EncodeToString(h[:])
	return s.repo.Execute(ctx, c)
}

func copyCommand(c Command) Command {
	text := func(p *string) *string {
		if p == nil {
			return nil
		}
		value := *p
		return &value
	}
	c.DisplayName = text(c.DisplayName)
	c.AdditionalEmail = text(c.AdditionalEmail)
	c.SupportingDocumentID = text(c.SupportingDocumentID)
	if c.Contact != nil {
		contact := *c.Contact
		c.Contact = &contact
	}
	if c.Changes != nil {
		changes := *c.Changes
		changes.Name = text(changes.Name)
		changes.INN = text(changes.INN)
		changes.RegistrationNumber = text(changes.RegistrationNumber)
		changes.RegistrationDate = text(changes.RegistrationDate)
		changes.OKVED = text(changes.OKVED)
		changes.DirectorName = text(changes.DirectorName)
		c.Changes = &changes
	}
	return c
}
