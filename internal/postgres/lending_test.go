package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/lending"
	"smart-credit-backend/internal/review"
)

// Every fixture traverses the persisted precheck/scoring/decision stages rather
// than inventing a manual_review application without its historical inputs.
func lendingFixture(t *testing.T, score int) (*pgxpool.Pool, string) {
	t.Helper()
	pool, queue, id := scoringFixture(t)
	job := claimScoring(t, queue)
	saveScoringRequest(t, queue, job)
	response := scoringResponse(t)
	response.Score = score
	if err := queue.CompleteScoring(context.Background(), job, response); err != nil {
		t.Fatal(err)
	}
	job = claimDecision(t, queue)
	if err := queue.CompleteDecision(context.Background(), job, evaluateDecision(t, job)); err != nil {
		t.Fatal(err)
	}
	return pool, id
}

func lendingAdditionalApplication(t *testing.T, pool *pgxpool.Pool, scope application.Scope, fields application.Fields, score int) string {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE companies SET financial_data_source='demo_bank_feed' WHERE id=$1`, scope.CompanyID); err != nil {
		t.Fatal(err)
	}
	verifyFixtureFinancials(t, pool, scope.CompanyID, fields)
	draft := createDraft(t, NewApplicationRepository(pool, 5*time.Second), scope, fields)
	if _, err := NewSubmissionRepository(pool, 5*time.Second).Submit(ctx, scope, draft.ApplicationID, draft.Version, nil); err != nil {
		t.Fatal(err)
	}
	queue := NewPrecheckQueue(pool, 5*time.Second)
	job := claimPrecheck(t, queue)
	if err := queue.Complete(ctx, job, runPrecheck(t, job)); err != nil {
		t.Fatal(err)
	}
	job = claimScoring(t, queue)
	request := saveScoringRequest(t, queue, job)
	response := scoringResponse(t)
	response.Score, response.ApprovedAmount = score, request.RequestedAmount
	response.Factors.OperatingCashFlow = float64(request.MonthlyRevenue - request.MonthlyExpenses)
	response.Factors.OverdueCount = request.OverdueCount
	if err := queue.CompleteScoring(ctx, job, response); err != nil {
		t.Fatal(err)
	}
	job = claimDecision(t, queue)
	if err := queue.CompleteDecision(ctx, job, evaluateDecision(t, job)); err != nil {
		t.Fatal(err)
	}
	return draft.ApplicationID
}

func lendingService(pool *pgxpool.Pool) *lending.Service {
	return lending.NewService(NewLendingRepository(pool, 5*time.Second))
}

func manualCommand(id, outcome string) lending.Command {
	cmd := lending.Command{ApplicationID: id, ActorID: demoEmployeeID, Key: uuid.NewString(), Operation: lending.Manual,
		Version: 4, Decision: outcome, Comment: "  Внутренняя оценка: подтверждена справка о выручке.  "}
	if outcome != "rejected" {
		cmd.Offer = &lending.ManualOffer{AmountRub: 1_500_000, TermMonths: 12}
		if outcome == "counter_offer" {
			cmd.Offer.AmountRub = 1_000_000
		}
	}
	return cmd
}

func offerCommand(id, offerID, operation string) lending.Command {
	return lending.Command{ApplicationID: id, ActorID: draftOwner.UserID, CompanyID: draftOwner.CompanyID,
		Key: uuid.NewString(), Operation: operation, Version: 4, OfferID: offerID}
}

func executeLending(t *testing.T, service *lending.Service, cmd lending.Command, wantStatus int) (lending.Response, review.CreditDecision) {
	t.Helper()
	response, err := service.Execute(context.Background(), cmd)
	if err != nil || response.Status != wantStatus {
		t.Fatalf("execute %s: HTTP=%d error=%v body=%s", cmd.Operation, response.Status, err, response.Body)
	}
	var body review.CreditDecision
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("invalid CreditDecision: %v", err)
	}
	exportReadContract(t, "CreditDecision", body)
	return response, body
}

func lendingFingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var result string
	if err := pool.QueryRow(context.Background(), `SELECT md5(jsonb_build_object(
 'applications',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM applications a),
 'decisions',(SELECT jsonb_agg(to_jsonb(d) ORDER BY id) FROM credit_decisions d),
 'offers',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM offers o),
 'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM application_events e),
 'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM jobs j),
 'idempotency',(SELECT jsonb_agg(to_jsonb(k) ORDER BY id) FROM idempotency_keys k))::text)`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertLendingResponseEqual(t *testing.T, a, b lending.Response) {
	t.Helper()
	var original, replay any
	if json.Unmarshal(a.Body, &original) != nil || json.Unmarshal(b.Body, &replay) != nil ||
		a.Status != b.Status || !reflect.DeepEqual(original, replay) {
		t.Fatalf("replay differs: original=%d %s replay=%d %s", a.Status, a.Body, b.Status, b.Body)
	}
}

func assertLendingCounts(t *testing.T, pool *pgxpool.Pool, id string, version int64, wantDecisions, wantOffers, wantEvents int) {
	t.Helper()
	var gotVersion int64
	var decisions, offers, events int
	if err := pool.QueryRow(context.Background(), `SELECT version,
 (SELECT count(*) FROM credit_decisions WHERE application_id=$1),
 (SELECT count(*) FROM offers WHERE application_id=$1),
 (SELECT count(*) FROM application_events WHERE application_id=$1 AND event_type IN
 ('MANUAL_DECISION_APPROVED','MANUAL_DECISION_REJECTED','MANUAL_COUNTER_OFFER_CREATED','OFFER_ACCEPTED','OFFER_DECLINED'))
 FROM applications WHERE id=$1`, id).Scan(&gotVersion, &decisions, &offers, &events); err != nil {
		t.Fatal(err)
	}
	if gotVersion != version || decisions != wantDecisions || offers != wantOffers || events != wantEvents {
		t.Fatalf("artifacts version=%d decisions=%d offers=%d events=%d", gotVersion, decisions, offers, events)
	}
}

func TestLendingManualOutcomesCommentPrivacyAndFrozenTerms(t *testing.T) {
	for _, outcome := range []string{"approved", "rejected", "counter_offer"} {
		t.Run(outcome, func(t *testing.T) {
			pool, id := lendingFixture(t, 65)
			ctx := context.Background()
			// A later catalogue/company update cannot change the submitted terms.
			if _, err := pool.Exec(ctx, `UPDATE credit_products SET annual_rate_percent=99,max_amount=100000 WHERE code='turnover'`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE companies SET monthly_revenue=1,monthly_expenses=1 WHERE id=$1`, draftOwner.CompanyID); err != nil {
				t.Fatal(err)
			}
			service, cmd := lendingService(pool), manualCommand(id, outcome)
			original, body := executeLending(t, service, cmd, 201)
			if body.ApplicationID != id || string(body.Status) != outcome || body.Version != 5 || body.Decision == nil || *body.Decision != outcome ||
				body.ReviewedBy == nil || *body.ReviewedBy != demoEmployeeID || body.AssessedAt == nil ||
				body.Score == nil || *body.Score != 65 || body.RiskLevel == nil || *body.RiskLevel != "MEDIUM" || body.Processing != nil || len(body.Reasons) == 0 {
				t.Fatalf("manual credit result %+v", body)
			}
			if strings.Contains(string(original.Body), "Внутренняя оценка") || strings.Contains(string(original.Body), "справка") {
				t.Fatal("internal comment exposed in client CreditDecision")
			}
			wantOffers := 0
			if outcome != "rejected" {
				wantOffers = 1
				wantPayment, err := credit.MonthlyPayment(cmd.Offer.AmountRub, json.Number("22.0000"), cmd.Offer.TermMonths)
				if err != nil || body.Offer == nil || body.Offer.AmountRub != cmd.Offer.AmountRub || body.Offer.TermMonths != cmd.Offer.TermMonths ||
					body.Offer.ProductID != "turnover" || body.Offer.AnnualRatePercent != "22.0000" || body.Offer.MonthlyPaymentRub != wantPayment || body.Offer.Status != "active" {
					t.Fatalf("frozen manual offer %+v error=%v", body.Offer, err)
				}
			} else if body.Offer != nil {
				t.Fatal("rejection has an offer")
			}
			var source, reviewer, comment, eventActor string
			var eventTime time.Time
			if err := pool.QueryRow(ctx, `SELECT d.source,d.decided_by::text,d.internal_comment,e.actor_id::text,e.created_at
 FROM credit_decisions d JOIN application_events e ON e.application_id=d.application_id
 WHERE d.application_id=$1 AND e.action='manual'`, id).
				Scan(&source, &reviewer, &comment, &eventActor, &eventTime); err != nil {
				t.Fatal(err)
			}
			if source != "MANUAL" || reviewer != demoEmployeeID || eventActor != demoEmployeeID || comment != strings.TrimSpace(cmd.Comment) || eventTime.IsZero() {
				t.Fatal("manual decision audit not persisted")
			}
			var retentionSeconds float64
			var storedStatus int
			var storedBody []byte
			if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM (expires_at-created_at)),response_status,response_body
 FROM idempotency_keys WHERE user_id=$1 AND key=$2`, cmd.ActorID, cmd.Key).
				Scan(&retentionSeconds, &storedStatus, &storedBody); err != nil || retentionSeconds < 86399 || retentionSeconds > 86401 {
				t.Fatalf("idempotency retention=%f error=%v", retentionSeconds, err)
			}
			assertLendingResponseEqual(t, original, lending.Response{Status: storedStatus, Body: storedBody})
			reads := review.NewService(NewReviewRepository(pool, time.Second))
			assessment, err := reads.BankAssessment(ctx, id)
			if err != nil || assessment.ManualDecision == nil || assessment.ManualDecision.Decision != outcome || assessment.ManualDecision.Comment != comment ||
				assessment.ManualDecision.ReviewedBy != demoEmployeeID || !assessment.ManualDecision.ReviewedAt.Equal(*body.AssessedAt) ||
				assessment.Scoring == nil || assessment.Scoring.Score != 65 || assessment.RiskLevel == nil || *assessment.RiskLevel != "MEDIUM" {
				t.Fatalf("bank manual audit %+v error=%v", assessment, err)
			}
			exportReadContract(t, "BankAssessment", assessment)
			get, err := reads.Decision(ctx, draftOwner, id)
			if err != nil || !reflect.DeepEqual(body, get) {
				t.Fatalf("GET differs from POST %+v error=%v", get, err)
			}
			detail, err := reads.BankDetail(ctx, id)
			if err != nil || len(detail.AvailableActions) != 0 {
				t.Fatal("bank decide action remains after final result")
			}
			assertLendingCounts(t, pool, id, 5, 1, wantOffers, 1)
			before := lendingFingerprint(t, pool)
			// A new repository/service represents a restart after a lost HTTP response.
			replay, _ := executeLending(t, lendingService(pool), cmd, 201)
			assertLendingResponseEqual(t, original, replay)
			if lendingFingerprint(t, pool) != before {
				t.Fatal("manual replay mutated durable data")
			}
			changed := cmd
			changed.Comment += " Другое основание."
			if _, err := service.Execute(ctx, changed); !errors.Is(err, lending.ErrIdempotencyConflict) {
				t.Fatalf("same key changed manual body: %v", err)
			}
		})
	}
}

func TestLendingManualRejectsInvalidTermsAndDataWithoutMutation(t *testing.T) {
	pool, id := lendingFixture(t, 65)
	service := lendingService(pool)
	for _, tc := range []struct {
		name    string
		outcome string
		amount  int64
		term    int
	}{
		{"approval amount differs", "approved", 1_000_000, 12},
		{"approval term differs", "approved", 1_500_000, 18},
		{"unchanged counter", "counter_offer", 1_500_000, 12},
		{"increased counter", "counter_offer", 1_500_001, 12},
		{"below product minimum", "counter_offer", 99_999, 12},
		{"below product term", "counter_offer", 1_000_000, 1},
		{"above product term", "counter_offer", 1_000_000, 120},
		{"unaffordable short counter", "counter_offer", 1_500_000, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := manualCommand(id, tc.outcome)
			cmd.Offer = &lending.ManualOffer{AmountRub: tc.amount, TermMonths: tc.term}
			before := lendingFingerprint(t, pool)
			if _, err := service.Execute(context.Background(), cmd); !errors.Is(err, lending.ErrValidation) {
				t.Fatalf("invalid manual offer: %v", err)
			}
			if lendingFingerprint(t, pool) != before {
				t.Fatal("invalid offer changed state")
			}
		})
	}
	// Valid counter-offers may keep the amount and change only the term.
	cmd := manualCommand(id, "counter_offer")
	cmd.Offer = &lending.ManualOffer{AmountRub: 1_500_000, TermMonths: 18}
	executeLending(t, service, cmd, 201)
}

func TestLendingManualCorruptSnapshotsAndInsufficientBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"missing snapshot field", `UPDATE applications SET financial_snapshot=financial_snapshot #- '{fields,monthlyRevenueRub}' WHERE id=$1`},
		{"broken product", `UPDATE applications SET product_snapshot=product_snapshot || '{"annualRatePercent":"bad"}' WHERE id=$1`},
		{"no financing budget", `UPDATE applications SET financial_snapshot=jsonb_set(financial_snapshot,'{fields,monthlyExpensesRub}','2400000') WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, id := lendingFixture(t, 65)
			if _, err := pool.Exec(context.Background(), `ALTER TABLE applications DISABLE TRIGGER applications_immutable_snapshots`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(context.Background(), tc.sql, id); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(context.Background(), `ALTER TABLE applications ENABLE TRIGGER applications_immutable_snapshots`); err != nil {
				t.Fatal(err)
			}
			before := lendingFingerprint(t, pool)
			if _, err := lendingService(pool).Execute(context.Background(), manualCommand(id, "approved")); err == nil {
				t.Fatal("corrupt data or insufficient budget accepted")
			}
			if lendingFingerprint(t, pool) != before {
				t.Fatal("failed validation changed state")
			}
		})
	}
}

func TestLendingManualFinancialBudgetUsesFrozenInputs(t *testing.T) {
	pool := authTestPool(t)
	fields := draftFields()
	fields.MonthlyExpensesRub = draftValue(int64(2_100_000))
	id := lendingAdditionalApplication(t, pool, draftOwner, fields, 65)
	service := lendingService(pool)
	cmd := manualCommand(id, "approved")
	before := lendingFingerprint(t, pool)
	if _, err := service.Execute(context.Background(), cmd); !errors.Is(err, lending.ErrValidation) {
		t.Fatalf("unaffordable manual approval: %v", err)
	}
	if lendingFingerprint(t, pool) != before {
		t.Fatal("unaffordable approval mutated data")
	}
	cmd = manualCommand(id, "counter_offer")
	cmd.Offer.AmountRub = 500_000
	_, body := executeLending(t, service, cmd, 201)
	if body.Offer.MonthlyPaymentRub > credit.PaymentBudget(2_400_000, 2_100_000, 90_000) {
		t.Fatal("counter offer exceeds persisted payment budget")
	}
}

func TestLendingManualIntegrityBindingsRejectUnrelatedPersistedData(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   string
		snapshot bool
	}{
		{"assessment score differs from scoring", `UPDATE jobs SET assessment_result=jsonb_set(
 jsonb_set(assessment_result,'{result,score}','66'),'{result,riskLevel}','"MEDIUM"') WHERE application_id=$1`, false},
		{"frozen owner differs from application", `UPDATE applications SET financial_snapshot=jsonb_set(
 financial_snapshot,'{userId}','"20000000-0000-4000-8000-000000000002"') WHERE id=$1`, true},
		{"frozen company differs from application", `UPDATE applications SET financial_snapshot=jsonb_set(
 jsonb_set(financial_snapshot,'{companyId}','"10000000-0000-4000-8000-000000000002"'),
 '{company,profile,id}','"10000000-0000-4000-8000-000000000002"') WHERE id=$1`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, id := lendingFixture(t, 65)
			ctx := context.Background()
			// Only this fixture's private schema permits injecting invalid stored
			// data; production snapshot immutability is restored before the call.
			if tc.snapshot {
				if _, err := pool.Exec(ctx, `ALTER TABLE applications DISABLE TRIGGER applications_immutable_snapshots`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, tc.mutate, id); err != nil {
				t.Fatal(err)
			}
			if tc.snapshot {
				if _, err := pool.Exec(ctx, `ALTER TABLE applications ENABLE TRIGGER applications_immutable_snapshots`); err != nil {
					t.Fatal(err)
				}
			}
			before := lendingFingerprint(t, pool)
			if _, err := lendingService(pool).Execute(ctx, manualCommand(id, "approved")); !errors.Is(err, application.ErrStorage) {
				t.Fatalf("unbound persisted data error=%v", err)
			}
			if lendingFingerprint(t, pool) != before {
				t.Fatal("failed integrity validation mutated durable data")
			}
		})
	}
}

func TestLendingRecognizedEventsRequireNonNullAction(t *testing.T) {
	pool, id := lendingFixture(t, 90)
	ctx := context.Background()
	var offerID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM offers WHERE application_id=$1`, id).Scan(&offerID); err != nil {
		t.Fatal(err)
	}
	for _, eventType := range []string{"MANUAL_DECISION_APPROVED", "MANUAL_DECISION_REJECTED", "MANUAL_COUNTER_OFFER_CREATED", "OFFER_ACCEPTED", "OFFER_DECLINED"} {
		t.Run(eventType, func(t *testing.T) {
			var eventOffer any = offerID
			if eventType == "MANUAL_DECISION_REJECTED" {
				eventOffer = nil
			}
			before := lendingFingerprint(t, pool)
			_, err := pool.Exec(ctx, `INSERT INTO application_events(application_id,sequence,event_type,status,title,actor_id,offer_id,action,action_version)
 SELECT $1,COALESCE(max(sequence),0)+1,$2,'approved','Synthetic invalid null-action audit',$3,$4,NULL,4
 FROM application_events WHERE application_id=$1`, id, eventType, demoEmployeeID, eventOffer)
			var dbError *pgconn.PgError
			if !errors.As(err, &dbError) || dbError.Code != "23514" || dbError.ConstraintName != "application_events_action_audit" {
				t.Fatalf("missing action accepted or wrong constraint: %v", err)
			}
			if lendingFingerprint(t, pool) != before {
				t.Fatal("null-action audit record escaped CHECK constraint")
			}
		})
	}
}

func TestLendingAvailableActionsTrackActualOfferAndTerminalState(t *testing.T) {
	for _, source := range []string{"automatic", "manual"} {
		for _, operation := range []string{lending.Accept, lending.Decline} {
			t.Run(source+"/"+operation, func(t *testing.T) {
				score := 90
				if source == "manual" {
					score = 65
				}
				pool, id := lendingFixture(t, score)
				if source == "manual" {
					executeLending(t, lendingService(pool), manualCommand(id, "approved"), 201)
				}
				ctx := context.Background()
				repo := NewApplicationRepository(pool, time.Second)
				approved, err := repo.Get(ctx, draftOwner, id)
				if err != nil || !reflect.DeepEqual(approved.AvailableActions, []string{"accept", "decline"}) {
					t.Fatalf("active offer actions=%v error=%v", approved.AvailableActions, err)
				}
				var offerID string
				if err := pool.QueryRow(ctx, `UPDATE offers SET status='withdrawn' WHERE application_id=$1 RETURNING id::text`, id).Scan(&offerID); err != nil {
					t.Fatal(err)
				}
				withoutOffer, err := repo.Get(ctx, draftOwner, id)
				if err != nil || withoutOffer.Status != application.Approved || withoutOffer.AvailableActions == nil || len(withoutOffer.AvailableActions) != 0 {
					t.Fatalf("missing active offer actions=%v error=%v", withoutOffer.AvailableActions, err)
				}
				if _, err := pool.Exec(ctx, `UPDATE offers SET status='active' WHERE id=$1`, offerID); err != nil {
					t.Fatal(err)
				}
				cmd := offerCommand(id, offerID, operation)
				cmd.Version = approved.Version
				executeLending(t, lendingService(pool), cmd, 200)
				terminal, err := repo.Get(ctx, draftOwner, id)
				if err != nil || terminal.AvailableActions == nil || len(terminal.AvailableActions) != 0 {
					t.Fatalf("terminal offer actions=%v error=%v", terminal.AvailableActions, err)
				}
			})
		}
	}
}

func TestLendingOfferActionsReplayExpiryAndTerminalDecision(t *testing.T) {
	for _, operation := range []string{lending.Accept, lending.Decline} {
		t.Run(operation, func(t *testing.T) {
			pool, id := lendingFixture(t, 90)
			ctx := context.Background()
			reads := review.NewService(NewReviewRepository(pool, time.Second))
			initial, err := reads.Decision(ctx, draftOwner, id)
			if err != nil || initial.Offer == nil {
				t.Fatal("missing automatic offer")
			}
			cmd := offerCommand(id, initial.Offer.OfferID, operation)
			original, body := executeLending(t, lendingService(pool), cmd, 200)
			wantStatus, wantOfferStatus, eventType := application.Signed, "accepted", "OFFER_ACCEPTED"
			if operation == lending.Decline {
				wantStatus, wantOfferStatus, eventType = application.Closed, "withdrawn", "OFFER_DECLINED"
			}
			if body.Status != wantStatus || body.Version != 5 || body.Decision == nil || *body.Decision != "approved" ||
				body.Offer == nil || body.Offer.Status != wantOfferStatus || body.Offer.OfferID != initial.Offer.OfferID {
				t.Fatalf("offer action result %+v", body)
			}
			var actor, offerID, action string
			if err := pool.QueryRow(ctx, `SELECT actor_id::text,offer_id::text,action FROM application_events WHERE application_id=$1 AND event_type=$2`, id, eventType).
				Scan(&actor, &offerID, &action); err != nil || actor != draftOwner.UserID || offerID != cmd.OfferID || action != operation {
				t.Fatalf("action audit actor=%s offer=%s action=%s error=%v", actor, offerID, action, err)
			}
			var acceptedOffer, signedBy *string
			var signedAt *time.Time
			if err := pool.QueryRow(ctx, `SELECT accepted_offer_id::text,signed_by::text,signed_at FROM applications WHERE id=$1`, id).
				Scan(&acceptedOffer, &signedBy, &signedAt); err != nil {
				t.Fatal(err)
			}
			if operation == lending.Accept {
				if acceptedOffer == nil || *acceptedOffer != cmd.OfferID || signedBy == nil || *signedBy != draftOwner.UserID || signedAt == nil {
					t.Fatal("demonstration confirmation audit not persisted")
				}
			} else if acceptedOffer != nil || signedBy != nil || signedAt != nil {
				t.Fatal("decline persisted a demonstration signature")
			}
			assertLendingCounts(t, pool, id, 5, 1, 1, 1)
			before := lendingFingerprint(t, pool)
			replay, _ := executeLending(t, lendingService(pool), cmd, 200)
			assertLendingResponseEqual(t, original, replay)
			if lendingFingerprint(t, pool) != before {
				t.Fatal("stale-version idempotency replay mutated state")
			}
			changed := cmd
			changed.Version++
			if _, err := lendingService(pool).Execute(ctx, changed); !errors.Is(err, lending.ErrIdempotencyConflict) {
				t.Fatalf("same key changed offer body: %v", err)
			}
			// Preserve the schema constraint expires_at > created_at while simulating
			// the passage of 24 hours. Durable business replay survives key retention.
			if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at=clock_timestamp()-interval '2 days',expires_at=clock_timestamp()-interval '1 day'`); err != nil {
				t.Fatal(err)
			}
			executeLending(t, lendingService(pool), cmd, 200)
			assertLendingCounts(t, pool, id, 5, 1, 1, 1)
			newKey := cmd
			newKey.Key = uuid.NewString()
			executeLending(t, lendingService(pool), newKey, 200)
			assertLendingCounts(t, pool, id, 5, 1, 1, 1)
			resultingVersion := newKey
			resultingVersion.Key, resultingVersion.Version = uuid.NewString(), 5
			executeLending(t, lendingService(pool), resultingVersion, 200)
			unrelatedVersion := newKey
			unrelatedVersion.Key, unrelatedVersion.Version = uuid.NewString(), 2
			if _, err := lendingService(pool).Execute(ctx, unrelatedVersion); !errors.Is(err, application.ErrVersionConflict) {
				t.Fatalf("unrelated completed-action version accepted: %v", err)
			}
			assertLendingCounts(t, pool, id, 5, 1, 1, 1)
			// The opposite business action is never considered a successful replay.
			opposite := newKey
			opposite.Key = uuid.NewString()
			opposite.Operation = lending.Accept
			if operation == lending.Accept {
				opposite.Operation = lending.Decline
			}
			before = lendingFingerprint(t, pool)
			if _, err := lendingService(pool).Execute(ctx, opposite); err == nil {
				t.Fatal("opposite terminal action succeeded")
			}
			if lendingFingerprint(t, pool) != before {
				t.Fatal("opposite action mutated state")
			}
			got, err := reads.Decision(ctx, draftOwner, id)
			if err != nil || !reflect.DeepEqual(body, got) {
				t.Fatal("terminal GET lost original decision or offer")
			}
			app, err := NewApplicationRepository(pool, time.Second).Get(ctx, draftOwner, id)
			if err != nil || len(app.AvailableActions) != 0 {
				t.Fatal("terminal action remains available")
			}
		})
	}
}

func TestLendingManualExpiredKeyCannotCreateSecondDecision(t *testing.T) {
	pool, id := lendingFixture(t, 65)
	cmd := manualCommand(id, "approved")
	executeLending(t, lendingService(pool), cmd, 201)
	if _, err := pool.Exec(context.Background(), `UPDATE idempotency_keys SET created_at=clock_timestamp()-interval '2 days',expires_at=clock_timestamp()-interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	if _, err := lendingService(pool).Execute(context.Background(), cmd); !errors.Is(err, application.ErrInvalidStatus) && !errors.Is(err, application.ErrVersionConflict) {
		t.Fatalf("expired manual key repeated final business action: %v", err)
	}
	assertLendingCounts(t, pool, id, 5, 1, 1, 1)
}

func TestLendingManualStoredReplayAfterClientAccept(t *testing.T) {
	pool, id := lendingFixture(t, 65)
	cmd := manualCommand(id, "approved")
	original, approved := executeLending(t, lendingService(pool), cmd, 201)
	accept := offerCommand(id, approved.Offer.OfferID, lending.Accept)
	accept.Version = 5
	_, signed := executeLending(t, lendingService(pool), accept, 200)
	if signed.Status != application.Signed || signed.Version != 6 {
		t.Fatal("manual offer acceptance did not advance revision")
	}
	before := lendingFingerprint(t, pool)
	replay, oldBody := executeLending(t, lendingService(pool), cmd, 201)
	assertLendingResponseEqual(t, original, replay)
	if oldBody.Status != application.Approved || oldBody.Offer.Status != "active" || oldBody.Version != 5 || lendingFingerprint(t, pool) != before {
		t.Fatal("manual replay was recomputed using current signed state")
	}
	assertLendingCounts(t, pool, id, 6, 1, 1, 2)
}

func TestLendingManualConcurrentSameKeyDifferentBodies(t *testing.T) {
	pool, id := lendingFixture(t, 65)
	one := manualCommand(id, "approved")
	two := one
	two.Comment = "Другое независимое внутреннее основание."
	results := runLendingConcurrent(pool, []lending.Command{one, two})
	winners, conflicts := 0, 0
	for _, result := range results {
		if result.err == nil {
			winners++
		} else if errors.Is(result.err, lending.ErrIdempotencyConflict) {
			conflicts++
		} else {
			t.Fatalf("same key concurrent different bodies error=%v", result.err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("same key winners=%d conflicts=%d", winners, conflicts)
	}
	assertLendingCounts(t, pool, id, 5, 1, 1, 1)
}

func TestLendingOfferOwnershipVersionAndCurrentOffer(t *testing.T) {
	pool, id := lendingFixture(t, 90)
	ctx := context.Background()
	var offerID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM offers WHERE application_id=$1`, id).Scan(&offerID); err != nil {
		t.Fatal(err)
	}
	service := lendingService(pool)
	base := offerCommand(id, offerID, lending.Accept)
	foreignApplication := lendingAdditionalApplication(t, pool, otherDraftOwner, draftFields(), 90)
	var foreignOffer string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM offers WHERE application_id=$1`, foreignApplication).Scan(&foreignOffer); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*lending.Command)
		want error
	}{
		{"another company", func(c *lending.Command) { c.ActorID, c.CompanyID = otherDraftOwner.UserID, otherDraftOwner.CompanyID }, application.ErrNotFound},
		{"wrong company claim", func(c *lending.Command) { c.CompanyID = otherDraftOwner.CompanyID }, application.ErrNotFound},
		{"stale version", func(c *lending.Command) { c.Version-- }, application.ErrVersionConflict},
		{"unknown offer", func(c *lending.Command) { c.OfferID = uuid.NewString() }, lending.ErrOfferNotActive},
		{"another application's offer", func(c *lending.Command) { c.OfferID = foreignOffer }, lending.ErrOfferNotActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := base
			tc.edit(&cmd)
			before := lendingFingerprint(t, pool)
			if _, err := service.Execute(ctx, cmd); !errors.Is(err, tc.want) {
				t.Fatalf("access/state validation error=%v want=%v", err, tc.want)
			}
			if lendingFingerprint(t, pool) != before {
				t.Fatal("forbidden action changed state")
			}
		})
	}
	// Reusing an owner's successful key cannot expose its response to another user.
	executeLending(t, service, base, 200)
	foreign := base
	foreign.ActorID, foreign.CompanyID = otherDraftOwner.UserID, otherDraftOwner.CompanyID
	before := lendingFingerprint(t, pool)
	if _, err := service.Execute(ctx, foreign); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("foreign key replay: %v", err)
	}
	if lendingFingerprint(t, pool) != before {
		t.Fatal("foreign replay changed state")
	}
}

func TestLendingIdempotencyKeysAreIsolatedPerUser(t *testing.T) {
	pool, one := lendingFixture(t, 90)
	two := lendingAdditionalApplication(t, pool, otherDraftOwner, draftFields(), 90)
	key := uuid.NewString()
	for _, tc := range []struct {
		id    string
		scope application.Scope
	}{{one, draftOwner}, {two, otherDraftOwner}} {
		var offerID string
		if err := pool.QueryRow(context.Background(), `SELECT id::text FROM offers WHERE application_id=$1`, tc.id).Scan(&offerID); err != nil {
			t.Fatal(err)
		}
		cmd := offerCommand(tc.id, offerID, lending.Accept)
		cmd.ActorID, cmd.CompanyID, cmd.Key = tc.scope.UserID, tc.scope.CompanyID, key
		_, body := executeLending(t, lendingService(pool), cmd, 200)
		if body.ApplicationID != tc.id || body.Status != application.Signed {
			t.Fatal("another user's idempotency result was returned")
		}
		assertLendingCounts(t, pool, tc.id, 5, 1, 1, 1)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(DISTINCT user_id) FROM idempotency_keys WHERE key=$1`, key).Scan(&count); err != nil || count != 2 {
		t.Fatal("user-scoped idempotency keys collided")
	}
}

func TestLendingConcurrentBankDecisions(t *testing.T) {
	for _, different := range []bool{false, true} {
		name := "two approvals"
		if different {
			name = "approval versus rejection"
		}
		t.Run(name, func(t *testing.T) {
			pool, id := lendingFixture(t, 65)
			secondEmployee := uuid.NewString()
			if _, err := pool.Exec(context.Background(), `INSERT INTO users(id,profile_key,phone,full_name,role)
 VALUES($1,'second-employee','+79999990001','Synthetic second employee','BANK_EMPLOYEE')`, secondEmployee); err != nil {
				t.Fatal(err)
			}
			one, two := manualCommand(id, "approved"), manualCommand(id, "approved")
			two.ActorID = secondEmployee
			if different {
				two.Decision, two.Offer = "rejected", nil
			}
			results := runLendingConcurrent(pool, []lending.Command{one, two})
			winners := 0
			for _, result := range results {
				if result.err == nil {
					winners++
				} else if !errors.Is(result.err, application.ErrInvalidStatus) && !errors.Is(result.err, application.ErrVersionConflict) {
					t.Fatalf("concurrent manual error: %v", result.err)
				}
			}
			if winners != 1 {
				t.Fatalf("concurrent manual winners=%d", winners)
			}
			var offers int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM offers WHERE application_id=$1`, id).Scan(&offers); err != nil {
				t.Fatal(err)
			}
			assertLendingCounts(t, pool, id, 5, 1, offers, 1)
		})
	}
}

type lendingConcurrentResult struct {
	response lending.Response
	err      error
}

func runLendingConcurrent(pool *pgxpool.Pool, commands []lending.Command) []lendingConcurrentResult {
	results := make([]lendingConcurrentResult, len(commands))
	start := make(chan struct{})
	var group sync.WaitGroup
	for i, cmd := range commands {
		group.Go(func() {
			<-start
			results[i].response, results[i].err = lendingService(pool).Execute(context.Background(), cmd)
		})
	}
	close(start)
	group.Wait()
	return results
}

func TestLendingConcurrentOfferActionsAndIdempotency(t *testing.T) {
	for _, scenario := range []string{"accept versus decline", "double accept", "double decline", "same key", "same key changed body"} {
		t.Run(scenario, func(t *testing.T) {
			pool, id := lendingFixture(t, 90)
			var offerID string
			if err := pool.QueryRow(context.Background(), `SELECT id::text FROM offers WHERE application_id=$1`, id).Scan(&offerID); err != nil {
				t.Fatal(err)
			}
			one := offerCommand(id, offerID, lending.Accept)
			two := one
			two.Key = uuid.NewString()
			switch scenario {
			case "accept versus decline":
				two.Operation = lending.Decline
			case "double decline":
				one.Operation, two.Operation = lending.Decline, lending.Decline
			case "same key":
				two.Key = one.Key
			case "same key changed body":
				two.Key, two.Version = one.Key, 5
			}
			results := runLendingConcurrent(pool, []lending.Command{one, two})
			winners, conflicts := 0, 0
			for _, result := range results {
				if result.err == nil {
					winners++
				} else {
					conflicts++
					if !errors.Is(result.err, application.ErrInvalidStatus) && !errors.Is(result.err, application.ErrVersionConflict) &&
						!errors.Is(result.err, lending.ErrOfferNotActive) && !errors.Is(result.err, lending.ErrIdempotencyConflict) {
						t.Fatalf("concurrent offer error: %v", result.err)
					}
				}
			}
			if scenario == "accept versus decline" || scenario == "same key changed body" {
				if winners != 1 || conflicts != 1 {
					t.Fatalf("contradictory actions winners=%d conflicts=%d", winners, conflicts)
				}
			} else if winners != 2 {
				t.Fatalf("identical action failed to replay: winners=%d", winners)
			}
			if scenario == "same key" {
				assertLendingResponseEqual(t, results[0].response, results[1].response)
			}
			assertLendingCounts(t, pool, id, 5, 1, 1, 1)
		})
	}
}

func TestLendingRollbackAndRestartRetry(t *testing.T) {
	for _, operation := range []string{lending.Manual, lending.Accept, lending.Decline} {
		for _, failureTable := range []string{"application_events", "idempotency_keys"} {
			t.Run(operation+"/"+failureTable, func(t *testing.T) {
				score := 90
				if operation == lending.Manual {
					score = 65
				}
				pool, id := lendingFixture(t, score)
				cmd := manualCommand(id, "approved")
				if operation != lending.Manual {
					var offerID string
					if err := pool.QueryRow(context.Background(), `SELECT id::text FROM offers WHERE application_id=$1`, id).Scan(&offerID); err != nil {
						t.Fatal(err)
					}
					cmd = offerCommand(id, offerID, operation)
				}
				if _, err := pool.Exec(context.Background(), `CREATE FUNCTION fail_lending_write() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic lending transaction failure'; END; $$;
 CREATE TRIGGER fail_lending_write BEFORE INSERT OR UPDATE ON `+failureTable+` FOR EACH ROW EXECUTE FUNCTION fail_lending_write()`); err != nil {
					t.Fatal(err)
				}
				before := lendingFingerprint(t, pool)
				if _, err := lendingService(pool).Execute(context.Background(), cmd); !errors.Is(err, application.ErrStorage) {
					t.Fatalf("write failure error=%v", err)
				}
				if lendingFingerprint(t, pool) != before {
					t.Fatal("partial lending transaction survived rollback")
				}
				if _, err := pool.Exec(context.Background(), `DROP TRIGGER fail_lending_write ON `+failureTable); err != nil {
					t.Fatal(err)
				}
				wantHTTP := 200
				if operation == lending.Manual {
					wantHTTP = 201
				}
				original, _ := executeLending(t, lendingService(pool), cmd, wantHTTP)
				// Independent connections and repository objects recover the durable result.
				restartedPool, err := pgxpool.NewWithConfig(context.Background(), pool.Config().Copy())
				if err != nil {
					t.Fatal(err)
				}
				defer restartedPool.Close()
				before = lendingFingerprint(t, pool)
				replay, _ := executeLending(t, lendingService(restartedPool), cmd, wantHTTP)
				assertLendingResponseEqual(t, original, replay)
				if lendingFingerprint(t, pool) != before {
					t.Fatal("reconnect replay mutated durable state")
				}
				assertLendingCounts(t, pool, id, 5, 1, 1, 1)
			})
		}
	}
}

func TestLendingMigrationRoundTripAndRefusesLossyDowngrade(t *testing.T) {
	t.Run("clean down and up", func(t *testing.T) {
		pool := authTestPool(t)
		ctx := context.Background()
		for _, direction := range []string{"down", "up"} {
			migration, err := os.ReadFile("../../migrations/000011_manual_offer_actions." + direction + ".sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, string(migration)); err != nil {
				t.Fatalf("migration11 %s: %v", direction, err)
			}
		}
		var columns int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND
 ((table_name='credit_decisions' AND column_name='internal_comment') OR
 (table_name='application_events' AND column_name IN ('offer_id','action','action_version')))`).Scan(&columns); err != nil || columns != 4 {
			t.Fatalf("migration11 columns=%d error=%v", columns, err)
		}
		id := lendingAdditionalApplication(t, pool, draftOwner, draftFields(), 65)
		executeLending(t, lendingService(pool), manualCommand(id, "approved"), 201)
	})
	for _, operation := range []string{lending.Manual, lending.Accept, lending.Decline} {
		t.Run("protect "+operation+" audit", func(t *testing.T) {
			score := 90
			if operation == lending.Manual {
				score = 65
			}
			pool, id := lendingFixture(t, score)
			cmd := manualCommand(id, "approved")
			wantStatus := 201
			if operation != lending.Manual {
				var offerID string
				if err := pool.QueryRow(context.Background(), `SELECT id::text FROM offers WHERE application_id=$1`, id).Scan(&offerID); err != nil {
					t.Fatal(err)
				}
				cmd, wantStatus = offerCommand(id, offerID, operation), 200
			}
			executeLending(t, lendingService(pool), cmd, wantStatus)
			migration, err := os.ReadFile("../../migrations/000011_manual_offer_actions.down.sql")
			if err != nil {
				t.Fatal(err)
			}
			before := lendingFingerprint(t, pool)
			conn, err := pool.Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(context.Background(), string(migration)); err == nil {
				t.Fatal("migration11 discarded durable action audit")
			}
			if _, err := conn.Exec(context.Background(), "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			if lendingFingerprint(t, pool) != before {
				t.Fatal("refused migration11 downgrade changed audit")
			}
		})
	}
}
