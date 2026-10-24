package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
)

// CompanyCreditRepository reads the existing company and product records. Product
// codes are the public IDs; UUID primary keys remain available for internal FKs.
type CompanyCreditRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

var (
	_ company.Repository = (*CompanyCreditRepository)(nil)
	_ credit.Repository  = (*CompanyCreditRepository)(nil)
)

func NewCompanyCreditRepository(pool *pgxpool.Pool, timeout time.Duration) *CompanyCreditRepository {
	return &CompanyCreditRepository{pool: pool, timeout: timeout}
}

func (r *CompanyCreditRepository) CompanyByID(ctx context.Context, id string) (company.Company, error) {
	if !validUUID(id) {
		return company.Company{}, company.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var result company.Company
	err := r.pool.QueryRow(ctx, `SELECT id::text, name, bank_relationship, organization_type, inn,
		registration_number, to_char(registration_date, 'YYYY-MM-DD'), okved,
		director_name, contact_phone, contact_email, contact_address,
		monthly_revenue, monthly_expenses, existing_debt, monthly_debt_payments, overdue_count
		FROM companies WHERE id = $1`, id).Scan(
		&result.ID, &result.Name, &result.BankRelationship, &result.OrganizationType, &result.INN,
		&result.RegistrationNumber, &result.RegistrationDate, &result.OKVED,
		&result.DirectorName, &result.Contact.Phone, &result.Contact.Email, &result.Contact.Address,
		&result.Financials.MonthlyRevenueRub, &result.Financials.MonthlyExpensesRub,
		&result.Financials.ExistingDebtRub, &result.Financials.ExistingMonthlyPaymentsRub,
		&result.Financials.OverdueCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return company.Company{}, company.ErrNotFound
	}
	if err != nil {
		return company.Company{}, company.ErrStorage
	}
	result.Simulation = true
	return result, nil
}

const productColumns = `code, name, purposes, min_amount, max_amount,
	min_term_months, max_term_months, annual_rate_percent::text,
	min_business_age_months, version`

func scanProduct(row pgx.Row) (credit.Product, error) {
	var result credit.Product
	var rate string
	err := row.Scan(&result.ID, &result.Name, &result.Purposes,
		&result.MinAmountRub, &result.MaxAmountRub, &result.MinTermMonths, &result.MaxTermMonths,
		&rate, &result.MinBusinessAgeMonths, &result.Version)
	if err != nil {
		return credit.Product{}, err
	}
	// Keep PostgreSQL NUMERIC exact through JSON serialization and calculations.
	result.AnnualRatePercent = json.Number(rate)
	result.Illustrative = true
	return result, nil
}

func (r *CompanyCreditRepository) ListProducts(ctx context.Context) ([]credit.Product, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT `+productColumns+` FROM credit_products
		WHERE active ORDER BY CASE code WHEN 'turnover' THEN 1 WHEN 'investment' THEN 2 ELSE 3 END, code`)
	if err != nil {
		return nil, credit.ErrStorage
	}
	defer rows.Close()
	products := make([]credit.Product, 0, 2)
	for rows.Next() {
		product, err := scanProduct(rows)
		if err != nil {
			return nil, credit.ErrStorage
		}
		products = append(products, product)
	}
	if rows.Err() != nil {
		return nil, credit.ErrStorage
	}
	return products, nil
}

func (r *CompanyCreditRepository) ProductByID(ctx context.Context, code string) (credit.Product, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	product, err := scanProduct(r.pool.QueryRow(ctx, `SELECT `+productColumns+`
		FROM credit_products WHERE code = $1 AND active`, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return credit.Product{}, credit.ErrProductNotFound
	}
	if err != nil {
		return credit.Product{}, credit.ErrStorage
	}
	return product, nil
}
