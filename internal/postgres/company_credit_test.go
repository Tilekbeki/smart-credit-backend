package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
)

func TestCompanyCreditRepository(t *testing.T) {
	pool := authTestPool(t)
	repo := NewCompanyCreditRepository(pool, 5*time.Second)
	ctx := context.Background()
	const companyID = "10000000-0000-4000-8000-000000000001"

	t.Run("all synthetic company details and existing finances load from PostgreSQL", func(t *testing.T) {
		cases := []struct {
			id, organization, registration, director string
			payments                                 int64
		}{
			{companyID, "ИП", "2020-03-15", "Иван Лавкин", 90000},
			{"10000000-0000-4000-8000-000000000002", "ООО", "2019-06-10", "Анна Северова", 180000},
			{"10000000-0000-4000-8000-000000000003", "ООО", "2022-09-01", "Павел Вкусов", 130000},
		}
		for _, tc := range cases {
			got, err := repo.CompanyByID(ctx, tc.id)
			if err != nil {
				t.Fatalf("company %s: %v", tc.id, err)
			}
			if got.ID != tc.id || got.Name == "" || got.OrganizationType != tc.organization ||
				got.RegistrationDate != tc.registration || got.DirectorName != tc.director ||
				got.OKVED == "" || got.Contact.Phone == "" || got.Contact.Address == "" ||
				!strings.HasSuffix(got.Contact.Email, "@example.test") || !got.Simulation {
				t.Fatalf("company profile fields differ from seed: %+v", got)
			}
			innLength, registrationLength := 10, 13
			if tc.organization == "ИП" {
				innLength, registrationLength = 12, 15
			}
			if len(got.INN) != innLength || len(got.RegistrationNumber) != registrationLength {
				t.Fatal("synthetic company identifiers have invalid lengths")
			}
			if got.Financials.ExistingMonthlyPaymentsRub != tc.payments ||
				got.Financials.MonthlyRevenueRub <= 0 || got.Financials.MonthlyExpensesRub <= 0 {
				t.Fatalf("existing financial columns were not reused: %+v", got.Financials)
			}
		}
	})

	t.Run("existing debt payments have one source of truth", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE companies SET monthly_debt_payments=123456 WHERE id=$1`, companyID); err != nil {
			t.Fatal("cannot update financial fixture")
		}
		defer pool.Exec(ctx, `UPDATE companies SET monthly_debt_payments=90000 WHERE id=$1`, companyID)
		got, err := repo.CompanyByID(ctx, companyID)
		if err != nil || got.Financials.ExistingMonthlyPaymentsRub != 123456 {
			t.Fatalf("live existing payment value was not loaded: %+v, %v", got.Financials, err)
		}
	})

	t.Run("catalog uses public codes and exact database terms", func(t *testing.T) {
		products, err := repo.ListProducts(ctx)
		if err != nil || len(products) != 2 {
			t.Fatalf("product catalog: length=%d err=%v", len(products), err)
		}
		want := []credit.Product{
			{ID: "turnover", Name: "Оборотный кредит", Purposes: []string{"stock", "expenses", "seasonal"},
				MinAmountRub: 100000, MaxAmountRub: 3000000, MinTermMonths: 6, MaxTermMonths: 24,
				AnnualRatePercent: json.Number("22.0000"), MinBusinessAgeMonths: 6, Version: 1, Illustrative: true},
			{ID: "investment", Name: "Инвестиционный кредит", Purposes: []string{"equipment", "upgrade"},
				MinAmountRub: 300000, MaxAmountRub: 10000000, MinTermMonths: 12, MaxTermMonths: 60,
				AnnualRatePercent: json.Number("20.0000"), MinBusinessAgeMonths: 12, Version: 1, Illustrative: true},
		}
		if !reflect.DeepEqual(products, want) {
			t.Fatalf("product terms: got %+v, want %+v", products, want)
		}
		for _, p := range products {
			got, err := repo.ProductByID(ctx, p.ID)
			if err != nil || !reflect.DeepEqual(got, p) {
				t.Fatalf("product %s detail differs from catalog: %+v, %v", p.ID, got, err)
			}
			encoded, err := json.Marshal(got)
			if err != nil || !strings.Contains(string(encoded), `"annualRatePercent":`+p.AnnualRatePercent.String()) {
				t.Fatal("numeric annual rate was converted to a string or rounded")
			}
		}
	})

	t.Run("active terms and exact fractional numeric rates come from the database", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE credit_products SET annual_rate_percent=21.1250 WHERE code='turnover'`); err != nil {
			t.Fatal("cannot set fractional rate fixture")
		}
		defer pool.Exec(ctx, `UPDATE credit_products SET annual_rate_percent=22.0000 WHERE code='turnover'`)
		got, err := repo.ProductByID(ctx, "turnover")
		if err != nil || got.AnnualRatePercent.String() != "21.1250" {
			t.Fatalf("rate did not retain NUMERIC precision: %+v, %v", got, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE credit_products SET active=false WHERE code='investment'`); err != nil {
			t.Fatal("cannot deactivate product fixture")
		}
		defer pool.Exec(ctx, `UPDATE credit_products SET active=true WHERE code='investment'`)
		products, err := repo.ListProducts(ctx)
		if err != nil || len(products) != 1 || products[0].ID != "turnover" {
			t.Fatalf("inactive product appeared in catalog: %+v, %v", products, err)
		}
		if _, err := repo.ProductByID(ctx, "investment"); !errors.Is(err, credit.ErrProductNotFound) {
			t.Fatalf("inactive product lookup: %v", err)
		}
	})

	t.Run("empty catalog is an empty array", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE credit_products SET active=false`); err != nil {
			t.Fatal("cannot deactivate product fixtures")
		}
		defer pool.Exec(ctx, `UPDATE credit_products SET active=true`)
		products, err := repo.ListProducts(ctx)
		if err != nil || products == nil || len(products) != 0 {
			t.Fatalf("empty catalog: %+v, %v", products, err)
		}
	})

	t.Run("unknown and injection shaped IDs return not found", func(t *testing.T) {
		for _, id := range []string{"not-a-uuid", "' OR true--", "00000000-0000-4000-8000-000000000000"} {
			if _, err := repo.CompanyByID(ctx, id); !errors.Is(err, company.ErrNotFound) {
				t.Fatalf("unknown company: %v", err)
			}
		}
		for _, id := range []string{"", "missing", "' OR true--", "30000000-0000-4000-8000-000000000001"} {
			if _, err := repo.ProductByID(ctx, id); !errors.Is(err, credit.ErrProductNotFound) {
				t.Fatalf("unknown product code: %v", err)
			}
		}
	})

	t.Run("canceled queries return sanitized domain errors", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := repo.CompanyByID(canceled, companyID); err != company.ErrStorage {
			t.Fatalf("canceled company query: %v", err)
		}
		if _, err := repo.ListProducts(canceled); err != credit.ErrStorage {
			t.Fatalf("canceled catalog query: %v", err)
		}
		if _, err := repo.ProductByID(canceled, "turnover"); err != credit.ErrStorage {
			t.Fatalf("canceled product query: %v", err)
		}
	})

	t.Run("repository timeout bounds table lock waits", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal("cannot create lock fixture")
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `LOCK TABLE companies, credit_products IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal("cannot lock company and product tables")
		}
		shortRepo := NewCompanyCreditRepository(pool, 50*time.Millisecond)
		started := time.Now()
		if _, err := shortRepo.CompanyByID(ctx, companyID); err != company.ErrStorage {
			t.Fatalf("locked company query: %v", err)
		}
		if _, err := shortRepo.ListProducts(ctx); err != credit.ErrStorage {
			t.Fatalf("locked catalog query: %v", err)
		}
		if _, err := shortRepo.ProductByID(ctx, "turnover"); err != credit.ErrStorage {
			t.Fatalf("locked product query: %v", err)
		}
		if time.Since(started) > 3*time.Second {
			t.Fatal("repository queries exceeded bounded lock waits")
		}
	})

	t.Run("schema rejects malformed additional fields", func(t *testing.T) {
		queries := []string{
			`UPDATE companies SET inn='invalid' WHERE profile_key='shop'`,
			`UPDATE companies SET inn='0000000000' WHERE profile_key='shop'`,
			`UPDATE companies SET registration_number='0000000000000' WHERE profile_key='shop'`,
			`UPDATE companies SET organization_type='OTHER' WHERE profile_key='shop'`,
			`UPDATE credit_products SET purposes=ARRAY['unknown'] WHERE code='turnover'`,
			`UPDATE credit_products SET purposes=ARRAY[NULL]::text[] WHERE code='turnover'`,
			`UPDATE credit_products SET min_business_age_months=-1 WHERE code='turnover'`,
		}
		for i, query := range queries {
			if _, err := pool.Exec(ctx, query); err == nil {
				t.Fatalf("schema accepted invalid details in case %d", i)
			}
		}
	})

	t.Run("product seed is repeatable and preserves IDs and versions", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE credit_products SET version=7 WHERE code='turnover'`); err != nil {
			t.Fatal("cannot set product version fixture")
		}
		defer pool.Exec(ctx, `UPDATE credit_products SET version=1 WHERE code='turnover'`)
		before, err := repo.ListProducts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var beforeIDs, afterIDs []string
		if err := pool.QueryRow(ctx, `SELECT array_agg(id::text ORDER BY code) FROM credit_products`).Scan(&beforeIDs); err != nil {
			t.Fatal("cannot read product UUIDs")
		}
		migration, err := os.ReadFile("../../migrations/000005_credit_product_seed.up.sql")
		if err != nil {
			t.Fatal("cannot read product seed")
		}
		for range 2 {
			if _, err := pool.Exec(ctx, string(migration)); err != nil {
				t.Fatal("cannot reapply product seed")
			}
		}
		after, err := repo.ListProducts(ctx)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("seed changed product terms or versions: %+v, %v", after, err)
		}
		if err := pool.QueryRow(ctx, `SELECT array_agg(id::text ORDER BY code) FROM credit_products`).Scan(&afterIDs); err != nil {
			t.Fatal("cannot reread product UUIDs")
		}
		if !reflect.DeepEqual(beforeIDs, afterIDs) {
			t.Fatal("seed replaced internal product UUIDs")
		}
	})

	t.Run("new migrations down and up preserve original company data", func(t *testing.T) {
		before, err := repo.CompanyByID(ctx, companyID)
		if err != nil {
			t.Fatal(err)
		}
		paths := []string{
			"000005_credit_product_seed.down.sql",
			"000004_company_product_details.down.sql",
			"000004_company_product_details.up.sql",
			"000005_credit_product_seed.up.sql",
		}
		for _, path := range paths {
			migration, err := os.ReadFile("../../migrations/" + path)
			if err != nil {
				t.Fatalf("cannot read migration %s", path)
			}
			if _, err := pool.Exec(ctx, string(migration)); err != nil {
				t.Fatalf("cannot apply migration %s", path)
			}
		}
		after, err := repo.CompanyByID(ctx, companyID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("migration cycle changed company: %+v, %v", after, err)
		}
		products, err := repo.ListProducts(ctx)
		if err != nil || len(products) != 2 {
			t.Fatalf("migration cycle did not restore catalog: %+v, %v", products, err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM companies`).Scan(&count); err != nil || count != 3 {
			t.Fatal("migration cycle changed original company count")
		}
	})
}
