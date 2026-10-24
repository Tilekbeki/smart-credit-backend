package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/review"
)

type reviewStub struct {
	record review.Record
	calls  int
	filter review.Filter
	err    error
}

func (s *reviewStub) Read(_ context.Context, scope *application.Scope, id string) (review.Record, error) {
	s.calls++
	if s.err != nil {
		return review.Record{}, s.err
	}
	a := s.record.Application
	if a.ApplicationID != id || (scope == nil && a.Status == application.Draft) || (scope != nil && (scope.UserID != a.UserID || scope.CompanyID != a.CompanyID)) {
		return review.Record{}, application.ErrNotFound
	}
	return s.record, nil
}
func (s *reviewStub) List(_ context.Context, f review.Filter) ([]review.Record, int64, error) {
	s.calls++
	s.filter = f
	return []review.Record{}, 0, s.err
}
func reviewHTTP(t *testing.T, repo *reviewStub, role auth.Role) (http.Handler, string) {
	t.Helper()
	u := demoUser()
	u.Role = role
	if role == auth.RoleBankEmployee {
		u.CompanyID = nil
	}
	tokens, err := auth.NewTokens(strings.Repeat("r", 32), "read-test", "read-test")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Issue(u)
	if err != nil {
		t.Fatal(err)
	}
	h := NewReviewHandler(review.NewService(repo), auth.NewMiddleware(tokens, &authRepoStub{user: u}, time.Second))
	return NewHandler(nil, testLogger(), time.Second, nil, h.Register), token
}
func readFixture() *reviewStub {
	return &reviewStub{record: review.Record{Application: application.Application{ApplicationID: testApplicationID, UserID: testUserID, CompanyID: ownCompanyID, Status: application.Draft, Version: 1}}}
}

func TestReadRoutesRequireExactRoles(t *testing.T) {
	for _, tc := range []struct {
		path    string
		allowed auth.Role
	}{
		{"/api/v1/applications/" + testApplicationID + "/decision", auth.RoleClient},
		{"/api/v1/bank/applications", auth.RoleBankEmployee},
		{"/api/v1/bank/applications/" + testApplicationID, auth.RoleBankEmployee},
		{"/api/v1/bank/applications/" + testApplicationID + "/assessment", auth.RoleBankEmployee},
	} {
		t.Run(tc.path, func(t *testing.T) {
			for _, role := range []auth.Role{auth.RoleClient, auth.RoleBankEmployee} {
				repo := readFixture()
				h, token := reviewHTTP(t, repo, role)
				if w := authRequest(h, "GET", tc.path, "", ""); w.Code != 401 {
					t.Fatal("anonymous access")
				}
				w := authRequest(h, "GET", tc.path, "", token)
				if role != tc.allowed {
					if w.Code != 403 || repo.calls != 0 {
						t.Fatalf("role bypass: %d calls=%d", w.Code, repo.calls)
					}
				} else if repo.calls != 1 {
					t.Fatal("allowed role did not reach storage")
				}
			}
		})
	}
}

func TestDecisionOwnDraftNullsOwnershipAndSafeErrors(t *testing.T) {
	repo := readFixture()
	h, token := reviewHTTP(t, repo, auth.RoleClient)
	w := authRequest(h, "GET", "/api/v1/applications/"+testApplicationID+"/decision", "", token)
	if w.Code != 200 {
		t.Fatalf("own decision: %d %s", w.Code, w.Body)
	}
	var v map[string]any
	if json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatal("invalid decision JSON")
	}
	if len(v) != 12 || v["status"] != "draft" {
		t.Fatal("CreditDecision shape")
	}
	for _, key := range []string{"decision", "score", "riskLevel", "assessedAt", "reviewedBy", "offer", "processing", "customerExplanation"} {
		value, exists := v[key]
		if !exists || value != nil {
			t.Fatalf("%s must explicitly be null", key)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cache enabled")
	}
	for _, change := range []string{"owner", "company"} {
		repo.record = readFixture().record
		if change == "owner" {
			repo.record.Application.UserID = testChallengeID
		} else {
			repo.record.Application.CompanyID = testChallengeID
		}
		w = authRequest(h, "GET", "/api/v1/applications/"+testApplicationID+"/decision", "", token)
		if w.Code != 404 || responseJSON(t, w)["error"] != "application_not_found" {
			t.Fatal("foreign application visible")
		}
	}
	repo.err = errors.New("postgres://private-host secret SQL lease-token")
	w = authRequest(h, "GET", "/api/v1/applications/"+testApplicationID+"/decision", "", token)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private-host") || strings.Contains(w.Body.String(), "lease-token") {
		t.Fatal("storage diagnostic leaked")
	}
}

func TestBankDraftAndStrictFilters(t *testing.T) {
	repo := readFixture()
	h, token := reviewHTTP(t, repo, auth.RoleBankEmployee)
	for _, suffix := range []string{"", "/assessment"} {
		w := authRequest(h, "GET", "/api/v1/bank/applications/"+testApplicationID+suffix, "", token)
		if w.Code != 404 || responseJSON(t, w)["error"] != "application_not_found" {
			t.Fatal("bank saw draft")
		}
	}
	for _, query := range []string{"status=draft", "status=garbage", "limit=0", "limit=101", "offset=-1", "offset=2147483648", "companyId=bad", "status=processing&status=approved", "unknown=x", "limit=1&limit=2", "companyId="} {
		before := repo.calls
		w := authRequest(h, "GET", "/api/v1/bank/applications?"+query, "", token)
		if w.Code != 400 || responseJSON(t, w)["error"] != "invalid_request" || repo.calls != before {
			t.Fatalf("filter accepted: %s %d %s", query, w.Code, w.Body)
		}
	}
	w := authRequest(h, "GET", "/api/v1/bank/applications?limit=3&offset=7&status=processing&companyId="+ownCompanyID, "", token)
	if w.Code != 200 || repo.filter.Limit != 3 || repo.filter.Offset != 7 || repo.filter.Status == nil || *repo.filter.Status != application.Processing || repo.filter.CompanyID == nil || *repo.filter.CompanyID != ownCompanyID {
		t.Fatalf("valid filter lost: %d %s", w.Code, w.Body)
	}
	for _, route := range []string{"/api/v1/applications/bad/decision", "/api/v1/bank/applications/bad", "/api/v1/bank/applications/bad/assessment"} {
		role := auth.RoleBankEmployee
		if strings.HasPrefix(route, "/api/v1/applications/") {
			role = auth.RoleClient
		}
		h, token := reviewHTTP(t, repo, role)
		if w := authRequest(h, "GET", route, "", token); w.Code != 400 {
			t.Fatal("bad UUID accepted")
		}
	}
}
