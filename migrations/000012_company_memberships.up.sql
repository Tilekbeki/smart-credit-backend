BEGIN;
CREATE TABLE user_company_memberships (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES users(id),
 company_id uuid NOT NULL REFERENCES companies(id),
 membership_role varchar(32) NOT NULL CHECK (membership_role IN ('OWNER','AUTHORIZED_REPRESENTATIVE','VIEWER')),
 status varchar(16) NOT NULL CHECK (status IN ('active','revoked')),
 version bigint NOT NULL DEFAULT 1 CHECK (version>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 revoked_at timestamptz,
 granted_by uuid REFERENCES users(id),
 UNIQUE(user_id,company_id),
 CHECK ((status='active' AND revoked_at IS NULL) OR (status='revoked' AND revoked_at IS NOT NULL))
);
CREATE INDEX memberships_access_idx ON user_company_memberships(user_id,company_id,membership_role) WHERE status='active';
CREATE INDEX memberships_company_idx ON user_company_memberships(company_id,status);
INSERT INTO user_company_memberships(user_id,company_id,membership_role,status)
 SELECT id,company_id,'OWNER','active' FROM users WHERE role='CLIENT' AND company_id IS NOT NULL;
ALTER TABLE applications DROP CONSTRAINT applications_owner_company_fk;
ALTER TABLE applications ADD CONSTRAINT applications_creator_fk FOREIGN KEY(owner_id) REFERENCES users(id);
ALTER TABLE applications ADD CONSTRAINT applications_creator_membership_fk FOREIGN KEY(owner_id,company_id)
 REFERENCES user_company_memberships(user_id,company_id);
ALTER TABLE applications ADD CONSTRAINT applications_id_company_unique UNIQUE(id,company_id);
ALTER TABLE users DROP CONSTRAINT users_client_company;
-- Deprecated compatibility column is deliberately retained for rollback inspection.
COMMENT ON COLUMN users.company_id IS 'Deprecated: authorization uses user_company_memberships only';

CREATE TABLE data_audit_events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 actor_id uuid REFERENCES users(id),
 company_id uuid REFERENCES companies(id),
 entity_type varchar(32) NOT NULL,
 entity_id uuid NOT NULL,
 action varchar(64) NOT NULL,
 old_values jsonb,
 new_values jsonb,
 reason text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX data_audit_entity_idx ON data_audit_events(entity_type,entity_id,created_at,id);
CREATE INDEX data_audit_company_idx ON data_audit_events(company_id,created_at,id);
CREATE FUNCTION immutable_data_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'audit is append only' USING ERRCODE='23514'; END; $$;
CREATE TRIGGER data_audit_immutable BEFORE UPDATE OR DELETE ON data_audit_events FOR EACH ROW EXECUTE FUNCTION immutable_data_audit();
CREATE FUNCTION audit_membership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO data_audit_events(actor_id,company_id,entity_type,entity_id,action,old_values,new_values,reason)
 VALUES(NULLIF(current_setting('app.actor_id',true),'')::uuid,NEW.company_id,'membership',NEW.id,TG_OP,
 CASE WHEN TG_OP='UPDATE' THEN to_jsonb(OLD) END,to_jsonb(NEW),NULLIF(current_setting('app.audit_reason',true),''));
 RETURN NEW;
END; $$;
CREATE TRIGGER membership_audit AFTER INSERT OR UPDATE ON user_company_memberships FOR EACH ROW EXECUTE FUNCTION audit_membership();
INSERT INTO data_audit_events(company_id,entity_type,entity_id,action,new_values,reason)
 SELECT company_id,'membership',id,'BACKFILL',to_jsonb(m),'Перенос существующей связи пользователя и компании' FROM user_company_memberships m;

CREATE FUNCTION has_company_access(actor uuid,company uuid,writing boolean DEFAULT false) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM user_company_memberships m JOIN users u ON u.id=m.user_id
 WHERE u.id=actor AND u.role='CLIENT' AND m.company_id=company AND m.status='active'
 AND (NOT writing OR m.membership_role IN ('OWNER','AUTHORIZED_REPRESENTATIVE')));
$$;
-- Writers acquire this lock in the same transaction as the business mutation.
-- Revocation must wait; an operation beginning after revocation cannot pass.
CREATE FUNCTION lock_company_access(actor uuid,company uuid,writing boolean DEFAULT true) RETURNS boolean
LANGUAGE plpgsql AS $$
BEGIN
 PERFORM m.id FROM user_company_memberships m JOIN users u ON u.id=m.user_id
 WHERE u.id=actor AND u.role='CLIENT' AND m.company_id=company AND m.status='active'
 AND (NOT writing OR m.membership_role IN ('OWNER','AUTHORIZED_REPRESENTATIVE')) FOR SHARE OF m,u;
 RETURN FOUND;
END; $$;
CREATE FUNCTION can_read_application(actor uuid,app uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM applications a WHERE a.id=app AND has_company_access(actor,a.company_id,false)
 AND (a.status<>'draft' OR a.owner_id=actor));
$$;
COMMIT;
