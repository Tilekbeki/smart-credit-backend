BEGIN;

-- Refuse to discard persisted scoring inputs, results or recovery state.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM jobs WHERE processing_stage='scoring'
        OR scoring_attempt_count<>0 OR scoring_received_at IS NOT NULL
        OR scoring_request IS NOT NULL OR scoring_response IS NOT NULL)
        OR EXISTS (SELECT 1 FROM application_events WHERE event_type IN ('SCORING_STARTED','SCORING_COMPLETED')) THEN
        RAISE EXCEPTION 'cannot downgrade while durable scoring state exists';
    END IF;
END;
$$;

DROP INDEX application_events_scoring_once_idx;
DROP INDEX jobs_awaiting_scoring_idx;
ALTER TABLE jobs
    DROP CONSTRAINT jobs_decision_input,
    DROP CONSTRAINT jobs_scoring_receipt,
    DROP CONSTRAINT jobs_status_check,
    DROP CONSTRAINT jobs_finished_state,
    DROP COLUMN processing_stage,
    DROP COLUMN scoring_attempt_count,
    DROP COLUMN scoring_received_at,
    ALTER COLUMN status TYPE varchar(16),
    ADD CONSTRAINT jobs_status_check CHECK (
        status IN ('pending','running','completed','failed','awaiting_scoring')
    ),
    ADD CONSTRAINT jobs_finished_state CHECK (
        (status IN ('completed','failed') AND finished_at IS NOT NULL)
        OR (status IN ('pending','running','awaiting_scoring') AND finished_at IS NULL)
    );

COMMIT;
