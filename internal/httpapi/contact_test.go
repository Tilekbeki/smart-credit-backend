package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/contact"
)

type contactRepoStub struct {
	calls int
	err   error
}

func (s *contactRepoStub) UpdateUser(_ context.Context, id string, c contact.UserChanges) (auth.User, error) {
	s.calls++
	u := demoUser()
	u.Version = c.Version + 1
	u.AdditionalEmail = c.AdditionalEmail.Value
	return u, s.err
}
func (s *contactRepoStub) UpdateCompany(_ context.Context, _, id string, c contact.CompanyChanges) (company.Company, error) {
	s.calls++
	return company.Company{ID: id, Version: c.Version + 1}, s.err
}

func contactHTTP(t *testing.T, repo *contactRepoStub, role auth.Role) (http.Handler, string) {
	t.Helper()
	u := demoUser()
	u.Role = role
	tokens, err := auth.NewTokens(strings.Repeat("c", 32), "contacts-test", "contacts-test")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Issue(u)
	if err != nil {
		t.Fatal(err)
	}
	h := NewContactHandler(contact.NewService(repo), auth.NewMiddleware(tokens, &authRepoStub{user: u}, time.Second))
	return NewHandler(nil, testLogger(), time.Second, nil, h.Register), token
}

func TestContactHTTPWhitelistRolesAndVersions(t *testing.T) {
	companyPath := "/api/v1/companies/" + ownCompanyID
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"email", "/api/v1/me", `{"version":1,"additionalEmail":"extra@example.test"}`, 200},
		{"clear", "/api/v1/me", `{"version":1,"additionalEmail":null}`, 200},
		{"company", companyPath, `{"version":1,"contact":{"phone":"+79990000099","email":"hello@example.test","address":"Адрес для связи"}}`, 200},
		{"legal name", "/api/v1/me", `{"version":1,"additionalEmail":null,"name":"Other"}`, 400},
		{"login phone", "/api/v1/me", `{"version":1,"phone":"+79990000099"}`, 400},
		{"role", "/api/v1/me", `{"version":1,"additionalEmail":null,"role":"BANK_EMPLOYEE"}`, 400},
		{"company binding", "/api/v1/me", `{"version":1,"additionalEmail":null,"companyId":"` + ownCompanyID + `"}`, 400},
		{"duplicate", "/api/v1/me", `{"version":1,"version":2,"additionalEmail":null}`, 400},
		{"wrong case", "/api/v1/me", `{"Version":1,"additionalEmail":null}`, 400},
		{"version missing", "/api/v1/me", `{"additionalEmail":null}`, 400},
		{"blank email", "/api/v1/me", `{"version":1,"additionalEmail":""}`, 400},
		{"foreign", "/api/v1/companies/10000000-0000-4000-8000-000000000099", `{"version":1,"contact":{"email":"a@example.test"}}`, 403},
		{"INN", companyPath, `{"version":1,"contact":{"email":"a@example.test"},"inn":"0000000002"}`, 400},
		{"nested INN", companyPath, `{"version":1,"contact":{"inn":"0000000002"}}`, 400},
		{"financials", companyPath, `{"version":1,"contact":{"phone":"+79990000099"},"financials":{"monthlyRevenueRub":999}}`, 400},
		{"empty contact", companyPath, `{"version":1,"contact":{}}`, 400},
		{"null contact", companyPath, `{"version":1,"contact":null}`, 400},
		{"null phone", companyPath, `{"version":1,"contact":{"phone":null}}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &contactRepoStub{}
			h, token := contactHTTP(t, repo, auth.RoleClient)
			w := authRequest(h, "PATCH", tc.path, tc.body, token)
			if w.Code != tc.status {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body)
			}
			if tc.status != 200 && repo.calls != 0 {
				t.Fatal("invalid request mutated data")
			}
		})
	}
	for _, role := range []auth.Role{auth.RoleClient, auth.RoleBankEmployee} {
		repo := &contactRepoStub{}
		h, token := contactHTTP(t, repo, role)
		for _, path := range []string{"/api/v1/me", companyPath} {
			if w := authRequest(h, "PATCH", path, `{}`, ""); w.Code != 401 {
				t.Fatal("unauthenticated edit")
			}
			if role == auth.RoleBankEmployee {
				if w := authRequest(h, "PATCH", path, `{}`, token); w.Code != 403 {
					t.Fatal("bank edited contacts")
				}
			}
		}
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{contact.ErrVersionConflict, 409}, {contact.ErrStorage, 503}} {
		repo := &contactRepoStub{err: tc.err}
		h, token := contactHTTP(t, repo, auth.RoleClient)
		w := authRequest(h, "PATCH", "/api/v1/me", `{"version":1,"additionalEmail":null}`, token)
		if w.Code != tc.status || !json.Valid(w.Body.Bytes()) {
			t.Fatal("incorrect contact error", w.Code, w.Body)
		}
	}
}
