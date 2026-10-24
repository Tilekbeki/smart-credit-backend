BEGIN;

ALTER TABLE jobs
    DROP CONSTRAINT jobs_processing_stage_check,
    DROP CONSTRAINT jobs_decision_input,
    ADD COLUMN decision_attempt_count integer NOT NULL DEFAULT 0
        CONSTRAINT jobs_decision_attempt_budget CHECK (decision_attempt_count BETWEEN 0 AND max_attempts),
    ADD COLUMN assessment_result jsonb CHECK (jsonb_typeof(assessment_result) = 'object'),
    ADD CONSTRAINT jobs_processing_stage_check CHECK (processing_stage IN ('prechecks', 'scoring', 'decision')),
    ADD CONSTRAINT jobs_decision_input CHECK (
        (status <> 'awaiting_decision' AND processing_stage <> 'decision') OR (
            processing_stage IN ('scoring', 'decision') AND precheck_results IS NOT NULL
            AND scoring_request IS NOT NULL AND scoring_response IS NOT NULL AND scoring_received_at IS NOT NULL
        )
    );

CREATE INDEX jobs_awaiting_decision_idx ON jobs(available_at,id) WHERE status='awaiting_decision';
CREATE UNIQUE INDEX credit_decisions_auto_job_once_idx ON credit_decisions(scoring_job_id) WHERE source='AUTO';
CREATE UNIQUE INDEX application_events_decision_once_idx ON application_events(application_id,event_type)
    WHERE event_type IN ('DECISION_PROCESSING_STARTED','APPLICATION_APPROVED','APPLICATION_REJECTED',
        'COUNTER_OFFER_CREATED','MANUAL_REVIEW_REQUIRED');

COMMIT;
