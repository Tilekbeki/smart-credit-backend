package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/contact"
)

type ContactRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var _ contact.Repository = (*ContactRepository)(nil)

func NewContactRepository(pool *pgxpool.Pool, timeout time.Duration) *ContactRepository {
	return &ContactRepository{pool: pool, timeout: timeout}
}

func (r *ContactRepository) UpdateUser(ctx context.Context, actorID string, changes contact.UserChanges) (auth.User, error) {
	if !validUUID(actorID) {
		return auth.User{}, contact.ErrForbidden
	}
	if changes.Version < 1 || !changes.AdditionalEmail.Present {
		return auth.User{}, contact.ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return auth.User{}, contact.ErrStorage
	}
	defer tx.Rollback(ctx)
	var u auth.User
	err = tx.QueryRow(ctx, `SELECT id::text,full_name,phone,role,company_id::text,additional_email,version
        FROM users WHERE id=$1 AND role='CLIENT' AND company_id IS NOT NULL FOR UPDATE`, actorID).
		Scan(&u.ID, &u.Name, &u.Phone, &u.Role, &u.CompanyID, &u.AdditionalEmail, &u.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, contact.ErrForbidden
	}
	if err != nil {
		return auth.User{}, contact.ErrStorage
	}
	if u.Version != changes.Version {
		return auth.User{}, contact.ErrVersionConflict
	}
	before, _ := json.Marshal(map[string]any{"additionalEmail": u.AdditionalEmail})
	u.AdditionalEmail = changes.AdditionalEmail.Value
	after, _ := json.Marshal(map[string]any{"additionalEmail": u.AdditionalEmail})
	err = tx.QueryRow(ctx, `UPDATE users SET additional_email=$2,version=version+1,updated_at=clock_timestamp()
        WHERE id=$1 RETURNING version`, actorID, u.AdditionalEmail).Scan(&u.Version)
	if err != nil {
		return auth.User{}, contact.ErrStorage
	}
	if _, err = tx.Exec(ctx, `INSERT INTO contact_changes(actor_id,user_id,version,before_contact,after_contact)
        VALUES($1,$1,$2,$3::jsonb,$4::jsonb)`, actorID, u.Version, before, after); err != nil {
		return auth.User{}, contact.ErrStorage
	}
	if tx.Commit(ctx) != nil {
		return auth.User{}, contact.ErrStorage
	}
	return u, nil
}

func (r *ContactRepository) UpdateCompany(ctx context.Context, actorID, id string, changes contact.CompanyChanges) (company.Company, error) {
	if !validUUID(actorID) || !validUUID(id) {
		return company.Company{}, contact.ErrForbidden
	}
	if changes.Version < 1 || !(changes.Contact.Phone.Present || changes.Contact.Email.Present || changes.Contact.Address.Present) {
		return company.Company{}, contact.ErrInvalidInput
	}
	for _, f := range []application.Optional[string]{changes.Contact.Phone, changes.Contact.Email, changes.Contact.Address} {
		if f.Present && f.Value == nil {
			return company.Company{}, contact.ErrInvalidInput
		}
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return company.Company{}, contact.ErrStorage
	}
	defer tx.Rollback(ctx)
	// Recheck role and the single company binding inside the write transaction.
	var allowed string
	err = tx.QueryRow(ctx, `SELECT id::text FROM users WHERE id=$1 AND role='CLIENT' AND company_id=$2 FOR SHARE`, actorID, id).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return company.Company{}, contact.ErrForbidden
	}
	if err != nil {
		return company.Company{}, contact.ErrStorage
	}
	c, err := scanCompany(tx.QueryRow(ctx, `SELECT `+companyColumns+` FROM companies WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return company.Company{}, contact.ErrStorage
	}
	if c.Version != changes.Version {
		return company.Company{}, contact.ErrVersionConflict
	}
	before, _ := json.Marshal(c.Contact)
	f := changes.Contact
	if f.Phone.Present {
		c.Contact.Phone = *f.Phone.Value
	}
	if f.Email.Present {
		c.Contact.Email = *f.Email.Value
	}
	if f.Address.Present {
		c.Contact.Address = *f.Address.Value
	}
	after, _ := json.Marshal(c.Contact)
	err = tx.QueryRow(ctx, `UPDATE companies SET contact_phone=$2,contact_email=$3,contact_address=$4,
        version=version+1,updated_at=clock_timestamp() WHERE id=$1 RETURNING version`,
		id, c.Contact.Phone, c.Contact.Email, c.Contact.Address).Scan(&c.Version)
	if err != nil {
		return company.Company{}, contact.ErrStorage
	}
	if _, err = tx.Exec(ctx, `INSERT INTO contact_changes(actor_id,company_id,version,before_contact,after_contact)
        VALUES($1,$2,$3,$4::jsonb,$5::jsonb)`, actorID, id, c.Version, before, after); err != nil {
		return company.Company{}, contact.ErrStorage
	}
	if tx.Commit(ctx) != nil {
		return company.Company{}, contact.ErrStorage
	}
	return c, nil
}
