BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM jobs WHERE processing_stage='decision' OR decision_attempt_count<>0 OR assessment_result IS NOT NULL)
       OR EXISTS (SELECT 1 FROM application_events WHERE event_type IN ('DECISION_PROCESSING_STARTED',
            'APPLICATION_APPROVED','APPLICATION_REJECTED','COUNTER_OFFER_CREATED','MANUAL_REVIEW_REQUIRED')) THEN
        RAISE EXCEPTION 'cannot downgrade while durable decision state exists';
    END IF;
END;
$$;

DROP INDEX application_events_decision_once_idx;
DROP INDEX credit_decisions_auto_job_once_idx;
DROP INDEX jobs_awaiting_decision_idx;
ALTER TABLE jobs
    DROP CONSTRAINT jobs_processing_stage_check,
    DROP CONSTRAINT jobs_decision_input,
    DROP COLUMN decision_attempt_count,
    DROP COLUMN assessment_result,
    ADD CONSTRAINT jobs_processing_stage_check CHECK (processing_stage IN ('prechecks','scoring')),
    ADD CONSTRAINT jobs_decision_input CHECK (
        status <> 'awaiting_decision' OR (
            processing_stage='scoring' AND scoring_request IS NOT NULL
            AND scoring_response IS NOT NULL AND scoring_received_at IS NOT NULL
        )
    );

COMMIT;
