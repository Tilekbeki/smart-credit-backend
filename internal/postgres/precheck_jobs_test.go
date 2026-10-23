package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/pipeline"
)

func precheckFixture(t *testing.T) (*pgxpool.Pool, *PrecheckQueue, string) {
	t.Helper()
	pool, submit, draft := submissionFixture(t, draftFields())
	if _, err := submit.Submit(context.Background(), draftOwner, draft.ApplicationID, 1, nil); err != nil {
		t.Fatalf("submit precheck fixture: %v", err)
	}
	return pool, NewPrecheckQueue(pool, 5*time.Second), draft.ApplicationID
}

func claimPrecheck(t *testing.T, queue *PrecheckQueue) pipeline.Job {
	t.Helper()
	job, err := queue.Claim(context.Background(), time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim precheck: job=%+v err=%v", job, err)
	}
	return *job
}

func runPrecheck(t *testing.T, job pipeline.Job) pipeline.Results {
	t.Helper()
	results, err := pipeline.NewSimulator().Run(context.Background(), job.Snapshot)
	if err != nil {
		t.Fatalf("simulate precheck: %v", err)
	}
	return results
}

func TestPrecheckQueuePersistsResultsAndSystemHistory(t *testing.T) {
	pool, queue, appID := precheckFixture(t)
	ctx := context.Background()
	// Live edits cannot change the financial profile already frozen at submit.
	if _, err := pool.Exec(ctx, `UPDATE companies SET existing_debt=999, monthly_debt_payments=1,
		overdue_count=8 WHERE id=$1`, draftOwner.CompanyID); err != nil {
		t.Fatal(err)
	}
	job := claimPrecheck(t, queue)
	if job.ApplicationID != appID || job.Attempt != 1 || !validUUID(job.LeaseToken) || job.Snapshot.SchemaVersion != 1 {
		t.Fatalf("claimed job differs: %+v", job)
	}
	results := runPrecheck(t, job)
	if results.CreditHistory.ExistingDebtRub != 500_000 || results.CreditHistory.ExistingMonthlyPaymentsRub != 90_000 ||
		results.CreditHistory.OverdueCount != 0 {
		t.Fatal("prechecks used mutable live company data")
	}
	if err := queue.Complete(ctx, job, results); err != nil {
		t.Fatal(err)
	}
	var jobStatus, appStatus string
	var resultsJSON []byte
	var leaseUntil, finishedAt *time.Time
	var errorCode *string
	var version int64
	if err := pool.QueryRow(ctx, `SELECT j.status,j.precheck_results,j.lease_until,j.finished_at,
		a.status,a.version,a.processing_error FROM jobs j JOIN applications a ON a.id=j.application_id WHERE j.id=$1`, job.ID).
		Scan(&jobStatus, &resultsJSON, &leaseUntil, &finishedAt, &appStatus, &version, &errorCode); err != nil {
		t.Fatal(err)
	}
	var stored pipeline.Results
	if err := json.Unmarshal(resultsJSON, &stored); err != nil {
		t.Fatal(err)
	}
	if jobStatus != pipeline.AwaitingScoring || appStatus != "processing" || version != 3 || leaseUntil != nil ||
		finishedAt != nil || errorCode != nil || stored.SchemaVersion != 1 || stored.CreditHistory.ExistingDebtRub != 500_000 {
		t.Fatalf("durable waiting state differs: job=%s application=%s version=%d results=%+v", jobStatus, appStatus, version, stored)
	}
	if err := queue.Complete(ctx, job, results); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatalf("duplicate completion must lose its lease: %v", err)
	}
	if claimed, err := NewPrecheckQueue(pool, time.Second).Claim(ctx, time.Minute); err != nil || claimed != nil {
		t.Fatal("awaiting_scoring was claimed again after queue recreation")
	}
	events, err := NewApplicationRepository(pool, time.Second).Timeline(ctx, draftOwner, appID)
	if err != nil || len(events) != 6 {
		t.Fatalf("expected six durable timeline events: %+v %v", events, err)
	}
	wanted := []string{"draft_created", "APPLICATION_SUBMITTED", "PROCESSING_STARTED", "COMPANY_CHECK_COMPLETED", "CREDIT_HISTORY_CHECK_COMPLETED", "PRECHECKS_COMPLETED"}
	for index, event := range events {
		if event.EventType != wanted[index] || event.Sequence != int64(index+1) || event.Title == "" || event.CreatedAt.IsZero() {
			t.Fatalf("timeline order or fields differ: %+v", event)
		}
		if index >= 2 && (event.ActorID != nil || event.Status != application.Processing) {
			t.Fatal("system events must have no user actor and preserve processing status")
		}
	}
}

func TestPrecheckQueueSkipLockedAndConcurrentClaims(t *testing.T) {
	pool, queue, firstID := precheckFixture(t)
	ctx := context.Background()
	for range 7 {
		app := createDraft(t, NewApplicationRepository(pool, time.Second), draftOwner, draftFields())
		if _, err := NewSubmissionRepository(pool, time.Second).Submit(ctx, draftOwner, app.ApplicationID, 1, nil); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var lockedJob string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM jobs WHERE application_id=$1 FOR UPDATE`, firstID).Scan(&lockedJob); err != nil {
		t.Fatal(err)
	}
	// A locked oldest job must not block another processing slot.
	claimCtx, cancel := context.WithTimeout(ctx, time.Second)
	claimed, err := queue.Claim(claimCtx, time.Minute)
	cancel()
	if err != nil || claimed == nil || claimed.ID == lockedJob {
		t.Fatalf("SKIP LOCKED did not skip an unavailable row: %+v %v", claimed, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	jobs := make(chan pipeline.Job, 16)
	errorsChannel := make(chan error, 16)
	var claimers sync.WaitGroup
	for range 16 {
		claimers.Go(func() {
			job, err := queue.Claim(ctx, time.Minute)
			if err != nil {
				errorsChannel <- err
			} else if job != nil {
				jobs <- *job
			}
		})
	}
	claimers.Wait()
	close(jobs)
	close(errorsChannel)
	for err := range errorsChannel {
		t.Errorf("concurrent claim: %v", err)
	}
	seen := map[string]bool{claimed.ID: true}
	all := []pipeline.Job{*claimed}
	for job := range jobs {
		if seen[job.ID] {
			t.Fatal("same job claimed by two workers")
		}
		seen[job.ID] = true
		all = append(all, job)
	}
	if len(seen) != 8 {
		t.Fatalf("expected all eight jobs, got %d", len(seen))
	}
	for _, job := range all {
		if err := queue.Complete(ctx, job, runPrecheck(t, job)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrecheckQueueLeaseRecoveryFencesOldWorker(t *testing.T) {
	pool, queue, appID := precheckFixture(t)
	ctx := context.Background()
	old := claimPrecheck(t, queue)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, old, runPrecheck(t, old)); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatalf("expired owner completed a job: %v", err)
	}
	if err := queue.Fail(ctx, old, "prechecks_failed", 0); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatalf("expired owner failed a job: %v", err)
	}
	restarted := NewPrecheckQueue(pool, 5*time.Second)
	current := claimPrecheck(t, restarted)
	if current.ID != old.ID || current.LeaseToken == old.LeaseToken || current.Attempt != 2 {
		t.Fatalf("expired lease was not recovered with a fresh token: %+v", current)
	}
	if err := queue.Complete(ctx, old, runPrecheck(t, old)); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatalf("previous worker overwrote new lease: %v", err)
	}
	if err := queue.Fail(ctx, old, "prechecks_failed", 0); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatalf("previous worker cleared new lease: %v", err)
	}
	if err := restarted.Complete(ctx, current, runPrecheck(t, current)); err != nil {
		t.Fatal(err)
	}
	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM application_events WHERE application_id=$1`, appID).Scan(&eventCount); err != nil || eventCount != 6 {
		t.Fatalf("retry duplicated processing history: %d %v", eventCount, err)
	}
}

func TestPrecheckQueueRetriesAreDelayedAndBounded(t *testing.T) {
	t.Run("retry then success", func(t *testing.T) {
		pool, queue, appID := precheckFixture(t)
		ctx := context.Background()
		job := claimPrecheck(t, queue)
		if err := queue.Fail(ctx, job, "sensitive details must not persist", 20*time.Second); err != nil {
			t.Fatal(err)
		}
		if next, err := queue.Claim(ctx, time.Minute); err != nil || next != nil {
			t.Fatal("retry was claimed before available_at")
		}
		var status, code, appCode string
		var delay float64
		if err := pool.QueryRow(ctx, `SELECT j.status,j.last_error,a.processing_error,
			extract(epoch FROM j.available_at-clock_timestamp()) FROM jobs j JOIN applications a ON a.id=j.application_id WHERE j.id=$1`, job.ID).
			Scan(&status, &code, &appCode, &delay); err != nil {
			t.Fatal(err)
		}
		if status != "pending" || code != "prechecks_failed" || appCode != code || delay < 18 {
			t.Fatalf("retry state differs: %s %s %s %f", status, code, appCode, delay)
		}
		if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, job.ID); err != nil {
			t.Fatal(err)
		}
		job = claimPrecheck(t, queue)
		if job.Attempt != 2 {
			t.Fatal("retry did not increment attempt count")
		}
		if err := queue.Complete(ctx, job, runPrecheck(t, job)); err != nil {
			t.Fatal(err)
		}
		var cleared bool
		if err := pool.QueryRow(ctx, `SELECT processing_error IS NULL FROM applications WHERE id=$1`, appID).Scan(&cleared); err != nil || !cleared {
			t.Fatal("successful retry did not clear technical diagnostic")
		}
	})
	t.Run("five attempts", func(t *testing.T) {
		pool, queue, appID := precheckFixture(t)
		ctx := context.Background()
		for attempt := 1; attempt <= 5; attempt++ {
			job := claimPrecheck(t, queue)
			if job.Attempt != attempt {
				t.Fatalf("attempt=%d wanted=%d", job.Attempt, attempt)
			}
			if err := queue.Fail(ctx, job, "prechecks_timeout", 0); err != nil {
				t.Fatal(err)
			}
		}
		assertPrecheckExhausted(t, pool, appID, "prechecks_timeout")
		if job, err := queue.Claim(ctx, time.Minute); err != nil || job != nil {
			t.Fatal("exhausted task was retried")
		}
	})
	t.Run("final attempt crashed", func(t *testing.T) {
		pool, queue, appID := precheckFixture(t)
		ctx := context.Background()
		job := claimPrecheck(t, queue)
		if _, err := pool.Exec(ctx, `UPDATE jobs SET attempt_count=max_attempts,
			lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
			t.Fatal(err)
		}
		if next, err := NewPrecheckQueue(pool, time.Second).Claim(ctx, time.Minute); err != nil || next != nil {
			t.Fatal("final crashed attempt must be marked failed without exceeding max_attempts")
		}
		assertPrecheckExhausted(t, pool, appID, "attempts_exhausted")
	})
}

func assertPrecheckExhausted(t *testing.T, pool *pgxpool.Pool, appID, code string) {
	t.Helper()
	var status, appStatus, errorCode, appErrorCode string
	var attempts int
	var finished *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT j.status,j.attempt_count,j.last_error,j.finished_at,
		a.status,a.processing_error FROM jobs j JOIN applications a ON a.id=j.application_id WHERE a.id=$1`, appID).
		Scan(&status, &attempts, &errorCode, &finished, &appStatus, &appErrorCode); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != 5 || finished == nil || appStatus != "processing" || errorCode != code || appErrorCode != code {
		t.Fatalf("unexpected exhaustion state: %s %d %s %s %s", status, attempts, errorCode, appStatus, appErrorCode)
	}
}

func TestPrecheckQueueBusinessFailureStillAwaitsScoring(t *testing.T) {
	pool, submit, draft := submissionFixture(t, draftFields())
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE companies SET registration_status='suspended' WHERE id=$1`, draftOwner.CompanyID); err != nil {
		t.Fatal(err)
	}
	if _, err := submit.Submit(ctx, draftOwner, draft.ApplicationID, 1, nil); err != nil {
		t.Fatal(err)
	}
	queue := NewPrecheckQueue(pool, time.Second)
	job := claimPrecheck(t, queue)
	result := runPrecheck(t, job)
	if result.CompanyCheck.Status != "failed" || len(result.CompanyCheck.Problems) != 1 || result.CompanyCheck.Problems[0].Code != "registration_active" {
		t.Fatal("negative FNS simulation was not preserved")
	}
	if err := queue.Complete(ctx, job, result); err != nil {
		t.Fatal(err)
	}
	var state, appState string
	if err := pool.QueryRow(ctx, `SELECT j.status,a.status FROM jobs j JOIN applications a ON a.id=j.application_id WHERE j.id=$1`, job.ID).
		Scan(&state, &appState); err != nil || state != pipeline.AwaitingScoring || appState != "processing" {
		t.Fatal("negative business result became a credit decision")
	}
}

func TestPrecheckQueueCompleteRollsBackWithHistoryFailure(t *testing.T) {
	pool, queue, _ := precheckFixture(t)
	ctx := context.Background()
	job := claimPrecheck(t, queue)
	_, err := pool.Exec(ctx, `CREATE FUNCTION reject_precheck_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.event_type='PRECHECKS_COMPLETED' THEN RAISE EXCEPTION 'test write failure'; END IF; RETURN NEW; END; $$;
		CREATE TRIGGER reject_precheck_event BEFORE INSERT ON application_events FOR EACH ROW EXECUTE FUNCTION reject_precheck_event()`)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, job, runPrecheck(t, job)); !errors.Is(err, pipeline.ErrStorage) {
		t.Fatalf("expected storage error, got %v", err)
	}
	var status string
	var empty bool
	var count int
	if err := pool.QueryRow(ctx, `SELECT status,precheck_results IS NULL,
		(SELECT count(*) FROM application_events WHERE application_id=$2) FROM jobs WHERE id=$1`, job.ID, job.ApplicationID).
		Scan(&status, &empty, &count); err != nil {
		t.Fatal(err)
	}
	if status != "running" || !empty || count != 3 {
		t.Fatal("completion/history were not rolled back atomically")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_precheck_event ON application_events`); err != nil {
		t.Fatal(err)
	}
	if err := queue.Complete(ctx, job, runPrecheck(t, job)); err != nil {
		t.Fatal("failed transaction could not be safely repeated")
	}
}

func TestPrecheckWorkerRunsDurableQueueAndStopsGracefully(t *testing.T) {
	pool, queue, appID := precheckFixture(t)
	config := pipeline.DefaultConfig()
	config.PollInterval = 5 * time.Millisecond
	worker, err := pipeline.NewWorker(queue, pipeline.NewSimulator(), config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("worker did not stop")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var status string
		if err := pool.QueryRow(context.Background(), `SELECT status FROM jobs WHERE application_id=$1`, appID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == pipeline.AwaitingScoring {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never persisted preliminary checks")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPrecheckQueueStorageFailureAndMalformedInput(t *testing.T) {
	queue := NewPrecheckQueue(nil, time.Second)
	ctx := context.Background()
	for _, lease := range []time.Duration{0, -time.Second, 25 * time.Hour} {
		if _, err := queue.Claim(ctx, lease); !errors.Is(err, pipeline.ErrStorage) {
			t.Fatal("invalid lease accepted")
		}
	}
	if err := queue.Complete(ctx, pipeline.Job{}, pipeline.Results{SchemaVersion: 1}); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("malformed completion accepted")
	}
	if err := queue.Fail(ctx, pipeline.Job{}, "private", 0); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("malformed failure accepted")
	}
	pool, realQueue, _ := precheckFixture(t)
	job := claimPrecheck(t, realQueue)
	pool.Close()
	if _, err := realQueue.Claim(ctx, time.Second); !errors.Is(err, pipeline.ErrStorage) {
		t.Fatal("closed connection was not reported as a storage failure")
	}
	if err := realQueue.Complete(ctx, job, runPrecheck(t, job)); !errors.Is(err, pipeline.ErrStorage) {
		t.Fatal("completion storage failure was not sanitized")
	}
	if err := realQueue.Fail(ctx, job, "private", 0); !errors.Is(err, pipeline.ErrStorage) {
		t.Fatal("failure storage error was not sanitized")
	}
}

func TestPrecheckWorkerMalformedSnapshotConsumesBoundedAttempts(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{"invalid field type", `{"schemaVersion":"invalid"}`},
		{"incomplete object", `{"schemaVersion":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, apps := applicationFixture(t)
			draft := createDraft(t, apps, draftOwner, draftFields())
			ctx := context.Background()
			_, err := pool.Exec(ctx, `UPDATE applications SET status='submitted',
				product_id=(SELECT id FROM credit_products WHERE code='turnover'),
				contact_name='Synthetic owner',contact_role='Owner',submitted_at=clock_timestamp(),
				financial_snapshot=$2,product_snapshot='{}' WHERE id=$1;
				`, draft.ApplicationID, []byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO jobs(application_id,type,max_attempts) VALUES($1,'prechecks',2)`, draft.ApplicationID); err != nil {
				t.Fatal(err)
			}
			queue := NewPrecheckQueue(pool, time.Second)
			config := pipeline.DefaultConfig()
			config.PollInterval, config.RetryBaseDelay = time.Millisecond, time.Millisecond
			worker, err := pipeline.NewWorker(queue, pipeline.NewSimulator(), config, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- worker.Run(workerCtx) }()
			defer func() { cancel(); <-done }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				var state string
				var attempts int
				var code *string
				var noResults bool
				if err := pool.QueryRow(ctx, `SELECT status,attempt_count,last_error,precheck_results IS NULL
					FROM jobs WHERE application_id=$1`, draft.ApplicationID).Scan(&state, &attempts, &code, &noResults); err != nil {
					t.Fatal(err)
				}
				if state == "failed" {
					if attempts != 2 || code == nil || *code != "invalid_snapshot" || !noResults {
						t.Fatalf("malformed snapshot did not fail safely: %s %d %v %v", state, attempts, code, noResults)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("malformed snapshot entered an unbounded retry loop")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestPrecheckSnapshotDecoderRejectsAbsentFinancials(t *testing.T) {
	valid := []byte(`{"schemaVersion":1,"company":{"profile":{"financials":{
		"monthlyRevenueRub":0,"monthlyExpensesRub":0,"existingDebtRub":0,
		"existingMonthlyPaymentsRub":0,"overdueCount":0}}}}`)
	if decodePrecheckSnapshot(valid).SchemaVersion != 1 {
		t.Fatal("explicit zero financials must be preserved")
	}
	for _, malformed := range []string{
		`{"schemaVersion":1,"company":{"profile":{}}}`,
		`{"schemaVersion":1,"company":{"profile":{"financials":{"existingDebtRub":0}}}}`,
		`{"schemaVersion":1,"company":{"profile":{"financials":{"monthlyRevenueRub":0,"monthlyExpensesRub":0,"existingDebtRub":null,"existingMonthlyPaymentsRub":0,"overdueCount":0}}}}`,
	} {
		if decodePrecheckSnapshot([]byte(malformed)).SchemaVersion != 0 {
			t.Fatal("missing or null financial fields were converted into zero values")
		}
	}
}

type precheckBlockingProcessor struct{ started chan struct{} }

func (p *precheckBlockingProcessor) Run(ctx context.Context, _ pipeline.FinancialSnapshot) (pipeline.Results, error) {
	close(p.started)
	<-ctx.Done()
	return pipeline.Results{}, ctx.Err()
}

func TestPrecheckWorkerShutdownLeaseIsRecoveredAfterRestart(t *testing.T) {
	pool, queue, appID := precheckFixture(t)
	processor := &precheckBlockingProcessor{started: make(chan struct{})}
	config := pipeline.DefaultConfig()
	config.PollInterval = time.Millisecond
	worker, err := pipeline.NewWorker(queue, processor, config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	defer cancel()
	select {
	case <-processor.started:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not begin the pending task")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel processing")
	}
	var state string
	var attempts int
	var noError bool
	if err := pool.QueryRow(context.Background(), `SELECT status,attempt_count,last_error IS NULL FROM jobs WHERE application_id=$1`, appID).
		Scan(&state, &attempts, &noError); err != nil || state != "running" || attempts != 1 || !noError {
		t.Fatalf("shutdown changed the durable attempt: %s %d %v %v", state, attempts, noError, err)
	}
	restarted := NewPrecheckQueue(pool, time.Second)
	if job, err := restarted.Claim(context.Background(), time.Minute); err != nil || job != nil {
		t.Fatal("restart bypassed the unexpired lease")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE application_id=$1`, appID); err != nil {
		t.Fatal(err)
	}
	job := claimPrecheck(t, restarted)
	if job.Attempt != 2 {
		t.Fatal("recovered job did not preserve its previous attempt")
	}
	if err := restarted.Complete(context.Background(), job, runPrecheck(t, job)); err != nil {
		t.Fatal(err)
	}
	var eventCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM application_events WHERE application_id=$1`, appID).Scan(&eventCount); err != nil || eventCount != 6 {
		t.Fatal("shutdown/restart duplicated system events")
	}
}
