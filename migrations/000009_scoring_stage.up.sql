BEGIN;

ALTER TABLE jobs
    ALTER COLUMN status TYPE varchar(32),
    ADD COLUMN processing_stage varchar(16) NOT NULL DEFAULT 'prechecks'
        CHECK (processing_stage IN ('prechecks', 'scoring')),
    ADD COLUMN scoring_attempt_count integer NOT NULL DEFAULT 0
        CONSTRAINT jobs_scoring_attempt_budget CHECK (scoring_attempt_count >= 0 AND scoring_attempt_count <= max_attempts),
    ADD COLUMN scoring_received_at timestamptz,
    DROP CONSTRAINT jobs_status_check,
    DROP CONSTRAINT jobs_finished_state,
    ADD CONSTRAINT jobs_status_check CHECK (
        status IN ('pending', 'running', 'completed', 'failed', 'awaiting_scoring', 'awaiting_decision')
    ),
    ADD CONSTRAINT jobs_finished_state CHECK (
        (status IN ('completed', 'failed') AND finished_at IS NOT NULL)
        OR (status IN ('pending', 'running', 'awaiting_scoring', 'awaiting_decision') AND finished_at IS NULL)
    ),
    ADD CONSTRAINT jobs_decision_input CHECK (
        status <> 'awaiting_decision' OR (
            processing_stage='scoring' AND scoring_request IS NOT NULL
            AND scoring_response IS NOT NULL AND scoring_received_at IS NOT NULL
        )
    ),
    ADD CONSTRAINT jobs_scoring_receipt CHECK (
        (scoring_response IS NULL AND scoring_received_at IS NULL)
        OR (scoring_response IS NOT NULL AND scoring_received_at IS NOT NULL)
    );

UPDATE jobs SET processing_stage='scoring' WHERE status='awaiting_scoring';

CREATE INDEX jobs_awaiting_scoring_idx ON jobs(available_at, id)
    WHERE status='awaiting_scoring';
CREATE UNIQUE INDEX application_events_scoring_once_idx
    ON application_events(application_id, event_type)
    WHERE event_type IN ('SCORING_STARTED', 'SCORING_COMPLETED');

COMMIT;
