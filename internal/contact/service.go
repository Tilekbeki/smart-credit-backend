// Package contact limits self-service updates to personal and company contacts.
package contact

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
)

var (
	ErrInvalidInput    = errors.New("invalid contact input")
	ErrForbidden       = errors.New("contact update forbidden")
	ErrVersionConflict = errors.New("contact version conflict")
	ErrStorage         = errors.New("contact storage unavailable")
)

type UserChanges struct {
	Version         int64                        `json:"version"`
	AdditionalEmail application.Optional[string] `json:"additionalEmail"`
}

type CompanyFields struct {
	Phone   application.Optional[string] `json:"phone"`
	Email   application.Optional[string] `json:"email"`
	Address application.Optional[string] `json:"address"`
}

type CompanyChanges struct {
	Version int64         `json:"version"`
	Contact CompanyFields `json:"contact"`
}

type Repository interface {
	UpdateUser(context.Context, string, UserChanges) (auth.User, error)
	UpdateCompany(context.Context, string, string, CompanyChanges) (company.Company, error)
}

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

func validText(value string, limit int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func normalizeEmail(value string) (string, bool) {
	value = strings.TrimSpace(value)
	parsed, err := mail.ParseAddress(value)
	return value, validText(value, 254) && len(value) <= 254 && err == nil && parsed.Address == value &&
		parsed.Name == "" && !strings.ContainsAny(value, " \t\r\n<>")
}

func (s *Service) UpdateUser(ctx context.Context, user auth.User, changes UserChanges) (auth.User, error) {
	if user.Role != auth.RoleClient || user.CompanyID == nil {
		return auth.User{}, ErrForbidden
	}
	if changes.Version < 1 || !changes.AdditionalEmail.Present {
		return auth.User{}, ErrInvalidInput
	}
	if changes.AdditionalEmail.Value != nil {
		value, ok := normalizeEmail(*changes.AdditionalEmail.Value)
		if !ok {
			return auth.User{}, ErrInvalidInput
		}
		changes.AdditionalEmail.Value = &value
	}
	return s.repo.UpdateUser(ctx, user.ID, changes)
}

var phonePattern = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

func (s *Service) UpdateCompany(ctx context.Context, user auth.User, id string, changes CompanyChanges) (company.Company, error) {
	if user.Role != auth.RoleClient || user.CompanyID == nil || *user.CompanyID != id {
		return company.Company{}, ErrForbidden
	}
	f := &changes.Contact
	if changes.Version < 1 || !(f.Phone.Present || f.Email.Present || f.Address.Present) {
		return company.Company{}, ErrInvalidInput
	}
	for _, field := range []*application.Optional[string]{&f.Phone, &f.Email, &f.Address} {
		if !field.Present {
			continue
		}
		if field.Value == nil {
			return company.Company{}, ErrInvalidInput
		}
		value := strings.TrimSpace(*field.Value)
		field.Value = &value
	}
	if f.Phone.Present && !phonePattern.MatchString(*f.Phone.Value) {
		return company.Company{}, ErrInvalidInput
	}
	if f.Email.Present {
		value, ok := normalizeEmail(*f.Email.Value)
		if !ok {
			return company.Company{}, ErrInvalidInput
		}
		f.Email.Value = &value
	}
	if f.Address.Present && !validText(*f.Address.Value, 300) {
		return company.Company{}, ErrInvalidInput
	}
	return s.repo.UpdateCompany(ctx, user.ID, id, changes)
}
