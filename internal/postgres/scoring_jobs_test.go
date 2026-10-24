package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/scoring"
)

const validScoringJSON = `{"decision":"approved","score":72,"approved_amount":1500000,"reasons":["Synthetic response"],"factors":{"operating_cash_flow":900000,"operating_margin":0.45,"estimated_new_monthly_payment":130000,"debt_service_share":0.2,"existing_debt_to_annual_revenue":null,"overdue_count":0}}`

func scoringFixture(t *testing.T) (*pgxpool.Pool, *PrecheckQueue, string) {
	t.Helper()
	pool, queue, id := precheckFixture(t)
	job := claimPrecheck(t, queue)
	if err := queue.Complete(context.Background(), job, runPrecheck(t, job)); err != nil {
		t.Fatal(err)
	}
	return pool, queue, id
}

func claimScoring(t *testing.T, queue *PrecheckQueue) pipeline.Job {
	t.Helper()
	job, err := queue.ClaimScoring(context.Background(), time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim scoring: %+v %v", job, err)
	}
	return *job
}

func saveScoringRequest(t *testing.T, queue *PrecheckQueue, job pipeline.Job) scoring.Request {
	t.Helper()
	request, err := pipeline.BuildScoringRequest(job.Snapshot, job.PrecheckResults)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.SaveScoringRequest(context.Background(), job, request); err != nil {
		t.Fatal(err)
	}
	return request
}

func scoringResponse(t *testing.T) scoring.Response {
	t.Helper()
	response, err := scoring.DecodeResponse([]byte(validScoringJSON))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func startScoringWorker(t *testing.T, queue *PrecheckQueue, serverURL string) (context.CancelFunc, <-chan error) {
	t.Helper()
	client, err := scoring.NewClient(serverURL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	config := pipeline.DefaultConfig()
	config.PollInterval = 5 * time.Millisecond
	config.RetryBaseDelay = 5 * time.Millisecond
	worker, err := pipeline.NewWorker(queue, pipeline.NewSimulator(), config, slog.New(slog.NewTextHandler(io.Discard, nil)), pipeline.WithScoring(client))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("scoring worker did not stop")
		}
	})
	return cancel, done
}

func waitScoringState(t *testing.T, pool *pgxpool.Pool, applicationID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := pool.QueryRow(context.Background(), `SELECT status FROM jobs WHERE application_id=$1`, applicationID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	var status string
	_ = pool.QueryRow(context.Background(), `SELECT status FROM jobs WHERE application_id=$1`, applicationID).Scan(&status)
	t.Fatalf("job status=%s, wanted=%s", status, want)
}

func assertNoCreditDecision(t *testing.T, pool *pgxpool.Pool, applicationID string) {
	t.Helper()
	var appStatus string
	var decisions, offers int
	if err := pool.QueryRow(context.Background(), `SELECT status,(SELECT count(*) FROM credit_decisions WHERE application_id=$1),
		(SELECT count(*) FROM offers WHERE application_id=$1) FROM applications WHERE id=$1`, applicationID).Scan(&appStatus, &decisions, &offers); err != nil {
		t.Fatal(err)
	}
	if appStatus != "processing" || decisions != 0 || offers != 0 {
		t.Fatalf("scoring made final credit artifacts: %s decisions=%d offers=%d", appStatus, decisions, offers)
	}
}

func TestScoringWorkerPersistsRecommendationAndStopsAfterRestart(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	if _, err := pool.Exec(context.Background(), `UPDATE companies SET monthly_revenue=1,monthly_expenses=1,
		existing_debt=1,monthly_debt_payments=1,overdue_count=9 WHERE id=$1`, draftOwner.CompanyID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	requests := make(chan scoring.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/score" {
			t.Error("unexpected scoring route")
		}
		var request scoring.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validScoringJSON)
	}))
	defer server.Close()
	cancel, _ := startScoringWorker(t, queue, server.URL)
	waitScoringState(t, pool, id, pipeline.AwaitingDecision)
	cancel()
	request := <-requests
	if request.ApplicationID != id || request.MonthlyRevenue != *draftFields().MonthlyRevenueRub || request.MonthlyExpenses != *draftFields().MonthlyExpensesRub ||
		request.ExistingDebt != 500000 || request.MonthlyDebtPayments != 90000 || request.OverdueCount != 0 {
		t.Fatalf("scoring used live company data: %+v", request)
	}
	var rawRequest, rawResponse []byte
	var receivedAt, leaseUntil, finishedAt *time.Time
	var appVersion int64
	var errorCode, modelVersion *string
	if err := pool.QueryRow(context.Background(), `SELECT j.scoring_request,j.scoring_response,j.scoring_received_at,j.lease_until,j.finished_at,
		j.scoring_version,a.version,a.processing_error FROM jobs j JOIN applications a ON a.id=j.application_id WHERE a.id=$1`, id).
		Scan(&rawRequest, &rawResponse, &receivedAt, &leaseUntil, &finishedAt, &modelVersion, &appVersion, &errorCode); err != nil {
		t.Fatal(err)
	}
	response, err := scoring.DecodeResponse(rawResponse)
	if err != nil || response.Score != 72 || response.Decision != "approved" || response.ApprovedAmount != 1500000 || receivedAt == nil || leaseUntil != nil || finishedAt != nil || modelVersion != nil || errorCode != nil || appVersion != 3 || len(rawRequest) == 0 {
		t.Fatal("durable scoring result differs")
	}
	assertNoCreditDecision(t, pool, id)
	replay, err := NewSubmissionRepository(pool, time.Second).Submit(context.Background(), draftOwner, id, 1, nil)
	if err != nil || replay.NextStage != pipeline.AwaitingScoring {
		t.Fatalf("frozen submit DTO changed after scoring: %+v %v", replay, err)
	}
	// Recreated workers and two processing slots cannot claim awaiting_decision.
	startScoringWorker(t, NewPrecheckQueue(pool, time.Second), server.URL)
	if job, err := queue.ClaimScoring(context.Background(), time.Minute); err != nil || job != nil {
		t.Fatal("completed scoring was reclaimed")
	}
	events, err := NewApplicationRepository(pool, time.Second).Timeline(context.Background(), draftOwner, id)
	if err != nil || len(events) != 8 || events[6].EventType != "SCORING_STARTED" || events[7].EventType != "SCORING_COMPLETED" {
		t.Fatalf("scoring events: %+v %v", events, err)
	}
	for _, event := range events[6:] {
		if event.ActorID != nil || string(event.Status) != "processing" || event.Title == "" {
			t.Fatal("unsafe scoring timeline event")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("unnecessary repeated scoring: %d", calls.Load())
	}
}

func TestScoringQueueConcurrentClaimsAndLateResponseFencing(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	var claims sync.WaitGroup
	jobs := make(chan pipeline.Job, 8)
	for range 8 {
		claims.Go(func() {
			job, err := queue.ClaimScoring(context.Background(), time.Minute)
			if err != nil {
				t.Error(err)
			} else if job != nil {
				jobs <- *job
			}
		})
	}
	claims.Wait()
	close(jobs)
	var old pipeline.Job
	count := 0
	for job := range jobs {
		old = job
		count++
	}
	if count != 1 {
		t.Fatalf("same scoring claimed %d times", count)
	}
	request := saveScoringRequest(t, queue, old)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validScoringJSON)
	}))
	defer server.Close()
	client, err := scoring.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	responseCh := make(chan scoring.Response, 1)
	go func() {
		response, err := client.Score(context.Background(), request)
		if err != nil {
			t.Error(err)
		}
		responseCh <- response
	}()
	<-started
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	current := claimScoring(t, NewPrecheckQueue(pool, time.Second))
	if current.ID != old.ID || current.LeaseToken == old.LeaseToken || current.Attempt != 2 {
		t.Fatal("expired lease not recovered")
	}
	close(release)
	response := <-responseCh
	if err := queue.CompleteScoring(context.Background(), old, response); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatalf("late HTTP response replaced new lease: %v", err)
	}
	if err := queue.FailScoring(context.Background(), old, "scoring_timeout", 0, true); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("old worker cleared new lease")
	}
	if err := queue.SaveScoringRequest(context.Background(), old, request); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("old worker rewrote saved input")
	}
	saveScoringRequest(t, queue, current)
	if err := queue.CompleteScoring(context.Background(), current, response); err != nil {
		t.Fatal(err)
	}
	if err := queue.CompleteScoring(context.Background(), current, response); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("duplicate result accepted")
	}
	assertNoCreditDecision(t, pool, id)
}

func TestScoringWorkerRetriesTechnicalErrorsWithIndependentBudget(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET attempt_count=max_attempts WHERE application_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validScoringJSON)
	}))
	defer server.Close()
	startScoringWorker(t, queue, server.URL)
	waitScoringState(t, pool, id, pipeline.AwaitingDecision)
	var prechecks, scoringAttempts int
	if err := pool.QueryRow(context.Background(), `SELECT attempt_count,scoring_attempt_count FROM jobs WHERE application_id=$1`, id).Scan(&prechecks, &scoringAttempts); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || prechecks != 5 || scoringAttempts != 2 {
		t.Fatalf("stage budgets mixed: calls=%d prechecks=%d scoring=%d", calls.Load(), prechecks, scoringAttempts)
	}
	assertNoCreditDecision(t, pool, id)
}

func TestScoringWorkerPermanentContractFailuresDoNotRetry(t *testing.T) {
	for _, tc := range []struct{ name, body, code string }{
		{"invalid JSON", `{`, "scoring_invalid_json"},
		{"missing fields", `{"score":50}`, "scoring_invalid_response"},
		{"score range", `{"decision":"approved","score":101,"approved_amount":1,"reasons":["test"],"factors":{"operating_cash_flow":1,"operating_margin":1,"estimated_new_monthly_payment":1,"debt_service_share":null,"existing_debt_to_annual_revenue":null,"overdue_count":0}}`, "scoring_invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, queue, id := scoringFixture(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			startScoringWorker(t, queue, server.URL)
			waitScoringState(t, pool, id, "failed")
			var attempts int
			var code string
			var empty bool
			if err := pool.QueryRow(context.Background(), `SELECT scoring_attempt_count,last_error,scoring_response IS NULL AND scoring_received_at IS NULL FROM jobs WHERE application_id=$1`, id).Scan(&attempts, &code, &empty); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || calls.Load() != 1 || code != tc.code || !empty {
				t.Fatalf("permanent failure retried or saved: %d %d %s %v", attempts, calls.Load(), code, empty)
			}
			assertNoCreditDecision(t, pool, id)
		})
	}
}

func TestScoringQueueRetriesBoundedDelayedAndRecoverable(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	for attempt := 1; attempt <= 5; attempt++ {
		job := claimScoring(t, queue)
		if job.Attempt != attempt {
			t.Fatal("wrong scoring attempt count")
		}
		saveScoringRequest(t, queue, job)
		if err := queue.FailScoring(context.Background(), job, "scoring_timeout", 20*time.Second, true); err != nil {
			t.Fatal(err)
		}
		if next, err := queue.ClaimScoring(context.Background(), time.Minute); err != nil || next != nil {
			t.Fatal("early retry bypassed backoff")
		}
		if attempt < 5 {
			if _, err := pool.Exec(context.Background(), `UPDATE jobs SET available_at=clock_timestamp() WHERE application_id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	var status, code, appError string
	var attempts int
	var finished *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT j.status,j.scoring_attempt_count,j.last_error,j.finished_at,a.processing_error FROM jobs j JOIN applications a ON a.id=j.application_id WHERE a.id=$1`, id).Scan(&status, &attempts, &code, &finished, &appError); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != 5 || code != "scoring_timeout" || appError != code || finished == nil {
		t.Fatal("scoring retries were unbounded or diagnostic lost")
	}
	assertNoCreditDecision(t, pool, id)
	// Recovery is an explicit operator action on the existing durable job, not
	// automatic infinite retries or a new public restart endpoint.
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET status='awaiting_scoring',scoring_attempt_count=0,available_at=clock_timestamp(),finished_at=NULL WHERE application_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	job := claimScoring(t, NewPrecheckQueue(pool, time.Second))
	saveScoringRequest(t, queue, job)
	if err := queue.CompleteScoring(context.Background(), job, scoringResponse(t)); err != nil {
		t.Fatal(err)
	}
	assertNoCreditDecision(t, pool, id)
}

func TestScoringWorkerRejectsMissingPersistedFinancialsWithoutHTTP(t *testing.T) {
	for _, raw := range []string{
		`{"schemaVersion":1,"creditHistory":{"existingMonthlyPaymentsRub":0,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":null,"existingMonthlyPaymentsRub":0,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":-1,"existingMonthlyPaymentsRub":0,"overdueCount":0}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			pool, queue, id := scoringFixture(t)
			if _, err := pool.Exec(context.Background(), `UPDATE jobs SET precheck_results=$2::jsonb WHERE application_id=$1`, id, raw); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, validScoringJSON) }))
			defer server.Close()
			startScoringWorker(t, queue, server.URL)
			waitScoringState(t, pool, id, "failed")
			var code string
			if err := pool.QueryRow(context.Background(), `SELECT last_error FROM jobs WHERE application_id=$1`, id).Scan(&code); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 || code != "scoring_invalid_data" {
				t.Fatal("corrupted financials reached scoring")
			}
			assertNoCreditDecision(t, pool, id)
		})
	}
}

func TestScoringQueueCrashWindowsAndCommittedResultRecovery(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	old := claimScoring(t, queue)
	request := saveScoringRequest(t, queue, old)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, validScoringJSON)
	}))
	defer server.Close()
	client, err := scoring.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Score(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	// Crash after the HTTP response but before the fenced transaction commits.
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	cancel, done := startScoringWorker(t, NewPrecheckQueue(pool, time.Second), server.URL)
	waitScoringState(t, pool, id, pipeline.AwaitingDecision)
	cancel()
	<-done
	if calls.Load() != 2 {
		t.Fatal("lost HTTP response was not safely retried")
	}
	// Simulate a legacy/recovery marker with an already committed valid receipt.
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET status='awaiting_scoring',scoring_attempt_count=max_attempts WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if job, err := NewPrecheckQueue(pool, time.Second).ClaimScoring(context.Background(), time.Minute); err != nil || job != nil {
		t.Fatalf("saved result not recovered without HTTP: %+v %v", job, err)
	}
	waitScoringState(t, pool, id, pipeline.AwaitingDecision)
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM application_events WHERE application_id=$1 AND event_type IN ('SCORING_STARTED','SCORING_COMPLETED')`, id).Scan(&events); err != nil || events != 2 {
		t.Fatal("crash recovery duplicated events")
	}
	if calls.Load() != 2 {
		t.Fatal("committed result called scoring again")
	}
	assertNoCreditDecision(t, pool, id)
}

func TestScoringQueueCompletionRollsBackAndPrecheckStageIsFenced(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	job := claimScoring(t, queue)
	saveScoringRequest(t, queue, job)
	// A precheck-stage method must not operate with a valid scoring-stage lease.
	if err := queue.Fail(context.Background(), job, "prechecks_failed", 0); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("precheck failure crossed processing stage")
	}
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_scoring_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.event_type='SCORING_COMPLETED' THEN RAISE EXCEPTION 'synthetic event failure'; END IF; RETURN NEW; END; $$;
		CREATE TRIGGER reject_scoring_event BEFORE INSERT ON application_events FOR EACH ROW EXECUTE FUNCTION reject_scoring_event()`); err != nil {
		t.Fatal(err)
	}
	if err := queue.CompleteScoring(context.Background(), job, scoringResponse(t)); !errors.Is(err, pipeline.ErrStorage) {
		t.Fatal("expected scoring completion rollback")
	}
	var status string
	var empty bool
	if err := pool.QueryRow(context.Background(), `SELECT status,scoring_response IS NULL AND scoring_received_at IS NULL FROM jobs WHERE application_id=$1`, id).Scan(&status, &empty); err != nil {
		t.Fatal(err)
	}
	if status != "running" || !empty {
		t.Fatal("scoring response committed without its milestone")
	}
	if _, err := pool.Exec(context.Background(), `DROP TRIGGER reject_scoring_event ON application_events`); err != nil {
		t.Fatal(err)
	}
	if err := queue.CompleteScoring(context.Background(), job, scoringResponse(t)); err != nil {
		t.Fatal(err)
	}
}

func TestScoringMigrationRoundTripAndLosslessRollback(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	// Roll back newer stages first, matching the migration runner's ordering.
	decisionDown, err := os.ReadFile("../../migrations/000010_decision_stage.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(decisionDown)); err != nil {
		t.Fatal(err)
	}
	apply := func(direction string) error {
		data, err := os.ReadFile("../../migrations/000009_scoring_stage." + direction + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, string(data))
		return err
	}
	if err := apply("down"); err != nil {
		t.Fatal(err)
	}
	if err := apply("up"); err != nil {
		t.Fatal(err)
	}
	app := createDraft(t, NewApplicationRepository(pool, time.Second), draftOwner, draftFields())
	if _, err := NewSubmissionRepository(pool, time.Second).Submit(ctx, draftOwner, app.ApplicationID, 1, nil); err != nil {
		t.Fatal(err)
	}
	queue := NewPrecheckQueue(pool, time.Second)
	precheck := claimPrecheck(t, queue)
	if err := queue.Complete(ctx, precheck, runPrecheck(t, precheck)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../migrations/000009_scoring_stage.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, string(data)); err == nil {
		t.Fatal("downgrade discarded scoring recovery state")
	}
	if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE application_id=$1`, app.ApplicationID).Scan(&status); err != nil || status != pipeline.AwaitingScoring {
		t.Fatal("refused downgrade changed durable state")
	}
}

func TestScoringQueueRejectsCorruptSavedRequestDuringReceiptRecovery(t *testing.T) {
	for _, raw := range []string{`{}`, `{"application_id":"other"}`, `{"application_id":null}`, ""} {
		t.Run(raw, func(t *testing.T) {
			pool, queue, id := scoringFixture(t)
			job := claimScoring(t, queue)
			saveScoringRequest(t, queue, job)
			if err := queue.CompleteScoring(context.Background(), job, scoringResponse(t)); err != nil {
				t.Fatal(err)
			}
			var savedRequest any = raw
			if raw == "" {
				savedRequest = nil
			}
			if _, err := pool.Exec(context.Background(), `UPDATE jobs SET status='awaiting_scoring',scoring_request=$2::jsonb WHERE application_id=$1`, id, savedRequest); err != nil {
				t.Fatal(err)
			}
			if job, err := NewPrecheckQueue(pool, time.Second).ClaimScoring(context.Background(), time.Minute); err != nil || job != nil {
				t.Fatalf("bad committed input restored: %+v %v", job, err)
			}
			var status, code string
			if err := pool.QueryRow(context.Background(), `SELECT status,last_error FROM jobs WHERE application_id=$1`, id).Scan(&status, &code); err != nil {
				t.Fatal(err)
			}
			if status != "failed" || code != "scoring_invalid_data" {
				t.Fatalf("corrupt receipt input passed: %s %s", status, code)
			}
			assertNoCreditDecision(t, pool, id)
		})
	}
}

func TestScoringWorkerRejectsCorruptCommittedResponseWithoutHTTP(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	job := claimScoring(t, queue)
	saveScoringRequest(t, queue, job)
	if err := queue.CompleteScoring(context.Background(), job, scoringResponse(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET status='awaiting_scoring',scoring_response='{"score":101}'::jsonb WHERE application_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, validScoringJSON) }))
	defer server.Close()
	startScoringWorker(t, NewPrecheckQueue(pool, time.Second), server.URL)
	waitScoringState(t, pool, id, "failed")
	var code string
	if err := pool.QueryRow(context.Background(), `SELECT last_error FROM jobs WHERE application_id=$1`, id).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || code != "scoring_invalid_response" {
		t.Fatal("corrupt saved response was trusted or resent")
	}
	assertNoCreditDecision(t, pool, id)
}
