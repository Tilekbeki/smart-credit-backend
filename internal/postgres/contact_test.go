package postgres

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/contact"
)

func emailChange(version int64, value string) contact.UserChanges {
	return contact.UserChanges{Version: version, AdditionalEmail: application.Optional[string]{Present: true, Value: &value}}
}

func TestContactUpdatesPersistAuditAndProtectIdentity(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	repo := NewContactRepository(pool, 5*time.Second)
	original, err := NewAuthRepository(pool, time.Second).UserByID(ctx, demoClientID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := repo.UpdateUser(ctx, demoClientID, emailChange(original.Version, "extra@example.test"))
	if err != nil || updated.Version != original.Version+1 || updated.AdditionalEmail == nil || *updated.AdditionalEmail != "extra@example.test" {
		t.Fatalf("update %+v %v", updated, err)
	}
	identity := updated
	identity.Version = original.Version
	identity.AdditionalEmail = original.AdditionalEmail
	if !reflect.DeepEqual(identity, original) {
		t.Fatal("identity fields changed")
	}
	if _, err := repo.UpdateUser(ctx, demoClientID, emailChange(original.Version, "stale@example.test")); !errors.Is(err, contact.ErrVersionConflict) {
		t.Fatal(err)
	}
	updated, err = repo.UpdateUser(ctx, demoClientID, contact.UserChanges{Version: updated.Version, AdditionalEmail: application.Optional[string]{Present: true}})
	if err != nil || updated.AdditionalEmail != nil {
		t.Fatal("clear email", err)
	}
	catalog := NewCompanyCreditRepository(pool, time.Second)
	before, err := catalog.CompanyByID(ctx, draftOwner.CompanyID)
	if err != nil {
		t.Fatal(err)
	}
	phone := "+79990000099"
	changes := contact.CompanyChanges{Version: before.Version, Contact: contact.CompanyFields{Phone: application.Optional[string]{Present: true, Value: &phone}}}
	after, err := repo.UpdateCompany(ctx, demoClientID, draftOwner.CompanyID, changes)
	if err != nil || after.Contact.Phone != phone || after.Version != before.Version+1 {
		t.Fatalf("company %+v %v", after, err)
	}
	identityCompany := after
	identityCompany.Contact = before.Contact
	identityCompany.Version = before.Version
	if !reflect.DeepEqual(identityCompany, before) {
		t.Fatal("company legal/financial data changed")
	}
	for _, actor := range []string{demoEmployeeID, otherDraftOwner.UserID} {
		if _, err := repo.UpdateCompany(ctx, actor, draftOwner.CompanyID, changes); !errors.Is(err, contact.ErrForbidden) {
			t.Fatal("unauthorized company write", err)
		}
	}
	if _, err := repo.UpdateUser(ctx, demoEmployeeID, emailChange(1, "employee@example.test")); !errors.Is(err, contact.ErrForbidden) {
		t.Fatal("employee edited contacts", err)
	}
	var count int
	if pool.QueryRow(ctx, `SELECT count(*) FROM contact_changes`).Scan(&count) != nil || count != 3 {
		t.Fatalf("audit count %d", count)
	}
	// OTP login reads the same persisted email/version, while its phone remains immutable.
	authRepo := NewAuthRepository(pool, time.Second)
	otp, err := authRepo.RequestOTP(ctx, demoClientID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := authRepo.VerifyOTP(ctx, otp.ID, true)
	if err != nil || user.Version != updated.Version || user.Phone != original.Phone || user.Role != auth.RoleClient {
		t.Fatalf("OTP user %+v %v", user, err)
	}
}

func TestContactConcurrentVersionsAndAuditRollback(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	repo := NewContactRepository(pool, 5*time.Second)
	results := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, email := range []string{"first@example.test", "second@example.test"} {
		wg.Add(1)
		go func(value string) {
			defer wg.Done()
			<-start
			_, err := repo.UpdateUser(ctx, demoClientID, emailChange(1, value))
			results <- err
		}(email)
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, contact.ErrVersionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_contact_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic failure'; END $$;
        CREATE TRIGGER fail_contact_audit BEFORE INSERT ON contact_changes FOR EACH ROW EXECUTE FUNCTION fail_contact_audit()`); err != nil {
		t.Fatal(err)
	}
	before, err := NewAuthRepository(pool, time.Second).UserByID(ctx, demoClientID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateUser(ctx, demoClientID, emailChange(before.Version, "rollback@example.test")); !errors.Is(err, contact.ErrStorage) {
		t.Fatal(err)
	}
	after, err := NewAuthRepository(pool, time.Second).UserByID(ctx, demoClientID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("contact mutation survived failed audit")
	}
}
