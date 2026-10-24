BEGIN;
ALTER TABLE users ADD COLUMN display_name varchar(160), ADD COLUMN additional_email varchar(254),
 ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0);
UPDATE users SET display_name=full_name;
CREATE FUNCTION default_user_display_name() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.display_name IS NULL THEN NEW.display_name=NEW.full_name; END IF; RETURN NEW; END; $$;
CREATE TRIGGER users_default_display_name BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION default_user_display_name();
ALTER TABLE users ALTER COLUMN display_name SET NOT NULL;
ALTER TABLE users ADD CONSTRAINT users_display_name CHECK(char_length(btrim(display_name)) BETWEEN 1 AND 160);
ALTER TABLE companies ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 ADD COLUMN registration_data_source varchar(32) NOT NULL DEFAULT 'demo_registry'
 CHECK(registration_data_source IN ('demo_registry','bank_confirmed'));
ALTER TABLE documents ADD COLUMN company_id uuid REFERENCES companies(id);
UPDATE documents d SET company_id=a.company_id FROM applications a WHERE a.id=d.application_id;
CREATE FUNCTION default_document_company() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.company_id IS NULL THEN SELECT company_id INTO NEW.company_id FROM applications WHERE id=NEW.application_id; END IF; RETURN NEW; END; $$;
CREATE TRIGGER documents_default_company BEFORE INSERT ON documents FOR EACH ROW EXECUTE FUNCTION default_document_company();
ALTER TABLE documents ALTER COLUMN company_id SET NOT NULL;
ALTER TABLE documents ADD CONSTRAINT documents_application_company_fk FOREIGN KEY(application_id,company_id) REFERENCES applications(id,company_id);
ALTER TABLE documents ADD CONSTRAINT documents_id_company_unique UNIQUE(id,company_id);
CREATE TABLE company_financial_declarations (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 company_id uuid NOT NULL REFERENCES companies(id),
 monthly_revenue bigint NOT NULL CHECK(monthly_revenue BETWEEN 0 AND 1000000000000),
 monthly_expenses bigint NOT NULL CHECK(monthly_expenses BETWEEN 0 AND 1000000000000),
 source varchar(32) NOT NULL CHECK(source IN ('demo_bank_feed','client_declared','legacy_unverified')),
 status varchar(16) NOT NULL CHECK(status IN ('verified','pending_review','rejected')),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 declared_by uuid REFERENCES users(id),
 supporting_document_id uuid,
 reason varchar(1000) NOT NULL CHECK(char_length(btrim(reason)) BETWEEN 1 AND 1000),
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 verified_at timestamptz,
 reviewed_at timestamptz,
 reviewed_by uuid REFERENCES users(id),
 review_comment varchar(1000),
 FOREIGN KEY(supporting_document_id,company_id) REFERENCES documents(id,company_id),
 CHECK((status='verified' AND verified_at IS NOT NULL) OR (status<>'verified' AND verified_at IS NULL)),
 CHECK((reviewed_by IS NULL AND reviewed_at IS NULL) OR (reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL))
);
ALTER TABLE company_financial_declarations ADD COLUMN application_id uuid;
 ALTER TABLE company_financial_declarations ADD CONSTRAINT financial_declaration_application_fk FOREIGN KEY(application_id,company_id) REFERENCES applications(id,company_id);
 CREATE INDEX financial_declarations_company_idx ON company_financial_declarations(company_id,received_at DESC,id);
INSERT INTO company_financial_declarations(company_id,monthly_revenue,monthly_expenses,source,status,reason,verified_at)
 SELECT id,monthly_revenue,monthly_expenses,CASE WHEN financial_data_source='demo_bank_feed' THEN 'demo_bank_feed' ELSE 'legacy_unverified' END,
 CASE WHEN financial_data_source='demo_bank_feed' THEN 'verified' ELSE 'pending_review' END,
 'Исходные синтетические данные компании',CASE WHEN financial_data_source='demo_bank_feed' THEN clock_timestamp() END FROM companies;
CREATE FUNCTION protect_financial_observation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.status<>'pending_review' OR (to_jsonb(NEW)-ARRAY['status','version','verified_at','reviewed_at','reviewed_by','review_comment']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','version','verified_at','reviewed_at','reviewed_by','review_comment']) THEN
 RAISE EXCEPTION 'financial observations are immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER financial_observation_immutable BEFORE UPDATE ON company_financial_declarations FOR EACH ROW EXECUTE FUNCTION protect_financial_observation();
CREATE TABLE company_change_requests (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 company_id uuid NOT NULL REFERENCES companies(id),
 initiated_by uuid NOT NULL REFERENCES users(id),
 company_version bigint NOT NULL CHECK(company_version>0),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 original_values jsonb NOT NULL CHECK(jsonb_typeof(original_values)='object'),
 proposed_values jsonb NOT NULL CHECK(jsonb_typeof(proposed_values)='object'),
 reason varchar(1000) NOT NULL CHECK(char_length(btrim(reason)) BETWEEN 1 AND 1000),
 status varchar(16) NOT NULL DEFAULT 'pending_review' CHECK(status IN ('pending_review','approved','rejected','cancelled')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 reviewed_at timestamptz,
 reviewed_by uuid REFERENCES users(id),
 review_comment varchar(1000),
 CHECK((status='pending_review' AND reviewed_at IS NULL AND reviewed_by IS NULL) OR
 (status<>'pending_review' AND reviewed_at IS NOT NULL AND reviewed_by IS NOT NULL))
);
CREATE INDEX change_requests_company_idx ON company_change_requests(company_id,created_at DESC,id);
ALTER TABLE credit_decisions ADD COLUMN customer_explanation varchar(1000) CHECK(char_length(btrim(customer_explanation)) BETWEEN 1 AND 1000);
ALTER TABLE jobs ADD COLUMN recovery_count integer NOT NULL DEFAULT 0 CHECK(recovery_count BETWEEN 0 AND 3);
-- Old snapshot content remains untouched. The new policy derives legacy trust by
-- comparing the frozen applicant amounts against the frozen source amounts.
COMMIT;
