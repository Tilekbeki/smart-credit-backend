BEGIN;

-- Trust is supplied by the demonstration profile, never by application JSON.
ALTER TABLE companies
    ADD COLUMN financial_data_source varchar(32) NOT NULL DEFAULT 'unverified'
        CHECK (financial_data_source IN ('unverified', 'demo_bank_feed')),
    ADD COLUMN registration_status varchar(16) NOT NULL DEFAULT 'active'
        CHECK (registration_status IN ('active', 'suspended', 'closed', 'unknown'));
UPDATE companies SET financial_data_source='demo_bank_feed'
    WHERE id='10000000-0000-4000-8000-000000000001';

ALTER TABLE applications ALTER COLUMN contact_name TYPE varchar(160);
ALTER TABLE jobs
    ADD COLUMN precheck_results jsonb CHECK (jsonb_typeof(precheck_results) = 'object'),
    DROP CONSTRAINT jobs_status_check,
    DROP CONSTRAINT jobs_check2,
    ADD CONSTRAINT jobs_status_check CHECK (
        status IN ('pending', 'running', 'completed', 'failed', 'awaiting_scoring')
    ),
    ADD CONSTRAINT jobs_finished_state CHECK (
        (status IN ('completed', 'failed') AND finished_at IS NOT NULL)
        OR (status IN ('pending', 'running', 'awaiting_scoring') AND finished_at IS NULL)
    ),
    ADD CONSTRAINT jobs_awaiting_results CHECK (
        status <> 'awaiting_scoring' OR precheck_results IS NOT NULL
    );

-- The application row lock allocates sequence numbers; this index is a second
-- guard against repeating durable milestones after a lease expiry or restart.
CREATE UNIQUE INDEX application_events_pipeline_once_idx
    ON application_events(application_id, event_type)
    WHERE event_type IN (
        'APPLICATION_SUBMITTED', 'PROCESSING_STARTED', 'COMPANY_CHECK_COMPLETED',
        'CREDIT_HISTORY_CHECK_COMPLETED', 'PRECHECKS_COMPLETED'
    );

CREATE FUNCTION protect_submitted_application_snapshots() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.submitted_at IS NOT NULL AND (
        NEW.submitted_at IS DISTINCT FROM OLD.submitted_at
        OR NEW.financial_snapshot IS DISTINCT FROM OLD.financial_snapshot
        OR NEW.product_snapshot IS DISTINCT FROM OLD.product_snapshot
    ) THEN
        RAISE EXCEPTION 'submitted application snapshots are immutable'
            USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER applications_immutable_snapshots
    BEFORE UPDATE ON applications FOR EACH ROW
    EXECUTE FUNCTION protect_submitted_application_snapshots();

COMMIT;
