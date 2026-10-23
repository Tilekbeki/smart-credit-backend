package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/scoring"
)

var _ pipeline.ScoringQueue = (*PrecheckQueue)(nil)

func (q *PrecheckQueue) ClaimScoring(ctx context.Context, lease time.Duration) (*pipeline.Job, error) {
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
	job := pipeline.Job{Stage: pipeline.ScoringStage}
	var attempts, maxAttempts int
	var rawResponse, rawRequest []byte
	var receivedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT id::text,application_id::text,scoring_attempt_count,max_attempts,
		precheck_results,scoring_request,scoring_response,scoring_received_at FROM jobs
		WHERE type=$1 AND ((status='awaiting_scoring' AND available_at<=clock_timestamp())
		OR (status='running' AND processing_stage='scoring' AND lease_until<=clock_timestamp()))
		ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1`, pipeline.JobType).
		Scan(&job.ID, &job.ApplicationID, &attempts, &maxAttempts, &job.PrecheckResults, &rawRequest, &rawResponse, &receivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, pipeline.ErrStorage
	}
	var status string
	var snapshot []byte
	if err := tx.QueryRow(ctx, `SELECT status,financial_snapshot FROM applications WHERE id=$1 FOR UPDATE`, job.ApplicationID).
		Scan(&status, &snapshot); err != nil {
		return nil, pipeline.ErrStorage
	}
	job.Snapshot = decodePrecheckSnapshot(snapshot)
	validSavedInput := false
	if expected, err := pipeline.BuildScoringRequest(job.Snapshot, job.PrecheckResults); err == nil && expected.ApplicationID == job.ApplicationID {
		data, err := json.Marshal(expected)
		if err != nil {
			return nil, pipeline.ErrStorage
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(scoring_request=$2::jsonb,false) FROM jobs WHERE id=$1`, job.ID, data).Scan(&validSavedInput); err != nil {
			return nil, pipeline.ErrStorage
		}
	}
	// A committed response is authoritative after process recreation. Repair the
	// waiting marker without another network call, even if the attempt budget ended.
	if status == "processing" && len(rawResponse) > 0 && receivedAt != nil && validSavedInput {
		if _, err := scoring.DecodeResponse(rawResponse); err == nil {
			if _, err := tx.Exec(ctx, `UPDATE jobs SET status='awaiting_decision',processing_stage='scoring',
				lease_token=NULL,lease_until=NULL,finished_at=NULL,last_error=NULL,updated_at=clock_timestamp()
				WHERE id=$1`, job.ID); err != nil {
				return nil, pipeline.ErrStorage
			}
			if err := appendPrecheckEvent(ctx, tx, job.ApplicationID, "SCORING_COMPLETED", "Результат скоринга сохранён. Ожидание кредитного решения"); err != nil {
				return nil, pipeline.ErrStorage
			}
			if _, err := tx.Exec(ctx, `UPDATE applications SET processing_error=NULL WHERE id=$1`, job.ApplicationID); err != nil {
				return nil, pipeline.ErrStorage
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, pipeline.ErrStorage
			}
			return nil, nil
		}
	}
	code := ""
	if status != "processing" {
		code = "invalid_application_state"
	} else if len(rawResponse) > 0 && !validSavedInput {
		code = "scoring_invalid_data"
	} else if len(rawResponse) > 0 {
		code = "scoring_invalid_response"
	} else if attempts >= maxAttempts {
		code = "scoring_attempts_exhausted"
	}
	if code != "" {
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status='failed',processing_stage='scoring',last_error=$2,
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
	if err := tx.QueryRow(ctx, `UPDATE jobs SET status='running',processing_stage='scoring',
		scoring_attempt_count=scoring_attempt_count+1,lease_token=gen_random_uuid(),
		lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond',finished_at=NULL,updated_at=clock_timestamp()
		WHERE id=$1 RETURNING lease_token::text,scoring_attempt_count`, job.ID, lease.Microseconds()).Scan(&job.LeaseToken, &job.Attempt); err != nil {
		return nil, pipeline.ErrStorage
	}
	if err := appendPrecheckEvent(ctx, tx, job.ApplicationID, "SCORING_STARTED", "Оценка риска начата"); err != nil {
		return nil, pipeline.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, pipeline.ErrStorage
	}
	return &job, nil
}

func (q *PrecheckQueue) SaveScoringRequest(ctx context.Context, job pipeline.Job, request scoring.Request) error {
	if request.ApplicationID != job.ApplicationID || scoring.ValidateRequest(request) != nil {
		return pipeline.ErrInvalidScoringData
	}
	data, err := json.Marshal(request)
	if err != nil {
		return pipeline.ErrInvalidScoringData
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	if err := lockScoringJob(ctx, tx, job); err != nil {
		return err
	}
	if err := lockScoringApplication(ctx, tx, job.ApplicationID); err != nil {
		return err
	}
	var same bool
	if err := tx.QueryRow(ctx, `SELECT scoring_request IS NULL OR scoring_request=$2::jsonb FROM jobs WHERE id=$1`, job.ID, data).Scan(&same); err != nil {
		return pipeline.ErrStorage
	}
	if !same {
		return pipeline.ErrInvalidScoringData
	}
	updated, err := tx.Exec(ctx, `UPDATE jobs SET scoring_request=COALESCE(scoring_request,$4::jsonb),updated_at=clock_timestamp()
		WHERE id=$1 AND application_id=$2 AND lease_token=$3 AND lease_until>clock_timestamp()
		AND status='running' AND processing_stage='scoring' AND scoring_response IS NULL`, job.ID, job.ApplicationID, job.LeaseToken, data)
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

func (q *PrecheckQueue) CompleteScoring(ctx context.Context, job pipeline.Job, response scoring.Response) error {
	if scoring.ValidateResponse(response) != nil {
		return pipeline.ErrInvalidScoringData
	}
	data, err := json.Marshal(response)
	if err != nil {
		return pipeline.ErrInvalidScoringData
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	if err := lockScoringJob(ctx, tx, job); err != nil {
		return err
	}
	if err := lockScoringApplication(ctx, tx, job.ApplicationID); err != nil {
		return err
	}
	updated, err := tx.Exec(ctx, `UPDATE jobs SET status='awaiting_decision',scoring_response=$4::jsonb,
		scoring_received_at=clock_timestamp(),lease_token=NULL,lease_until=NULL,last_error=NULL,
		finished_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND application_id=$2 AND lease_token=$3
		AND lease_until>clock_timestamp() AND processing_stage='scoring' AND status='running'
		AND scoring_request IS NOT NULL AND scoring_response IS NULL`, job.ID, job.ApplicationID, job.LeaseToken, data)
	if err != nil {
		return pipeline.ErrStorage
	}
	if updated.RowsAffected() != 1 {
		return pipeline.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET processing_error=NULL WHERE id=$1`, job.ApplicationID); err != nil {
		return pipeline.ErrStorage
	}
	if err := appendPrecheckEvent(ctx, tx, job.ApplicationID, "SCORING_COMPLETED", "Результат скоринга сохранён. Ожидание кредитного решения"); err != nil {
		return pipeline.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return pipeline.ErrStorage
	}
	return nil
}

func (q *PrecheckQueue) FailScoring(ctx context.Context, job pipeline.Job, code string, delay time.Duration, retryable bool) error {
	code = safeScoringErrorCode(code)
	delay = max(0, min(delay, 30*time.Second))
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	if err := lockScoringJob(ctx, tx, job); err != nil {
		return err
	}
	if err := lockScoringApplication(ctx, tx, job.ApplicationID); err != nil {
		return err
	}
	updated, err := tx.Exec(ctx, `UPDATE jobs SET
		status=CASE WHEN NOT $6::boolean OR scoring_attempt_count>=max_attempts THEN 'failed' ELSE 'awaiting_scoring' END,
		available_at=clock_timestamp()+$5::bigint*interval '1 microsecond',
		finished_at=CASE WHEN NOT $6::boolean OR scoring_attempt_count>=max_attempts THEN clock_timestamp() ELSE NULL END,
		last_error=$4,lease_token=NULL,lease_until=NULL,updated_at=clock_timestamp()
		WHERE id=$1 AND application_id=$2 AND lease_token=$3 AND status='running'
		AND processing_stage='scoring' AND scoring_response IS NULL AND lease_until>clock_timestamp()`,
		job.ID, job.ApplicationID, job.LeaseToken, code, delay.Microseconds(), retryable)
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

func lockScoringJob(ctx context.Context, tx pgx.Tx, job pipeline.Job) error {
	if !validUUID(job.ID) || !validUUID(job.ApplicationID) || !validUUID(job.LeaseToken) || job.Stage != pipeline.ScoringStage {
		return pipeline.ErrLeaseLost
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM jobs WHERE id=$1 AND application_id=$2 AND lease_token=$3
		AND type=$4 AND processing_stage='scoring' AND status='running' AND lease_until>clock_timestamp() FOR UPDATE`, job.ID, job.ApplicationID, job.LeaseToken, pipeline.JobType).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pipeline.ErrLeaseLost
	}
	if err != nil {
		return pipeline.ErrStorage
	}
	return nil
}

func lockScoringApplication(ctx context.Context, tx pgx.Tx, applicationID string) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM applications WHERE id=$1 FOR UPDATE`, applicationID).Scan(&status); err != nil {
		return pipeline.ErrStorage
	}
	if status != "processing" {
		return pipeline.ErrLeaseLost
	}
	return nil
}

func safeScoringErrorCode(code string) string {
	switch code {
	case "scoring_timeout", "scoring_connection_refused", "scoring_dns", "scoring_connectivity",
		"scoring_http5xx", "scoring_http429", "scoring_invalid_json", "scoring_invalid_response",
		"scoring_unexpected_status", "scoring_invalid_data", "scoring_storage_failed",
		"scoring_canceled", "scoring_invalid_request", "scoring_failed":
		return code
	default:
		return "scoring_failed"
	}
}
