package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/document"
	"smart-credit-backend/internal/pipeline"
)

// SubmissionRepository commits the frozen inputs, transition, history and job
// together. The application lock also serializes competing PATCH and submit.
type SubmissionRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var _ pipeline.SubmissionRepository = (*SubmissionRepository)(nil)

func NewSubmissionRepository(pool *pgxpool.Pool, timeout time.Duration) *SubmissionRepository {
	return &SubmissionRepository{pool: pool, timeout: timeout}
}

func (r *SubmissionRepository) Submit(ctx context.Context, scope application.Scope, id string, expectedVersion int64, verifiedDocumentIDs []string) (pipeline.SubmitResult, error) {
	if !validApplicationScope(scope) || !validUUID(id) {
		return pipeline.SubmitResult{}, application.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	defer tx.Rollback(ctx)
	app, err := scanApplication(tx.QueryRow(ctx, `SELECT `+applicationColumns+`
		FROM applications a LEFT JOIN credit_products p ON p.id=a.product_id
		WHERE a.id=$1 AND a.owner_id=$2 AND a.company_id=$3 FOR UPDATE OF a`, id, scope.UserID, scope.CompanyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return pipeline.SubmitResult{}, application.ErrNotFound
	}
	if err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	if app.Status != application.Draft {
		return repeatedSubmission(ctx, tx, app)
	}
	if app.Version != expectedVersion {
		return pipeline.SubmitResult{}, application.ErrVersionConflict
	}
	profile, err := submissionCompany(ctx, tx, scope.CompanyID)
	if err != nil {
		return pipeline.SubmitResult{}, err
	}
	var product *credit.Product
	if app.ProductID != nil {
		value, err := scanProduct(tx.QueryRow(ctx, `SELECT `+productColumns+`
			FROM credit_products WHERE code=$1 AND active FOR SHARE`, *app.ProductID))
		if errors.Is(err, pgx.ErrNoRows) {
			return pipeline.SubmitResult{}, &pipeline.ValidationError{Details: []pipeline.FieldError{
				{Field: "productId", Code: "product_unavailable", Message: "Кредитный продукт недоступен."},
			}}
		}
		if err != nil {
			return pipeline.SubmitResult{}, pipeline.ErrStorage
		}
		product = &value
	}
	documents, err := submissionDocuments(ctx, tx, id, verifiedDocumentIDs)
	if err != nil {
		return pipeline.SubmitResult{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	if issues := pipeline.ValidateSubmission(pipeline.ValidationInput{
		Fields: app.Fields, Product: product, Company: profile, Documents: documents,
	}, now.UTC()); len(issues) > 0 {
		return pipeline.SubmitResult{}, &pipeline.ValidationError{Details: issues}
	}
	financialSnapshot, err := json.Marshal(pipeline.FinancialSnapshot{
		SchemaVersion: 1, ApplicationID: id, UserID: scope.UserID, CompanyID: scope.CompanyID,
		Fields: app.Fields, Company: profile, Documents: documents,
	})
	if err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	productSnapshot, err := json.Marshal(product)
	if err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	result := pipeline.SubmitResult{ApplicationID: id, Status: application.Submitted, NextStage: pipeline.JobType}
	err = tx.QueryRow(ctx, `UPDATE applications SET status='submitted', version=version+1,
		submitted_at=clock_timestamp(), updated_at=clock_timestamp(), processing_error=NULL,
		financial_snapshot=$4::jsonb, product_snapshot=$5::jsonb,
		contact_name=(SELECT full_name FROM users WHERE id=$2 AND company_id=$3 AND role='CLIENT'),
		contact_role='Предприниматель'
		WHERE id=$1 AND owner_id=$2 AND company_id=$3 AND status='draft' AND version=$6
		RETURNING submitted_at`, id, scope.UserID, scope.CompanyID,
		financialSnapshot, productSnapshot, expectedVersion).Scan(&result.SubmittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pipeline.SubmitResult{}, application.ErrVersionConflict
	}
	if err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	if _, err := tx.Exec(ctx, `INSERT INTO application_events
		(application_id, sequence, event_type, status, title, actor_id, created_at)
		SELECT $1, COALESCE(max(sequence),0)+1, 'APPLICATION_SUBMITTED', 'submitted',
		'Заявка отправлена', $2, clock_timestamp() FROM application_events WHERE application_id=$1`, id, scope.UserID); err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	if _, err := tx.Exec(ctx, `INSERT INTO jobs (application_id,type,status,max_attempts)
		VALUES ($1,$2,'pending',5)`, id, pipeline.JobType); err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	result.SubmittedAt = result.SubmittedAt.UTC()
	return result, nil
}

func repeatedSubmission(ctx context.Context, tx pgx.Tx, app application.Application) (pipeline.SubmitResult, error) {
	if (app.Status != application.Submitted && app.Status != application.Processing) || app.SubmittedAt == nil {
		return pipeline.SubmitResult{}, application.ErrInvalidStatus
	}
	var jobStatus string
	var prechecksCompleted bool
	err := tx.QueryRow(ctx, `SELECT status,precheck_results IS NOT NULL FROM jobs WHERE application_id=$1 AND type=$2`,
		app.ApplicationID, pipeline.JobType).Scan(&jobStatus, &prechecksCompleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return pipeline.SubmitResult{}, application.ErrInvalidStatus
	}
	if err != nil {
		return pipeline.SubmitResult{}, pipeline.ErrStorage
	}
	next := pipeline.JobType
	if prechecksCompleted || jobStatus == pipeline.AwaitingScoring || jobStatus == pipeline.AwaitingDecision {
		next = pipeline.AwaitingScoring
	}
	// This branch has no mutations. Releasing the lock by rollback avoids a
	// second write transaction and retains the original submission timestamp.
	return pipeline.SubmitResult{ApplicationID: app.ApplicationID, Status: app.Status,
		SubmittedAt: app.SubmittedAt.UTC(), NextStage: next}, nil
}

func submissionCompany(ctx context.Context, tx pgx.Tx, id string) (pipeline.CompanySnapshot, error) {
	var result pipeline.CompanySnapshot
	profile := &result.Profile
	err := tx.QueryRow(ctx, `SELECT id::text, name, COALESCE(organization_type,''), COALESCE(inn,''),
		COALESCE(registration_number,''), COALESCE(to_char(registration_date,'YYYY-MM-DD'),''), COALESCE(okved,''),
		COALESCE(director_name,''), COALESCE(contact_phone,''), COALESCE(contact_email,''), COALESCE(contact_address,''),
		monthly_revenue, monthly_expenses, existing_debt, monthly_debt_payments, overdue_count,
		financial_data_source, registration_status FROM companies WHERE id=$1 FOR SHARE`, id).Scan(
		&profile.ID, &profile.Name, &profile.OrganizationType, &profile.INN,
		&profile.RegistrationNumber, &profile.RegistrationDate, &profile.OKVED,
		&profile.DirectorName, &profile.Contact.Phone, &profile.Contact.Email, &profile.Contact.Address,
		&profile.Financials.MonthlyRevenueRub, &profile.Financials.MonthlyExpensesRub,
		&profile.Financials.ExistingDebtRub, &profile.Financials.ExistingMonthlyPaymentsRub,
		&profile.Financials.OverdueCount, &result.FinancialDataSource, &result.RegistrationStatus,
	)
	if err != nil {
		return pipeline.CompanySnapshot{}, pipeline.ErrStorage
	}
	profile.Simulation = true
	return result, nil
}

func submissionDocuments(ctx context.Context, tx pgx.Tx, id string, verifiedIDs []string) ([]document.Metadata, error) {
	// File integrity was checked outside this transaction. Lock only the same
	// ready records so a pending/missing/unverified upload can never satisfy policy.
	rows, err := tx.Query(ctx, `SELECT `+documentColumns+` FROM documents d
		WHERE d.application_id=$1 AND d.status='ready' AND d.id::text=ANY($2::text[])
		ORDER BY d.created_at,d.id FOR SHARE`, id, verifiedIDs)
	if err != nil {
		return nil, pipeline.ErrStorage
	}
	defer rows.Close()
	items := []document.Metadata{}
	for rows.Next() {
		item, err := scanDocument(rows)
		if err != nil {
			return nil, pipeline.ErrStorage
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return nil, pipeline.ErrStorage
	}
	return items, nil
}
