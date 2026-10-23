package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testSecret = "test-only-secret-at-least-32-bytes-long"
	testUserID = "20000000-0000-4000-8000-000000000001"
)

func testTokens(t *testing.T) *Tokens {
	t.Helper()
	tokens, err := NewTokens(testSecret, "smart-credit", "demo-frontend")
	if err != nil {
		t.Fatal(err)
	}
	tokens.now = func() time.Time { return time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC) }
	return tokens
}

func TestNewTokensConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, secret, issuer, audience string }{
		{"short secret", "short", "issuer", "audience"},
		{"blank secret", strings.Repeat(" ", 32), "issuer", "audience"},
		{"empty issuer", testSecret, "", "audience"},
		{"blank issuer", testSecret, " \t", "audience"},
		{"empty audience", testSecret, "issuer", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewTokens(tc.secret, tc.issuer, tc.audience); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestIssueAndParseToken(t *testing.T) {
	tokens := testTokens(t)
	encoded, expiresAt, err := tokens.Issue(User{ID: testUserID, Role: RoleClient})
	if err != nil {
		t.Fatal(err)
	}
	if !expiresAt.Equal(tokens.now().Add(8 * time.Hour)) {
		t.Fatalf("unexpected token lifetime: %v", expiresAt.Sub(tokens.now()))
	}
	subject, err := tokens.Parse(encoded)
	if err != nil || subject != testUserID {
		t.Fatalf("parse failed: subject=%q error=%v", subject, err)
	}
	claims := jwt.MapClaims{}
	if _, _, err := new(jwt.Parser).ParseUnverified(encoded, claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != "smart-credit" || claims["sub"] != testUserID {
		t.Fatal("incorrect standard identity claims")
	}
	if _, present := claims["role"]; present {
		t.Fatal("role must come from the database")
	}
	if _, present := claims["companyId"]; present {
		t.Fatal("company access must come from the database")
	}
	tokens.now = func() time.Time { return expiresAt }
	if _, err := tokens.Parse(encoded); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestTokenRejectsInvalidClaimsAndSignatures(t *testing.T) {
	tokens := testTokens(t)
	validClaims := func() jwt.RegisteredClaims {
		return jwt.RegisteredClaims{
			Issuer: "smart-credit", Subject: testUserID, Audience: jwt.ClaimStrings{"demo-frontend"},
			IssuedAt: jwt.NewNumericDate(tokens.now()), ExpiresAt: jwt.NewNumericDate(tokens.now().Add(time.Hour)),
		}
	}
	cases := []struct {
		name   string
		change func(*jwt.RegisteredClaims)
		method jwt.SigningMethod
		key    any
	}{
		{name: "wrong signature", key: []byte("another-signing-key-at-least-32-bytes")},
		{name: "wrong algorithm", method: jwt.SigningMethodHS384},
		{name: "unsigned", method: jwt.SigningMethodNone, key: jwt.UnsafeAllowNoneSignatureType},
		{name: "expired", change: func(c *jwt.RegisteredClaims) { c.ExpiresAt = jwt.NewNumericDate(tokens.now().Add(-time.Second)) }},
		{name: "missing expiration", change: func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil }},
		{name: "missing issuer", change: func(c *jwt.RegisteredClaims) { c.Issuer = "" }},
		{name: "wrong issuer", change: func(c *jwt.RegisteredClaims) { c.Issuer = "other-service" }},
		{name: "missing audience", change: func(c *jwt.RegisteredClaims) { c.Audience = nil }},
		{name: "wrong audience", change: func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"other-app"} }},
		{name: "missing subject", change: func(c *jwt.RegisteredClaims) { c.Subject = "" }},
		{name: "invalid subject", change: func(c *jwt.RegisteredClaims) { c.Subject = "not-a-uuid" }},
		{name: "nil UUID", change: func(c *jwt.RegisteredClaims) { c.Subject = "00000000-0000-0000-0000-000000000000" }},
		{name: "missing issued at", change: func(c *jwt.RegisteredClaims) { c.IssuedAt = nil }},
		{name: "future issued at", change: func(c *jwt.RegisteredClaims) { c.IssuedAt = jwt.NewNumericDate(tokens.now().Add(time.Minute)) }},
		{name: "future not before", change: func(c *jwt.RegisteredClaims) { c.NotBefore = jwt.NewNumericDate(tokens.now().Add(time.Minute)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims()
			if tc.change != nil {
				tc.change(&claims)
			}
			method, key := tc.method, tc.key
			if method == nil {
				method = jwt.SigningMethodHS256
			}
			if key == nil {
				key = []byte(testSecret)
			}
			encoded, err := jwt.NewWithClaims(method, claims).SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			if subject, err := tokens.Parse(encoded); !errors.Is(err, ErrUnauthorized) || subject != "" {
				t.Fatalf("invalid token accepted: subject=%q error=%v", subject, err)
			}
		})
	}
	for _, encoded := range []string{"", "malformed", "a.b.c"} {
		if _, err := tokens.Parse(encoded); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("malformed token accepted")
		}
	}
}

func TestIssueRejectsInvalidSubject(t *testing.T) {
	if token, _, err := testTokens(t).Issue(User{ID: "invalid"}); !errors.Is(err, ErrUnauthorized) || token != "" {
		t.Fatal("invalid subject accepted for issuance")
	}
}
