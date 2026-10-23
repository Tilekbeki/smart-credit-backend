package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/document"
	"smart-credit-backend/internal/pipeline"
)

type submitRepoStub struct {
	record    application.Application
	err       error
	submitErr error
	submits   int
	docs      int
}

func (s *submitRepoStub) Get(_ context.Context, scope application.Scope, id string) (application.Application, error) {
	if s.err != nil {
		return application.Application{}, s.err
	}
	if scope.UserID != testUserID || scope.CompanyID != ownCompanyID || id != testApplicationID {
		return application.Application{}, application.ErrNotFound
	}
	return s.record, nil
}
func (s *submitRepoStub) List(context.Context, document.Scope, string) ([]document.Metadata, error) {
	s.docs++
	return []document.Metadata{}, nil
}
func (s *submitRepoStub) Submit(_ context.Context, _ application.Scope, id string, _ int64, _ []string) (pipeline.SubmitResult, error) {
	s.submits++
	return pipeline.SubmitResult{ApplicationID: id, Status: application.Submitted, SubmittedAt: time.Now().UTC(), NextStage: "prechecks"}, s.submitErr
}
func submitHTTPFixture(t *testing.T, role auth.Role) (http.Handler, *submitRepoStub, string) {
	t.Helper()
	u := demoUser()
	u.Role = role
	if role == auth.RoleBankEmployee {
		u.CompanyID = nil
	}
	repo := &submitRepoStub{record: application.Application{Status: application.Draft, Version: 1}}
	tokens, err := auth.NewTokens(strings.Repeat("p", 32), "submit-test", "submit-test")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Issue(u)
	if err != nil {
		t.Fatal(err)
	}
	h := NewSubmissionHandler(pipeline.NewSubmissionService(repo, repo, repo), auth.NewMiddleware(tokens, &authRepoStub{user: u}, time.Second))
	return NewHandler(nil, testLogger(), time.Second, nil, h.Register), repo, token
}
func completeHTTPFields() application.Fields {
	id, purpose := "turnover", "stock"
	amount, revenue, expenses := int64(150000), int64(0), int64(1)
	term := 12
	return application.Fields{ProductID: &id, Purpose: &purpose, RequestedAmountRub: &amount, TermMonths: &term, MonthlyRevenueRub: &revenue, MonthlyExpensesRub: &expenses}
}

func submissionResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSubmitHTTPAcceptedAndValidation(t *testing.T) {
	h, repo, token := submitHTTPFixture(t, auth.RoleClient)
	path := "/api/v1/applications/" + testApplicationID + "/submit"
	w := authRequest(h, "POST", path, "", token)
	result := submissionResponse(t, w)
	if w.Code != 422 || result["error"] != "validation_failed" || len(result["details"].([]any)) != 6 || repo.submits != 0 {
		t.Fatalf("incomplete response: %d %s", w.Code, w.Body)
	}
	repo.record.Fields = completeHTTPFields()
	w = authRequest(h, "POST", path, "", token)
	result = submissionResponse(t, w)
	if w.Code != 202 || result["applicationId"] != testApplicationID || result["status"] != "submitted" || result["nextStage"] != "prechecks" || repo.submits != 1 {
		t.Fatal("submit response")
	}
	repo.submitErr = &pipeline.ValidationError{Details: []pipeline.FieldError{{Field: "documents.EQUIPMENT_INVOICE", Code: "required_document_missing", Message: "Загрузите счёт."}}}
	w = authRequest(h, "POST", path, "", token)
	result = submissionResponse(t, w)
	if w.Code != 422 || result["details"].([]any)[0].(map[string]any)["field"] != "documents.EQUIPMENT_INVOICE" {
		t.Fatal("specific validation detail lost")
	}
}

func TestSubmitHTTPAuthorizationBodyAndErrors(t *testing.T) {
	path := "/api/v1/applications/" + testApplicationID + "/submit"
	h, repo, token := submitHTTPFixture(t, auth.RoleBankEmployee)
	if w := authRequest(h, "POST", path, "", ""); w.Code != 401 {
		t.Fatal("JWT required")
	}
	if w := authRequest(h, "POST", path, "", token); w.Code != 403 || repo.submits != 0 {
		t.Fatal("employee submitted application")
	}
	h, repo, token = submitHTTPFixture(t, auth.RoleClient)
	repo.record.Fields = completeHTTPFields()
	for _, body := range []string{`{}`, `{"role":"BANK_EMPLOYEE"}`, " ", strings.Repeat("x", 1<<20)} {
		if w := authRequest(h, "POST", path, body, token); w.Code != 400 || repo.submits != 0 {
			t.Fatal("body should not be accepted")
		}
	}
	if w := authRequest(h, "POST", "/api/v1/applications/not-a-uuid/submit", "", token); w.Code != 400 {
		t.Fatal("bad UUID accepted")
	}
	if w := authRequest(h, "POST", "/api/v1/applications/ffffffff-ffff-4fff-8fff-ffffffffffff/submit", "", token); w.Code != 404 {
		t.Fatal("missing/foreign exposed")
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{application.ErrInvalidStatus, 409, "invalid_status"}, {application.ErrVersionConflict, 409, "version_conflict"}, {application.ErrNotFound, 404, "application_not_found"}, {pipeline.ErrStorage, 503, "service_unavailable"}, {errors.New("DSN secret"), 503, "service_unavailable"}} {
		repo.submitErr = tc.err
		w := authRequest(h, "POST", path, "", token)
		if w.Code != tc.status || responseJSON(t, w)["error"] != tc.code || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("error mapping:%d %s", w.Code, w.Body)
		}
	}
}
