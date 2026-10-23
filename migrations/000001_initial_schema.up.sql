BEGIN;

-- Monetary values use integer units, shared with the synthetic scoring v1 input.
-- Percentage rates are stored as percentages (20.0000 means 20%, not 0.2).
CREATE TABLE companies (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_key varchar(32) NOT NULL UNIQUE CHECK (btrim(profile_key) <> ''),
    name varchar(160) NOT NULL CHECK (btrim(name) <> ''),
    monthly_revenue bigint NOT NULL CHECK (monthly_revenue BETWEEN 0 AND 1000000000000),
    monthly_expenses bigint NOT NULL CHECK (monthly_expenses BETWEEN 0 AND 1000000000000),
    existing_debt bigint NOT NULL CHECK (existing_debt BETWEEN 0 AND 1000000000000),
    monthly_debt_payments bigint NOT NULL CHECK (monthly_debt_payments BETWEEN 0 AND 1000000000000),
    overdue_count integer NOT NULL CHECK (overdue_count BETWEEN 0 AND 100),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_key varchar(32) NOT NULL UNIQUE CHECK (btrim(profile_key) <> ''),
    company_id uuid REFERENCES companies(id),
    phone varchar(16) NOT NULL UNIQUE CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    full_name varchar(160) NOT NULL CHECK (btrim(full_name) <> ''),
    role varchar(16) NOT NULL CHECK (role IN ('CLIENT', 'BANK_EMPLOYEE')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_client_company CHECK (role <> 'CLIENT' OR company_id IS NOT NULL),
    CONSTRAINT users_id_company_unique UNIQUE (id, company_id)
);
CREATE INDEX users_company_idx ON users(company_id);

CREATE TABLE otp_challenges (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id),
    phone varchar(16) NOT NULL CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    CHECK (attempt_count <= max_attempts),
    CHECK (expires_at > created_at),
    CHECK (used_at IS NULL OR used_at >= created_at)
);
CREATE INDEX otp_challenges_user_created_idx ON otp_challenges(user_id, created_at DESC);
CREATE INDEX otp_challenges_expiry_idx ON otp_challenges(expires_at) WHERE used_at IS NULL;

CREATE TABLE credit_products (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code varchar(32) NOT NULL UNIQUE CHECK (btrim(code) <> ''),
    name varchar(160) NOT NULL CHECK (btrim(name) <> ''),
    min_amount bigint NOT NULL CHECK (min_amount > 0),
    max_amount bigint NOT NULL CHECK (max_amount <= 1000000000000),
    min_term_months integer NOT NULL CHECK (min_term_months >= 1),
    max_term_months integer NOT NULL CHECK (max_term_months <= 120),
    annual_rate_percent numeric(7,4) NOT NULL CHECK (annual_rate_percent >= 0),
    active boolean NOT NULL DEFAULT true,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (min_amount <= max_amount),
    CHECK (min_term_months <= max_term_months)
);

CREATE TABLE applications (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL,
    company_id uuid NOT NULL REFERENCES companies(id),
    product_id uuid REFERENCES credit_products(id),
    goal_id varchar(32) CHECK (btrim(goal_id) <> ''),
    goal_description varchar(300),
    amount bigint CHECK (amount BETWEEN 1 AND 1000000000000),
    term_months integer CHECK (term_months BETWEEN 1 AND 120),
    contact_name varchar(120) CHECK (btrim(contact_name) <> ''),
    contact_role varchar(120) CHECK (btrim(contact_role) <> ''),
    status varchar(24) NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft', 'submitted', 'processing', 'manual_review', 'approved',
        'counter_offer', 'rejected', 'processing_failed', 'confirmed'
    )),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    -- Copied at submit; future services must never rebuild these from live profiles.
    financial_snapshot jsonb CHECK (jsonb_typeof(financial_snapshot) = 'object'),
    product_snapshot jsonb CHECK (jsonb_typeof(product_snapshot) = 'object'),
    accepted_offer_id uuid,
    confirmed_by uuid REFERENCES users(id),
    confirmed_at timestamptz,
    submitted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT applications_owner_company_fk FOREIGN KEY (owner_id, company_id)
        REFERENCES users(id, company_id),
    CONSTRAINT applications_submitted_fields CHECK (status = 'draft' OR (
        product_id IS NOT NULL AND goal_id IS NOT NULL AND amount IS NOT NULL
        AND term_months IS NOT NULL AND contact_name IS NOT NULL AND contact_role IS NOT NULL
        AND submitted_at IS NOT NULL AND financial_snapshot IS NOT NULL AND product_snapshot IS NOT NULL
    )),
    CONSTRAINT applications_confirmation CHECK (
        (status = 'confirmed' AND accepted_offer_id IS NOT NULL AND confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)
        OR (status <> 'confirmed' AND accepted_offer_id IS NULL AND confirmed_by IS NULL AND confirmed_at IS NULL)
    )
);
CREATE INDEX applications_owner_created_idx ON applications(owner_id, created_at DESC, id);
CREATE INDEX applications_status_created_idx ON applications(status, created_at, id);
CREATE INDEX applications_company_idx ON applications(company_id);
CREATE INDEX applications_product_idx ON applications(product_id);

CREATE TABLE application_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES applications(id),
    sequence bigint NOT NULL CHECK (sequence > 0),
    event_type varchar(64) NOT NULL CHECK (btrim(event_type) <> ''),
    status varchar(24) NOT NULL CHECK (status IN (
        'draft', 'submitted', 'processing', 'manual_review', 'approved',
        'counter_offer', 'rejected', 'processing_failed', 'confirmed'
    )),
    title varchar(300) NOT NULL CHECK (btrim(title) <> ''),
    actor_id uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (application_id, sequence)
);

CREATE TABLE documents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES applications(id),
    uploaded_by uuid NOT NULL REFERENCES users(id),
    original_name varchar(255) NOT NULL CHECK (btrim(original_name) <> ''),
    content_type varchar(128) NOT NULL CHECK (btrim(content_type) <> ''),
    size_bytes bigint NOT NULL CHECK (size_bytes > 0),
    checksum_sha256 char(64) NOT NULL CHECK (checksum_sha256 ~ '^[0-9a-f]{64}$'),
    storage_key varchar(255) NOT NULL UNIQUE CHECK (btrim(storage_key) <> ''),
    status varchar(16) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'failed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX documents_application_created_idx ON documents(application_id, created_at, id);

CREATE TABLE jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES applications(id),
    type varchar(64) NOT NULL CHECK (btrim(type) <> ''),
    status varchar(16) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    lease_token uuid,
    scoring_request jsonb CHECK (jsonb_typeof(scoring_request) = 'object'),
    scoring_response jsonb CHECK (jsonb_typeof(scoring_response) = 'object'),
    scoring_version varchar(128),
    last_error text,
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (application_id, type),
    UNIQUE (id, application_id),
    CHECK (attempt_count <= max_attempts),
    CHECK ((status = 'running' AND lease_until IS NOT NULL AND lease_token IS NOT NULL)
        OR (status <> 'running' AND lease_until IS NULL AND lease_token IS NULL)),
    CHECK ((status IN ('completed', 'failed') AND finished_at IS NOT NULL)
        OR (status IN ('pending', 'running') AND finished_at IS NULL))
);
CREATE INDEX jobs_pending_idx ON jobs(available_at, id) WHERE status = 'pending';
CREATE INDEX jobs_lease_idx ON jobs(lease_until, id) WHERE status = 'running';

CREATE TABLE credit_decisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES applications(id),
    version bigint NOT NULL CHECK (version > 0),
    source varchar(8) NOT NULL CHECK (source IN ('AUTO', 'MANUAL')),
    outcome varchar(16) NOT NULL CHECK (outcome IN ('approved', 'counter_offer', 'rejected')),
    risk_category varchar(8) CHECK (risk_category IN ('LOW', 'MEDIUM', 'HIGH')),
    approved_amount bigint NOT NULL CHECK (approved_amount >= 0),
    reasons jsonb NOT NULL CHECK (jsonb_typeof(reasons) = 'array'),
    policy_version varchar(64) NOT NULL CHECK (btrim(policy_version) <> ''),
    scoring_job_id uuid,
    decided_by uuid REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (application_id, version),
    UNIQUE (id, application_id),
    FOREIGN KEY (scoring_job_id, application_id) REFERENCES jobs(id, application_id),
    CHECK ((outcome = 'rejected' AND approved_amount = 0)
        OR (outcome IN ('approved', 'counter_offer') AND approved_amount > 0)),
    CHECK ((source = 'AUTO' AND scoring_job_id IS NOT NULL AND decided_by IS NULL)
        OR (source = 'MANUAL' AND decided_by IS NOT NULL))
);

CREATE TABLE offers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES applications(id),
    decision_id uuid NOT NULL,
    product_id uuid NOT NULL REFERENCES credit_products(id),
    version bigint NOT NULL CHECK (version > 0),
    is_primary boolean NOT NULL DEFAULT false,
    status varchar(16) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'accepted', 'withdrawn')),
    amount bigint NOT NULL CHECK (amount BETWEEN 1 AND 1000000000000),
    term_months integer NOT NULL CHECK (term_months BETWEEN 1 AND 120),
    annual_rate_percent numeric(7,4) NOT NULL CHECK (annual_rate_percent >= 0),
    monthly_payment bigint NOT NULL CHECK (monthly_payment > 0),
    total_payment bigint NOT NULL CHECK (total_payment > 0),
    overpayment bigint NOT NULL CHECK (overpayment >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (application_id, version),
    UNIQUE (id, application_id),
    FOREIGN KEY (decision_id, application_id) REFERENCES credit_decisions(id, application_id),
    CHECK (total_payment = amount + overpayment)
);
CREATE UNIQUE INDEX offers_primary_idx ON offers(application_id) WHERE is_primary AND status = 'active';
CREATE UNIQUE INDEX offers_accepted_idx ON offers(application_id) WHERE status = 'accepted';
CREATE INDEX offers_product_idx ON offers(product_id);
ALTER TABLE applications ADD CONSTRAINT applications_accepted_offer_fk
    FOREIGN KEY (accepted_offer_id, id) REFERENCES offers(id, application_id);

CREATE TABLE idempotency_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id),
    operation varchar(128) NOT NULL CHECK (btrim(operation) <> ''),
    key varchar(200) NOT NULL CHECK (btrim(key) <> ''),
    request_hash char(64) NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    status varchar(16) NOT NULL DEFAULT 'processing' CHECK (status IN ('processing', 'completed')),
    response_status smallint CHECK (response_status BETWEEN 100 AND 599),
    response_body jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    UNIQUE (user_id, operation, key),
    CHECK (expires_at > created_at),
    CHECK ((status = 'processing' AND response_status IS NULL AND response_body IS NULL)
        OR (status = 'completed' AND response_status IS NOT NULL))
);
CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys(expires_at);

COMMIT;
