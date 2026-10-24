package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/document"
	"smart-credit-backend/internal/pipeline"
)

func submissionFixture(t *testing.T, fields application.Fields) (*pgxpool.Pool, *SubmissionRepository, application.Application) {
	t.Helper()
	pool, apps := applicationFixture(t)
	return pool, NewSubmissionRepository(pool, 5*time.Second), createDraft(t, apps, draftOwner, fields)
}

func assertSubmissionCounts(t *testing.T, pool *pgxpool.Pool, id string, wantVersion int64, wantEvents, wantJobs int) {
	t.Helper()
	var version int64
	var events, jobs int
	if err := pool.QueryRow(context.Background(), `SELECT version,
		(SELECT count(*) FROM application_events WHERE application_id=$1),
		(SELECT count(*) FROM jobs WHERE application_id=$1) FROM applications WHERE id=$1`, id).
		Scan(&version, &events, &jobs); err != nil {
		t.Fatal(err)
	}
	if version != wantVersion || events != wantEvents || jobs != wantJobs {
		t.Fatalf("partial or repeated submission: version=%d events=%d jobs=%d", version, events, jobs)
	}
}

func TestSubmissionRepositoryRejectsMalformedScope(t *testing.T) {
	repo := NewSubmissionRepository(nil, time.Second)
	for _, tc := range []struct {
		scope application.Scope
		id    string
	}{
		{application.Scope{}, uuid.NewString()},
		{application.Scope{UserID: demoClientID}, uuid.NewString()},
		{draftOwner, "invalid-id"},
		{draftOwner, "' OR true--"},
	} {
		if _, err := repo.Submit(context.Background(), tc.scope, tc.id, 1, nil); err != application.ErrNotFound {
			t.Fatalf("malformed identity: %v", err)
		}
	}
}

func TestSubmissionRepositoryCommitsFrozenInputsAndJob(t *testing.T) {
	pool, repo, draft := submissionFixture(t, draftFields())
	ctx := context.Background()
	result, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, draft.Version, nil)
	if err != nil || result.ApplicationID != draft.ApplicationID || result.Status != application.Submitted ||
		result.NextStage != pipeline.JobType || result.SubmittedAt.IsZero() {
		t.Fatalf("submit result: %+v %v", result, err)
	}
	assertSubmissionCounts(t, pool, draft.ApplicationID, 2, 2, 1)
	var financialJSON, productJSON []byte
	var contactName, contactRole, jobType, jobStatus string
	var maxAttempts, attempts int
	if err := pool.QueryRow(ctx, `SELECT financial_snapshot, product_snapshot, contact_name,contact_role,
		j.type,j.status,j.max_attempts,j.attempt_count FROM applications a JOIN jobs j ON j.application_id=a.id WHERE a.id=$1`,
		draft.ApplicationID).Scan(&financialJSON, &productJSON, &contactName, &contactRole,
		&jobType, &jobStatus, &maxAttempts, &attempts); err != nil {
		t.Fatal(err)
	}
	var snapshot pipeline.FinancialSnapshot
	var product credit.Product
	if err := json.Unmarshal(financialJSON, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(productJSON, &product); err != nil {
		t.Fatal(err)
	}
	if snapshot.SchemaVersion != 1 || snapshot.ApplicationID != draft.ApplicationID ||
		snapshot.UserID != draftOwner.UserID || snapshot.CompanyID != draftOwner.CompanyID ||
		!reflect.DeepEqual(snapshot.Fields, draft.Fields) || snapshot.Company.Profile.ID != draftOwner.CompanyID ||
		snapshot.Company.Profile.Financials.ExistingDebtRub != 500_000 ||
		snapshot.Company.FinancialDataSource != pipeline.TrustedFinancialSource ||
		snapshot.Company.RegistrationStatus != "active" || len(snapshot.Documents) != 0 ||
		product.ID != "turnover" || product.Version != 1 || contactName != "Иван Лавкин" ||
		contactRole != "Предприниматель" || jobType != pipeline.JobType || jobStatus != "pending" || maxAttempts != 5 || attempts != 0 {
		t.Fatalf("frozen input or job contract differs: %+v product=%+v contact=%s/%s job=%s/%s/%d/%d", snapshot,
			product, contactName, contactRole, jobType, jobStatus, maxAttempts, attempts)
	}
	apps := NewApplicationRepository(pool, time.Second)
	events, err := apps.Timeline(ctx, draftOwner, draft.ApplicationID)
	if err != nil || len(events) != 2 || events[1].EventType != "APPLICATION_SUBMITTED" ||
		events[1].Status != application.Submitted || events[1].Sequence != 2 ||
		events[1].ActorID == nil || *events[1].ActorID != draftOwner.UserID || events[1].Title == "" {
		t.Fatalf("submission history: %+v %v", events, err)
	}
	// A recreated repository/process reads the same durable result.
	repeated, err := NewSubmissionRepository(pool, time.Second).Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
	if err != nil || !reflect.DeepEqual(result, repeated) {
		t.Fatalf("idempotency after repository restart: %+v %v", repeated, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE companies SET monthly_revenue=999,registration_status='suspended' WHERE id=$1`, draftOwner.CompanyID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE credit_products SET version=version+1,annual_rate_percent=99,active=false WHERE code='turnover'`); err != nil {
		t.Fatal(err)
	}
	var financialAfter, productAfter []byte
	if err := pool.QueryRow(ctx, `SELECT financial_snapshot,product_snapshot FROM applications WHERE id=$1`, draft.ApplicationID).
		Scan(&financialAfter, &productAfter); err != nil || string(financialJSON) != string(financialAfter) || string(productJSON) != string(productAfter) {
		t.Fatal("live company/catalog edits changed frozen submission inputs")
	}
	for _, query := range []string{
		`UPDATE applications SET financial_snapshot='{}' WHERE id=$1`,
		`UPDATE applications SET product_snapshot='{}' WHERE id=$1`,
		`UPDATE applications SET submitted_at=submitted_at+interval '1 second' WHERE id=$1`,
	} {
		if _, err := pool.Exec(ctx, query, draft.ApplicationID); err == nil {
			t.Fatal("database allowed mutation of frozen submission inputs")
		}
	}
}

func TestSubmissionRepositoryValidationIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields application.Fields
		setup  string
		field  string
	}{
		{"empty", application.Fields{}, "", "productId"},
		{"financials absent", func() application.Fields { f := draftFields(); f.MonthlyRevenueRub = nil; return f }(), "", "monthlyRevenueRub"},
		{"untrusted statement required", draftFields(), `UPDATE companies SET financial_data_source='unverified'`, "documents.BANK_STATEMENT"},
		{"deactivated product", draftFields(), `UPDATE credit_products SET active=false WHERE code='turnover'`, "productId"},
		{"changed amount limit", draftFields(), `UPDATE credit_products SET max_amount=1000000 WHERE code='turnover'`, "requestedAmountRub"},
		{"changed term limit", draftFields(), `UPDATE credit_products SET max_term_months=6 WHERE code='turnover'`, "termMonths"},
		{"changed purpose", draftFields(), `UPDATE credit_products SET purposes=ARRAY['expenses'] WHERE code='turnover'`, "purpose"},
		{"young company", draftFields(), `UPDATE companies SET registration_date=current_date`, "company.registrationDate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, repo, draft := submissionFixture(t, tc.fields)
			ctx := context.Background()
			if tc.setup != "" {
				if _, err := pool.Exec(ctx, tc.setup); err != nil {
					t.Fatal(err)
				}
			}
			_, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
			var validation *pipeline.ValidationError
			if !errors.As(err, &validation) || len(validation.Details) == 0 {
				t.Fatalf("expected structured validation: %v", err)
			}
			found := false
			for _, issue := range validation.Details {
				if issue.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected field %s: %+v", tc.field, validation.Details)
			}
			assertSubmissionCounts(t, pool, draft.ApplicationID, 1, 1, 0)
			stored, err := NewApplicationRepository(pool, time.Second).Get(ctx, draftOwner, draft.ApplicationID)
			if err != nil || !reflect.DeepEqual(stored, draft) {
				t.Fatalf("failed submit changed application: %+v %v", stored, err)
			}
		})
	}
}

func TestSubmissionRepositoryOnlyReadyVerifiedDocumentsCount(t *testing.T) {
	for _, state := range []string{"pending", "failed", "ready unverified", "ready verified"} {
		t.Run(state, func(t *testing.T) {
			pool, repo, draft := submissionFixture(t, draftFields())
			ctx := context.Background()
			if _, err := pool.Exec(ctx, `UPDATE companies SET financial_data_source='unverified' WHERE id=$1`, draftOwner.CompanyID); err != nil {
				t.Fatal(err)
			}
			docs := NewDocumentRepository(pool, time.Second)
			var meta document.Metadata
			if state == "pending" {
				meta = reserveDocument(t, docs, draft.ApplicationID)
			} else {
				meta = activateDocument(t, docs, draft.ApplicationID)
				if state == "failed" {
					if err := docs.Invalidate(ctx, meta.DocumentID); err != nil {
						t.Fatal(err)
					}
				}
			}
			ids := []string{meta.DocumentID}
			if state == "ready unverified" {
				ids = nil
			}
			_, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, ids)
			if state != "ready verified" {
				var validation *pipeline.ValidationError
				if !errors.As(err, &validation) {
					t.Fatalf("incomplete upload satisfied required document: %v", err)
				}
				assertSubmissionCounts(t, pool, draft.ApplicationID, 1, 1, 0)
				return
			}
			if err != nil {
				t.Fatalf("verified ready upload did not satisfy policy: %v", err)
			}
			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT financial_snapshot FROM applications WHERE id=$1`, draft.ApplicationID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var snapshot pipeline.FinancialSnapshot
			if err := json.Unmarshal(raw, &snapshot); err != nil || len(snapshot.Documents) != 1 || snapshot.Documents[0].DocumentID != meta.DocumentID {
				t.Fatalf("required document not frozen: %+v %v", snapshot.Documents, err)
			}
		})
	}
}

func TestSubmissionRepositoryInvestmentRequiresInvoice(t *testing.T) {
	fields := draftFields()
	fields.ProductID, fields.Purpose = draftValue("investment"), draftValue("equipment")
	pool, repo, draft := submissionFixture(t, fields)
	ctx := context.Background()
	_, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
	var validation *pipeline.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("investment accepted without invoice: %v", err)
	}
	docs := NewDocumentRepository(pool, time.Second)
	meta := documentMetadata(draft.ApplicationID)
	meta.DocumentType = document.EquipmentInvoice
	if _, err := docs.Reserve(ctx, documentOwner, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := docs.Activate(ctx, documentOwner, draft.ApplicationID, meta.DocumentID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, []string{meta.DocumentID}); err != nil {
		t.Fatalf("ready investment invoice rejected: %v", err)
	}
}

func TestSubmissionRepositoryCannotUseAnotherApplicationsDocument(t *testing.T) {
	pool, repo, draft := submissionFixture(t, draftFields())
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE companies SET financial_data_source='unverified' WHERE id=$1`, draftOwner.CompanyID); err != nil {
		t.Fatal(err)
	}
	otherDraft := createDraft(t, NewApplicationRepository(pool, time.Second), draftOwner, draftFields())
	foreign := activateDocument(t, NewDocumentRepository(pool, time.Second), otherDraft.ApplicationID)
	_, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, []string{foreign.DocumentID, uuid.NewString()})
	var validation *pipeline.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("foreign document satisfied required statement: %v", err)
	}
	assertSubmissionCounts(t, pool, draft.ApplicationID, 1, 1, 0)
}

func TestSubmissionRepositoryConcurrentSubmitIsIdempotent(t *testing.T) {
	pool, repo, draft := submissionFixture(t, draftFields())
	ctx := context.Background()
	const requests = 20
	results := make(chan pipeline.SubmitResult, requests)
	errors := concurrently(requests, func() error {
		result, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
		if err == nil {
			results <- result
		}
		return err
	})
	for _, err := range errors {
		if err != nil {
			t.Fatalf("concurrent submit failed: %v", err)
		}
	}
	close(results)
	var first *pipeline.SubmitResult
	for result := range results {
		if first == nil {
			first = &result
		} else if !reflect.DeepEqual(result, *first) {
			t.Fatalf("idempotent response changed: %+v versus %+v", result, *first)
		}
	}
	assertSubmissionCounts(t, pool, draft.ApplicationID, 2, 2, 1)
}

func TestSubmissionRepositoryPatchRacePreservesNewestFields(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	apps := NewApplicationRepository(pool, 5*time.Second)
	repo := NewSubmissionRepository(pool, 5*time.Second)
	for range 10 {
		verifyFixtureFinancials(t, pool, draftOwner.CompanyID, draftFields())
		draft := createDraft(t, apps, draftOwner, draftFields())
		updated := draftFields()
		updated.MonthlyRevenueRub = draftValue(int64(3_000_000))
		start := make(chan struct{})
		var patchErr, submitErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, patchErr = apps.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 1, updated)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, submitErr = repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
		}()
		close(start)
		wg.Wait()
		switch {
		case submitErr == nil && errors.Is(patchErr, application.ErrInvalidStatus):
			assertSubmissionCounts(t, pool, draft.ApplicationID, 2, 2, 1)
		case patchErr == nil && errors.Is(submitErr, application.ErrVersionConflict):
			assertSubmissionCounts(t, pool, draft.ApplicationID, 2, 2, 0)
			verifyFixtureFinancials(t, pool, draftOwner.CompanyID, updated)
			if _, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 2, nil); err != nil {
				t.Fatalf("retry after patch: %v", err)
			}
		default:
			t.Fatalf("invalid concurrent outcome: patch=%v submit=%v", patchErr, submitErr)
		}
		var frozenRevenue int64
		if err := pool.QueryRow(ctx, `SELECT (financial_snapshot->'fields'->>'monthlyRevenueRub')::bigint
			FROM applications WHERE id=$1`, draft.ApplicationID).Scan(&frozenRevenue); err != nil {
			t.Fatal(err)
		}
		want := *draft.MonthlyRevenueRub
		if patchErr == nil {
			want = *updated.MonthlyRevenueRub
		}
		if frozenRevenue != want {
			t.Fatalf("submission lost committed financial inputs: %d want %d", frozenRevenue, want)
		}
	}
}

func TestSubmissionRepositoryErrorsAndRollback(t *testing.T) {
	for _, failure := range []string{"event insert", "job insert"} {
		t.Run(failure, func(t *testing.T) {
			pool, repo, draft := submissionFixture(t, draftFields())
			ctx := context.Background()
			if failure == "event insert" {
				installEventFailure(t, pool)
			} else if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_submission_job() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'Synthetic job persistence failure'; END; $$;
				CREATE TRIGGER fail_submission_job BEFORE INSERT ON jobs FOR EACH ROW EXECUTE FUNCTION fail_submission_job()`); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil); err != pipeline.ErrStorage {
				t.Fatalf("unsanitized persistence failure: %v", err)
			}
			assertSubmissionCounts(t, pool, draft.ApplicationID, 1, 1, 0)
			var status string
			var pristine bool
			if err := pool.QueryRow(ctx, `SELECT status, financial_snapshot IS NULL AND product_snapshot IS NULL AND submitted_at IS NULL
				FROM applications WHERE id=$1`, draft.ApplicationID).Scan(&status, &pristine); err != nil || status != "draft" || !pristine {
				t.Fatalf("partial submission survived rollback: status=%s pristine=%v err=%v", status, pristine, err)
			}
		})
	}
	pool, repo, draft := submissionFixture(t, draftFields())
	ctx := context.Background()
	for _, scope := range []application.Scope{otherDraftOwner, {UserID: draftOwner.UserID, CompanyID: otherDraftOwner.CompanyID}} {
		if _, err := repo.Submit(ctx, scope, draft.ApplicationID, 1, nil); err != application.ErrNotFound {
			t.Fatalf("foreign application disclosed: %v", err)
		}
	}
	if _, err := repo.Submit(ctx, draftOwner, uuid.NewString(), 1, nil); err != application.ErrNotFound {
		t.Fatalf("missing application: %v", err)
	}
	if _, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 7, nil); err != application.ErrVersionConflict {
		t.Fatalf("stale version: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repo.Submit(canceled, draftOwner, draft.ApplicationID, 1, nil); err != pipeline.ErrStorage {
		t.Fatalf("canceled operation: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM applications WHERE id=$1 FOR UPDATE`, draft.ApplicationID); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := NewSubmissionRepository(pool, 50*time.Millisecond).Submit(ctx, draftOwner, draft.ApplicationID, 1, nil); err != pipeline.ErrStorage {
		t.Fatalf("bounded lock wait: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("submission lock wait exceeded repository timeout")
	}
}

func TestSubmissionRepositoryIdempotentProcessingAndIncompatibleStates(t *testing.T) {
	for _, state := range []application.Status{application.Submitted, application.Processing, application.ManualReview, application.Approved, application.Rejected, application.CounterOffer, application.Closed} {
		t.Run(string(state), func(t *testing.T) {
			pool, repo, draft := submissionFixture(t, draftFields())
			ctx := context.Background()
			first, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE applications SET status=$2 WHERE id=$1`, draft.ApplicationID, state); err != nil {
				t.Fatal(err)
			}
			result, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
			if state == application.Submitted || state == application.Processing {
				if err != nil || result.Status != state || !result.SubmittedAt.Equal(first.SubmittedAt) {
					t.Fatalf("idempotent active state: %+v %v", result, err)
				}
			} else if err != application.ErrInvalidStatus {
				t.Fatalf("incompatible credit status resubmitted: %+v %v", result, err)
			}
			assertSubmissionCounts(t, pool, draft.ApplicationID, 2, 2, 1)
		})
	}
}

func TestSubmissionRepositoryReplayReportsAwaitingScoring(t *testing.T) {
	pool, repo, draft := submissionFixture(t, draftFields())
	ctx := context.Background()
	first, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE applications SET status='processing' WHERE id=$1`, draft.ApplicationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='awaiting_scoring',precheck_results='{}' WHERE application_id=$1`, draft.ApplicationID); err != nil {
		t.Fatal(err)
	}
	result, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil)
	if err != nil || result.Status != application.Processing || result.NextStage != pipeline.AwaitingScoring || !result.SubmittedAt.Equal(first.SubmittedAt) {
		t.Fatalf("awaiting scoring replay: %+v %v", result, err)
	}
	assertSubmissionCounts(t, pool, draft.ApplicationID, 2, 2, 1)
}

func TestSubmissionRepositoryCannotReplayWithoutOriginalJob(t *testing.T) {
	pool, repo, draft := submissionFixture(t, draftFields())
	draftStatusFixture(t, pool, draft.ApplicationID, application.Submitted)
	if _, err := repo.Submit(context.Background(), draftOwner, draft.ApplicationID, 1, nil); err != application.ErrInvalidStatus {
		t.Fatalf("legacy state without original job was silently reprocessed: %v", err)
	}
	assertSubmissionCounts(t, pool, draft.ApplicationID, 1, 1, 0)
}

func applySubmissionMigration(t *testing.T, pool *pgxpool.Pool, direction string) {
	t.Helper()
	data, err := os.ReadFile("../../migrations/000008_submission_prechecks." + direction + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(data)); err != nil {
		t.Fatalf("submission migration %s: %v", direction, err)
	}
}

func TestSubmissionMigrationCycle(t *testing.T) {
	pool, _, draft := submissionFixture(t, draftFields())
	applySubmissionMigration(t, pool, "down")
	applySubmissionMigration(t, pool, "up")
	if _, err := NewSubmissionRepository(pool, time.Second).Submit(context.Background(), draftOwner, draft.ApplicationID, 1, nil); err != nil {
		t.Fatalf("cannot submit after migration cycle: %v", err)
	}
	// A pending task is representable in the old schema and must survive rollback.
	applySubmissionMigration(t, pool, "down")
	applySubmissionMigration(t, pool, "up")
	assertSubmissionCounts(t, pool, draft.ApplicationID, 2, 2, 1)
}

func TestSubmissionMigrationRejectsLossyDowngrade(t *testing.T) {
	pool, repo, draft := submissionFixture(t, draftFields())
	ctx := context.Background()
	if _, err := repo.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='awaiting_scoring',precheck_results='{}' WHERE application_id=$1`, draft.ApplicationID); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/000008_submission_prechecks.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, string(migration)); err == nil {
		t.Fatal("downgrade silently discarded durable precheck result")
	}
	if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var status string
	var results []byte
	if err := pool.QueryRow(ctx, `SELECT status,precheck_results FROM jobs WHERE application_id=$1`, draft.ApplicationID).Scan(&status, &results); err != nil || status != pipeline.AwaitingScoring || string(results) != "{}" {
		t.Fatalf("failed downgrade lost result: %s %s %v", status, results, err)
	}
}
