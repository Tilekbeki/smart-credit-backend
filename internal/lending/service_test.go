package lending

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"smart-credit-backend/internal/application"
)

const applicationID = "40000000-0000-4000-8000-000000000001"
const actorID = "20000000-0000-4000-8000-000000000001"
const companyID = "10000000-0000-4000-8000-000000000001"
const keyID = "70000000-0000-4000-8000-00000000000a"
const offerID = "60000000-0000-4000-8000-00000000000a"

type repositoryStub struct {
	command Command
	calls   int
	err     error
}

func TestManualCommentAliasesPreserveLegacyHashAndSeparatePublicExplanation(t *testing.T) {
	repo := new(repositoryStub)
	service := NewService(repo)
	legacy := manualCommand()
	if _, err := service.Execute(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	legacyHash := repo.command.RequestHash
	// The previous canonical body must retain exactly the same hash after the
	// optional fields are introduced, including requests persisted before upgrade.
	body, err := json.Marshal(struct {
		Version  int64        `json:"version"`
		Decision string       `json:"decision"`
		Comment  string       `json:"comment"`
		Offer    *ManualOffer `json:"offer"`
	}{legacy.Version, legacy.Decision, strings.TrimSpace(legacy.Comment), legacy.Offer})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if legacyHash != hex.EncodeToString(sum[:]) {
		t.Fatal("legacy idempotency hash changed")
	}

	alias := legacy
	alias.Comment = ""
	alias.InternalComment = legacy.Comment
	if _, err := service.Execute(context.Background(), alias); err != nil || repo.command.RequestHash != legacyHash {
		t.Fatalf("internalComment alias changes request identity: %v", err)
	}
	alias.Comment = strings.TrimSpace(legacy.Comment)
	if _, err := service.Execute(context.Background(), alias); err != nil || repo.command.RequestHash != legacyHash {
		t.Fatalf("matching aliases change request identity: %v", err)
	}
	alias.CustomerExplanation = "  Публичное объяснение решения.  "
	if _, err := service.Execute(context.Background(), alias); err != nil {
		t.Fatal(err)
	}
	customHash := repo.command.RequestHash
	if customHash == legacyHash || repo.command.CustomerExplanation != "Публичное объяснение решения." || repo.command.Comment != strings.TrimSpace(legacy.Comment) {
		t.Fatal("public explanation not normalized or excluded from idempotency identity")
	}
	alias.CustomerExplanation = "Публичное объяснение решения."
	if _, err := service.Execute(context.Background(), alias); err != nil || repo.command.RequestHash != customHash {
		t.Fatal("equivalent explanation changes hash", err)
	}
	alias.CustomerExplanation = " \n\t "
	if _, err := service.Execute(context.Background(), alias); err != nil || repo.command.RequestHash != legacyHash {
		t.Fatal("blank optional explanation must select the same default", err)
	}
}

func TestManualNewCommentValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Command)
		want   error
	}{
		{"conflicting aliases", func(c *Command) { c.InternalComment = "Другое внутреннее основание." }, application.ErrInvalidInput},
		{"empty alias", func(c *Command) { c.Comment = ""; c.InternalComment = " \t " }, ErrValidation},
		{"long alias", func(c *Command) { c.Comment = ""; c.InternalComment = strings.Repeat("я", 1001) }, ErrValidation},
		{"invalid alias", func(c *Command) { c.Comment = ""; c.InternalComment = string([]byte{255}) }, ErrValidation},
		{"long explanation", func(c *Command) { c.CustomerExplanation = strings.Repeat("я", 1001) }, ErrValidation},
		{"NUL explanation", func(c *Command) { c.CustomerExplanation = "public\x00text" }, ErrValidation},
		{"invalid explanation", func(c *Command) { c.CustomerExplanation = string([]byte{255}) }, ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := manualCommand()
			tc.change(&c)
			repo := new(repositoryStub)
			if _, err := NewService(repo).Execute(context.Background(), c); !errors.Is(err, tc.want) || repo.calls != 0 {
				t.Fatalf("err=%v calls=%d", err, repo.calls)
			}
		})
	}
}

func (s *repositoryStub) Execute(_ context.Context, command Command) (Response, error) {
	s.command = command
	s.calls++
	return Response{Status: 201, Body: []byte(`{"stored":true}`)}, s.err
}

func manualCommand() Command {
	return Command{ApplicationID: applicationID, ActorID: actorID, Key: keyID, Operation: Manual, Version: 4, Decision: "approved", Comment: "  Проверено сотрудником.\n ", Offer: &ManualOffer{AmountRub: 1_000_000, TermMonths: 12}}
}

func TestCommandCanonicalHashAndComment(t *testing.T) {
	repo := new(repositoryStub)
	service := NewService(repo)
	command := manualCommand()
	if _, err := service.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	first := repo.command
	if first.Comment != "Проверено сотрудником." || len(first.RequestHash) != 64 || first.RequestHash == command.RequestHash {
		t.Fatal("comment/hash not normalized")
	}
	command.Comment = "Проверено сотрудником."
	command.Key = strings.ToUpper(keyID)
	command.ActorID = "20000000-0000-4000-8000-000000000002"
	command.RequestHash = "untrusted"
	if _, err := service.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if repo.command.Key != keyID || repo.command.RequestHash != first.RequestHash {
		t.Fatal("identity fields leaked into body hash")
	}
	command.Comment = "Проверено\nсотрудником."
	if _, err := service.Execute(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if repo.command.RequestHash == first.RequestHash || repo.command.Comment != command.Comment {
		t.Fatal("comment meaning erased")
	}
	command.Version++
	previous := repo.command.RequestHash
	if _, err := service.Execute(context.Background(), command); err != nil || repo.command.RequestHash == previous {
		t.Fatal("version excluded from canonical body")
	}
}

func TestManualInputValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Command)
		want   error
	}{
		{"missing application", func(c *Command) { c.ApplicationID = "" }, application.ErrInvalidInput},
		{"nil actor", func(c *Command) { c.ActorID = "00000000-0000-0000-0000-000000000000" }, application.ErrInvalidInput},
		{"bad key", func(c *Command) { c.Key = "not-uuid" }, application.ErrInvalidInput},
		{"short UUID", func(c *Command) { c.Key = strings.ReplaceAll(keyID, "-", "") }, application.ErrInvalidInput},
		{"zero version", func(c *Command) { c.Version = 0 }, application.ErrInvalidInput},
		{"negative version", func(c *Command) { c.Version = -1 }, application.ErrInvalidInput},
		{"unknown operation", func(c *Command) { c.Operation = "disburse" }, application.ErrInvalidInput},
		{"unknown decision", func(c *Command) { c.Decision = "manual_review" }, application.ErrInvalidInput},
		{"missing proposal", func(c *Command) { c.Offer = nil }, application.ErrInvalidInput},
		{"rejection proposal", func(c *Command) { c.Decision = "rejected" }, application.ErrInvalidInput},
		{"empty comment", func(c *Command) { c.Comment = " \n\t" }, ErrValidation},
		{"long Unicode", func(c *Command) { c.Comment = strings.Repeat("я", 1001) }, ErrValidation},
		{"null comment", func(c *Command) { c.Comment = "audit\x00value" }, ErrValidation},
		{"invalid UTF8", func(c *Command) { c.Comment = string([]byte{255}) }, ErrValidation},
		{"zero amount", func(c *Command) { c.Offer.AmountRub = 0 }, ErrValidation},
		{"huge amount", func(c *Command) { c.Offer.AmountRub = 1_000_000_000_001 }, ErrValidation},
		{"zero term", func(c *Command) { c.Offer.TermMonths = 0 }, ErrValidation},
		{"huge term", func(c *Command) { c.Offer.TermMonths = 121 }, ErrValidation},
		{"injected company", func(c *Command) { c.CompanyID = companyID }, application.ErrInvalidInput},
		{"injected offer ID", func(c *Command) { c.OfferID = offerID }, application.ErrInvalidInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := manualCommand()
			tc.change(&c)
			repo := new(repositoryStub)
			_, err := NewService(repo).Execute(context.Background(), c)
			if !errors.Is(err, tc.want) || repo.calls != 0 {
				t.Fatalf("err=%v calls=%d", err, repo.calls)
			}
		})
	}
	for _, outcome := range []string{"approved", "counter_offer", "rejected"} {
		c := manualCommand()
		c.Decision = outcome
		c.Comment = strings.Repeat("я", 1000)
		if outcome == "rejected" {
			c.Offer = nil
		}
		if _, err := NewService(new(repositoryStub)).Execute(context.Background(), c); err != nil {
			t.Fatalf("valid %s rejected: %v", outcome, err)
		}
	}
}

func TestOfferActionValidationAndCanonicalIdentity(t *testing.T) {
	for _, operation := range []string{Accept, Decline} {
		command := Command{ApplicationID: applicationID, ActorID: actorID, CompanyID: companyID, Key: keyID, Version: 5, OfferID: strings.ToUpper(offerID), Operation: operation}
		repo := new(repositoryStub)
		service := NewService(repo)
		if _, err := service.Execute(context.Background(), command); err != nil || repo.command.OfferID != offerID {
			t.Fatalf("valid %s: %v", operation, err)
		}
		for _, change := range []func(*Command){func(c *Command) { c.CompanyID = "" }, func(c *Command) { c.OfferID = "bad" }, func(c *Command) { c.Offer = &ManualOffer{AmountRub: 1, TermMonths: 1} }, func(c *Command) { c.Comment = "injected" }, func(c *Command) { c.Decision = "approved" }} {
			c := command
			change(&c)
			before := repo.calls
			if _, err := service.Execute(context.Background(), c); !errors.Is(err, application.ErrInvalidInput) || repo.calls != before {
				t.Fatal("invalid action reached storage")
			}
		}
	}
}
