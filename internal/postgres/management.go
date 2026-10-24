package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/lending"
	"time"
)

type ManagementRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

func NewManagementRepository(pool *pgxpool.Pool, timeout time.Duration) *ManagementRepository {
	return &ManagementRepository{pool: pool, timeout: timeout}
}

var _ company.ManagementRepository = (*ManagementRepository)(nil)

func (r *ManagementRepository) Memberships(ctx context.Context, actor, companyID string) ([]company.Membership, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT m.id::text,m.user_id::text,m.company_id::text,c.name,COALESCE(c.organization_type,''),m.membership_role,m.status,m.version
 FROM user_company_memberships m JOIN companies c ON c.id=m.company_id JOIN users u ON u.id=$1
 WHERE (m.user_id=$1 AND u.role='CLIENT' AND m.status='active' AND $2::uuid IS NULL)
 OR ($2::uuid IS NOT NULL AND m.company_id=$2 AND u.role='BANK_EMPLOYEE') ORDER BY c.name,m.id`, actor, nullableCompany(companyID))
	if err != nil {
		return nil, company.ErrStorage
	}
	defer rows.Close()
	items := []company.Membership{}
	for rows.Next() {
		var m company.Membership
		if rows.Scan(&m.ID, &m.UserID, &m.CompanyID, &m.CompanyName, &m.OrganizationType, &m.MembershipRole, &m.Status, &m.Version) != nil {
			return nil, company.ErrStorage
		}
		m.AvailableActions = []string{}
		if m.Status == "active" {
			m.AvailableActions = []string{"read"}
		}
		if m.Status == "active" && m.MembershipRole != "VIEWER" {
			m.AvailableActions = append(m.AvailableActions, "create_application", "submit", "manage_contacts", "request_correction", "declare_financials", "accept", "decline")
		}
		items = append(items, m)
	}
	if rows.Err() != nil {
		return nil, company.ErrStorage
	}
	return items, nil
}

const profileColumns = `id::text,display_name,full_name,phone,role,
 CASE WHEN role='CLIENT' THEN (SELECT CASE WHEN count(*)=1 THEN min(m.company_id::text) END FROM user_company_memberships m WHERE m.user_id=users.id AND m.status='active') END,display_name,additional_email,version`

func scanProfile(row pgx.Row) (company.UserProfile, error) {
	var p company.UserProfile
	err := row.Scan(&p.ID, &p.Name, &p.LegalName, &p.Phone, &p.Role, &p.CompanyID, &p.DisplayName, &p.AdditionalEmail, &p.Version)
	return p, err
}
func (r *ManagementRepository) Profile(ctx context.Context, actor string) (company.UserProfile, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	p, err := scanProfile(r.pool.QueryRow(ctx, `SELECT `+profileColumns+` FROM users WHERE id=$1`, actor))
	if err != nil {
		return p, company.ErrStorage
	}
	return p, nil
}
func (r *ManagementRepository) History(ctx context.Context, actor, companyID, kind string, bank bool) ([]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	table := "company_change_requests"
	if kind == "financial" {
		table = "company_financial_declarations"
	} else if kind != "registration" {
		return nil, company.ErrInvalidInput
	}
	var allowed bool
	if err := r.pool.QueryRow(ctx, `SELECT CASE WHEN $3::boolean THEN EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='BANK_EMPLOYEE') ELSE has_company_access($1,$2,false) END`, actor, companyID, bank).Scan(&allowed); err != nil {
		return nil, company.ErrStorage
	}
	if !allowed {
		return nil, company.ErrNotFound
	}
	// All values are intentional business audit fields, no infrastructure columns.
	privacy := ` AND ($2::uuid IS NOT NULL AND $3::boolean IN (true,false))`
	if kind == "financial" {
		privacy = ` AND (application_id IS NULL OR EXISTS(SELECT 1 FROM applications a WHERE a.id=t.application_id AND ((a.status<>'draft' AND a.submitted_at IS NOT NULL) OR (a.owner_id=$2 AND NOT $3::boolean))))`
	}
	order := "created_at"
	if kind == "financial" {
		order = "received_at"
	}
	rows, err := r.pool.Query(ctx, `SELECT to_jsonb(t)||jsonb_build_object('companyVersion',(SELECT version FROM companies WHERE id=t.company_id)) FROM `+table+` t WHERE company_id=$1`+privacy+` ORDER BY `+order+` DESC,id LIMIT 100`, companyID, actor, bank)
	if err != nil {
		return nil, company.ErrStorage
	}
	defer rows.Close()
	items := []json.RawMessage{}
	for rows.Next() {
		var b json.RawMessage
		if rows.Scan(&b) != nil {
			return nil, company.ErrStorage
		}
		items = append(items, managementRecord(b))
	}
	if rows.Err() != nil {
		return nil, company.ErrStorage
	}
	return items, nil
}
func (r *ManagementRepository) Execute(ctx context.Context, c company.Command) (company.Response, error) {
	if company.ValidateCommand(&c) != nil {
		return company.Response{}, company.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return company.Response{}, company.ErrStorage
	}
	defer tx.Rollback(ctx)
	var role auth.Role
	actorSQL := `SELECT role FROM users WHERE id=$1 FOR SHARE`
	if c.Action == "profile" {
		actorSQL = `SELECT role FROM users WHERE id=$1 FOR UPDATE`
	}
	if tx.QueryRow(ctx, actorSQL, c.ActorID).Scan(&role) != nil {
		return company.Response{}, company.ErrForbidden
	}
	bank := c.Action == "membership" || c.Action == "review_registration" || c.Action == "review_financial"
	if bank && role != auth.RoleBankEmployee {
		return company.Response{}, company.ErrForbidden
	}
	if c.Action != "profile" && !bank {
		var access bool
		if tx.QueryRow(ctx, `SELECT lock_company_access($1,$2,true)`, c.ActorID, c.CompanyID).Scan(&access) != nil {
			return company.Response{}, company.ErrStorage
		}
		if !access {
			return company.Response{}, company.ErrNotFound
		}
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('app.actor_id',$1,true),set_config('app.audit_reason',$2,true)`, c.ActorID, c.Reason); err != nil {
		return company.Response{}, company.ErrStorage
	}
	// Membership updates never lock the company: submit locks membership before
	// reading company. This avoids an inverse lock order during revocation.
	if c.Action != "profile" && c.Action != "membership" {
		var id string
		if tx.QueryRow(ctx, `SELECT id::text FROM companies WHERE id=$1 FOR NO KEY UPDATE`, c.CompanyID).Scan(&id) != nil {
			return company.Response{}, company.ErrNotFound
		}
	}
	var keyCommand lending.Command
	if c.Action != "profile" && c.Action != "contact" {
		keyCommand = lending.Command{ActorID: c.ActorID, Key: c.Key, RequestHash: c.RequestHash}
		op := "data:" + c.Action + ":" + c.CompanyID + ":" + c.TargetID
		// Serialize keys even for two different new memberships of the same actor.
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, c.ActorID+":"+op+":"+c.Key); err != nil {
			return company.Response{}, company.ErrStorage
		}
		replay, e := beginLendingKey(ctx, tx, keyCommand, op)
		if errors.Is(e, lending.ErrIdempotencyConflict) {
			return company.Response{}, company.ErrIdempotencyConflict
		}
		if e != nil {
			return company.Response{}, company.ErrStorage
		}
		if replay != nil {
			if tx.Commit(ctx) != nil {
				return company.Response{}, company.ErrStorage
			}
			return company.Response{Status: replay.Status, Body: replay.Body}, nil
		}
	}
	body, status, err := applyManagement(ctx, tx, c)
	if err != nil {
		return company.Response{}, err
	}
	body = managementRecord(body)
	if c.Action != "profile" && c.Action != "contact" {
		op := "data:" + c.Action + ":" + c.CompanyID + ":" + c.TargetID
		err = tx.QueryRow(ctx, `UPDATE idempotency_keys SET status='completed',response_status=$4,response_body=$5::jsonb,updated_at=clock_timestamp() WHERE user_id=$1 AND operation=$2 AND key=$3 RETURNING response_body`, c.ActorID, op, c.Key, status, body).Scan(&body)
		if err != nil {
			return company.Response{}, company.ErrStorage
		}
	}
	if tx.Commit(ctx) != nil {
		return company.Response{}, company.ErrStorage
	}
	return company.Response{Status: status, Body: body}, nil
}
func auditData(ctx context.Context, tx pgx.Tx, c company.Command, kind, id string, old, new any) error {
	_, err := tx.Exec(ctx, `INSERT INTO data_audit_events(actor_id,company_id,entity_type,entity_id,action,old_values,new_values,reason) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8)`, c.ActorID, nullableCompany(c.CompanyID), kind, id, c.Action, old, new, c.Reason)
	if err != nil {
		return company.ErrStorage
	}
	return nil
}
func bumpCompany(ctx context.Context, tx pgx.Tx, c company.Command) error {
	res, err := tx.Exec(ctx, `UPDATE companies SET version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND version=$2`, c.CompanyID, c.Version)
	if err != nil {
		return company.ErrStorage
	}
	if res.RowsAffected() != 1 {
		return company.ErrVersionConflict
	}
	return nil
}

func applyManagement(ctx context.Context, tx pgx.Tx, c company.Command) (json.RawMessage, int, error) {
	var body, old json.RawMessage
	fail := func(err error) (json.RawMessage, int, error) { return nil, 0, err }
	if c.Action == "profile" {
		if tx.QueryRow(ctx, `SELECT to_jsonb(u)-'company_id' FROM users u WHERE id=$1 FOR UPDATE`, c.ActorID).Scan(&old) != nil {
			return fail(company.ErrStorage)
		}
		res, err := tx.Exec(ctx, `UPDATE users SET display_name=COALESCE($3,display_name),additional_email=CASE WHEN $4::text IS NULL THEN additional_email ELSE NULLIF($4,'') END,version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND version=$2`, c.ActorID, c.Version, c.DisplayName, c.AdditionalEmail)
		if err != nil {
			return fail(company.ErrStorage)
		}
		if res.RowsAffected() != 1 {
			return fail(company.ErrVersionConflict)
		}
		p, err := scanProfile(tx.QueryRow(ctx, `SELECT `+profileColumns+` FROM users WHERE id=$1`, c.ActorID))
		if err != nil {
			return fail(company.ErrStorage)
		}
		body, _ = json.Marshal(p)
		if auditData(ctx, tx, c, "user", c.ActorID, old, body) != nil {
			return fail(company.ErrStorage)
		}
		return body, 200, nil
	}
	if c.Action == "membership" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, c.TargetID+":"+c.CompanyID); err != nil {
			return fail(company.ErrStorage)
		}
		var role string
		if tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 FOR SHARE`, c.TargetID).Scan(&role) != nil || role != "CLIENT" {
			return fail(company.ErrInvalidInput)
		}
		var version int64
		err := tx.QueryRow(ctx, `SELECT version FROM user_company_memberships WHERE user_id=$1 AND company_id=$2 FOR UPDATE`, c.TargetID, c.CompanyID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			if c.Version != 0 || c.Status != "active" {
				return fail(company.ErrVersionConflict)
			}
			err = tx.QueryRow(ctx, `INSERT INTO user_company_memberships AS m(user_id,company_id,membership_role,status,granted_by) VALUES($1,$2,$3,'active',$4) RETURNING to_jsonb(m)`, c.TargetID, c.CompanyID, c.MembershipRole, c.ActorID).Scan(&body)
		} else if err == nil {
			if version != c.Version {
				return fail(company.ErrVersionConflict)
			}
			err = tx.QueryRow(ctx, `UPDATE user_company_memberships AS m SET membership_role=$3::text,status=$4::text,revoked_at=CASE WHEN $4='revoked' THEN clock_timestamp() END,granted_by=CASE WHEN $4='active' THEN $5 ELSE granted_by END,version=version+1,updated_at=clock_timestamp() WHERE user_id=$1 AND company_id=$2 RETURNING to_jsonb(m)`, c.TargetID, c.CompanyID, c.MembershipRole, c.Status, c.ActorID).Scan(&body)
		}
		if err != nil {
			return fail(company.ErrStorage)
		}
		return body, 200, nil
	}
	if tx.QueryRow(ctx, `SELECT to_jsonb(c) FROM companies c WHERE id=$1`, c.CompanyID).Scan(&old) != nil {
		return fail(company.ErrNotFound)
	}
	if err := bumpCompany(ctx, tx, c); err != nil {
		return fail(err)
	}
	switch c.Action {
	case "contact":
		if _, err := tx.Exec(ctx, `UPDATE companies SET contact_phone=$2,contact_email=$3,contact_address=$4 WHERE id=$1`, c.CompanyID, c.Contact.Phone, c.Contact.Email, c.Contact.Address); err != nil {
			return fail(company.ErrStorage)
		}
		if tx.QueryRow(ctx, `SELECT jsonb_build_object('companyId',id,'version',version,'contact',jsonb_build_object('phone',contact_phone,'email',contact_email,'address',contact_address)) FROM companies WHERE id=$1`, c.CompanyID).Scan(&body) != nil {
			return fail(company.ErrStorage)
		}
	case "financial":
		if c.SupportingDocumentID != nil {
			var exists bool
			if tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM documents d WHERE id=$1 AND company_id=$2 AND status='ready' AND can_read_application($3,application_id))`, *c.SupportingDocumentID, c.CompanyID, c.ActorID).Scan(&exists) != nil {
				return fail(company.ErrStorage)
			}
			if !exists {
				return fail(company.ErrNotFound)
			}
		}
		if tx.QueryRow(ctx, `INSERT INTO company_financial_declarations AS d(company_id,monthly_revenue,monthly_expenses,source,status,declared_by,supporting_document_id,reason) VALUES($1,$2,$3,'client_declared','pending_review',$4,$5,$6) RETURNING to_jsonb(d)||jsonb_build_object('companyVersion',$7::bigint)`, c.CompanyID, *c.MonthlyRevenueRub, *c.MonthlyExpensesRub, c.ActorID, c.SupportingDocumentID, c.Reason, c.Version+1).Scan(&body) != nil {
			return fail(company.ErrStorage)
		}
	case "registration":
		proposed, _ := json.Marshal(c.Changes)
		if tx.QueryRow(ctx, `INSERT INTO company_change_requests AS r(company_id,initiated_by,company_version,original_values,proposed_values,reason) SELECT id,$2,$3,jsonb_build_object('name',name,'inn',inn,'registrationNumber',registration_number,'registrationDate',to_char(registration_date,'YYYY-MM-DD'),'okved',okved,'directorName',director_name),$4::jsonb,$5 FROM companies WHERE id=$1 RETURNING to_jsonb(r)||jsonb_build_object('companyVersion',$3::bigint)`, c.CompanyID, c.ActorID, c.Version+1, proposed, c.Reason).Scan(&body) != nil {
			return fail(company.ErrStorage)
		}
	case "review_registration", "cancel_registration":
		var status string
		var original, proposed json.RawMessage
		err := tx.QueryRow(ctx, `SELECT status,original_values,proposed_values FROM company_change_requests WHERE id=$1 AND company_id=$2 FOR UPDATE`, c.TargetID, c.CompanyID).Scan(&status, &original, &proposed)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail(company.ErrNotFound)
		}
		if err != nil {
			return fail(company.ErrStorage)
		}
		if status != "pending_review" {
			return fail(company.ErrInvalidStatus)
		}
		outcome := c.Outcome
		if c.Action == "cancel_registration" {
			outcome = "cancelled"
		}
		if outcome == "approved" {
			var same bool
			if tx.QueryRow(ctx, `SELECT jsonb_build_object('name',name,'inn',inn,'registrationNumber',registration_number,'registrationDate',to_char(registration_date,'YYYY-MM-DD'),'okved',okved,'directorName',director_name)=$2::jsonb FROM companies WHERE id=$1`, c.CompanyID, original).Scan(&same) != nil {
				return fail(company.ErrStorage)
			}
			if !same {
				return fail(company.ErrVersionConflict)
			}
			var ch company.RegistrationChanges
			if json.Unmarshal(proposed, &ch) != nil {
				return fail(company.ErrStorage)
			}
			// SQL names are fixed, never derived from caller JSON.
			_, err = tx.Exec(ctx, `UPDATE companies SET name=COALESCE($2,name),inn=COALESCE($3,inn),registration_number=COALESCE($4,registration_number),registration_date=COALESCE($5::date,registration_date),okved=COALESCE($6,okved),director_name=COALESCE($7,director_name),registration_data_source='bank_confirmed' WHERE id=$1`, c.CompanyID, ch.Name, ch.INN, ch.RegistrationNumber, ch.RegistrationDate, ch.OKVED, ch.DirectorName)
			if err != nil {
				return fail(company.ErrInvalidInput)
			}
		}
		if tx.QueryRow(ctx, `UPDATE company_change_requests AS r SET status=$3,version=version+1,reviewed_at=clock_timestamp(),reviewed_by=$4,review_comment=$5 WHERE id=$1 AND company_id=$2 RETURNING to_jsonb(r)||jsonb_build_object('companyVersion',$6::bigint)`, c.TargetID, c.CompanyID, outcome, c.ActorID, c.Reason, c.Version+1).Scan(&body) != nil {
			return fail(company.ErrStorage)
		}
	case "review_financial":
		var status string
		var revenue, expenses int64
		err := tx.QueryRow(ctx, `SELECT status,monthly_revenue,monthly_expenses FROM company_financial_declarations WHERE id=$1 AND company_id=$2 AND (application_id IS NULL OR EXISTS(SELECT 1 FROM applications a WHERE a.id=application_id AND a.status<>'draft' AND a.submitted_at IS NOT NULL)) FOR UPDATE`, c.TargetID, c.CompanyID).Scan(&status, &revenue, &expenses)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail(company.ErrNotFound)
		}
		if err != nil {
			return fail(company.ErrStorage)
		}
		if status != "pending_review" {
			return fail(company.ErrInvalidStatus)
		}
		target := "rejected"
		if c.Outcome == "approved" {
			target = "verified"
		}
		if tx.QueryRow(ctx, `UPDATE company_financial_declarations AS d SET status=$3::text,version=version+1,reviewed_at=clock_timestamp(),reviewed_by=$4,review_comment=$5,verified_at=CASE WHEN $3='verified' THEN clock_timestamp() END WHERE id=$1 AND company_id=$2 RETURNING to_jsonb(d)||jsonb_build_object('companyVersion',$6::bigint)`, c.TargetID, c.CompanyID, target, c.ActorID, c.Reason, c.Version+1).Scan(&body) != nil {
			return fail(company.ErrStorage)
		}
		if target == "verified" {
			if _, err := tx.Exec(ctx, `UPDATE companies SET monthly_revenue=$2,monthly_expenses=$3 WHERE id=$1`, c.CompanyID, revenue, expenses); err != nil {
				return fail(company.ErrStorage)
			}
		}
	default:
		return fail(company.ErrInvalidInput)
	}
	if auditData(ctx, tx, c, "company", c.CompanyID, old, body) != nil {
		return fail(company.ErrStorage)
	}
	status := 200
	if c.Action == "registration" || c.Action == "financial" {
		status = 201
	}
	return body, status, nil
}

// Fixed wire names keep database naming out of the HTTP contract.
func managementRecord(raw json.RawMessage) json.RawMessage {
	var record map[string]json.RawMessage
	if json.Unmarshal(raw, &record) != nil {
		return raw
	}
	names := map[string]string{"company_id": "companyId", "user_id": "userId", "membership_role": "membershipRole", "created_at": "createdAt", "updated_at": "updatedAt", "revoked_at": "revokedAt", "granted_by": "grantedBy", "monthly_revenue": "monthlyRevenueRub", "monthly_expenses": "monthlyExpensesRub", "declared_by": "declaredBy", "initiated_by": "initiatedBy", "supporting_document_id": "supportingDocumentId", "received_at": "receivedAt", "verified_at": "verifiedAt", "reviewed_at": "reviewedAt", "reviewed_by": "reviewedBy", "review_comment": "reviewComment", "company_version": "companyVersionAtRequest", "original_values": "originalValues", "proposed_values": "proposedValues", "application_id": "applicationId"}
	for old, new := range names {
		if value, ok := record[old]; ok {
			delete(record, old)
			record[new] = value
		}
	}
	data, err := json.Marshal(record)
	if err != nil {
		return raw
	}
	return data
}
