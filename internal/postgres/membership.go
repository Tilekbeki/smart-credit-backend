package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/company"
)

// An absent selector is resolved only when exactly one active membership exists.
// The selected membership is locked until commit, including before key replay.
func resolveCompany(ctx context.Context, tx pgx.Tx, actor, selected string, write bool) (string, error) {
	if selected == "" {
		var ids []string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(m.company_id::text),'{}') FROM user_company_memberships m JOIN users u ON u.id=m.user_id WHERE u.id=$1 AND u.role='CLIENT' AND m.status='active'`, actor).Scan(&ids); err != nil {
			return "", application.ErrStorage
		}
		if len(ids) == 0 {
			return "", application.ErrNotFound
		}
		if len(ids) != 1 {
			return "", application.ErrCompanyRequired
		}
		selected = ids[0]
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT lock_company_access($1,$2,$3)`, actor, selected, write).Scan(&allowed); err != nil {
		return "", application.ErrStorage
	}
	if !allowed {
		return "", application.ErrNotFound
	}
	return selected, nil
}

func (r *CompanyCreditRepository) ResolveCompany(ctx context.Context, actor, selected string, write bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", application.ErrStorage
	}
	defer tx.Rollback(ctx)
	id, err := resolveCompany(ctx, tx, actor, selected, write)
	if errors.Is(err, application.ErrNotFound) {
		return "", company.ErrForbidden
	}
	if errors.Is(err, application.ErrStorage) {
		return "", company.ErrStorage
	}
	if err != nil {
		return "", err
	}
	if tx.Commit(ctx) != nil {
		return "", application.ErrStorage
	}
	return id, nil
}

// Draft overrides are durable declarations, never updates of trusted company
// balances or credit history. Each revision preserves the previous observation.
func recordDraftFinancials(ctx context.Context, tx pgx.Tx, scope application.Scope, id string, fields application.Fields) error {
	if fields.MonthlyRevenueRub == nil || fields.MonthlyExpensesRub == nil {
		return nil
	}
	evidence, err := companyProvenance(ctx, tx, scope.CompanyID)
	if err != nil {
		return application.ErrStorage
	}
	if evidence != nil && evidence.Status == "verified" && evidence.MonthlyRevenueRub == *fields.MonthlyRevenueRub && evidence.MonthlyExpensesRub == *fields.MonthlyExpensesRub {
		return nil
	}
	var declarationID string
	err = tx.QueryRow(ctx, `INSERT INTO company_financial_declarations(company_id,application_id,monthly_revenue,monthly_expenses,source,status,declared_by,reason) SELECT $1,$2,$3,$4,'client_declared','pending_review',$5,'Значения из кредитного черновика' WHERE NOT EXISTS(SELECT 1 FROM company_financial_declarations WHERE application_id=$2 AND monthly_revenue=$3 AND monthly_expenses=$4 AND status='pending_review') RETURNING id::text`, scope.CompanyID, id, *fields.MonthlyRevenueRub, *fields.MonthlyExpensesRub, scope.UserID).Scan(&declarationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return application.ErrStorage
	}
	c := company.Command{ActorID: scope.UserID, CompanyID: scope.CompanyID, Action: "draft_financial_declaration", Reason: "Значения из кредитного черновика"}
	data, _ := json.Marshal(map[string]any{"monthlyRevenueRub": *fields.MonthlyRevenueRub, "monthlyExpensesRub": *fields.MonthlyExpensesRub, "applicationId": id, "status": "pending_review", "source": "client_declared"})
	return auditData(ctx, tx, c, "financial_declaration", declarationID, nil, data)
}
