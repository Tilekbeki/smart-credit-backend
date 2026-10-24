BEGIN;

ALTER TABLE users
    ADD COLUMN additional_email varchar(254),
    ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    ADD CONSTRAINT users_additional_email_nonempty CHECK (additional_email IS NULL OR btrim(additional_email) <> '');
ALTER TABLE companies ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0);

-- Contact changes are separate from the client-visible credit timeline.
CREATE TABLE contact_changes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id uuid NOT NULL REFERENCES users(id),
    user_id uuid REFERENCES users(id),
    company_id uuid REFERENCES companies(id),
    version bigint NOT NULL CHECK (version > 1),
    before_contact jsonb NOT NULL CHECK (jsonb_typeof(before_contact) = 'object'),
    after_contact jsonb NOT NULL CHECK (jsonb_typeof(after_contact) = 'object'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (num_nonnulls(user_id, company_id) = 1)
);
CREATE UNIQUE INDEX contact_changes_user_version_idx ON contact_changes(user_id, version) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX contact_changes_company_version_idx ON contact_changes(company_id, version) WHERE company_id IS NOT NULL;

ALTER TABLE credit_decisions ADD COLUMN customer_explanation text
    CHECK (customer_explanation IS NULL OR char_length(btrim(customer_explanation)) BETWEEN 1 AND 1000);

ALTER TABLE applications ADD COLUMN deleted_at timestamptz,
    ADD CONSTRAINT applications_deleted_draft CHECK (deleted_at IS NULL OR status = 'draft');
CREATE INDEX applications_active_drafts_owner_idx ON applications(owner_id)
    WHERE status = 'draft' AND deleted_at IS NULL;

COMMIT;
