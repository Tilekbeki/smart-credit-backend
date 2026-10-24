BEGIN;

-- A downgrade must not silently restore deleted drafts or discard contact audit.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM applications WHERE deleted_at IS NOT NULL)
       OR EXISTS (SELECT 1 FROM contact_changes)
       OR EXISTS (SELECT 1 FROM users WHERE additional_email IS NOT NULL)
       OR EXISTS (SELECT 1 FROM credit_decisions WHERE customer_explanation IS NOT NULL) THEN
        RAISE EXCEPTION 'Migration 000012 downgrade would lose contact or lending data';
    END IF;
END $$;

DROP INDEX applications_active_drafts_owner_idx;
ALTER TABLE applications DROP CONSTRAINT applications_deleted_draft, DROP COLUMN deleted_at;
ALTER TABLE credit_decisions DROP COLUMN customer_explanation;
DROP TABLE contact_changes;
ALTER TABLE companies DROP COLUMN version;
ALTER TABLE users DROP CONSTRAINT users_additional_email_nonempty, DROP COLUMN additional_email, DROP COLUMN version;

COMMIT;
