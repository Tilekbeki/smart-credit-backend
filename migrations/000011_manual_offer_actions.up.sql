BEGIN;

ALTER TABLE credit_decisions ADD COLUMN internal_comment text;
ALTER TABLE credit_decisions ADD CONSTRAINT credit_decisions_manual_comment CHECK (
    (source='AUTO' AND internal_comment IS NULL) OR
    (source='MANUAL' AND internal_comment IS NOT NULL AND
        char_length(btrim(internal_comment)) BETWEEN 1 AND 1000)
) NOT VALID;

ALTER TABLE application_events
    ADD COLUMN offer_id uuid,
    ADD COLUMN action varchar(16),
    ADD COLUMN action_version bigint CHECK (action_version > 0),
    ADD CONSTRAINT application_events_offer_fk FOREIGN KEY (offer_id,application_id)
        REFERENCES offers(id,application_id),
    ADD CONSTRAINT application_events_action_audit CHECK (COALESCE((
        (event_type NOT IN ('MANUAL_DECISION_APPROVED','MANUAL_DECISION_REJECTED',
            'MANUAL_COUNTER_OFFER_CREATED','OFFER_ACCEPTED','OFFER_DECLINED')
            AND offer_id IS NULL AND action IS NULL AND action_version IS NULL)
        OR (event_type IN ('MANUAL_DECISION_APPROVED','MANUAL_COUNTER_OFFER_CREATED')
            AND action='manual' AND offer_id IS NOT NULL AND actor_id IS NOT NULL AND action_version IS NOT NULL)
        OR (event_type='MANUAL_DECISION_REJECTED' AND action='manual' AND offer_id IS NULL
            AND actor_id IS NOT NULL AND action_version IS NOT NULL)
        OR (event_type='OFFER_ACCEPTED' AND action='accept' AND offer_id IS NOT NULL
            AND actor_id IS NOT NULL AND action_version IS NOT NULL)
        OR (event_type='OFFER_DECLINED' AND action='decline' AND offer_id IS NOT NULL
            AND actor_id IS NOT NULL AND action_version IS NOT NULL)
    ),false));

CREATE UNIQUE INDEX application_events_manual_decision_once_idx ON application_events(application_id)
    WHERE action='manual';
CREATE UNIQUE INDEX application_events_offer_action_once_idx ON application_events(application_id)
    WHERE action IN ('accept','decline');

COMMIT;
