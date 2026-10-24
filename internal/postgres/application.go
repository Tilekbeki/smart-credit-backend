package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/credit"
)

// ApplicationRepository scopes every application read and write to its owner and company.
type ApplicationRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var _ application.Repository = (*ApplicationRepository)(nil)

func NewApplicationRepository(pool *pgxpool.Pool, timeout time.Duration) *ApplicationRepository {
	return &ApplicationRepository{pool: pool, timeout: timeout}
}

const applicationColumns = `a.id::text, a.owner_id::text, a.company_id::text,
	p.code, a.goal_id, a.goal_description, a.amount, a.term_months,
	a.monthly_revenue, a.monthly_expenses, p.name, a.status, a.version,
	a.created_at, a.updated_at, a.submitted_at,
	EXISTS(SELECT 1 FROM offers o WHERE o.application_id=a.id AND o.status='active' AND o.is_primary
	 AND o.decision_id=(SELECT id FROM credit_decisions WHERE application_id=a.id ORDER BY version DESC LIMIT 1))`

func scanApplication(row pgx.Row) (application.Application, error) {
	var result application.Application
	var activeOffer bool
	err := row.Scan(&result.ApplicationID, &result.UserID, &result.CompanyID,
		&result.ProductID, &result.Purpose, &result.PurposeDescription,
		&result.RequestedAmountRub, &result.TermMonths, &result.MonthlyRevenueRub,
		&result.MonthlyExpensesRub, &result.ProductName, &result.Status, &result.Version,
		&result.CreatedAt, &result.UpdatedAt, &result.SubmittedAt, &activeOffer)
	if err != nil {
		return application.Application{}, err
	}
	result.CreatedAt = result.CreatedAt.UTC()
	result.UpdatedAt = result.UpdatedAt.UTC()
	if result.SubmittedAt != nil {
		utc := result.SubmittedAt.UTC()
		result.SubmittedAt = &utc
	}
	result.AvailableActions = []string{}
	if result.Status == application.Draft {
		result.AvailableActions = []string{"edit", "submit"}
	} else if (result.Status == application.Approved || result.Status == application.CounterOffer) && activeOffer {
		result.AvailableActions = []string{"accept", "decline"}
	}
	return result, nil
}

func validApplicationScope(scope application.Scope) bool {
	return validUUID(scope.UserID) && validUUID(scope.CompanyID)
}

func applicationByID(ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, scope application.Scope, id string) (application.Application, error) {
	result, err := scanApplication(query.QueryRow(ctx, `SELECT `+applicationColumns+`
		FROM applications a LEFT JOIN credit_products p ON p.id=a.product_id
		WHERE a.id=$1 AND a.owner_id=$2 AND a.company_id=$3`, id, scope.UserID, scope.CompanyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Application{}, application.ErrNotFound
	}
	if err != nil {
		return application.Application{}, application.ErrStorage
	}
	return result, nil
}

// Hold a shared product lock until commit, so an administrator cannot deactivate
// the selected product between the lookup and the application write.
func applicationProductID(ctx context.Context, tx pgx.Tx, fields application.Fields) (*string, error) {
	if fields.ProductID == nil {
		return nil, nil
	}
	var id string
	var minAmount, maxAmount int64
	var minTerm, maxTerm int
	var purposes []string
	err := tx.QueryRow(ctx, `SELECT id::text, min_amount, max_amount,
		min_term_months, max_term_months, purposes FROM credit_products
		WHERE code=$1 AND active FOR SHARE`, *fields.ProductID).
		Scan(&id, &minAmount, &maxAmount, &minTerm, &maxTerm, &purposes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, application.ErrProductNotFound
	}
	if err != nil {
		return nil, application.ErrStorage
	}
	// Recheck product-dependent rules while holding the catalog row lock: terms
	// may have changed since service validation, before this transaction started.
	if len(application.ValidateProductFields(fields, credit.Product{
		MinAmountRub: minAmount, MaxAmountRub: maxAmount, MinTermMonths: minTerm, MaxTermMonths: maxTerm, Purposes: purposes,
	})) != 0 {
		return nil, application.ErrInvalidInput
	}
	return &id, nil
}

func (r *ApplicationRepository) Create(ctx context.Context, scope application.Scope, fields application.Fields) (application.Application, error) {
	if !validApplicationScope(scope) {
		return application.Application{}, application.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return application.Application{}, application.ErrStorage
	}
	defer tx.Rollback(ctx)
	productID, err := applicationProductID(ctx, tx, fields)
	if err != nil {
		return application.Application{}, err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO applications
		(owner_id, company_id, product_id, goal_id, goal_description, amount,
		term_months, monthly_revenue, monthly_expenses)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`,
		scope.UserID, scope.CompanyID, productID, fields.Purpose, fields.PurposeDescription,
		fields.RequestedAmountRub, fields.TermMonths, fields.MonthlyRevenueRub, fields.MonthlyExpensesRub).Scan(&id)
	if err != nil {
		return application.Application{}, application.ErrStorage
	}
	if _, err := tx.Exec(ctx, `INSERT INTO application_events
		(application_id, sequence, event_type, status, title, actor_id, created_at)
		VALUES ($1,1,'draft_created','draft','Черновик создан',$2,clock_timestamp())`, id, scope.UserID); err != nil {
		return application.Application{}, application.ErrStorage
	}
	result, err := applicationByID(ctx, tx, scope, id)
	if err != nil {
		return application.Application{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return application.Application{}, application.ErrStorage
	}
	return result, nil
}

func (r *ApplicationRepository) Get(ctx context.Context, scope application.Scope, id string) (application.Application, error) {
	if !validApplicationScope(scope) || !validUUID(id) {
		return application.Application{}, application.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return applicationByID(ctx, r.pool, scope, id)
}

func (r *ApplicationRepository) List(ctx context.Context, scope application.Scope, filter application.ListFilter) (application.Page, error) {
	if !validApplicationScope(scope) {
		return application.Page{}, application.ErrNotFound
	}
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 ||
		(filter.Status != nil && !application.ValidStatus(*filter.Status)) {
		return application.Page{}, application.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	// Both statements see the same snapshot even if drafts are added concurrently.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return application.Page{}, application.ErrStorage
	}
	defer tx.Rollback(ctx)
	var status any
	if filter.Status != nil {
		status = string(*filter.Status)
	}
	result := application.Page{Items: []application.Summary{}, Limit: filter.Limit, Offset: filter.Offset}
	const scopeWhere = ` WHERE a.owner_id=$1 AND a.company_id=$2 AND ($3::text IS NULL OR a.status=$3)`
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM applications a`+scopeWhere,
		scope.UserID, scope.CompanyID, status).Scan(&result.Total); err != nil {
		return application.Page{}, application.ErrStorage
	}
	rows, err := tx.Query(ctx, `SELECT a.id::text, p.code, p.name, a.amount, a.term_months,
		a.status, a.version, a.created_at, a.updated_at
		FROM applications a LEFT JOIN credit_products p ON p.id=a.product_id`+scopeWhere+`
		ORDER BY a.created_at DESC, a.id DESC LIMIT $4 OFFSET $5`,
		scope.UserID, scope.CompanyID, status, filter.Limit, filter.Offset)
	if err != nil {
		return application.Page{}, application.ErrStorage
	}
	defer rows.Close()
	for rows.Next() {
		var item application.Summary
		if err := rows.Scan(&item.ApplicationID, &item.ProductID, &item.ProductName,
			&item.RequestedAmountRub, &item.TermMonths, &item.Status, &item.Version,
			&item.CreatedAt, &item.UpdatedAt); err != nil {
			return application.Page{}, application.ErrStorage
		}
		item.CreatedAt, item.UpdatedAt = item.CreatedAt.UTC(), item.UpdatedAt.UTC()
		result.Items = append(result.Items, item)
	}
	if rows.Err() != nil {
		return application.Page{}, application.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return application.Page{}, application.ErrStorage
	}
	return result, nil
}

func (r *ApplicationRepository) UpdateDraft(ctx context.Context, scope application.Scope, id string, expectedVersion int64, fields application.Fields) (application.Application, error) {
	if !validApplicationScope(scope) || !validUUID(id) {
		return application.Application{}, application.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return application.Application{}, application.ErrStorage
	}
	defer tx.Rollback(ctx)
	var status application.Status
	var version int64
	err = tx.QueryRow(ctx, `SELECT status, version FROM applications
		WHERE id=$1 AND owner_id=$2 AND company_id=$3 FOR UPDATE`, id, scope.UserID, scope.CompanyID).Scan(&status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.Application{}, application.ErrNotFound
	}
	if err != nil {
		return application.Application{}, application.ErrStorage
	}
	if status != application.Draft {
		return application.Application{}, application.ErrInvalidStatus
	}
	if version != expectedVersion {
		return application.Application{}, application.ErrVersionConflict
	}
	productID, err := applicationProductID(ctx, tx, fields)
	if err != nil {
		return application.Application{}, err
	}
	updated, err := tx.Exec(ctx, `UPDATE applications SET product_id=$4, goal_id=$5,
		goal_description=$6, amount=$7, term_months=$8, monthly_revenue=$9, monthly_expenses=$10,
		version=version+1, updated_at=clock_timestamp()
		WHERE id=$1 AND owner_id=$2 AND company_id=$3 AND status='draft' AND version=$11`,
		id, scope.UserID, scope.CompanyID, productID, fields.Purpose, fields.PurposeDescription,
		fields.RequestedAmountRub, fields.TermMonths, fields.MonthlyRevenueRub, fields.MonthlyExpensesRub, expectedVersion)
	if err != nil {
		return application.Application{}, application.ErrStorage
	}
	if updated.RowsAffected() != 1 {
		return application.Application{}, application.ErrVersionConflict
	}
	// The application lock serializes sequence allocation and version changes.
	if _, err := tx.Exec(ctx, `INSERT INTO application_events
		(application_id, sequence, event_type, status, title, actor_id, created_at)
		SELECT $1, COALESCE(max(sequence),0)+1, 'draft_updated', 'draft', 'Черновик обновлён', $2, clock_timestamp()
		FROM application_events WHERE application_id=$1`, id, scope.UserID); err != nil {
		return application.Application{}, application.ErrStorage
	}
	result, err := applicationByID(ctx, tx, scope, id)
	if err != nil {
		return application.Application{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return application.Application{}, application.ErrStorage
	}
	return result, nil
}

func (r *ApplicationRepository) Timeline(ctx context.Context, scope application.Scope, id string) ([]application.Event, error) {
	if !validApplicationScope(scope) || !validUUID(id) {
		return nil, application.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, application.ErrStorage
	}
	defer tx.Rollback(ctx)
	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM applications
		WHERE id=$1 AND owner_id=$2 AND company_id=$3)`, id, scope.UserID, scope.CompanyID).Scan(&exists)
	if err != nil {
		return nil, application.ErrStorage
	}
	if !exists {
		return nil, application.ErrNotFound
	}
	rows, err := tx.Query(ctx, `SELECT e.id::text, e.event_type, e.sequence, e.status,
		e.created_at, e.actor_id::text, e.title FROM application_events e
		JOIN applications a ON a.id=e.application_id
		WHERE e.application_id=$1 AND a.owner_id=$2 AND a.company_id=$3 ORDER BY e.sequence`,
		id, scope.UserID, scope.CompanyID)
	if err != nil {
		return nil, application.ErrStorage
	}
	defer rows.Close()
	events := []application.Event{}
	for rows.Next() {
		var event application.Event
		if err := rows.Scan(&event.EventID, &event.EventType, &event.Sequence, &event.Status,
			&event.CreatedAt, &event.ActorID, &event.Title); err != nil {
			return nil, application.ErrStorage
		}
		event.CreatedAt = event.CreatedAt.UTC()
		events = append(events, event)
	}
	if rows.Err() != nil {
		return nil, application.ErrStorage
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, application.ErrStorage
	}
	return events, nil
}
