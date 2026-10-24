BEGIN;

-- The relationship is separate from the source of financial data. Both values
-- describe synthetic demo companies; neither is supplied by an application.
ALTER TABLE companies
    ADD COLUMN bank_relationship varchar(20) NOT NULL DEFAULT 'external_account'
        CHECK (bank_relationship IN ('alfa_account', 'external_account'));

UPDATE companies SET bank_relationship = 'alfa_account'
    WHERE id = '10000000-0000-4000-8000-000000000001';

-- A demo bank feed must never be attributed to a company without an Alfa account.
UPDATE companies SET financial_data_source = 'unverified'
    WHERE bank_relationship = 'external_account'
      AND financial_data_source = 'demo_bank_feed';
ALTER TABLE companies
    ADD CONSTRAINT companies_bank_feed_requires_alfa_account
        CHECK (financial_data_source <> 'demo_bank_feed' OR bank_relationship = 'alfa_account');

COMMIT;
