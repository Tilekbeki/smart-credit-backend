package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type userReaderFunc func(context.Context, string) (User, error)

func (f userReaderFunc) UserByID(ctx context.Context, id string) (User, error) {
	return f(ctx, id)
}

func TestAuthenticationCurrentUser(t *testing.T) {
	tokens := testTokens(t)
	company := "10000000-0000-4000-8000-000000000001"
	user := User{ID: testUserID, Name: "Иван Лавкин", Phone: "+79990000001", Role: RoleClient, CompanyID: &company}
	encoded, _, err := tokens.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	var queried bool
	repo := userReaderFunc(func(ctx context.Context, id string) (User, error) {
		queried = true
		if id != user.ID {
			t.Fatalf("queried user %q", id)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("database query has no timeout")
		}
		return user, nil
	})
	mw := NewMiddleware(tokens, repo, time.Second)
	handler := mw.Authentication(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := CurrentUser(r.Context())
		if !ok || got.ID != user.ID || got.Role != RoleClient || got.CompanyID == nil || *got.CompanyID != company {
			t.Fatal("current database user missing from context")
		}
		// The database lookup timeout must not cancel the downstream handler.
		if err := r.Context().Err(); err != nil {
			t.Fatalf("downstream context canceled: %v", err)
		}
		if !HasSingleCompanyContext(r.Context(), company) || HasSingleCompanyContext(r.Context(), "another-company") {
			t.Fatal("company ownership is not scoped to the current client")
		}
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "bEaReR "+encoded)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !queried {
		t.Fatalf("authentication failed: status=%d queried=%v", w.Code, queried)
	}
}

func TestAuthenticationRejectsBadHeaders(t *testing.T) {
	tokens := testTokens(t)
	repo := userReaderFunc(func(context.Context, string) (User, error) {
		t.Fatal("database queried for malformed authorization")
		return User{}, nil
	})
	handler := NewMiddleware(tokens, repo, time.Second).Authentication(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unauthorized handler executed")
	}))
	for _, header := range []string{"", "Basic credentials", "Bearer", "Bearer invalid", "Bearer invalid extra", "Bearer one,Bearer two"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		assertAuthError(t, w, http.StatusUnauthorized, "unauthorized")
		if w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("missing bearer challenge")
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Add("Authorization", "Bearer one")
	r.Header.Add("Authorization", "Bearer two")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assertAuthError(t, w, http.StatusUnauthorized, "unauthorized")
}

func TestAuthenticationHandlesRepositoryFailures(t *testing.T) {
	tokens := testTokens(t)
	encoded, _, err := tokens.Issue(User{ID: testUserID})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		user User
		err  error
		code int
	}{
		{name: "deleted user", err: ErrUserNotFound, code: http.StatusUnauthorized},
		{name: "unauthorized user", err: ErrUnauthorized, code: http.StatusUnauthorized},
		{name: "database failure", err: errors.New("secret internal database details"), code: http.StatusServiceUnavailable},
		{name: "wrong database identity", user: User{ID: "another-id", Role: RoleClient}, code: http.StatusUnauthorized},
		{name: "unknown database role", user: User{ID: testUserID, Role: "ADMIN"}, code: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := userReaderFunc(func(context.Context, string) (User, error) { return tc.user, tc.err })
			handler := NewMiddleware(tokens, repo, time.Second).Authentication(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("handler executed after authentication failure")
			}))
			r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			r.Header.Set("Authorization", "Bearer "+encoded)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("status=%d want=%d", w.Code, tc.code)
			}
			if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), encoded) {
				t.Fatal("internal details leaked")
			}
		})
	}
}

func TestAuthenticationDatabaseTimeout(t *testing.T) {
	tokens := testTokens(t)
	encoded, _, err := tokens.Issue(User{ID: testUserID})
	if err != nil {
		t.Fatal(err)
	}
	repo := userReaderFunc(func(ctx context.Context, _ string) (User, error) {
		<-ctx.Done()
		return User{}, ctx.Err()
	})
	handler := NewMiddleware(tokens, repo, time.Millisecond).Authentication(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("timed out lookup authenticated")
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+encoded)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assertAuthError(t, w, http.StatusServiceUnavailable, "service_unavailable")
}

func TestRequireRole(t *testing.T) {
	mw := NewMiddleware(nil, nil, time.Second)
	for _, tc := range []struct {
		name    string
		user    *User
		allowed []Role
		status  int
	}{
		{"missing identity", nil, []Role{RoleClient}, http.StatusUnauthorized},
		{"client allowed", &User{Role: RoleClient}, []Role{RoleClient}, http.StatusOK},
		{"employee allowed", &User{Role: RoleBankEmployee}, []Role{RoleBankEmployee}, http.StatusOK},
		{"client denied", &User{Role: RoleClient}, []Role{RoleBankEmployee}, http.StatusForbidden},
		{"employee denied", &User{Role: RoleBankEmployee}, []Role{RoleClient}, http.StatusForbidden},
		{"either role", &User{Role: RoleBankEmployee}, []Role{RoleClient, RoleBankEmployee}, http.StatusOK},
		{"no roles allowed", &User{Role: RoleClient}, nil, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := mw.RequireRole(tc.allowed...)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			r := httptest.NewRequest(http.MethodGet, "/role-protected-test", nil)
			if tc.user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey{}, *tc.user))
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
		})
	}
}

func TestRolesComeFromDatabase(t *testing.T) {
	tokens := testTokens(t)
	encoded, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": testUserID, "iss": "smart-credit", "aud": "demo-frontend",
		"iat": tokens.now().Unix(), "exp": tokens.now().Add(time.Hour).Unix(),
		"role": "BANK_EMPLOYEE", "companyId": "another-company",
	}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	repo := userReaderFunc(func(context.Context, string) (User, error) {
		return User{ID: testUserID, Role: RoleClient}, nil
	})
	mw := NewMiddleware(tokens, repo, time.Second)
	handler := mw.Authentication(mw.RequireRole(RoleBankEmployee)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("token role overrode database role")
	})))
	r := httptest.NewRequest(http.MethodGet, "/role-protected-test", nil)
	r.Header.Set("Authorization", "Bearer "+encoded)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	assertAuthError(t, w, http.StatusForbidden, "forbidden")
}

func TestHasSingleCompanyContext(t *testing.T) {
	company := "company-id"
	if HasSingleCompanyContext(context.Background(), company) {
		t.Fatal("unauthenticated identity owns a company")
	}
	for _, user := range []User{{Role: RoleClient}, {Role: RoleBankEmployee, CompanyID: &company}} {
		ctx := context.WithValue(context.Background(), userContextKey{}, user)
		if HasSingleCompanyContext(ctx, company) {
			t.Fatal("unexpected ownership")
		}
	}
}

func assertAuthError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != status || body["error"] != code || body["message"] == "" {
		t.Fatalf("unexpected error: status=%d body=%v", w.Code, body)
	}
	if w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatal("missing authentication response headers")
	}
}
