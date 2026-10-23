package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const TokenTTL = 8 * time.Hour

// Tokens issues access tokens containing only standard identity claims. Roles
// and company access are resolved from the database by Authentication.
type Tokens struct {
	secret   []byte
	issuer   string
	audience string
	now      func() time.Time
}

func NewTokens(secret, issuer, audience string) (*Tokens, error) {
	if len(secret) < 32 || strings.TrimSpace(secret) == "" {
		return nil, errors.New("JWT secret must contain at least 32 bytes")
	}
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return nil, errors.New("JWT issuer and audience are required")
	}
	return &Tokens{secret: []byte(secret), issuer: issuer, audience: audience, now: time.Now}, nil
}

func (t *Tokens) Issue(user User) (string, time.Time, error) {
	if !validSubject(user.ID) {
		return "", time.Time{}, ErrUnauthorized
	}
	now := t.now().UTC().Truncate(time.Second)
	expiresAt := now.Add(TokenTTL)
	claims := jwt.RegisteredClaims{
		Issuer:    t.issuer,
		Subject:   user.ID,
		Audience:  jwt.ClaimStrings{t.audience},
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		IssuedAt:  jwt.NewNumericDate(now),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, errors.New("cannot issue access token")
	}
	return token, expiresAt, nil
}

func (t *Tokens) Parse(encoded string) (string, error) {
	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(encoded, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, ErrUnauthorized
		}
		return t.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(), jwt.WithIssuer(t.issuer), jwt.WithAudience(t.audience),
		jwt.WithIssuedAt(), jwt.WithTimeFunc(t.now))
	if err != nil || token == nil || !token.Valid || claims.IssuedAt == nil || !validSubject(claims.Subject) {
		return "", ErrUnauthorized
	}
	return claims.Subject, nil
}

func validSubject(subject string) bool {
	id, err := uuid.Parse(subject)
	return err == nil && id != uuid.Nil && id.String() == subject
}
