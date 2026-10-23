BEGIN;

-- Illustrative hackathon terms; these are not current offers from Alfa-Bank.
-- Code conflicts preserve UUIDs referenced internally by later credit scenarios.
INSERT INTO credit_products (
    id, code, name, purposes, min_amount, max_amount,
    min_term_months, max_term_months, annual_rate_percent, min_business_age_months
) VALUES
    ('30000000-0000-4000-8000-000000000001', 'turnover', 'Оборотный кредит',
     ARRAY['stock', 'expenses', 'seasonal'], 100000, 3000000, 6, 24, 22.0000, 6),
    ('30000000-0000-4000-8000-000000000002', 'investment', 'Инвестиционный кредит',
     ARRAY['equipment', 'upgrade'], 300000, 10000000, 12, 60, 20.0000, 12)
ON CONFLICT (code) DO UPDATE SET
    name = EXCLUDED.name,
    purposes = EXCLUDED.purposes,
    min_amount = EXCLUDED.min_amount,
    max_amount = EXCLUDED.max_amount,
    min_term_months = EXCLUDED.min_term_months,
    max_term_months = EXCLUDED.max_term_months,
    annual_rate_percent = EXCLUDED.annual_rate_percent,
    min_business_age_months = EXCLUDED.min_business_age_months,
    active = true
WHERE (credit_products.name, credit_products.purposes, credit_products.min_amount,
       credit_products.max_amount, credit_products.min_term_months, credit_products.max_term_months,
       credit_products.annual_rate_percent, credit_products.min_business_age_months, credit_products.active)
    IS DISTINCT FROM
      (EXCLUDED.name, EXCLUDED.purposes, EXCLUDED.min_amount, EXCLUDED.max_amount,
       EXCLUDED.min_term_months, EXCLUDED.max_term_months, EXCLUDED.annual_rate_percent,
       EXCLUDED.min_business_age_months, true);

COMMIT;
