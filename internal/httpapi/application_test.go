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
)

const testApplicationID = "40000000-0000-4000-8000-000000000001"

type applicationRepoStub struct {
	record application.Application
	err    error
	writes int
	filter application.ListFilter
}

func (s *applicationRepoStub) Create(_ context.Context, scope application.Scope, f application.Fields) (application.Application, error) {
	s.writes++
	s.record = application.Application{ApplicationID: testApplicationID, UserID: scope.UserID, CompanyID: scope.CompanyID, Fields: f, Status: application.Draft, Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), AvailableActions: []string{"edit", "submit"}}
	return s.record, s.err
}
func (s *applicationRepoStub) Get(_ context.Context, scope application.Scope, id string) (application.Application, error) {
	if s.err != nil {
		return application.Application{}, s.err
	}
	if s.record.ApplicationID != id || scope.UserID != s.record.UserID || scope.CompanyID != s.record.CompanyID {
		return application.Application{}, application.ErrNotFound
	}
	return s.record, nil
}
func (s *applicationRepoStub) List(_ context.Context, _ application.Scope, f application.ListFilter) (application.Page, error) {
	s.filter = f
	return application.Page{Items: []application.Summary{}, Total: 0, Limit: f.Limit, Offset: f.Offset}, s.err
}
func (s *applicationRepoStub) UpdateDraft(_ context.Context, _ application.Scope, _ string, v int64, f application.Fields) (application.Application, error) {
	s.writes++
	s.record.Fields = f
	s.record.Version = v + 1
	return s.record, s.err
}
func (s *applicationRepoStub) Timeline(ctx context.Context, scope application.Scope, id string) ([]application.Event, error) {
	if _, err := s.Get(ctx, scope, id); err != nil {
		return nil, err
	}
	return []application.Event{{EventID: testChallengeID, EventType: "draft_created", Sequence: 1, Status: application.Draft, CreatedAt: s.record.CreatedAt, ActorID: &s.record.UserID, Title: "Черновик создан"}}, nil
}

func applicationTestHandler(t *testing.T, repo *applicationRepoStub, role auth.Role) (http.Handler, string) {
	t.Helper()
	u := demoUser()
	u.Role = role
	if role == auth.RoleBankEmployee {
		u.CompanyID = nil
	}
	tokens, err := auth.NewTokens(strings.Repeat("a", 32), "applications-test", "applications-test")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Issue(u)
	if err != nil {
		t.Fatal(err)
	}
	catalog := creditFixture()
	h := NewApplicationHandler(application.NewService(repo, catalog, catalog), auth.NewMiddleware(tokens, &authRepoStub{user: u}, time.Second))
	return NewHandler(nil, testLogger(), time.Second, nil, h.Register), token
}

func draftHTTPFixture() *applicationRepoStub {
	return &applicationRepoStub{record: application.Application{ApplicationID: testApplicationID, UserID: testUserID, CompanyID: ownCompanyID, Status: application.Draft, Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), AvailableActions: []string{"edit", "submit"}}}
}

func TestApplicationHTTPDraftFlow(t *testing.T) {
	repo := draftHTTPFixture()
	h, token := applicationTestHandler(t, repo, auth.RoleClient)
	w := authRequest(h, "POST", "/api/v1/applications", `{}`, token)
	if w.Code != 201 || w.Header().Get("Location") != "/api/v1/applications/"+testApplicationID {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var a application.Application
	if json.Unmarshal(w.Body.Bytes(), &a) != nil || a.ProductID != nil || a.UserID != testUserID || a.CompanyID != ownCompanyID || a.Version != 1 || a.Status != application.Draft {
		t.Fatal("partial draft response")
	}
	w = authRequest(h, "GET", "/api/v1/applications/"+testApplicationID, "", token)
	var detail application.Detail
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Company.ID != ownCompanyID || detail.Product != nil || len(detail.AvailableActions) != 2 || detail.AvailableActions[0] != "edit" {
		t.Fatal("get own draft details")
	}
	w = authRequest(h, "PATCH", "/api/v1/applications/"+testApplicationID, `{"version":1,"productId":"turnover","purpose":"stock","requestedAmountRub":150000,"monthlyRevenueRub":0}`, token)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.Version != 2 || a.ProductID == nil || *a.RequestedAmountRub != 150000 {
		t.Fatalf("patch:%d %s", w.Code, w.Body)
	}
	w = authRequest(h, "PATCH", "/api/v1/applications/"+testApplicationID, `{"version":2,"productId":null,"requestedAmountRub":null}`, token)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.Version != 3 || a.ProductID != nil || a.RequestedAmountRub != nil || *a.Purpose != "stock" || *a.MonthlyRevenueRub != 0 {
		t.Fatal("patch null vs absent")
	}
	w = authRequest(h, "PATCH", "/api/v1/applications/"+testApplicationID, `{"version":2,"purpose":null}`, token)
	if w.Code != 409 || responseJSON(t, w)["error"] != "version_conflict" {
		t.Fatal("stale patch accepted")
	}
	w = authRequest(h, "GET", "/api/v1/applications/"+testApplicationID+"/timeline", "", token)
	var events struct {
		Items []application.Event `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &events) != nil || len(events.Items) != 1 || events.Items[0].ActorID == nil || *events.Items[0].ActorID != testUserID {
		t.Fatal("timeline response")
	}
	w = authRequest(h, "GET", "/api/v1/applications?limit=5&offset=2&status=draft", "", token)
	if w.Code != 200 || repo.filter.Limit != 5 || repo.filter.Offset != 2 || repo.filter.Status == nil || *repo.filter.Status != application.Draft {
		t.Fatal("pagination parameters lost")
	}
	w = authRequest(h, "POST", "/api/v1/applications/"+testApplicationID+"/submit", `{}`, token)
	if w.Code != 404 {
		t.Fatal("submit must remain unavailable")
	}
}

func TestApplicationHTTPRequiresClientForEveryRoute(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{{"POST", "/api/v1/applications", `{}`}, {"GET", "/api/v1/applications", ""}, {"GET", "/api/v1/applications/" + testApplicationID, ""}, {"PATCH", "/api/v1/applications/" + testApplicationID, `{"version":1,"purpose":null}`}, {"GET", "/api/v1/applications/" + testApplicationID + "/timeline", ""}} {
		t.Run(route.method+route.path, func(t *testing.T) {
			r := draftHTTPFixture()
			h, token := applicationTestHandler(t, r, auth.RoleBankEmployee)
			if w := authRequest(h, route.method, route.path, route.body, ""); w.Code != 401 {
				t.Fatal("missing JWT accepted")
			}
			if w := authRequest(h, route.method, route.path, route.body, token); w.Code != 403 {
				t.Fatal("employee accepted")
			}
			if r.writes != 0 {
				t.Fatal("unauthorized write")
			}
		})
	}
}

func TestApplicationHTTPForeignMissingAndInvalidID(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		r := draftHTTPFixture()
		if foreign {
			r.record.UserID = "20000000-0000-4000-8000-000000000002"
			r.record.CompanyID = otherCompanyID
		} else {
			r.err = application.ErrNotFound
		}
		h, token := applicationTestHandler(t, r, auth.RoleClient)
		for _, route := range []struct{ method, suffix, body string }{{"GET", "", ""}, {"PATCH", "", `{"version":1,"purpose":"stock"}`}, {"GET", "/timeline", ""}} {
			w := authRequest(h, route.method, "/api/v1/applications/"+testApplicationID+route.suffix, route.body, token)
			if w.Code != 404 || responseJSON(t, w)["error"] != "application_not_found" || strings.Contains(w.Body.String(), otherCompanyID) {
				t.Fatal("foreign/missing leak")
			}
			w = authRequest(h, route.method, "/api/v1/applications/not-a-uuid"+route.suffix, route.body, token)
			if w.Code != 400 {
				t.Fatal("malformed UUID accepted")
			}
		}
	}
}

func TestApplicationHTTPStrictJSONAndImmutableFields(t *testing.T) {
	for _, tc := range []struct{ method, body string }{
		{"POST", `null`}, {"POST", `[]`}, {"POST", `{} {}`}, {"POST", `{"termMonths":"12"}`}, {"POST", `{"requestedAmountRub":1.5}`},
		{"PATCH", `{}`}, {"PATCH", `null`}, {"PATCH", `{"version":null,"purpose":null}`}, {"PATCH", `{"version":1}`}, {"PATCH", `{"version":0,"purpose":null}`}, {"PATCH", `{"version":1.5,"purpose":null}`},
	} {
		t.Run(tc.method+tc.body, func(t *testing.T) {
			r := draftHTTPFixture()
			h, token := applicationTestHandler(t, r, auth.RoleClient)
			path := "/api/v1/applications"
			if tc.method == "PATCH" {
				path += "/" + testApplicationID
			}
			w := authRequest(h, tc.method, path, tc.body, token)
			if w.Code != 400 || r.writes != 0 {
				t.Fatalf("bad input accepted %d", w.Code)
			}
		})
	}
	for _, field := range []string{"applicationId", "userId", "companyId", "status", "scoring", "decision", "createdAt", "submittedAt", "unknown"} {
		for _, method := range []string{"POST", "PATCH"} {
			r := draftHTTPFixture()
			h, token := applicationTestHandler(t, r, auth.RoleClient)
			path := "/api/v1/applications"
			body := `{"` + field + `":"injected"}`
			if method == "PATCH" {
				path += "/" + testApplicationID
				body = `{"version":1,"purpose":null,"` + field + `":"injected"}`
			}
			w := authRequest(h, method, path, body, token)
			if w.Code != 400 || r.writes != 0 {
				t.Fatalf("immutable %s accepted", field)
			}
		}
	}
}

func TestApplicationHTTPInvalidFiltersAndErrors(t *testing.T) {
	r := draftHTTPFixture()
	h, token := applicationTestHandler(t, r, auth.RoleClient)
	for _, query := range []string{"limit=0", "limit=101", "limit=1.5", "offset=-1", "offset=2147483648", "status=confirmed", "status=processing_failed", "status=", "status=draft&status=draft", "limit=1&limit=2", "extra=1", "status=%ZZ", "offset=%ZZ", "limit=1;offset=1"} {
		if w := authRequest(h, "GET", "/api/v1/applications?"+query, "", token); w.Code != 400 {
			t.Fatalf("query accepted %s", query)
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{application.ErrStorage, 503, "service_unavailable"}, {application.ErrProductNotFound, 404, "product_not_found"}, {application.ErrVersionConflict, 409, "version_conflict"}, {application.ErrInvalidStatus, 409, "invalid_status"}, {errors.New("database password"), 503, "service_unavailable"}} {
		r.err = tc.err
		w := authRequest(h, "GET", "/api/v1/applications/"+testApplicationID, "", token)
		if w.Code != tc.status || responseJSON(t, w)["error"] != tc.code || strings.Contains(w.Body.String(), "password") {
			t.Fatal("error mapping/leak")
		}
	}
}
