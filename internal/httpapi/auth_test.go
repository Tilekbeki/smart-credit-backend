package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/auth"
)

const testUserID = "20000000-0000-4000-8000-000000000001"
const testChallengeID = "30000000-0000-4000-8000-000000000001"

type authRepoStub struct {
	user        auth.User
	profiles    []auth.Profile
	err         error
	requestedID string
	verifiedID  string
	matched     bool
}

func (s *authRepoStub) ListProfiles(context.Context) ([]auth.Profile, error) {
	return s.profiles, s.err
}
func (s *authRepoStub) RequestOTP(_ context.Context, id string) (auth.OTPChallenge, error) {
	s.requestedID = id
	return auth.OTPChallenge{ID: testChallengeID, ExpiresAt: time.Now().UTC().Add(5 * time.Minute), Phone: s.user.Phone}, s.err
}
func (s *authRepoStub) VerifyOTP(_ context.Context, id string, matched bool) (auth.User, error) {
	s.verifiedID = id
	s.matched = matched
	return s.user, s.err
}
func (s *authRepoStub) UserByID(_ context.Context, id string) (auth.User, error) {
	if s.err != nil {
		return auth.User{}, s.err
	}
	if id != s.user.ID {
		return auth.User{}, auth.ErrUserNotFound
	}
	return s.user, nil
}

func authTestHandler(t *testing.T, repo *authRepoStub) (http.Handler, *auth.Tokens, *bytes.Buffer) {
	t.Helper()
	tokens, err := auth.NewTokens(strings.Repeat("s", 32), "test-issuer", "test-audience")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	return NewHandler(nil, logger, time.Second, NewAuthHandler(repo, tokens, "1234", time.Second, logger)), tokens, &logs
}

func demoUser() auth.User {
	company := "10000000-0000-4000-8000-000000000001"
	return auth.User{ID: testUserID, Name: "Иван Лавкин", Phone: "+79990000001", Role: auth.RoleClient, CompanyID: &company}
}

func authRequest(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestDemoProfilesUseRepository(t *testing.T) {
	company := "Учебная компания"
	alfaAccount := "alfa_account"
	externalAccount := "external_account"
	repo := &authRepoStub{profiles: []auth.Profile{
		{ID: testUserID, Name: "Первый", Phone: "+79990000001", Role: auth.RoleClient, CompanyName: &company, BankRelationship: &alfaAccount},
		{ID: "20000000-0000-4000-8000-000000000002", Name: "Второй", Phone: "+79990000002", Role: auth.RoleClient, CompanyName: &company, BankRelationship: &externalAccount},
		{ID: "20000000-0000-4000-8000-000000000003", Name: "Третий", Phone: "+79990000003", Role: auth.RoleClient, CompanyName: &company, BankRelationship: &externalAccount},
		{ID: "20000000-0000-4000-8000-000000000004", Name: "Сотрудник", Phone: "+79990000004", Role: auth.RoleBankEmployee},
	}}
	h, _, _ := authTestHandler(t, repo)
	w := authRequest(h, http.MethodGet, "/api/v1/demo/profiles", "", "")
	var result struct {
		Items []auth.Profile `json:"items"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Items) != 4 {
		t.Fatal("profiles did not return four database users")
	}
	if result.Items[0].CompanyName == nil || result.Items[3].CompanyName != nil || result.Items[3].Role != auth.RoleBankEmployee {
		t.Fatal("invalid profile company or role")
	}
	if result.Items[0].BankRelationship == nil || *result.Items[0].BankRelationship != alfaAccount ||
		result.Items[1].BankRelationship == nil || *result.Items[1].BankRelationship != externalAccount ||
		result.Items[2].BankRelationship == nil || *result.Items[2].BankRelationship != externalAccount ||
		result.Items[3].BankRelationship != nil {
		t.Fatal("demo profiles must expose the seeded account journeys")
	}
}

func TestOTPRequestReturnsMaskedPhoneWithoutCode(t *testing.T) {
	repo := &authRepoStub{user: demoUser()}
	h, _, logs := authTestHandler(t, repo)
	w := authRequest(h, http.MethodPost, "/api/v1/auth/otp/request", `{"profileId":"`+testUserID+`"}`, "")
	var result struct {
		ID          string    `json:"otpRequestId"`
		ExpiresAt   time.Time `json:"expiresAt"`
		MaskedPhone string    `json:"maskedPhone"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("OTP request failed")
	}
	if result.ID != testChallengeID || result.MaskedPhone != "+7*** *** 0001" || repo.requestedID != testUserID {
		t.Fatal("incorrect OTP response")
	}
	if remaining := time.Until(result.ExpiresAt); remaining < 4*time.Minute || remaining > 5*time.Minute {
		t.Fatal("invalid expiry")
	}
	for _, secret := range []string{"1234", repo.user.Phone} {
		if strings.Contains(w.Body.String()+logs.String(), secret) {
			t.Fatal("OTP or unmasked phone leaked")
		}
	}
}

func TestOTPVerifyIssuesJWTAndMeLoadsCurrentDatabaseRole(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleClient, auth.RoleBankEmployee} {
		t.Run(string(role), func(t *testing.T) {
			repo := &authRepoStub{user: demoUser()}
			repo.user.Role = role
			if role == auth.RoleBankEmployee {
				repo.user.CompanyID = nil
			}
			h, tokens, logs := authTestHandler(t, repo)
			w := authRequest(h, http.MethodPost, "/api/v1/auth/otp/verify", `{"otpRequestId":"`+testChallengeID+`","code":"1234"}`, "")
			var result struct {
				AccessToken string    `json:"accessToken"`
				TokenType   string    `json:"tokenType"`
				ExpiresAt   time.Time `json:"expiresAt"`
				UserID      string    `json:"userId"`
				Role        auth.Role `json:"role"`
				CompanyID   *string   `json:"companyId"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.AccessToken == "" {
				t.Fatal("verification did not return token")
			}
			if result.TokenType != "Bearer" || result.Role != role || result.UserID != testUserID || repo.verifiedID != testChallengeID || !repo.matched {
				t.Fatal("wrong authentication result")
			}
			if (result.CompanyID == nil) != (role == auth.RoleBankEmployee) {
				t.Fatal("wrong company nullability")
			}
			if ttl := time.Until(result.ExpiresAt); ttl < 8*time.Hour-2*time.Second || ttl > 8*time.Hour {
				t.Fatal("JWT should expire in eight hours")
			}
			if sub, err := tokens.Parse(result.AccessToken); err != nil || sub != testUserID {
				t.Fatal("issued JWT could not be verified")
			}
			if strings.Contains(logs.String(), result.AccessToken) || strings.Contains(logs.String(), "1234") {
				t.Fatal("secrets leaked to logs")
			}
			// Role and company must come from current DB data, even for an older JWT.
			repo.user.Role = auth.RoleBankEmployee
			repo.user.CompanyID = nil
			me := authRequest(h, http.MethodGet, "/api/v1/me", "", result.AccessToken)
			var user auth.User
			if me.Code != http.StatusOK || json.Unmarshal(me.Body.Bytes(), &user) != nil || user.ID != testUserID || user.Role != auth.RoleBankEmployee || user.CompanyID != nil || user.Phone != repo.user.Phone {
				t.Fatal("me did not return current user")
			}
		})
	}
}

func TestOTPErrorMappingAndWrongCodeComparison(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{auth.ErrUserNotFound, 404, "profile_not_found"},
		{auth.ErrChallengeNotFound, 404, "otp_not_found"},
		{auth.ErrOTPInvalid, 400, "invalid_otp"},
		{auth.ErrOTPExpired, 410, "otp_expired"},
		{auth.ErrOTPUsed, 409, "otp_used"},
		{auth.ErrOTPInvalidated, 409, "otp_invalidated"},
		{auth.ErrOTPLimit, 429, "otp_attempts_exceeded"},
		{errors.New("private database details"), 503, "service_unavailable"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			repo := &authRepoStub{user: demoUser(), err: tc.err}
			h, _, logs := authTestHandler(t, repo)
			w := authRequest(h, http.MethodPost, "/api/v1/auth/otp/verify", `{"otpRequestId":"`+testChallengeID+`","code":"0000"}`, "")
			result := responseJSON(t, w)
			if w.Code != tc.status || result["error"] != tc.code || result["message"] == "" || repo.matched {
				t.Fatal("wrong domain error mapping or code comparison")
			}
			if strings.Contains(w.Body.String()+logs.String(), "private database") {
				t.Fatal("database error leaked")
			}
		})
	}
}

func TestAuthEndpointsRejectInvalidJSONAndRoleInjection(t *testing.T) {
	repo := &authRepoStub{user: demoUser()}
	h, _, _ := authTestHandler(t, repo)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{`, 400}, {`null`, 400}, {`[]`, 400}, {`{"profileId":123}`, 400},
		{`{"profileId":"';DROP TABLE users;--"}`, 400},
		{`{"profileId":"00000000-0000-0000-0000-000000000000"}`, 400},
		{`{"profileId":"` + testUserID + `","role":"BANK_EMPLOYEE"}`, 400},
		{`{"profileId":"` + testUserID + `"} {}`, 400},
		{`{"profileId":"` + strings.Repeat("x", 65536) + `"}`, 413},
		{`{"profileId":"` + testUserID + `"}` + strings.Repeat(" ", 65536), 413},
	} {
		w := authRequest(h, http.MethodPost, "/api/v1/auth/otp/request", tc.body, "")
		if w.Code != tc.status {
			t.Fatalf("invalid request returned status %d, wanted %d", w.Code, tc.status)
		}
	}
	if repo.requestedID != "" {
		t.Fatal("malformed requests reached repository")
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatal("non-JSON request accepted")
	}
}

func TestMeRequiresAuthentication(t *testing.T) {
	h, _, _ := authTestHandler(t, &authRepoStub{user: demoUser()})
	for _, token := range []string{"", "not-a-token"} {
		w := authRequest(h, http.MethodGet, "/api/v1/me", "", token)
		if w.Code != http.StatusUnauthorized || responseJSON(t, w)["message"] == "" {
			t.Fatal("unauthenticated access was allowed")
		}
	}
}
