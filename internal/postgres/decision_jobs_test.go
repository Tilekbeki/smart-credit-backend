package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/scoring"
)

func decisionResponseJSON(score int, recommendation string, amount, cashFlow int64) string {
	return fmt.Sprintf(`{"decision":%q,"score":%d,"approved_amount":%d,"reasons":["Synthetic response"],"factors":{"operating_cash_flow":%d,"operating_margin":0.375,"estimated_new_monthly_payment":130000,"debt_service_share":0.2,"existing_debt_to_annual_revenue":null,"overdue_count":0}}`, recommendation, score, amount, cashFlow)
}

func scoringClientForDecision(t *testing.T, url string) *scoring.Client {
	t.Helper()
	client, err := scoring.NewClient(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func decisionFixture(t *testing.T) (*pgxpool.Pool, *PrecheckQueue, string) {
	t.Helper()
	pool, queue, id := scoringFixture(t)
	job := claimScoring(t, queue)
	saveScoringRequest(t, queue, job)
	response := scoringResponse(t)
	response.Score = 90
	if err := queue.CompleteScoring(context.Background(), job, response); err != nil {
		t.Fatal(err)
	}
	return pool, queue, id
}

func claimDecision(t *testing.T, queue *PrecheckQueue) pipeline.Job {
	t.Helper()
	job, err := queue.ClaimDecision(context.Background(), time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim decision: %+v %v", job, err)
	}
	return *job
}

func evaluateDecision(t *testing.T, job pipeline.Job) decision.Result {
	t.Helper()
	result, err := decision.NewEngine().Evaluate(job.DecisionInput)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func startDecisionWorker(t *testing.T, queue *PrecheckQueue, options ...pipeline.WorkerOption) (context.CancelFunc, <-chan error) {
	t.Helper()
	cfg := pipeline.DefaultConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.RetryBaseDelay = 5 * time.Millisecond
	options = append(options, pipeline.WithDecisionEngine(decision.NewEngine()))
	worker, err := pipeline.NewWorker(queue, pipeline.NewSimulator(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), options...)
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
			t.Error("decision worker did not stop")
		}
	})
	return cancel, done
}

func assertDecisionArtifacts(t *testing.T, pool *pgxpool.Pool, id string, want application.Status, score int) {
	t.Helper()
	var appStatus, jobStatus, stage string
	var decisions, offers, events, starts int
	var version int64
	var assessment []byte
	var lease, finished *time.Time
	var processingError *string
	err := pool.QueryRow(context.Background(), `SELECT a.status,a.version,a.processing_error,j.status,j.processing_stage,j.assessment_result,
		j.lease_until,j.finished_at,(SELECT count(*) FROM credit_decisions WHERE application_id=a.id),
		(SELECT count(*) FROM offers WHERE application_id=a.id),
		(SELECT count(*) FROM application_events WHERE application_id=a.id AND event_type=$2),
		(SELECT count(*) FROM application_events WHERE application_id=a.id AND event_type='DECISION_PROCESSING_STARTED')
		FROM applications a JOIN jobs j ON j.application_id=a.id WHERE a.id=$1`, id, func() string { k, _ := decisionEvent(want); return k }()).
		Scan(&appStatus, &version, &processingError, &jobStatus, &stage, &assessment, &lease, &finished, &decisions, &offers, &events, &starts)
	if err != nil {
		t.Fatal(err)
	}
	wantDecisions, wantOffers := 1, 0
	if want == application.ManualReview {
		wantDecisions = 0
	}
	if want == application.Approved || want == application.CounterOffer {
		wantOffers = 1
	}
	if appStatus != string(want) || version != 4 || processingError != nil || jobStatus != "completed" || stage != pipeline.DecisionStage || lease != nil || finished == nil || decisions != wantDecisions || offers != wantOffers || events != 1 || starts != 1 {
		t.Fatalf("decision artifacts: app=%s version=%d job=%s/%s decisions=%d offers=%d events=%d starts=%d", appStatus, version, jobStatus, stage, decisions, offers, events, starts)
	}
	var stored struct {
		Source     string          `json:"source"`
		Result     decision.Result `json:"result"`
		AssessedAt time.Time       `json:"assessedAt"`
	}
	if json.Unmarshal(assessment, &stored) != nil || stored.Source != "automatic" || stored.Result.Status != want || stored.Result.Score != score || stored.Result.PolicyVersion != decision.PolicyVersion || stored.AssessedAt.IsZero() {
		t.Fatalf("assessment not persisted: %s", assessment)
	}
	if want == application.ManualReview && len(stored.Result.ManualReviewReasons) == 0 {
		t.Fatal("manual review reasons lost")
	}
	if wantDecisions == 1 {
		var source, risk, policy string
		var reasons []byte
		var decidedBy *string
		var createdAt time.Time
		if err := pool.QueryRow(context.Background(), `SELECT source,risk_category,policy_version,reasons,decided_by,created_at FROM credit_decisions WHERE application_id=$1`, id).
			Scan(&source, &risk, &policy, &reasons, &decidedBy, &createdAt); err != nil {
			t.Fatal(err)
		}
		if source != "AUTO" || risk != stored.Result.RiskLevel || policy != decision.PolicyVersion || decidedBy != nil || !createdAt.Equal(stored.AssessedAt) || len(reasons) == 0 {
			t.Fatal("final credit decision differs from assessment")
		}
	}
	if wantOffers == 1 {
		var product, rate string
		var amount, payment, total, overpayment int64
		var term int
		if err := pool.QueryRow(context.Background(), `SELECT p.code,o.amount,o.term_months,o.annual_rate_percent::text,o.monthly_payment,o.total_payment,o.overpayment
			FROM offers o JOIN credit_products p ON p.id=o.product_id WHERE o.application_id=$1`, id).
			Scan(&product, &amount, &term, &rate, &payment, &total, &overpayment); err != nil {
			t.Fatal(err)
		}
		offer := stored.Result.Offer
		if offer == nil || product != offer.ProductID || amount != offer.AmountRub || term != offer.TermMonths || payment != offer.MonthlyPaymentRub || total != offer.TotalPaymentRub || overpayment != offer.OverpaymentRub || rate == "" {
			t.Fatal("offer terms not persisted")
		}
	}
	eventsList, err := NewApplicationRepository(pool, time.Second).Timeline(context.Background(), draftOwner, id)
	if err != nil || len(eventsList) != 10 {
		t.Fatalf("expected ten ordered milestones: %+v %v", eventsList, err)
	}
	for index, event := range eventsList {
		if event.Sequence != int64(index+1) {
			t.Fatal("timeline sequence broken")
		}
		if index >= 2 && event.ActorID != nil {
			t.Fatal("system actor must be null")
		}
	}
	if eventsList[8].EventType != "DECISION_PROCESSING_STARTED" || eventsList[8].Status != application.Processing || eventsList[9].Status != want {
		t.Fatal("decision timeline statuses incorrect")
	}
}

func TestDecisionFullPipelineAllOutcomesAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name             string
		score            int
		recommendation   string
		amount, expenses int64
		want             application.Status
	}{
		{"approval", 90, "approved", 1500000, 1500000, application.Approved},
		{"rejection", 20, "rejected", 0, 1500000, application.Rejected},
		{"counter offer", 90, "counter_offer", 500000, 2100000, application.CounterOffer},
		{"manual review", 65, "approved", 1500000, 1500000, application.ManualReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := draftFields()
			fields.MonthlyExpensesRub = draftValue(tc.expenses)
			pool, submit, app := submissionFixture(t, fields)
			verifyFixtureFinancials(t, pool, draftOwner.CompanyID, fields)
			if _, err := submit.Submit(context.Background(), draftOwner, app.ApplicationID, 1, nil); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, decisionResponseJSON(tc.score, tc.recommendation, tc.amount, 2400000-tc.expenses))
			}))
			defer server.Close()
			client := scoringClientForDecision(t, server.URL)
			queue := NewPrecheckQueue(pool, time.Second)
			cancel, done := startDecisionWorker(t, queue, pipeline.WithScoring(client))
			waitScoringState(t, pool, app.ApplicationID, "completed")
			cancel()
			<-done
			assertDecisionArtifacts(t, pool, app.ApplicationID, tc.want, tc.score)
			// Worker recreation sees the committed result and cannot repeat any stage.
			startDecisionWorker(t, NewPrecheckQueue(pool, time.Second), pipeline.WithScoring(client))
			if job, err := queue.ClaimDecision(context.Background(), time.Minute); err != nil || job != nil {
				t.Fatal("completed decision reclaimed")
			}
			if calls.Load() != 1 {
				t.Fatalf("scoring repeated %d times", calls.Load())
			}
		})
	}
}

func TestDecisionQueueConcurrentClaimsLeaseRecoveryAndStaleWorkers(t *testing.T) {
	pool, queue, id := decisionFixture(t)
	var wg sync.WaitGroup
	jobs := make(chan pipeline.Job, 8)
	for range 8 {
		wg.Go(func() {
			job, err := queue.ClaimDecision(context.Background(), time.Minute)
			if err != nil {
				t.Error(err)
			} else if job != nil {
				jobs <- *job
			}
		})
	}
	wg.Wait()
	close(jobs)
	var old pipeline.Job
	count := 0
	for job := range jobs {
		old = job
		count++
	}
	if count != 1 {
		t.Fatalf("same decision claimed %d times", count)
	}
	result := evaluateDecision(t, old)
	// Crash after pure calculation, before the transaction commits.
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	current := claimDecision(t, NewPrecheckQueue(pool, time.Second))
	if current.LeaseToken == old.LeaseToken || current.Attempt != 2 {
		t.Fatal("decision lease not recovered")
	}
	if err := queue.CompleteDecision(context.Background(), old, result); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("stale decision accepted")
	}
	if err := queue.FailDecision(context.Background(), old, "decision_failed", 0, true); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("stale worker cleared replacement lease")
	}
	if err := queue.FailScoring(context.Background(), current, "scoring_failed", 0, true); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("scoring stage crossed decision fence")
	}
	if err := queue.Fail(context.Background(), current, "prechecks_failed", 0); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("prechecks crossed decision fence")
	}
	if err := queue.CompleteDecision(context.Background(), current, evaluateDecision(t, current)); err != nil {
		t.Fatal(err)
	}
	if err := queue.CompleteDecision(context.Background(), current, result); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("completion repeated artifacts")
	}
	assertDecisionArtifacts(t, pool, id, application.Approved, 90)
}

func TestDecisionCompletionRollsBackEveryArtifactAndRecovers(t *testing.T) {
	pool, queue, id := decisionFixture(t)
	job := claimDecision(t, queue)
	result := evaluateDecision(t, job)
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION reject_final_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.event_type='APPLICATION_APPROVED' THEN RAISE EXCEPTION 'synthetic failure'; END IF; RETURN NEW; END; $$;
		CREATE TRIGGER reject_final_event BEFORE INSERT ON application_events FOR EACH ROW EXECUTE FUNCTION reject_final_event()`); err != nil {
		t.Fatal(err)
	}
	if err := queue.CompleteDecision(context.Background(), job, result); !errors.Is(err, pipeline.ErrStorage) {
		t.Fatalf("expected rollback: %v", err)
	}
	assertNoCreditDecision(t, pool, id)
	var untouched bool
	if err := pool.QueryRow(context.Background(), `SELECT status='running' AND assessment_result IS NULL AND finished_at IS NULL FROM jobs WHERE id=$1`, job.ID).Scan(&untouched); err != nil || !untouched {
		t.Fatal("partial completion escaped rollback")
	}
	if err := queue.FailDecision(context.Background(), job, "decision_storage_failed", 0, true); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DROP TRIGGER reject_final_event ON application_events`); err != nil {
		t.Fatal(err)
	}
	retry := claimDecision(t, NewPrecheckQueue(pool, time.Second))
	if err := queue.CompleteDecision(context.Background(), retry, evaluateDecision(t, retry)); err != nil {
		t.Fatal(err)
	}
	assertDecisionArtifacts(t, pool, id, application.Approved, 90)
}

func TestDecisionCompletionRejectsAlteredOfferAndUsesFrozenProductRate(t *testing.T) {
	pool, queue, id := decisionFixture(t)
	// Editing the live catalogue after submit must not change decision terms.
	if _, err := pool.Exec(context.Background(), `UPDATE credit_products SET annual_rate_percent=99 WHERE code='turnover'`); err != nil {
		t.Fatal(err)
	}
	job := claimDecision(t, queue)
	result := evaluateDecision(t, job)
	if result.Status != application.Approved || result.Offer == nil {
		t.Fatal("decision unexpectedly used live product terms")
	}
	original := *result.Offer
	result.Offer.ProductID = "investment"
	if err := queue.CompleteDecision(context.Background(), job, result); !errors.Is(err, decision.ErrInvalidData) {
		t.Fatal("offer changed frozen product")
	}
	*result.Offer = original
	result.Offer.AnnualRatePercent = json.Number("1")
	result.Offer.MonthlyPaymentRub, _ = credit.MonthlyPayment(result.Offer.AmountRub, result.Offer.AnnualRatePercent, result.Offer.TermMonths)
	result.Offer.TotalPaymentRub = result.Offer.MonthlyPaymentRub * int64(result.Offer.TermMonths)
	result.Offer.OverpaymentRub = result.Offer.TotalPaymentRub - result.Offer.AmountRub
	if err := queue.CompleteDecision(context.Background(), job, result); !errors.Is(err, decision.ErrInvalidData) {
		t.Fatal("offer changed frozen interest rate")
	}
	for _, tc := range []struct {
		name   string
		status application.Status
		amount int64
		term   int
	}{
		{"larger approval", application.Approved, original.AmountRub + 1, original.TermMonths},
		{"smaller approval", application.Approved, original.AmountRub - 1, original.TermMonths},
		{"unchanged counter offer", application.CounterOffer, original.AmountRub, original.TermMonths},
		{"below product minimum", application.CounterOffer, 1, original.TermMonths},
		{"altered term", application.Approved, original.AmountRub, original.TermMonths + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*result.Offer = original
			result.Status = tc.status
			result.Offer.AmountRub = tc.amount
			result.Offer.TermMonths = tc.term
			payment, err := credit.MonthlyPayment(tc.amount, result.Offer.AnnualRatePercent, tc.term)
			if err != nil {
				t.Fatal(err)
			}
			result.Offer.MonthlyPaymentRub = payment
			result.Offer.TotalPaymentRub = payment * int64(tc.term)
			result.Offer.OverpaymentRub = result.Offer.TotalPaymentRub - tc.amount
			if err := decision.ValidateResult(result); err != nil {
				t.Fatal("fixture must be mathematically valid")
			}
			if err := queue.CompleteDecision(context.Background(), job, result); !errors.Is(err, decision.ErrInvalidData) {
				t.Fatal("offer changed immutable requested/product bounds")
			}
		})
	}
	assertNoCreditDecision(t, pool, id)
	*result.Offer = original
	result.Status = application.Approved
	if err := queue.CompleteDecision(context.Background(), job, result); err != nil {
		t.Fatal(err)
	}
	assertDecisionArtifacts(t, pool, id, application.Approved, 90)
}

func TestDecisionCompletionLeaseExpiryDuringTransactionRollsBack(t *testing.T) {
	pool, queue, id := decisionFixture(t)
	job := claimDecision(t, queue)
	result := evaluateDecision(t, job)
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION delay_final_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.event_type='APPLICATION_APPROVED' THEN PERFORM pg_sleep(0.2); END IF; RETURN NEW; END; $$;
		CREATE TRIGGER delay_final_event BEFORE INSERT ON application_events FOR EACH ROW EXECUTE FUNCTION delay_final_event()`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET lease_until=clock_timestamp()+interval '100 milliseconds' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := queue.CompleteDecision(context.Background(), job, result); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatalf("expired completion persisted artifacts: %v", err)
	}
	assertNoCreditDecision(t, pool, id)
	var noAssessment bool
	if err := pool.QueryRow(context.Background(), `SELECT assessment_result IS NULL FROM jobs WHERE id=$1`, job.ID).Scan(&noAssessment); err != nil || !noAssessment {
		t.Fatal("expired completion persisted assessment")
	}
	if _, err := pool.Exec(context.Background(), `DROP TRIGGER delay_final_event ON application_events`); err != nil {
		t.Fatal(err)
	}
	retry := claimDecision(t, NewPrecheckQueue(pool, time.Second))
	if err := queue.CompleteDecision(context.Background(), retry, evaluateDecision(t, retry)); err != nil {
		t.Fatal(err)
	}
	assertDecisionArtifacts(t, pool, id, application.Approved, 90)
}

func TestDecisionQueueBoundedIndependentRetryBudgetAndTechnicalFailure(t *testing.T) {
	pool, queue, id := decisionFixture(t)
	if _, err := pool.Exec(context.Background(), `UPDATE jobs SET attempt_count=max_attempts,scoring_attempt_count=max_attempts WHERE application_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 5; attempt++ {
		job := claimDecision(t, queue)
		if job.Attempt != attempt {
			t.Fatal("decision attempts mixed with another stage")
		}
		if err := queue.FailDecision(context.Background(), job, "decision_storage_failed", time.Second, true); err != nil {
			t.Fatal(err)
		}
		if next, err := queue.ClaimDecision(context.Background(), time.Minute); err != nil || next != nil {
			t.Fatal("backoff/exhaustion bypassed")
		}
		if attempt < 5 {
			if _, err := pool.Exec(context.Background(), `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1`, job.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	var status, code string
	var finished *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT status,last_error,finished_at FROM jobs WHERE application_id=$1`, id).Scan(&status, &code, &finished); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || code != "decision_storage_failed" || finished == nil {
		t.Fatal("decision retry budget unbounded")
	}
	assertNoCreditDecision(t, pool, id)
}

func TestDecisionWorkerRejectsCorruptSavedDataWithoutCreditRefusal(t *testing.T) {
	for _, column := range []string{"precheck_results", "scoring_request", "scoring_response"} {
		t.Run(column, func(t *testing.T) {
			pool, queue, id := decisionFixture(t)
			if _, err := pool.Exec(context.Background(), "UPDATE jobs SET "+column+"='{}'::jsonb WHERE application_id=$1", id); err != nil {
				t.Fatal(err)
			}
			startDecisionWorker(t, queue)
			waitScoringState(t, pool, id, "failed")
			var code string
			var attempts int
			if err := pool.QueryRow(context.Background(), `SELECT last_error,decision_attempt_count FROM jobs WHERE application_id=$1`, id).Scan(&code, &attempts); err != nil {
				t.Fatal(err)
			}
			if code != "decision_invalid_data" || attempts != 1 {
				t.Fatalf("bad input retried: %s %d", code, attempts)
			}
			assertNoCreditDecision(t, pool, id)
		})
	}
}

func TestDecisionMigrationRoundTripAndLosslessRollback(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	apply := func(direction string) error {
		data, err := os.ReadFile("../../migrations/000010_decision_stage." + direction + ".sql")
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
	scoringJob := claimScoring(t, queue)
	saveScoringRequest(t, queue, scoringJob)
	if err := queue.CompleteScoring(ctx, scoringJob, scoringResponse(t)); err != nil {
		t.Fatal(err)
	}
	job := claimDecision(t, queue)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	data, err := os.ReadFile("../../migrations/000010_decision_stage.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(data)); err == nil {
		t.Fatal("downgrade discarded durable decision lease")
	}
	if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var lease string
	if err := pool.QueryRow(ctx, `SELECT lease_token::text FROM jobs WHERE id=$1`, job.ID).Scan(&lease); err != nil || lease != job.LeaseToken {
		t.Fatal("refused downgrade mutated job")
	}
}
