// Package auth defines demonstration identity, token, and authorization boundaries.
package auth

import (
	"context"
	"errors"
	"time"
)

type Role string

const (
	RoleClient       Role = "CLIENT"
	RoleBankEmployee Role = "BANK_EMPLOYEE"
)

// User is always loaded from PostgreSQL; role and company claims supplied by a
// caller are never used to authorize a request.
type User struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Phone     string  `json:"phone"`
	Role      Role    `json:"role"`
	CompanyID *string `json:"companyId"`
}

type Profile struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Phone       string  `json:"phone"`
	Role        Role    `json:"role"`
	CompanyName *string `json:"companyName"`
}

type OTPChallenge struct {
	ID        string
	ExpiresAt time.Time
	Phone     string
}

var (
	ErrUserNotFound      = errors.New("user not found")
	ErrChallengeNotFound = errors.New("OTP challenge not found")
	ErrOTPInvalid        = errors.New("invalid OTP")
	ErrOTPExpired        = errors.New("OTP expired")
	ErrOTPUsed           = errors.New("OTP already used")
	ErrOTPLimit          = errors.New("OTP attempt limit reached")
	ErrOTPInvalidated    = errors.New("OTP invalidated")
	ErrStorage           = errors.New("authentication storage unavailable")
	ErrUnauthorized      = errors.New("unauthorized")
)

type Repository interface {
	ListProfiles(context.Context) ([]Profile, error)
	RequestOTP(context.Context, string) (OTPChallenge, error)
	VerifyOTP(context.Context, string, bool) (User, error)
	UserByID(context.Context, string) (User, error)
}
