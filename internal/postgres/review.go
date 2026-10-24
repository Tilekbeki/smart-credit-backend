package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/review"
	"smart-credit-backend/internal/scoring"
)

type ReviewRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var _ review.Repository = (*ReviewRepository)(nil)

func NewReviewRepository(pool *pgxpool.Pool, timeout time.Duration) *ReviewRepository {
	return &ReviewRepository{pool: pool, timeout: timeout}
}

const reviewColumns = `a.id::text,a.owner_id::text,a.company_id::text,a.status,a.version,
 a.created_at,a.updated_at,a.submitted_at,a.financial_snapshot,a.product_snapshot`

// Correlated reads use the same statement snapshot as the application. Only the
// latest real decision and its primary offer are selected, including closed or
// accepted offers. Infrastructure diagnostics and lease tokens are not selected.
const reviewArtifacts = `,
 (SELECT jsonb_build_object('status',j.status,'stage',j.processing_stage,'hasError',j.last_error IS NOT NULL,
  'availableAt',j.available_at,'leaseUntil',j.lease_until,'readAt',statement_timestamp(),
  'prechecks',j.precheck_results,'scoring',j.scoring_response,'scoringAt',j.scoring_received_at,'assessment',j.assessment_result)
  FROM jobs j WHERE j.application_id=a.id AND j.type=$4),
 (SELECT jsonb_build_object('outcome',d.outcome,'reasons',d.reasons,'riskLevel',d.risk_category,
  'assessedAt',d.created_at,'reviewedBy',d.decided_by,
  'offer',(SELECT jsonb_build_object('offerId',o.id,'applicationId',o.application_id,
   'productId',a.product_snapshot->>'id','amountRub',o.amount,'termMonths',o.term_months,
   'annualRatePercent',o.annual_rate_percent,'monthlyPaymentRub',o.monthly_payment,'expiresAt',NULL,'status',o.status)
   FROM offers o WHERE o.application_id=a.id AND o.decision_id=d.id AND o.is_primary ORDER BY o.version DESC LIMIT 1))
  FROM credit_decisions d WHERE d.application_id=a.id ORDER BY d.version DESC LIMIT 1)`

func scanReview(row pgx.Row, artifacts bool) (review.Record, error) {
	var r review.Record
	a := &r.Application
	args := []any{&a.ApplicationID, &a.UserID, &a.CompanyID, &a.Status, &a.Version, &a.CreatedAt, &a.UpdatedAt, &a.SubmittedAt, &r.FinancialSnapshot, &r.ProductSnapshot}
	var jobJSON, finalJSON []byte
	if artifacts {
		args = append(args, &jobJSON, &finalJSON)
	}
	if err := row.Scan(args...); err != nil {
		return r, err
	}
	a.CreatedAt = a.CreatedAt.UTC()
	a.UpdatedAt = a.UpdatedAt.UTC()
	if a.SubmittedAt != nil {
		t := a.SubmittedAt.UTC()
		a.SubmittedAt = &t
	}
	if len(jobJSON) > 0 {
		if json.Unmarshal(jobJSON, &r.Job) != nil || r.Job == nil {
			return r, application.ErrStorage
		}
		var raw struct {
			Scoring json.RawMessage `json:"scoring"`
		}
		if json.Unmarshal(jobJSON, &raw) != nil {
			return r, application.ErrStorage
		}
		if len(raw.Scoring) > 0 && string(raw.Scoring) != "null" {
			response, err := scoring.DecodeResponse(raw.Scoring)
			if err != nil {
				return r, application.ErrStorage
			}
			r.Job.Scoring = &response
		}
		if r.Job.ScoringAt != nil {
			t := r.Job.ScoringAt.UTC()
			r.Job.ScoringAt = &t
		}
	}
	if len(finalJSON) > 0 && json.Unmarshal(finalJSON, &r.Final) != nil {
		return r, application.ErrStorage
	}
	if r.Final != nil {
		r.Final.AssessedAt = r.Final.AssessedAt.UTC()
	}
	return r, nil
}

func (r *ReviewRepository) Read(ctx context.Context, scope *application.Scope, id string) (review.Record, error) {
	if !validUUID(id) {
		return review.Record{}, application.ErrInvalidInput
	}
	if scope != nil && !validApplicationScope(*scope) {
		return review.Record{}, application.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var owner, companyID *string
	if scope != nil {
		owner = &scope.UserID
		companyID = &scope.CompanyID
	}
	result, err := scanReview(r.pool.QueryRow(ctx, `SELECT `+reviewColumns+reviewArtifacts+` FROM applications a
 WHERE a.id=$1 AND (($2::uuid IS NOT NULL AND a.owner_id=$2 AND a.company_id=$3) OR
 ($2::uuid IS NULL AND a.status<>'draft' AND a.submitted_at IS NOT NULL))`, id, owner, companyID, pipeline.JobType), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return review.Record{}, application.ErrNotFound
	}
	if err != nil {
		return review.Record{}, application.ErrStorage
	}
	return result, nil
}

func (r *ReviewRepository) List(ctx context.Context, f review.Filter) ([]review.Record, int64, error) {
	if !review.ValidFilter(f) {
		return nil, 0, application.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	// Count and page must see one database snapshot during concurrent submit or
	// decision commits. This transaction is explicitly read-only and takes no locks.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, application.ErrStorage
	}
	defer tx.Rollback(ctx)
	const where = ` FROM applications a WHERE a.status<>'draft' AND a.submitted_at IS NOT NULL
 AND ($1::text IS NULL OR a.status=$1) AND ($2::uuid IS NULL OR a.company_id=$2)`
	var total int64
	if err := tx.QueryRow(ctx, `SELECT count(*)`+where, f.Status, f.CompanyID).Scan(&total); err != nil {
		return nil, 0, application.ErrStorage
	}
	rows, err := tx.Query(ctx, `SELECT `+reviewColumns+where+` ORDER BY a.created_at DESC,a.id DESC LIMIT $3 OFFSET $4`, f.Status, f.CompanyID, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, application.ErrStorage
	}
	defer rows.Close()
	result := []review.Record{}
	for rows.Next() {
		item, err := scanReview(rows, false)
		if err != nil {
			return nil, 0, application.ErrStorage
		}
		result = append(result, item)
	}
	if rows.Err() != nil {
		return nil, 0, application.ErrStorage
	}
	rows.Close()
	if tx.Commit(ctx) != nil {
		return nil, 0, application.ErrStorage
	}
	return result, total, nil
}
