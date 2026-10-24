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
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/lending"
)

const lendingKey = "70000000-0000-4000-8000-00000000000a"
const lendingOfferID = "60000000-0000-4000-8000-00000000000a"

type lendingStub struct {
	command  lending.Command
	calls    int
	err      error
	response lending.Response
}

func (s *lendingStub) Execute(_ context.Context, c lending.Command) (lending.Response, error) {
	s.calls++
	s.command = c
	return s.response, s.err
}

func lendingHTTP(t *testing.T, repo *lendingStub, role auth.Role) (http.Handler, string) {
	t.Helper()
	u := demoUser()
	u.Role = role
	if role == auth.RoleBankEmployee {
		u.CompanyID = nil
	}
	tokens, err := auth.NewTokens(strings.Repeat("l", 32), "lending-test", "lending-test")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Issue(u)
	if err != nil {
		t.Fatal(err)
	}
	h := NewLendingHandler(lending.NewService(repo), auth.NewMiddleware(tokens, &authRepoStub{user: u}, time.Second))
	return NewHandler(nil, testLogger(), time.Second, nil, h.Register), token
}

func lendingRequest(h http.Handler, path, body, token string, keys ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for _, key := range keys {
		r.Header.Add("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func manualBody() string {
	return `{"version":4,"decision":"approved","comment":"  Подтверждено.  ","offer":{"amountRub":1000000,"termMonths":12}}`
}
func actionBody() string { return `{"version":5,"offerId":"` + lendingOfferID + `"}` }
func lendingFixture(status int) *lendingStub {
	return &lendingStub{response: lending.Response{Status: status, Body: json.RawMessage(`{"applicationId":"` + testApplicationID + `","status":"approved","version":5,"decision":"approved","score":62,"riskLevel":"MEDIUM","reasons":["Условия подтверждены сотрудником банка."],"assessedAt":"2026-10-24T10:00:00Z","reviewedBy":null,"offer":null,"processing":null}`)}}
}

func lendingResponseJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	return body
}

func TestLendingRoutesSuccessfulCommandsAndExactReplay(t *testing.T) {
	for _, tc := range []struct {
		name, operation, path, body string
		role                        auth.Role
		status                      int
	}{
		{"manual", lending.Manual, "/api/v1/bank/applications/" + testApplicationID + "/decisions", manualBody(), auth.RoleBankEmployee, 201},
		{"accept", lending.Accept, "/api/v1/applications/" + testApplicationID + "/accept", actionBody(), auth.RoleClient, 200},
		{"decline", lending.Decline, "/api/v1/applications/" + testApplicationID + "/decline", actionBody(), auth.RoleClient, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := lendingFixture(tc.status)
			h, token := lendingHTTP(t, repo, tc.role)
			for i := 0; i < 2; i++ {
				w := lendingRequest(h, tc.path, tc.body, token, lendingKey)
				if w.Code != tc.status || w.Body.String() != string(repo.response.Body) || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
					t.Fatalf("status=%d body=%s", w.Code, w.Body)
				}
				if repo.command.ActorID != testUserID || repo.command.ApplicationID != testApplicationID || repo.command.Key != lendingKey || repo.command.Operation != tc.operation || len(repo.command.RequestHash) != 64 {
					t.Fatalf("identity not from authentication/path: %+v", repo.command)
				}
				if tc.operation == lending.Manual {
					if repo.command.Comment != "Подтверждено." || repo.command.CompanyID != "" || repo.command.Offer.AmountRub != 1_000_000 {
						t.Fatal("manual command mismatch")
					}
				} else if repo.command.CompanyID != ownCompanyID || repo.command.OfferID != lendingOfferID {
					t.Fatal("ownership scope missing")
				}
			}
		})
	}
}

func TestLendingRoutesRequireAuthenticationAndExactRoles(t *testing.T) {
	for _, tc := range []struct {
		path, body string
		role       auth.Role
	}{
		{"/api/v1/bank/applications/" + testApplicationID + "/decisions", manualBody(), auth.RoleBankEmployee},
		{"/api/v1/applications/" + testApplicationID + "/accept", actionBody(), auth.RoleClient},
		{"/api/v1/applications/" + testApplicationID + "/decline", actionBody(), auth.RoleClient},
	} {
		for _, role := range []auth.Role{auth.RoleClient, auth.RoleBankEmployee} {
			repo := lendingFixture(200)
			h, token := lendingHTTP(t, repo, role)
			if w := lendingRequest(h, tc.path, tc.body, "", lendingKey); w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("anonymous access")
			}
			parts := strings.Split(token, ".")
			parts[2] = "A" + parts[2][1:]
			if parts[2] == strings.Split(token, ".")[2] {
				parts[2] = "B" + parts[2][1:]
			}
			if w := lendingRequest(h, tc.path, tc.body, strings.Join(parts, "."), lendingKey); w.Code != 401 {
				t.Fatal("tampered signature accepted")
			}
			w := lendingRequest(h, tc.path, tc.body, token, lendingKey)
			if role != tc.role {
				if w.Code != 403 || repo.calls != 0 {
					t.Fatalf("role bypass %d calls=%d", w.Code, repo.calls)
				}
			} else if repo.calls != 1 {
				t.Fatal("authorized role blocked")
			}
		}
	}
}

func TestLendingStrictJSONAndHeaderValidation(t *testing.T) {
	manualPath := "/api/v1/bank/applications/" + testApplicationID + "/decisions"
	actionPath := "/api/v1/applications/" + testApplicationID + "/accept"
	for _, tc := range []struct {
		name, body string
		manual     bool
	}{
		{"null", "null", true}, {"array", "[]", true}, {"empty", "{}", true}, {"malformed", "{", true},
		{"missing offer", `{"version":4,"decision":"rejected","comment":"Отклонено."}`, true},
		{"missing version", `{"decision":"rejected","comment":"Отклонено.","offer":null}`, true},
		{"null comment", `{"version":4,"decision":"rejected","comment":null,"offer":null}`, true},
		{"null version", `{"version":null,"decision":"rejected","comment":"Отклонено.","offer":null}`, true},
		{"null decision", `{"version":4,"decision":null,"comment":"Отклонено.","offer":null}`, true},
		{"duplicate key", strings.Replace(manualBody(), `"version":4`, `"version":4,"version":5`, 1), true},
		{"duplicate nested", strings.Replace(manualBody(), `"amountRub":1000000`, `"amountRub":1000000,"amountRub":800000`, 1), true},
		{"missing amount", strings.Replace(manualBody(), `"amountRub":1000000,`, "", 1), true},
		{"null amount", strings.Replace(manualBody(), `"amountRub":1000000`, `"amountRub":null`, 1), true},
		{"missing term", strings.Replace(manualBody(), `,"termMonths":12`, "", 1), true},
		{"fraction amount", strings.Replace(manualBody(), `1000000`, `1.5`, 1), true},
		{"unknown nested", strings.Replace(manualBody(), `"termMonths":12`, `"termMonths":12,"annualRatePercent":1`, 1), true},
		{"injected reviewer", strings.Replace(manualBody(), `"version":4`, `"version":4,"reviewedBy":"`+testUserID+`"`, 1), true},
		{"unknown decision", strings.Replace(manualBody(), `"approved"`, `"manual_review"`, 1), true},
		{"approved null", strings.Replace(manualBody(), `{"amountRub":1000000,"termMonths":12}`, `null`, 1), true},
		{"rejected proposal", strings.Replace(manualBody(), `"approved"`, `"rejected"`, 1), true},
		{"zero version", strings.Replace(manualBody(), `"version":4`, `"version":0`, 1), true},
		{"trailing JSON", manualBody() + " {}", true},
		{"action empty", "{}", false}, {"null offer id", `{"version":5,"offerId":null}`, false},
		{"bad offer id", `{"version":5,"offerId":"bad"}`, false},
		{"action duplicate", strings.Replace(actionBody(), `"version":5`, `"version":5,"version":5`, 1), false},
		{"action injection", strings.Replace(actionBody(), `"version":5`, `"version":5,"actorId":"`+testUserID+`"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := lendingFixture(200)
			role := auth.RoleClient
			path := actionPath
			if tc.manual {
				role = auth.RoleBankEmployee
				path = manualPath
			}
			h, token := lendingHTTP(t, repo, role)
			w := lendingRequest(h, path, tc.body, token, lendingKey)
			if w.Code != 400 || repo.calls != 0 || responseJSON(t, w)["error"] != "invalid_request" {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, repo.calls, w.Body)
			}
		})
	}
	for _, keys := range [][]string{nil, {""}, {"bad"}, {"00000000-0000-0000-0000-000000000000"}, {lendingKey, lendingKey}, {lendingKey + ", " + lendingKey}, {" " + lendingKey}} {
		repo := lendingFixture(200)
		h, token := lendingHTTP(t, repo, auth.RoleClient)
		w := lendingRequest(h, actionPath, actionBody(), token, keys...)
		if w.Code != 400 || repo.calls != 0 {
			t.Fatalf("invalid keys accepted: %v", keys)
		}
	}
	repo := lendingFixture(200)
	h, token := lendingHTTP(t, repo, auth.RoleClient)
	w := lendingRequest(h, "/api/v1/applications/bad/accept", actionBody(), token, lendingKey)
	if w.Code != 400 || repo.calls != 0 {
		t.Fatal("invalid application ID")
	}
}

func TestLendingBusinessValidationAndSafeErrors(t *testing.T) {
	path := "/api/v1/bank/applications/" + testApplicationID + "/decisions"
	for _, body := range []string{strings.Replace(manualBody(), `  Подтверждено.  `, `   `, 1), strings.Replace(manualBody(), `  Подтверждено.  `, strings.Repeat("я", 1001), 1), strings.Replace(manualBody(), `1000000`, `0`, 1), strings.Replace(manualBody(), `"termMonths":12`, `"termMonths":121`, 1)} {
		repo := lendingFixture(201)
		h, token := lendingHTTP(t, repo, auth.RoleBankEmployee)
		w := lendingRequest(h, path, body, token, lendingKey)
		if w.Code != 422 || repo.calls != 0 || lendingResponseJSON(t, w)["error"] != "validation_failed" {
			t.Fatalf("business validation: %d %s", w.Code, w.Body)
		}
		var failure struct {
			Error   string               `json:"error"`
			Message string               `json:"message"`
			Details []lending.FieldError `json:"details"`
		}
		if json.Unmarshal(w.Body.Bytes(), &failure) != nil || len(failure.Details) != 1 || failure.Details[0].Field == "" || failure.Details[0].Code == "" || failure.Details[0].Message == "" || len(lendingResponseJSON(t, w)) != 3 {
			t.Fatal("422 must match DecisionValidationError")
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{application.ErrNotFound, 404, "application_not_found"}, {application.ErrVersionConflict, 409, "version_conflict"}, {application.ErrInvalidStatus, 409, "invalid_status"}, {lending.ErrOfferNotActive, 409, "offer_not_active"}, {lending.ErrIdempotencyConflict, 409, "idempotency_conflict"}, {lending.ErrValidation, 422, "validation_failed"}, {decision.ErrManualTerms, 422, "validation_failed"}, {decision.ErrInvalidData, 503, "service_unavailable"}, {errors.New("postgres://secret host=private SQL password=hidden"), 503, "service_unavailable"},
	} {
		repo := lendingFixture(201)
		repo.err = tc.err
		h, token := lendingHTTP(t, repo, auth.RoleBankEmployee)
		w := lendingRequest(h, path, manualBody(), token, lendingKey)
		if w.Code != tc.status || lendingResponseJSON(t, w)["error"] != tc.code || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "SQL") {
			t.Fatalf("err=%v status=%d body=%s", tc.err, w.Code, w.Body)
		}
	}
}

func TestLendingMediaLimitAndInvalidStoredResponse(t *testing.T) {
	repo := lendingFixture(201)
	h, token := lendingHTTP(t, repo, auth.RoleBankEmployee)
	path := "/api/v1/bank/applications/" + testApplicationID + "/decisions"
	r := httptest.NewRequest("POST", path, strings.NewReader(manualBody()))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", lendingKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 || repo.calls != 0 {
		t.Fatal("wrong media type")
	}
	w = lendingRequest(h, path, strings.Replace(manualBody(), `  Подтверждено.  `, strings.Repeat("x", 65536), 1), token, lendingKey)
	if w.Code != 413 || repo.calls != 0 {
		t.Fatalf("body limit: %d", w.Code)
	}
	for _, response := range []lending.Response{{Status: 500, Body: json.RawMessage(`{"secret":"db"}`)}, {Status: 201, Body: json.RawMessage(`invalid`)}} {
		repo.response = response
		w = lendingRequest(h, path, manualBody(), token, lendingKey)
		if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
			t.Fatal("invalid stored response exposed")
		}
	}
}
