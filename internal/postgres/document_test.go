package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/document"
)

var documentOwner = document.Scope{UserID: demoClientID, CompanyID: draftOwner.CompanyID, Role: auth.RoleClient}
var documentOtherOwner = document.Scope{UserID: otherDraftOwner.UserID, CompanyID: otherDraftOwner.CompanyID, Role: auth.RoleClient}
var documentEmployee = document.Scope{UserID: demoEmployeeID, Role: auth.RoleBankEmployee}

func documentFixture(t *testing.T) (*pgxpool.Pool, *DocumentRepository, string) {
	t.Helper()
	pool := authTestPool(t)
	app := createDraft(t, NewApplicationRepository(pool, 5*time.Second), draftOwner, application.Fields{})
	return pool, NewDocumentRepository(pool, 5*time.Second), app.ApplicationID
}

func documentMetadata(appID string) document.Metadata {
	id := uuid.NewString()
	hash := sha256.Sum256([]byte("synthetic document"))
	return document.Metadata{DocumentID: id, ApplicationID: appID, DocumentType: document.BankStatement,
		FileName: "statement.pdf", ContentType: "application/pdf", SizeBytes: 18, StorageKey: id,
		Checksum: hex.EncodeToString(hash[:])}
}

func reserveDocument(t *testing.T, repo *DocumentRepository, appID string) document.Metadata {
	t.Helper()
	metadata, err := repo.Reserve(context.Background(), documentOwner, documentMetadata(appID))
	if err != nil {
		t.Fatalf("reserve document: %v", err)
	}
	return metadata
}

func activateDocument(t *testing.T, repo *DocumentRepository, appID string) document.Metadata {
	t.Helper()
	pending := reserveDocument(t, repo, appID)
	metadata, err := repo.Activate(context.Background(), documentOwner, appID, pending.DocumentID)
	if err != nil {
		t.Fatalf("activate document: %v", err)
	}
	return metadata
}

func TestDocumentRepositoryRejectsMalformedInputWithoutDatabase(t *testing.T) {
	repo := NewDocumentRepository(nil, time.Second)
	ctx := context.Background()
	for _, scope := range []document.Scope{{}, {UserID: demoClientID, Role: "ADMIN"}, documentEmployee} {
		if _, err := repo.CheckApplication(ctx, scope, demoClientID, true); !errors.Is(err, document.ErrForbidden) {
			t.Fatalf("invalid write role: %v", err)
		}
		if _, err := repo.Reserve(ctx, scope, documentMetadata(demoClientID)); !errors.Is(err, document.ErrForbidden) {
			t.Fatalf("reserve invalid write role: %v", err)
		}
		if _, err := repo.Activate(ctx, scope, demoClientID, demoClientID); !errors.Is(err, document.ErrForbidden) {
			t.Fatalf("activate invalid write role: %v", err)
		}
	}
	for _, id := range []string{"", "invalid", "' OR true--"} {
		if _, err := repo.CheckApplication(ctx, documentOwner, id, false); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("malformed application: %v", err)
		}
		if _, err := repo.List(ctx, documentOwner, id); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("malformed list application: %v", err)
		}
		if _, err := repo.Get(ctx, documentOwner, demoClientID, id); !errors.Is(err, document.ErrNotFound) {
			t.Fatalf("malformed document: %v", err)
		}
		if err := repo.Invalidate(ctx, id); !errors.Is(err, document.ErrNotFound) {
			t.Fatalf("malformed invalidation: %v", err)
		}
	}
	metadata := documentMetadata(demoClientID)
	metadata.StorageKey = "../unsafe"
	if _, err := repo.Reserve(ctx, documentOwner, metadata); !errors.Is(err, document.ErrInvalidInput) {
		t.Fatalf("unsafe storage key: %v", err)
	}
}

func TestDocumentRepositoryLifecycle(t *testing.T) {
	_, repo, appID := documentFixture(t)
	ctx := context.Background()
	pending := reserveDocument(t, repo, appID)
	if pending.Status != "pending" || pending.UploadedBy != documentOwner.UserID || pending.UploadedAt.IsZero() || pending.ApplicationID != appID {
		t.Fatalf("unexpected pending metadata: %+v", pending)
	}
	items, err := repo.List(ctx, documentOwner, appID)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("pending documents leaked: %v, %v", items, err)
	}
	if _, err := repo.Get(ctx, documentOwner, appID, pending.DocumentID); !errors.Is(err, document.ErrNotFound) {
		t.Fatalf("pending document is public: %v", err)
	}
	ready, err := repo.Activate(ctx, documentOwner, appID, pending.DocumentID)
	if err != nil || ready.Status != "ready" || ready.DocumentID != pending.DocumentID || !ready.UploadedAt.Equal(pending.UploadedAt) {
		t.Fatalf("activation failed: %+v, %v", ready, err)
	}
	duplicate := activateDocument(t, repo, appID)
	if duplicate.DocumentID == ready.DocumentID {
		t.Fatal("repeated upload reused document ID")
	}
	items, err = repo.List(ctx, documentOwner, appID)
	if err != nil || len(items) != 2 || items[0].DocumentID != ready.DocumentID || items[1].DocumentID != duplicate.DocumentID {
		t.Fatalf("ready list differs: %+v, %v", items, err)
	}
	if err := repo.Invalidate(ctx, ready.DocumentID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, documentOwner, appID, ready.DocumentID); !errors.Is(err, document.ErrNotFound) {
		t.Fatalf("failed document is public: %v", err)
	}
	records, err := repo.RecoveryRecords(ctx)
	if err != nil || len(records) != 2 || records[0].Status != "failed" || records[1].Status != "ready" {
		t.Fatalf("recovery must see all lifecycle states: %+v, %v", records, err)
	}
}

func TestDocumentRepositoryIgnoresCallerControlledMetadataIdentity(t *testing.T) {
	_, repo, appID := documentFixture(t)
	input := documentMetadata(appID)
	input.UploadedBy = demoEmployeeID
	input.Status = "ready"
	input.UploadedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	result, err := repo.Reserve(context.Background(), documentOwner, input)
	if err != nil || result.UploadedBy != documentOwner.UserID || result.Status != "pending" || !result.UploadedAt.After(input.UploadedAt) {
		t.Fatalf("caller controlled trusted metadata: %+v, %v", result, err)
	}
}

func TestDocumentRepositoryAccessScope(t *testing.T) {
	pool, repo, appID := documentFixture(t)
	ctx := context.Background()
	ready := activateDocument(t, repo, appID)
	for _, scope := range []document.Scope{documentOtherOwner, {UserID: otherDraftOwner.UserID, CompanyID: draftOwner.CompanyID, Role: auth.RoleClient}, {UserID: demoClientID, CompanyID: otherDraftOwner.CompanyID, Role: auth.RoleClient}} {
		if _, err := repo.CheckApplication(ctx, scope, appID, false); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("foreign application exposed: %v", err)
		}
		if _, err := repo.List(ctx, scope, appID); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("foreign document list exposed: %v", err)
		}
		if _, err := repo.Get(ctx, scope, appID, ready.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("foreign document exposed: %v", err)
		}
		if _, err := repo.Reserve(ctx, scope, documentMetadata(appID)); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("foreign reserve permitted: %v", err)
		}
		if _, err := repo.Activate(ctx, scope, appID, ready.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("foreign activation permitted: %v", err)
		}
	}
	if _, err := repo.CheckApplication(ctx, documentEmployee, appID, false); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatalf("bank discovered draft: %v", err)
	}
	if _, err := repo.List(ctx, documentEmployee, appID); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatalf("bank listed draft documents: %v", err)
	}
	if _, err := repo.Get(ctx, documentEmployee, appID, ready.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatalf("bank read draft document metadata: %v", err)
	}
	other := createDraft(t, NewApplicationRepository(pool, 5*time.Second), draftOwner, application.Fields{})
	if _, err := repo.Get(ctx, documentOwner, other.ApplicationID, ready.DocumentID); !errors.Is(err, document.ErrNotFound) {
		t.Fatalf("document from another owned application exposed: %v", err)
	}
	if _, err := repo.Get(ctx, documentEmployee, other.ApplicationID, ready.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatalf("bank accessed another draft: %v", err)
	}
	missing := uuid.NewString()
	if _, err := repo.List(ctx, documentOwner, missing); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatalf("missing application list: %v", err)
	}
	if _, err := repo.List(ctx, documentEmployee, missing); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatalf("bank missing application list: %v", err)
	}
}

func TestDocumentRepositoryRechecksDraftOnActivation(t *testing.T) {
	pool, repo, appID := documentFixture(t)
	ctx := context.Background()
	pending := reserveDocument(t, repo, appID)
	draftStatusFixture(t, pool, appID, application.Submitted)
	if _, err := repo.Reserve(ctx, documentOwner, documentMetadata(appID)); !errors.Is(err, document.ErrInvalidStatus) {
		t.Fatalf("reserve outside draft: %v", err)
	}
	if _, err := repo.Activate(ctx, documentOwner, appID, pending.DocumentID); !errors.Is(err, document.ErrInvalidStatus) {
		t.Fatalf("activation accepted a state changed since reservation: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM documents WHERE id=$1", pending.DocumentID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("failed activation changed metadata: %s, %v", status, err)
	}
}

func TestDocumentRepositoryConcurrentActivation(t *testing.T) {
	_, repo, appID := documentFixture(t)
	pending := reserveDocument(t, repo, appID)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, err := repo.Activate(context.Background(), documentOwner, appID, pending.DocumentID)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, document.ErrNotFound) {
				t.Errorf("unexpected concurrent activation result: %v", err)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("activation winners = %d, want 1", winners.Load())
	}
}

func TestDocumentRepositoryActivationWaitsForApplicationStateTransaction(t *testing.T) {
	pool, repo, appID := documentFixture(t)
	pending := reserveDocument(t, repo, appID)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT id FROM applications WHERE id=$1 FOR UPDATE", appID); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := repo.Activate(ctx, documentOwner, appID, pending.DocumentID)
		result <- err
	}()
	<-started
	if _, err := tx.Exec(ctx, `UPDATE applications SET status='submitted',
		product_id=(SELECT id FROM credit_products WHERE code='turnover'), goal_id='stock',
		amount=1500000, term_months=12, contact_name='Synthetic user', contact_role='Owner',
		submitted_at=clock_timestamp(), financial_snapshot='{}', product_snapshot='{}' WHERE id=$1`, appID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, document.ErrInvalidStatus) {
		t.Fatalf("activation ignored a concurrent application state change: %v", err)
	}
}

func TestDocumentRepositoryWriteFailuresRollback(t *testing.T) {
	for _, operation := range []string{"reserve", "activate"} {
		t.Run(operation, func(t *testing.T) {
			pool, repo, appID := documentFixture(t)
			ctx := context.Background()
			metadata := documentMetadata(appID)
			if operation == "activate" {
				metadata = reserveDocument(t, repo, appID)
			}
			_, err := pool.Exec(ctx, `CREATE FUNCTION reject_document_write() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'synthetic private storage failure'; END $$;
				CREATE TRIGGER reject_document_write BEFORE INSERT OR UPDATE ON documents
				FOR EACH ROW EXECUTE FUNCTION reject_document_write()`)
			if err != nil {
				t.Fatalf("install failure fixture: %v", err)
			}
			if operation == "reserve" {
				_, err = repo.Reserve(ctx, documentOwner, metadata)
			} else {
				_, err = repo.Activate(ctx, documentOwner, appID, metadata.DocumentID)
			}
			if !errors.Is(err, document.ErrStorage) || strings.Contains(err.Error(), "private") {
				t.Fatalf("database error was not sanitized: %v", err)
			}
			var count int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM documents WHERE status='ready'").Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed write published metadata: %d, %v", count, err)
			}
			want := 0
			if operation == "activate" {
				want = 1
			}
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM documents").Scan(&count); err != nil || count != want {
				t.Fatalf("failed write was not rolled back: %d, %v", count, err)
			}
		})
	}
}

func TestDocumentRepositoryCanceledContext(t *testing.T) {
	_, repo, appID := documentFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.CheckApplication(ctx, documentOwner, appID, false); !errors.Is(err, document.ErrStorage) {
		t.Fatal(err)
	}
	if _, err := repo.Reserve(ctx, documentOwner, documentMetadata(appID)); !errors.Is(err, document.ErrStorage) {
		t.Fatal(err)
	}
	if _, err := repo.Activate(ctx, documentOwner, appID, uuid.NewString()); !errors.Is(err, document.ErrStorage) {
		t.Fatal(err)
	}
	if _, err := repo.List(ctx, documentOwner, appID); !errors.Is(err, document.ErrStorage) {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, documentOwner, appID, uuid.NewString()); !errors.Is(err, document.ErrStorage) {
		t.Fatal(err)
	}
	if _, err := repo.RecoveryRecords(ctx); !errors.Is(err, document.ErrStorage) {
		t.Fatal(err)
	}
	if err := repo.Invalidate(ctx, uuid.NewString()); !errors.Is(err, document.ErrStorage) {
		t.Fatal(err)
	}
}

func applyDocumentMigration(t *testing.T, pool *pgxpool.Pool, direction string) {
	t.Helper()
	data, err := os.ReadFile("../../migrations/000007_document_types." + direction + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(data)); err != nil {
		t.Fatalf("document migration %s: %v", direction, err)
	}
}

func TestDocumentMigrationPreservesAndHidesLegacyFiles(t *testing.T) {
	pool, repo, appID := documentFixture(t)
	ctx := context.Background()
	applyDocumentMigration(t, pool, "down")
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO documents(application_id,uploaded_by,original_name,content_type,size_bytes,checksum_sha256,storage_key,status)
		VALUES ($1,$2,'legacy.bin','application/octet-stream',6000000,$3,'legacy-key','ready') RETURNING id::text`,
		appID, demoClientID, strings.Repeat("a", 64)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	applyDocumentMigration(t, pool, "up")
	items, err := repo.List(ctx, documentOwner, appID)
	if err != nil || len(items) != 0 {
		t.Fatalf("legacy file advertised: %+v, %v", items, err)
	}
	records, err := repo.RecoveryRecords(ctx)
	if err != nil || len(records) != 1 || records[0].Status != "failed" || records[0].DocumentType != "" || records[0].StorageKey != "legacy-key" {
		t.Fatalf("legacy metadata lost: %+v, %v", records, err)
	}
	applyDocumentMigration(t, pool, "down")
	applyDocumentMigration(t, pool, "up")
	if _, err := repo.Reserve(ctx, documentOwner, documentMetadata(appID)); err != nil {
		t.Fatalf("cannot reserve after reapply: %v", err)
	}
	for _, mutation := range []string{
		"document_type=NULL", "document_type='UNKNOWN'", "size_bytes=5242881", "content_type='text/html'", "storage_key='../unsafe'",
	} {
		metadata := documentMetadata(appID)
		if _, err := repo.Reserve(ctx, documentOwner, metadata); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE documents SET "+mutation+" WHERE id=$1", metadata.DocumentID); err == nil {
			t.Fatalf("migration accepted invalid active metadata: %s", mutation)
		}
	}
}

func syntheticDocument(t *testing.T, format string) ([]byte, string) {
	t.Helper()
	if format == "pdf" {
		return []byte("%PDF-1.4\n% synthetic test document\n%%EOF\n"), "statement.pdf"
	}
	var content bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if format == "png" {
		if err := png.Encode(&content, picture); err != nil {
			t.Fatal(err)
		}
		return content.Bytes(), "statement.png"
	}
	if err := jpeg.Encode(&content, picture, nil); err != nil {
		t.Fatal(err)
	}
	return content.Bytes(), "statement.jpeg"
}

func TestDocumentServicePostgresAndLocalStorage(t *testing.T) {
	for _, format := range []string{"pdf", "png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			_, repo, appID := documentFixture(t)
			path := t.TempDir()
			store, err := document.NewLocalStorage(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if store != nil {
					store.Close()
				}
			}()
			service := document.NewService(repo, store)
			ctx := context.Background()
			if err := service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			content, name := syntheticDocument(t, format)
			metadata, err := service.Upload(ctx, documentOwner, appID, document.BankStatement, name, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("upload %s: %v", format, err)
			}
			if metadata.Status != "ready" || metadata.SizeBytes != int64(len(content)) {
				t.Fatalf("incorrect metadata: %+v", metadata)
			}
			duplicate, err := service.Upload(ctx, documentOwner, appID, document.BankStatement, name, bytes.NewReader(content))
			if err != nil || duplicate.DocumentID == metadata.DocumentID {
				t.Fatalf("repeated upload: %+v, %v", duplicate, err)
			}
			items, err := service.List(ctx, documentOwner, appID)
			if err != nil || len(items) != 2 {
				t.Fatalf("list: %+v, %v", items, err)
			}
			_, file, err := service.Download(ctx, documentOwner, appID, metadata.DocumentID)
			if err != nil {
				t.Fatalf("download: %v", err)
			}
			got, err := io.ReadAll(file)
			file.Close()
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("downloaded file differs: %v", err)
			}
			if _, _, err := service.Download(ctx, documentOtherOwner, appID, metadata.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
				t.Fatalf("foreign read: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = nil
			store, err = document.NewLocalStorage(path)
			if err != nil {
				t.Fatal(err)
			}
			service = document.NewService(repo, store)
			if err := service.Recover(ctx); err != nil {
				t.Fatalf("reopen recovery: %v", err)
			}
			_, file, err = service.Download(ctx, documentOwner, appID, metadata.DocumentID)
			if err != nil {
				t.Fatalf("document did not survive reopen: %v", err)
			}
			got, err = io.ReadAll(file)
			file.Close()
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("reopened contents changed: %v", err)
			}
		})
	}
}

func TestDocumentServiceBankAccessBeginsAfterSubmit(t *testing.T) {
	pool, apps := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, apps, draftOwner, draftFields())
	otherDraft := createDraft(t, apps, draftOwner, draftFields())
	store, err := document.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := NewDocumentRepository(pool, 5*time.Second)
	service := document.NewService(repo, store)
	content, name := syntheticDocument(t, "pdf")
	metadata, err := service.Upload(ctx, documentOwner, draft.ApplicationID, document.BankStatement, name, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []document.Scope{documentEmployee, documentOtherOwner} {
		if _, err := service.List(ctx, scope, draft.ApplicationID); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("draft document list exposed to %s: %v", scope.Role, err)
		}
		if _, _, err := service.Download(ctx, scope, draft.ApplicationID, metadata.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("draft document exposed to %s: %v", scope.Role, err)
		}
	}
	assertReadable := func(scope document.Scope) {
		t.Helper()
		items, err := service.List(ctx, scope, draft.ApplicationID)
		if err != nil || len(items) != 1 || items[0].DocumentID != metadata.DocumentID {
			t.Fatalf("document list for %s: %+v %v", scope.Role, items, err)
		}
		_, file, err := service.Download(ctx, scope, draft.ApplicationID, metadata.DocumentID)
		if err != nil {
			t.Fatalf("document download for %s: %v", scope.Role, err)
		}
		got, err := io.ReadAll(file)
		file.Close()
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("document contents for %s: %v", scope.Role, err)
		}
	}
	assertReadable(documentOwner)
	result, err := NewSubmissionRepository(pool, 5*time.Second).Submit(ctx, draftOwner, draft.ApplicationID, draft.Version, []string{metadata.DocumentID})
	if err != nil || result.Status != application.Submitted {
		t.Fatalf("real submission: %+v %v", result, err)
	}
	for _, status := range []application.Status{application.Submitted, application.Processing} {
		if status == application.Processing {
			job, err := NewPrecheckQueue(pool, 5*time.Second).Claim(ctx, time.Minute)
			if err != nil || job == nil || job.ApplicationID != draft.ApplicationID {
				t.Fatalf("start real processing: %+v %v", job, err)
			}
		}
		assertReadable(documentOwner)
		assertReadable(documentEmployee)
		if _, err := service.List(ctx, documentOtherOwner, draft.ApplicationID); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("foreign list after %s: %v", status, err)
		}
		if _, _, err := service.Download(ctx, documentOtherOwner, draft.ApplicationID, metadata.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("foreign download after %s: %v", status, err)
		}
		if _, err := service.List(ctx, documentEmployee, otherDraft.ApplicationID); !errors.Is(err, document.ErrApplicationNotFound) {
			t.Fatalf("submitting one draft exposed another draft: %v", err)
		}
		remaining, err := apps.Get(ctx, draftOwner, otherDraft.ApplicationID)
		if err != nil || remaining.Status != application.Draft || remaining.Version != otherDraft.Version || remaining.SubmittedAt != nil {
			t.Fatalf("submission changed another draft of the same product: %+v %v", remaining, err)
		}
	}
}

func TestDocumentServicePostgresRecovery(t *testing.T) {
	_, repo, appID := documentFixture(t)
	path := t.TempDir()
	store, err := document.NewLocalStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := document.NewService(repo, store)
	ctx := context.Background()
	content, name := syntheticDocument(t, "pdf")
	valid, err := service.Upload(ctx, documentOwner, appID, document.BankStatement, name, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	broken, err := service.Upload(ctx, documentOwner, appID, document.BankStatement, name, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, broken.StorageKey)); err != nil {
		t.Fatal(err)
	}
	pending := reserveDocument(t, repo, appID)
	if err := os.WriteFile(filepath.Join(path, pending.StorageKey), content, 0600); err != nil {
		t.Fatal(err)
	}
	temporary := ".upload-" + uuid.NewString() + ".tmp"
	orphan := uuid.NewString()
	for _, filename := range []string{temporary, orphan} {
		if err := os.WriteFile(filepath.Join(path, filename), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := service.List(ctx, documentOwner, appID)
	if err != nil || len(items) != 1 || items[0].DocumentID != valid.DocumentID {
		t.Fatalf("recovery exposed incomplete files: %+v, %v", items, err)
	}
	for _, filename := range []string{pending.StorageKey, temporary, orphan} {
		if _, err := os.Stat(filepath.Join(path, filename)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery left incomplete file %s: %v", filename, err)
		}
	}
}

func TestDocumentServicePostgresInsertFailureRemovesTemporaryFiles(t *testing.T) {
	pool, repo, appID := documentFixture(t)
	path := t.TempDir()
	store, err := document.NewLocalStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := document.NewService(repo, store)
	_, err = pool.Exec(context.Background(), `CREATE FUNCTION reject_upload() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic metadata failure'; END $$;
		CREATE TRIGGER reject_upload BEFORE INSERT ON documents FOR EACH ROW EXECUTE FUNCTION reject_upload()`)
	if err != nil {
		t.Fatal(err)
	}
	content, name := syntheticDocument(t, "pdf")
	if _, err := service.Upload(context.Background(), documentOwner, appID, document.BankStatement, name, bytes.NewReader(content)); !errors.Is(err, document.ErrStorage) {
		t.Fatalf("insert failure: %v", err)
	}
	names, err := store.Names()
	if err != nil || len(names) != 1 || names[0] != ".lock" {
		t.Fatalf("failed insert left files: %v, %v", names, err)
	}
	var count int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM documents").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed insert left metadata: %d, %v", count, err)
	}
}

type documentPublishFailure struct{ document.Storage }

func (documentPublishFailure) Publish(document.Staged) error { return document.ErrStorage }

func TestDocumentServicePostgresCompensatesPublishedAndTemporaryFiles(t *testing.T) {
	for _, phase := range []string{"publish", "activate"} {
		t.Run(phase, func(t *testing.T) {
			pool, repo, appID := documentFixture(t)
			store, err := document.NewLocalStorage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var storage document.Storage = store
			if phase == "publish" {
				storage = documentPublishFailure{Storage: store}
			} else {
				if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_ready_document() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN IF NEW.status='ready' THEN RAISE EXCEPTION 'synthetic activation failure'; END IF; RETURN NEW; END $$;
					CREATE TRIGGER reject_ready_document BEFORE UPDATE ON documents FOR EACH ROW EXECUTE FUNCTION reject_ready_document()`); err != nil {
					t.Fatal(err)
				}
			}
			content, name := syntheticDocument(t, "pdf")
			service := document.NewService(repo, storage)
			if _, err := service.Upload(context.Background(), documentOwner, appID, document.BankStatement, name, bytes.NewReader(content)); !errors.Is(err, document.ErrStorage) {
				t.Fatalf("%s failure: %v", phase, err)
			}
			names, err := store.Names()
			if err != nil || len(names) != 1 || names[0] != ".lock" {
				t.Fatalf("%s failure left files: %v, %v", phase, names, err)
			}
			records, err := repo.RecoveryRecords(context.Background())
			if err != nil || len(records) != 1 || records[0].Status != "failed" {
				t.Fatalf("%s failure left active metadata: %+v, %v", phase, records, err)
			}
		})
	}
}
