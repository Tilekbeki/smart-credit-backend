BEGIN;

ALTER TABLE companies DROP CONSTRAINT companies_bank_feed_requires_alfa_account;
ALTER TABLE companies DROP COLUMN bank_relationship;

COMMIT;
