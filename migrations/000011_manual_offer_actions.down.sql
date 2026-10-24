BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM credit_decisions WHERE internal_comment IS NOT NULL)
       OR EXISTS (SELECT 1 FROM application_events WHERE action IS NOT NULL OR offer_id IS NOT NULL)
       OR EXISTS (SELECT 1 FROM idempotency_keys WHERE operation LIKE 'manual:%'
            OR operation LIKE 'accept:%' OR operation LIKE 'decline:%') THEN
        RAISE EXCEPTION 'cannot downgrade while durable manual decision or offer action audit exists';
    END IF;
END;
$$;

DROP INDEX application_events_manual_decision_once_idx;
DROP INDEX application_events_offer_action_once_idx;
ALTER TABLE application_events DROP CONSTRAINT application_events_action_audit,
    DROP CONSTRAINT application_events_offer_fk,
    DROP COLUMN offer_id, DROP COLUMN action, DROP COLUMN action_version;
ALTER TABLE credit_decisions DROP CONSTRAINT credit_decisions_manual_comment, DROP COLUMN internal_comment;

COMMIT;
