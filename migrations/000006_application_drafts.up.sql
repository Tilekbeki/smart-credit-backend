BEGIN;

ALTER TABLE applications
    ADD COLUMN monthly_revenue bigint CHECK (monthly_revenue BETWEEN 0 AND 1000000000000),
    ADD COLUMN monthly_expenses bigint CHECK (monthly_expenses BETWEEN 0 AND 1000000000000),
    ADD COLUMN processing_error text,
    DROP CONSTRAINT applications_status_check,
    DROP CONSTRAINT applications_confirmation;
ALTER TABLE application_events DROP CONSTRAINT application_events_status_check;

-- Preserve existing data while separating technical errors from credit statuses.
-- Event types and descriptions remain unchanged; only their status vocabulary changes.
UPDATE applications SET processing_error='Legacy processing failure before migration 000006',
    status='processing' WHERE status='processing_failed';
UPDATE applications SET status='signed' WHERE status='confirmed';
UPDATE application_events SET status='processing' WHERE status='processing_failed';
UPDATE application_events SET status='signed' WHERE status='confirmed';

ALTER TABLE applications RENAME COLUMN confirmed_by TO signed_by;
ALTER TABLE applications RENAME COLUMN confirmed_at TO signed_at;
ALTER TABLE applications
    ADD CONSTRAINT applications_status_check CHECK (status IN (
        'draft', 'submitted', 'processing', 'manual_review', 'approved',
        'rejected', 'counter_offer', 'signed', 'closed'
    )),
    ADD CONSTRAINT applications_signing CHECK (
        (status = 'signed' AND accepted_offer_id IS NOT NULL AND signed_by IS NOT NULL AND signed_at IS NOT NULL)
        OR (status = 'closed' AND (
            (accepted_offer_id IS NULL AND signed_by IS NULL AND signed_at IS NULL)
            OR (accepted_offer_id IS NOT NULL AND signed_by IS NOT NULL AND signed_at IS NOT NULL)
        ))
        OR (status NOT IN ('signed', 'closed') AND accepted_offer_id IS NULL AND signed_by IS NULL AND signed_at IS NULL)
    );
ALTER TABLE application_events ADD CONSTRAINT application_events_status_check CHECK (status IN (
    'draft', 'submitted', 'processing', 'manual_review', 'approved',
    'rejected', 'counter_offer', 'signed', 'closed'
));

CREATE INDEX applications_owner_company_created_idx
    ON applications(owner_id, company_id, created_at DESC, id DESC);
CREATE INDEX applications_owner_company_status_created_idx
    ON applications(owner_id, company_id, status, created_at DESC, id DESC);

COMMIT;
