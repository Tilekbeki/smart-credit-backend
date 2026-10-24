package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type userContextKey struct{}

type userReader interface {
	UserByID(context.Context, string) (User, error)
}

type Middleware struct {
	tokens  *Tokens
	users   userReader
	timeout time.Duration
}

func NewMiddleware(tokens *Tokens, users userReader, timeout time.Duration) *Middleware {
	return &Middleware{tokens: tokens, users: users, timeout: timeout}
}

func (m *Middleware) Authentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			unauthorized(w)
			return
		}
		parts := strings.Fields(headers[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			unauthorized(w)
			return
		}
		subject, err := m.tokens.Parse(parts[1])
		if err != nil {
			unauthorized(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), m.timeout)
		user, err := m.users.UserByID(ctx, subject)
		cancel()
		if errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrUnauthorized) {
			unauthorized(w)
			return
		}
		if err != nil {
			writeAuthError(w, http.StatusServiceUnavailable, "service_unavailable", "Сервис временно недоступен")
			return
		}
		if user.ID != subject || (user.Role != RoleClient && user.Role != RoleBankEmployee) {
			unauthorized(w)
			return
		}
		ctx = context.WithValue(r.Context(), userContextKey{}, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (m *Middleware) RequireRole(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := CurrentUser(r.Context())
			if !ok {
				unauthorized(w)
				return
			}
			for _, role := range roles {
				if user.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeAuthError(w, http.StatusForbidden, "forbidden", "Недостаточно прав")
		})
	}
}

func CurrentUser(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(userContextKey{}).(User)
	return user, ok
}

// HasSingleCompanyContext is a deprecated display-context comparison, never an authorization
// check. Data repositories check current PostgreSQL membership for every operation.
func HasSingleCompanyContext(ctx context.Context, companyID string) bool {
	user, ok := CurrentUser(ctx)
	return ok && user.Role == RoleClient && user.CompanyID != nil && *user.CompanyID == companyID
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeAuthError(w, http.StatusUnauthorized, "unauthorized", "Требуется авторизация")
}

func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}
