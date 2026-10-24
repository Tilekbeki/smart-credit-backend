package postgres

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/document"
)

func TestDraftLimitIsAtomicAndDeletionReleasesSlot(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	var first application.Application
	for i := 0; i < 19; i++ {
		first = createDraft(t, repo, draftOwner, application.Fields{})
	}
	results := make(chan error, 32)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := repo.Create(ctx, draftOwner, application.Fields{})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success, limited := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, application.ErrDraftLimit) {
			limited++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || limited != 31 {
		t.Fatalf("concurrent creators success=%d limited=%d", success, limited)
	}
	page, err := repo.List(ctx, draftOwner, application.ListFilter{Limit: 100})
	if err != nil || page.Total != 20 {
		t.Fatalf("draft count %d %v", page.Total, err)
	}
	createDraft(t, repo, otherDraftOwner, application.Fields{})
	if err := repo.DeleteDraft(ctx, draftOwner, first.ApplicationID, 1); err != nil {
		t.Fatal(err)
	}
	createDraft(t, repo, draftOwner, application.Fields{})
	if _, err := repo.Create(ctx, draftOwner, application.Fields{}); !errors.Is(err, application.ErrDraftLimit) {
		t.Fatal(err)
	}
	var deleted, events int
	if pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM applications WHERE deleted_at IS NOT NULL),
        (SELECT count(*) FROM application_events WHERE event_type='draft_deleted')`).Scan(&deleted, &events) != nil || deleted != 1 || events != 1 {
		t.Fatal("tombstone/event not retained")
	}
}

func TestDraftLimitConfigurationAndSubmittedExclusion(t *testing.T) {
	pool, _ := applicationFixture(t)
	ctx := context.Background()
	repo := NewApplicationRepository(pool, 5*time.Second, 1)
	first := createDraft(t, repo, draftOwner, draftFields())
	if _, err := repo.Create(ctx, draftOwner, application.Fields{}); !errors.Is(err, application.ErrDraftLimit) {
		t.Fatal(err)
	}
	if _, err := NewSubmissionRepository(pool, 5*time.Second).Submit(ctx, draftOwner, first.ApplicationID, 1, nil); err != nil {
		t.Fatal(err)
	}
	createDraft(t, repo, draftOwner, application.Fields{})
	if err := repo.DeleteDraft(ctx, draftOwner, first.ApplicationID, 2); !errors.Is(err, application.ErrInvalidStatus) {
		t.Fatal("submitted draft deleted", err)
	}
}

func TestDeletedDraftIsPrivateAndCannotBeResurrected(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, application.Fields{})
	storage, err := document.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	docs := document.NewService(NewDocumentRepository(pool, 5*time.Second), storage)
	meta, err := docs.Upload(ctx, documentOwner, draft.ApplicationID, document.BankStatement, "statement.pdf", bytes.NewReader([]byte("%PDF-1.4\nsynthetic\n%%EOF\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteDraft(ctx, otherDraftOwner, draft.ApplicationID, 1); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("foreign delete", err)
	}
	if err := repo.DeleteDraft(ctx, draftOwner, draft.ApplicationID, 2); !errors.Is(err, application.ErrVersionConflict) {
		t.Fatal("stale delete", err)
	}
	for i := 0; i < 2; i++ {
		if err := repo.DeleteDraft(ctx, draftOwner, draft.ApplicationID, 1); err != nil {
			t.Fatal("delete/replay", err)
		}
	}
	if _, err := repo.Get(ctx, draftOwner, draft.ApplicationID); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("deleted GET", err)
	}
	if _, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 2, draftFields()); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("deleted PATCH", err)
	}
	if _, err := repo.Timeline(ctx, draftOwner, draft.ApplicationID); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("deleted timeline", err)
	}
	if _, err := NewReviewRepository(pool, time.Second).Read(ctx, &draftOwner, draft.ApplicationID); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("deleted decision", err)
	}
	if _, err := NewSubmissionRepository(pool, time.Second).Submit(ctx, draftOwner, draft.ApplicationID, 2, nil); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("deleted submit", err)
	}
	if _, err := docs.List(ctx, documentOwner, draft.ApplicationID); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatal("deleted documents", err)
	}
	if _, _, err := docs.Download(ctx, documentOwner, draft.ApplicationID, meta.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatal("deleted file downloaded", err)
	}
	if _, err := docs.Upload(ctx, documentOwner, draft.ApplicationID, document.BankStatement, "new.pdf", bytes.NewReader([]byte("%PDF-1.4\n%%EOF\n"))); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatal("deleted upload", err)
	}
	if err := docs.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := repo.List(ctx, draftOwner, application.ListFilter{Limit: 20})
	if err != nil || page.Total != 0 {
		t.Fatal("deleted draft listed")
	}
	var events, documents int
	if pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM application_events WHERE event_type='draft_deleted'),
        (SELECT count(*) FROM documents WHERE id=$1)`, meta.DocumentID).Scan(&events, &documents) != nil || events != 1 || documents != 1 {
		t.Fatal("audit lost or duplicate deletion event")
	}
}

func TestDraftDeleteAndSubmitSerialize(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	submit := NewSubmissionRepository(pool, 5*time.Second)
	for i := 0; i < 10; i++ {
		draft := createDraft(t, repo, draftOwner, draftFields())
		start := make(chan struct{})
		result := make(chan error, 2)
		go func() { <-start; result <- repo.DeleteDraft(ctx, draftOwner, draft.ApplicationID, 1) }()
		go func() { <-start; _, err := submit.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil); result <- err }()
		close(start)
		a, b := <-result, <-result
		if (a == nil) == (b == nil) {
			t.Fatalf("exactly one mutation must win: %v %v", a, b)
		}
		var deleted bool
		var status string
		var jobs int
		if pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL,status,(SELECT count(*) FROM jobs WHERE application_id=a.id) FROM applications a WHERE id=$1`, draft.ApplicationID).Scan(&deleted, &status, &jobs) != nil {
			t.Fatal("race result unreadable")
		}
		if deleted && (status != "draft" || jobs != 0) || !deleted && (status != "submitted" || jobs != 1) {
			t.Fatalf("inconsistent race result deleted=%v status=%s jobs=%d", deleted, status, jobs)
		}
	}
}

func TestDraftDeleteRollsBackWithAudit(t *testing.T) {
	pool, repo := applicationFixture(t)
	draft := createDraft(t, repo, draftOwner, application.Fields{})
	installEventFailure(t, pool)
	if err := repo.DeleteDraft(context.Background(), draftOwner, draft.ApplicationID, 1); !errors.Is(err, application.ErrStorage) {
		t.Fatal(err)
	}
	got, err := repo.Get(context.Background(), draftOwner, draft.ApplicationID)
	if err != nil || got.Version != 1 {
		t.Fatal("failed delete left a tombstone", err)
	}
}
