BEGIN;

ALTER TABLE credit_products
    DROP CONSTRAINT credit_products_purposes,
    DROP COLUMN min_business_age_months,
    DROP COLUMN purposes;

ALTER TABLE companies
    DROP CONSTRAINT companies_registration_type,
    DROP COLUMN contact_address,
    DROP COLUMN contact_email,
    DROP COLUMN contact_phone,
    DROP COLUMN director_name,
    DROP COLUMN okved,
    DROP COLUMN registration_date,
    DROP COLUMN registration_number,
    DROP COLUMN inn,
    DROP COLUMN organization_type;

COMMIT;
