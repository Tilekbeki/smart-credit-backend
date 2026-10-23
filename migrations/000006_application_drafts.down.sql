BEGIN;

-- Refuse a downgrade if the previous vocabulary cannot represent the data.
-- In particular, legacy failed timeline entries now share the processing status;
-- their exact former positions cannot be reconstructed without operator review.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM applications WHERE status='closed')
        OR EXISTS (SELECT 1 FROM application_events WHERE status='closed')
        OR EXISTS (SELECT 1 FROM applications WHERE processing_error IS NOT NULL AND (
            status <> 'processing' OR processing_error <> 'Legacy processing failure before migration 000006'
        ))
        OR EXISTS (SELECT 1 FROM application_events WHERE status='processing')
    THEN
        RAISE EXCEPTION 'Cannot downgrade application statuses without losing state; review closed applications and processing errors first';
    END IF;
END $$;

DROP INDEX applications_owner_company_status_created_idx;
DROP INDEX applications_owner_company_created_idx;
ALTER TABLE applications DROP CONSTRAINT applications_status_check,
    DROP CONSTRAINT applications_signing;
ALTER TABLE application_events DROP CONSTRAINT application_events_status_check;

UPDATE applications SET status='confirmed' WHERE status='signed';
UPDATE applications SET status='processing_failed'
    WHERE status='processing' AND processing_error='Legacy processing failure before migration 000006';
UPDATE application_events SET status='confirmed' WHERE status='signed';

ALTER TABLE applications RENAME COLUMN signed_by TO confirmed_by;
ALTER TABLE applications RENAME COLUMN signed_at TO confirmed_at;
ALTER TABLE applications
    DROP COLUMN monthly_revenue,
    DROP COLUMN monthly_expenses,
    DROP COLUMN processing_error,
    ADD CONSTRAINT applications_status_check CHECK (status IN (
        'draft', 'submitted', 'processing', 'manual_review', 'approved',
        'counter_offer', 'rejected', 'processing_failed', 'confirmed'
    )),
    ADD CONSTRAINT applications_confirmation CHECK (
        (status = 'confirmed' AND accepted_offer_id IS NOT NULL AND confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)
        OR (status <> 'confirmed' AND accepted_offer_id IS NULL AND confirmed_by IS NULL AND confirmed_at IS NULL)
    );
ALTER TABLE application_events ADD CONSTRAINT application_events_status_check CHECK (status IN (
    'draft', 'submitted', 'processing', 'manual_review', 'approved',
    'counter_offer', 'rejected', 'processing_failed', 'confirmed'
));

COMMIT;
