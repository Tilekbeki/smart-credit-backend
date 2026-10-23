BEGIN;

-- A completed precheck cannot be represented by the preceding schema. Refuse
-- to discard its durable result or silently turn it into a new pending task.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM jobs WHERE status='awaiting_scoring' OR precheck_results IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot downgrade while durable preliminary results exist';
    END IF;
    IF EXISTS (SELECT 1 FROM applications WHERE char_length(contact_name)>120) THEN
        RAISE EXCEPTION 'cannot downgrade application contact names longer than 120 characters';
    END IF;
END;
$$;

DROP TRIGGER applications_immutable_snapshots ON applications;
DROP FUNCTION protect_submitted_application_snapshots();
DROP INDEX application_events_pipeline_once_idx;
ALTER TABLE jobs
    DROP CONSTRAINT jobs_awaiting_results,
    DROP CONSTRAINT jobs_finished_state,
    DROP CONSTRAINT jobs_status_check,
    DROP COLUMN precheck_results,
    ADD CONSTRAINT jobs_status_check CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    ADD CONSTRAINT jobs_check2 CHECK (
        (status IN ('completed', 'failed') AND finished_at IS NOT NULL)
        OR (status IN ('pending', 'running') AND finished_at IS NULL)
    );
ALTER TABLE applications ALTER COLUMN contact_name TYPE varchar(120);
ALTER TABLE companies DROP COLUMN registration_status, DROP COLUMN financial_data_source;

COMMIT;
