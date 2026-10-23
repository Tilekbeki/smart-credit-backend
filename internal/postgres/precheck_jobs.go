package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/pipeline"
)

// PrecheckQueue uses short transactions and fenced leases. No external operation
// takes place while a transaction or application row lock is held.
type PrecheckQueue struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var _ pipeline.Queue = (*PrecheckQueue)(nil)

func NewPrecheckQueue(pool *pgxpool.Pool, timeout time.Duration) *PrecheckQueue {
	return &PrecheckQueue{pool: pool, timeout: timeout}
}

func (q *PrecheckQueue) Claim(ctx context.Context, lease time.Duration) (*pipeline.Job, error) {
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
	var job pipeline.Job
	var attempts, maxAttempts int
	err = tx.QueryRow(ctx, `SELECT id::text, application_id::text, attempt_count, max_attempts
		FROM jobs WHERE type=$1 AND processing_stage='prechecks' AND (
		(status='pending' AND available_at <= clock_timestamp()) OR
		(status='running' AND lease_until <= clock_timestamp()))
		ORDER BY available_at, id FOR UPDATE SKIP LOCKED LIMIT 1`, pipeline.JobType).
		Scan(&job.ID, &job.ApplicationID, &attempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, pipeline.ErrStorage
	}
	var status string
	var rawSnapshot []byte
	if err := tx.QueryRow(ctx, `SELECT status, financial_snapshot FROM applications
		WHERE id=$1 FOR UPDATE`, job.ApplicationID).Scan(&status, &rawSnapshot); err != nil {
		return nil, pipeline.ErrStorage
	}
	if attempts >= maxAttempts || (status != "submitted" && status != "processing") {
		code := "attempts_exhausted"
		if status != "submitted" && status != "processing" {
			code = "invalid_application_state"
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET status='failed', last_error=$2,
			lease_token=NULL, lease_until=NULL, finished_at=clock_timestamp(), updated_at=clock_timestamp()
			WHERE id=$1`, job.ID, code); err != nil {
			return nil, pipeline.ErrStorage
		}
		if _, err := tx.Exec(ctx, `UPDATE applications SET processing_error=$2
			WHERE id=$1 AND status IN ('submitted','processing')`, job.ApplicationID, code); err != nil {
			return nil, pipeline.ErrStorage
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, pipeline.ErrStorage
		}
		return nil, nil
	}
	if err := tx.QueryRow(ctx, `UPDATE jobs SET status='running', attempt_count=attempt_count+1,
		lease_token=gen_random_uuid(), lease_until=clock_timestamp()+$2::bigint*interval '1 microsecond',
		started_at=COALESCE(started_at,clock_timestamp()), finished_at=NULL, updated_at=clock_timestamp()
		WHERE id=$1 RETURNING lease_token::text, attempt_count`, job.ID, lease.Microseconds()).
		Scan(&job.LeaseToken, &job.Attempt); err != nil {
		return nil, pipeline.ErrStorage
	}
	if status == "submitted" {
		if _, err := tx.Exec(ctx, `UPDATE applications SET status='processing',
			version=version+1, updated_at=clock_timestamp() WHERE id=$1`, job.ApplicationID); err != nil {
			return nil, pipeline.ErrStorage
		}
		if err := appendPrecheckEvent(ctx, tx, job.ApplicationID, "PROCESSING_STARTED", "Автоматические проверки начаты"); err != nil {
			return nil, pipeline.ErrStorage
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, pipeline.ErrStorage
	}
	// A malformed persisted snapshot consumes an attempt and is retried through
	// the normal failure path. Rolling back here would create an infinite hot loop.
	job.Snapshot = decodePrecheckSnapshot(rawSnapshot)
	return &job, nil
}

func decodePrecheckSnapshot(data []byte) pipeline.FinancialSnapshot {
	var snapshot pipeline.FinancialSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return pipeline.FinancialSnapshot{}
	}
	// Numeric zero is valid, but an absent/null financial field must not silently
	// become zero through Go's JSON defaults and produce an invented BKI result.
	var shape struct {
		Company struct {
			Profile struct {
				Financials map[string]json.RawMessage `json:"financials"`
			} `json:"profile"`
		} `json:"company"`
	}
	if err := json.Unmarshal(data, &shape); err != nil {
		return pipeline.FinancialSnapshot{}
	}
	for _, key := range []string{"monthlyRevenueRub", "monthlyExpensesRub", "existingDebtRub", "existingMonthlyPaymentsRub", "overdueCount"} {
		value, exists := shape.Company.Profile.Financials[key]
		if !exists || string(value) == "null" {
			return pipeline.FinancialSnapshot{}
		}
	}
	return snapshot
}

func (q *PrecheckQueue) Complete(ctx context.Context, job pipeline.Job, results pipeline.Results) error {
	if !validUUID(job.ID) || !validUUID(job.ApplicationID) || !validUUID(job.LeaseToken) || results.SchemaVersion != 1 {
		return pipeline.ErrLeaseLost
	}
	data, err := json.Marshal(results)
	if err != nil {
		return pipeline.ErrStorage
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	if err := lockPrecheckJob(ctx, tx, job); err != nil {
		return err
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM applications WHERE id=$1 FOR UPDATE`, job.ApplicationID).Scan(&status); err != nil {
		return pipeline.ErrStorage
	}
	if status != "processing" {
		return pipeline.ErrLeaseLost
	}
	updated, err := tx.Exec(ctx, `UPDATE jobs SET status='awaiting_scoring', processing_stage='scoring', precheck_results=$4,
		available_at=clock_timestamp(),
		lease_token=NULL, lease_until=NULL, finished_at=NULL, last_error=NULL, updated_at=clock_timestamp()
		WHERE id=$1 AND application_id=$2 AND lease_token=$3 AND status='running'
		AND processing_stage='prechecks' AND lease_until>clock_timestamp()`, job.ID, job.ApplicationID, job.LeaseToken, data)
	if err != nil {
		return pipeline.ErrStorage
	}
	if updated.RowsAffected() != 1 {
		return pipeline.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET processing_error=NULL WHERE id=$1`, job.ApplicationID); err != nil {
		return pipeline.ErrStorage
	}
	for _, event := range []struct{ kind, title string }{
		{"COMPANY_CHECK_COMPLETED", "Проверка регистрационных данных компании завершена"},
		{"CREDIT_HISTORY_CHECK_COMPLETED", "Демонстрационная проверка кредитной истории завершена"},
		{"PRECHECKS_COMPLETED", "Предварительные проверки завершены. Ожидание скоринга"},
	} {
		if err := appendPrecheckEvent(ctx, tx, job.ApplicationID, event.kind, event.title); err != nil {
			return pipeline.ErrStorage
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return pipeline.ErrStorage
	}
	return nil
}

func (q *PrecheckQueue) Fail(ctx context.Context, job pipeline.Job, code string, delay time.Duration) error {
	if !validUUID(job.ID) || !validUUID(job.ApplicationID) || !validUUID(job.LeaseToken) {
		return pipeline.ErrLeaseLost
	}
	// Persist only a small diagnostic code; exceptions, SQL text and client data
	// never enter last_error or the public application representation.
	switch code {
	case "invalid_snapshot", "prechecks_timeout", "prechecks_failed":
	default:
		code = "prechecks_failed"
	}
	delay = max(0, min(delay, 30*time.Second))
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	if err := lockPrecheckJob(ctx, tx, job); err != nil {
		return err
	}
	var ignored string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM applications WHERE id=$1 FOR UPDATE`, job.ApplicationID).Scan(&ignored); err != nil {
		return pipeline.ErrStorage
	}
	updated, err := tx.Exec(ctx, `UPDATE jobs SET
		status=CASE WHEN attempt_count>=max_attempts THEN 'failed' ELSE 'pending' END,
		available_at=clock_timestamp()+$5::bigint*interval '1 microsecond',
		finished_at=CASE WHEN attempt_count>=max_attempts THEN clock_timestamp() ELSE NULL END,
		last_error=$4, lease_token=NULL, lease_until=NULL, updated_at=clock_timestamp()
		WHERE id=$1 AND application_id=$2 AND lease_token=$3 AND status='running'
		AND lease_until>clock_timestamp()`, job.ID, job.ApplicationID, job.LeaseToken, code, delay.Microseconds())
	if err != nil {
		return pipeline.ErrStorage
	}
	if updated.RowsAffected() != 1 {
		return pipeline.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE applications SET processing_error=$2 WHERE id=$1
		AND status IN ('submitted','processing')`, job.ApplicationID, code); err != nil {
		return pipeline.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return pipeline.ErrStorage
	}
	return nil
}

func lockPrecheckJob(ctx context.Context, tx pgx.Tx, job pipeline.Job) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM jobs WHERE id=$1 AND application_id=$2
		AND type=$4 AND processing_stage='prechecks' AND status='running' AND lease_token=$3 AND lease_until>clock_timestamp()
		FOR UPDATE`, job.ID, job.ApplicationID, job.LeaseToken, pipeline.JobType).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pipeline.ErrLeaseLost
	}
	if err != nil {
		return pipeline.ErrStorage
	}
	return nil
}

// The caller holds the application row lock, serializing sequence allocation.
// The unique event index is an additional guard against duplicate lifecycle events.
func appendPrecheckEvent(ctx context.Context, tx pgx.Tx, applicationID, kind, title string) error {
	_, err := tx.Exec(ctx, `INSERT INTO application_events
		(application_id, sequence, event_type, status, title, actor_id, created_at)
		SELECT $1, (SELECT COALESCE(max(sequence),0)+1 FROM application_events WHERE application_id=$1),
		$2::text, 'processing', $3, NULL, clock_timestamp()
		WHERE NOT EXISTS (SELECT 1 FROM application_events WHERE application_id=$1 AND event_type=$2::text)
		ON CONFLICT DO NOTHING`, applicationID, kind, title)
	return err
}
