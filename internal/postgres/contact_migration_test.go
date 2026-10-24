package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
)

func TestContactDraftMigrationCycle(t *testing.T) {
	pool, _ := applicationFixture(t)
	ctx := context.Background()
	for _, direction := range []string{"down", "up"} {
		data, err := os.ReadFile("../../migrations/000012_contacts_and_draft_lifecycle." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("migration %s: %v", direction, err)
		}
	}
	user, err := NewAuthRepository(pool, time.Second).UserByID(ctx, demoClientID)
	if err != nil || user.Version != 1 || user.AdditionalEmail != nil {
		t.Fatal("migration cycle changed demo identity", err)
	}
}

func TestContactDraftMigrationRefusesDataLoss(t *testing.T) {
	for _, kind := range []string{"deleted draft", "contact audit"} {
		t.Run(kind, func(t *testing.T) {
			pool, repo := applicationFixture(t)
			ctx := context.Background()
			if kind == "deleted draft" {
				draft := createDraft(t, repo, draftOwner, application.Fields{})
				if err := repo.DeleteDraft(ctx, draftOwner, draft.ApplicationID, 1); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := NewContactRepository(pool, time.Second).UpdateUser(ctx, demoClientID, emailChange(1, "audit@example.test")); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile("../../migrations/000012_contacts_and_draft_lifecycle.down.sql")
			if err != nil {
				t.Fatal(err)
			}
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, string(data)); err == nil {
				t.Fatal("lossy downgrade accepted")
			}
			if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			if _, err := NewAuthRepository(pool, time.Second).UserByID(ctx, demoClientID); err != nil {
				t.Fatal("failed downgrade altered schema", err)
			}
		})
	}
}
