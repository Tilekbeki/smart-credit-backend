package contact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
)

type contactStub struct {
	calls   int
	user    UserChanges
	company CompanyChanges
}

func (r *contactStub) UpdateUser(_ context.Context, _ string, c UserChanges) (auth.User, error) {
	r.calls++
	r.user = c
	return auth.User{}, nil
}
func (r *contactStub) UpdateCompany(_ context.Context, _, _ string, c CompanyChanges) (company.Company, error) {
	r.calls++
	r.company = c
	return company.Company{}, nil
}
func optional(v string) application.Optional[string] {
	return application.Optional[string]{Present: true, Value: &v}
}
func client() auth.User {
	id := "company"
	return auth.User{ID: "client", Role: auth.RoleClient, CompanyID: &id}
}

func TestUserContactValidationAndRoles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field application.Optional[string]
		want  bool
	}{
		{"trim email", optional("  user@example.test  "), true},
		{"clear", application.Optional[string]{Present: true}, true},
		{"missing", application.Optional[string]{}, false},
		{"empty", optional(" "), false}, {"display name", optional("Name <user@example.test>"), false},
		{"invalid", optional("invalid"), false}, {"header injection", optional("a@example.test\r\nBcc: b@example.test"), false},
		{"oversized", optional(strings.Repeat("a", 255) + "@example.test"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &contactStub{}
			_, err := NewService(r).UpdateUser(context.Background(), client(), UserChanges{Version: 1, AdditionalEmail: tc.field})
			if (err == nil) != tc.want || (r.calls == 1) != tc.want {
				t.Fatalf("calls=%d err=%v", r.calls, err)
			}
			if tc.name == "trim email" && *r.user.AdditionalEmail.Value != "user@example.test" {
				t.Fatal("email not normalized")
			}
		})
	}
	r := &contactStub{}
	s := NewService(r)
	if _, err := s.UpdateUser(context.Background(), auth.User{Role: auth.RoleBankEmployee}, UserChanges{}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if _, err := s.UpdateUser(context.Background(), client(), UserChanges{Version: 0, AdditionalEmail: optional("a@example.test")}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if r.calls != 0 {
		t.Fatal("invalid input reached repository")
	}
}

func TestCompanyContactValidationAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields CompanyFields
		want   bool
	}{
		{"phone", CompanyFields{Phone: optional(" +79990000099 ")}, true},
		{"email", CompanyFields{Email: optional("bank@example.test")}, true},
		{"address", CompanyFields{Address: optional(" Адрес для связи ")}, true},
		{"empty patch", CompanyFields{}, false},
		{"bad phone", CompanyFields{Phone: optional("8 (999) 123")}, false},
		{"null", CompanyFields{Phone: application.Optional[string]{Present: true}}, false},
		{"empty", CompanyFields{Address: optional(" ")}, false},
		{"control", CompanyFields{Address: optional("address\x00secret")}, false},
		{"long", CompanyFields{Address: optional(strings.Repeat("я", 301))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &contactStub{}
			_, err := NewService(r).UpdateCompany(context.Background(), client(), "company", CompanyChanges{Version: 1, Contact: tc.fields})
			if (err == nil) != tc.want || (r.calls == 1) != tc.want {
				t.Fatalf("calls=%d err=%v", r.calls, err)
			}
		})
	}
	for _, u := range []auth.User{client(), {ID: "employee", Role: auth.RoleBankEmployee}} {
		r := &contactStub{}
		_, err := NewService(r).UpdateCompany(context.Background(), u, "foreign", CompanyChanges{Version: 1, Contact: CompanyFields{Email: optional("a@example.test")}})
		if !errors.Is(err, ErrForbidden) || r.calls != 0 {
			t.Fatal("unauthorized update", err)
		}
	}
}
