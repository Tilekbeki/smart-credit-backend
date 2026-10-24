package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/pipeline"
)

var _ pipeline.DecisionQueue = (*PrecheckQueue)(nil)

func (q *PrecheckQueue) ClaimDecision(ctx context.Context, lease time.Duration) (*pipeline.Job, error) {
	if lease < time.Millisecond || lease > 24*time.Hour {
		return nil, pipeline.ErrStorage
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	job := pipeline.Job{Stage: pipeline.DecisionStage}
	var attempts, maxAttempts int
	err = tx.QueryRow(ctx, `SELECT id::text,application_id::text,decision_attempt_count,max_attempts,
		precheck_results,scoring_request,scoring_response FROM jobs WHERE type=$1 AND
		((status='awaiting_decision' AND available_at<=clock_timestamp()) OR
		(status='running' AND processing_stage='decision' AND lease_until<=clock_timestamp()))
		ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, pipeline.JobType).
		Scan(&job.ID, &job.ApplicationID, &attempts, &maxAttempts, &job.DecisionInput.PrecheckResults,
			&job.DecisionInput.ScoringRequest, &job.DecisionInput.ScoringResponse)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, pipeline.ErrStorage
	}
	job.DecisionInput.ApplicationID = job.ApplicationID
	var status string
	var submittedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT status,financial_snapshot,product_snapshot,submitted_at FROM applications
		WHERE id=$1 FOR UPDATE`, job.ApplicationID).Scan(&status, &job.DecisionInput.FinancialSnapshot,
		&job.DecisionInput.ProductSnapshot, &submittedAt); err != nil {
		return nil, pipeline.ErrStorage
	}
	if submittedAt != nil {
		job.DecisionInput.SubmittedAt = *submittedAt
	}
	if status != "processing" || attempts >= maxAttempts {
		code := "decision_attempts_exhausted"
		if status != "processing" {
			code = "invalid_application_state"
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status='failed',processing_stage='decision',last_error=$2,
			lease_token=NULL,lease_until=NULL,finished_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, job.ID, code); err != nil {
			return nil, pipeline.ErrStorage
		}
		if _, err := tx.Exec(ctx, `UPDATE applications SET processing_error=$2 WHERE id=$1 AND status='processing'`, job.ApplicationID, code); err != nil {
			return nil, pipeline.ErrStorage
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, pipeline.ErrStorage
		}
		return nil, nil
	}
	if err := tx.QueryRow(ctx, `UPDATE jobs SET status='running',processing_stage='decision',decision_attempt_count=decision_attempt_count+1,
		lease_token=gen_random_uuid(),lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond',
		finished_at=NULL,updated_at=clock_timestamp() WHERE id=$1 RETURNING lease_token::text,decision_attempt_count`, job.ID, lease.Microseconds()).
		Scan(&job.LeaseToken, &job.Attempt); err != nil {
		return nil, pipeline.ErrStorage
	}
	if err := appendPrecheckEvent(ctx, tx, job.ApplicationID, "DECISION_PROCESSING_STARTED", "Формирование кредитного решения начато"); err != nil {
		return nil, pipeline.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, pipeline.ErrStorage
	}
	return &job, nil
}

func (q *PrecheckQueue) CompleteDecision(ctx context.Context, job pipeline.Job, result decision.Result) error {
	if err := decision.ValidateResult(result); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	if err := lockDecisionJob(ctx, tx, job); err != nil {
		return err
	}
	var status string
	var productDBID *string
	var productDBCode *string
	var productSnapshot []byte
	var financialSnapshot []byte
	if err := tx.QueryRow(ctx, `SELECT status,product_id::text,product_snapshot,financial_snapshot,
		(SELECT code FROM credit_products WHERE id=applications.product_id) FROM applications WHERE id=$1 FOR UPDATE`, job.ApplicationID).
		Scan(&status, &productDBID, &productSnapshot, &financialSnapshot, &productDBCode); err != nil {
		return pipeline.ErrStorage
	}
	if status != "processing" {
		return pipeline.ErrLeaseLost
	}
	// Validate against the persisted immutable product, not a live product row.
	if result.Offer != nil {
		var frozen credit.Product
		var snapshot pipeline.FinancialSnapshot
		if json.Unmarshal(productSnapshot, &frozen) != nil || json.Unmarshal(financialSnapshot, &snapshot) != nil ||
			productDBID == nil || productDBCode == nil || *productDBCode != frozen.ID ||
			frozen.ID != result.Offer.ProductID || frozen.AnnualRatePercent != result.Offer.AnnualRatePercent ||
			!offerMatchesSnapshots(result, frozen, snapshot) {
			return decision.ErrInvalidData
		}
	}
	var decidedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&decidedAt); err != nil {
		return pipeline.ErrStorage
	}
	assessment, err := json.Marshal(struct {
		SchemaVersion int             `json:"schemaVersion"`
		Source        string          `json:"source"`
		Result        decision.Result `json:"result"`
		AssessedAt    time.Time       `json:"assessedAt"`
	}{1, "automatic", result, decidedAt})
	if err != nil {
		return decision.ErrInvalidData
	}
	if result.Status != application.ManualReview {
		reasons, err := json.Marshal(result.Reasons)
		if err != nil {
			return decision.ErrInvalidData
		}
		amount := int64(0)
		if result.Offer != nil {
			amount = result.Offer.AmountRub
		}
		var decisionID string
		if err := tx.QueryRow(ctx, `INSERT INTO credit_decisions(application_id,version,source,outcome,risk_category,
			approved_amount,reasons,policy_version,scoring_job_id,created_at)
			SELECT $1,COALESCE(max(version),0)+1,'AUTO',$2,$3,$4,$5,$6,$7,$8 FROM credit_decisions WHERE application_id=$1
			RETURNING id::text`, job.ApplicationID, string(result.Status), result.RiskLevel, amount, reasons, result.PolicyVersion, job.ID, decidedAt).
			Scan(&decisionID); err != nil {
			return pipeline.ErrStorage
		}
		if offer := result.Offer; offer != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO offers(application_id,decision_id,product_id,version,is_primary,status,amount,term_months,
				annual_rate_percent,monthly_payment,total_payment,overpayment,created_at)
				SELECT $1,$2,$3,COALESCE(max(version),0)+1,true,'active',$4,$5,$6::numeric,$7,$8,$9,$10 FROM offers WHERE application_id=$1`,
				job.ApplicationID, decisionID, *productDBID, offer.AmountRub, offer.TermMonths, string(offer.AnnualRatePercent), offer.MonthlyPaymentRub,
				offer.TotalPaymentRub, offer.OverpaymentRub, decidedAt); err != nil {
				return pipeline.ErrStorage
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET status=$2,version=version+1,processing_error=NULL,updated_at=$3 WHERE id=$1`, job.ApplicationID, string(result.Status), decidedAt); err != nil {
		return pipeline.ErrStorage
	}
	kind, title := decisionEvent(result.Status)
	if _, err := tx.Exec(ctx, `INSERT INTO application_events(application_id,sequence,event_type,status,title,actor_id,created_at)
		SELECT $1,COALESCE(max(sequence),0)+1,$2,$3,$4,NULL,$5 FROM application_events WHERE application_id=$1`, job.ApplicationID, kind, string(result.Status), title, decidedAt); err != nil {
		return pipeline.ErrStorage
	}
	// Fence again after writing artifacts. An expired lease rolls everything back.
	updated, err := tx.Exec(ctx, `UPDATE jobs SET status='completed',assessment_result=$4::jsonb,finished_at=$5,
		lease_token=NULL,lease_until=NULL,last_error=NULL,updated_at=$5 WHERE id=$1 AND application_id=$2
		AND lease_token=$3 AND status='running' AND processing_stage='decision' AND lease_until>clock_timestamp()`,
		job.ID, job.ApplicationID, job.LeaseToken, assessment, decidedAt)
	if err != nil {
		return pipeline.ErrStorage
	}
	if updated.RowsAffected() != 1 {
		return pipeline.ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return pipeline.ErrStorage
	}
	return nil
}

func offerMatchesSnapshots(result decision.Result, product credit.Product, snapshot pipeline.FinancialSnapshot) bool {
	fields, offer := snapshot.Fields, result.Offer
	if offer == nil || fields.ProductID == nil || *fields.ProductID != product.ID || fields.RequestedAmountRub == nil || fields.TermMonths == nil {
		return false
	}
	if offer.AmountRub < product.MinAmountRub || offer.AmountRub > product.MaxAmountRub ||
		offer.TermMonths < product.MinTermMonths || offer.TermMonths > product.MaxTermMonths || offer.TermMonths != *fields.TermMonths {
		return false
	}
	if result.Status == application.Approved {
		return offer.AmountRub == *fields.RequestedAmountRub
	}
	return result.Status == application.CounterOffer && offer.AmountRub < *fields.RequestedAmountRub
}

func (q *PrecheckQueue) FailDecision(ctx context.Context, job pipeline.Job, code string, delay time.Duration, retryable bool) error {
	switch code {
	case "decision_invalid_data", "decision_storage_failed", "decision_timeout", "decision_failed":
	default:
		code = "decision_failed"
	}
	delay = max(0, min(delay, 30*time.Second))
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	if err := lockDecisionJob(ctx, tx, job); err != nil {
		return err
	}
	if err := lockScoringApplication(ctx, tx, job.ApplicationID); err != nil {
		return err
	}
	updated, err := tx.Exec(ctx, `UPDATE jobs SET status=CASE WHEN NOT $6::boolean OR decision_attempt_count>=max_attempts THEN 'failed' ELSE 'awaiting_decision' END,
		available_at=clock_timestamp()+$5::bigint*interval '1 microsecond',
		finished_at=CASE WHEN NOT $6::boolean OR decision_attempt_count>=max_attempts THEN clock_timestamp() ELSE NULL END,
		last_error=$4,lease_token=NULL,lease_until=NULL,updated_at=clock_timestamp()
		WHERE id=$1 AND application_id=$2 AND lease_token=$3 AND processing_stage='decision'
		AND status='running' AND lease_until>clock_timestamp()`, job.ID, job.ApplicationID, job.LeaseToken, code, delay.Microseconds(), retryable)
	if err != nil {
		return pipeline.ErrStorage
	}
	if updated.RowsAffected() != 1 {
		return pipeline.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET processing_error=$2 WHERE id=$1`, job.ApplicationID, code); err != nil {
		return pipeline.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return pipeline.ErrStorage
	}
	return nil
}

func lockDecisionJob(ctx context.Context, tx pgx.Tx, job pipeline.Job) error {
	if !validUUID(job.ID) || !validUUID(job.ApplicationID) || !validUUID(job.LeaseToken) || job.Stage != pipeline.DecisionStage {
		return pipeline.ErrLeaseLost
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM jobs WHERE id=$1 AND application_id=$2 AND lease_token=$3
		AND type=$4 AND processing_stage='decision' AND status='running' AND lease_until>clock_timestamp() FOR UPDATE`, job.ID, job.ApplicationID, job.LeaseToken, pipeline.JobType).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pipeline.ErrLeaseLost
	}
	if err != nil {
		return pipeline.ErrStorage
	}
	return nil
}

func decisionEvent(status application.Status) (string, string) {
	switch status {
	case application.Approved:
		return "APPLICATION_APPROVED", "Кредитная заявка одобрена"
	case application.Rejected:
		return "APPLICATION_REJECTED", "По кредитной заявке принято решение об отказе"
	case application.CounterOffer:
		return "COUNTER_OFFER_CREATED", "Сформировано альтернативное предложение"
	default:
		return "MANUAL_REVIEW_REQUIRED", "Заявка передана на ручное рассмотрение"
	}
}
