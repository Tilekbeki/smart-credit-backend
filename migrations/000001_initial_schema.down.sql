BEGIN;

DROP TABLE idempotency_keys;
ALTER TABLE applications DROP CONSTRAINT applications_accepted_offer_fk;
DROP TABLE offers;
DROP TABLE credit_decisions;
DROP TABLE jobs;
DROP TABLE documents;
DROP TABLE application_events;
DROP TABLE applications;
DROP TABLE credit_products;
DROP TABLE otp_challenges;
DROP TABLE users;
DROP TABLE companies;

COMMIT;
