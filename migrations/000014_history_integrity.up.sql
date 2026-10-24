BEGIN;
ALTER TABLE company_financial_declarations ADD CONSTRAINT financial_verified_reviewer CHECK (
 source<>'client_declared' OR status<>'verified' OR (reviewed_by IS NOT NULL AND reviewed_at IS NOT NULL)
);
CREATE TRIGGER financial_observation_no_delete BEFORE DELETE ON company_financial_declarations FOR EACH ROW EXECUTE FUNCTION immutable_data_audit();
CREATE TRIGGER credit_decisions_immutable BEFORE UPDATE OR DELETE ON credit_decisions FOR EACH ROW EXECUTE FUNCTION immutable_data_audit();
CREATE FUNCTION protect_company_correction() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.status<>'pending_review' OR NEW.version<>OLD.version+1 OR
 (to_jsonb(NEW)-ARRAY['status','version','reviewed_at','reviewed_by','review_comment']) IS DISTINCT FROM
 (to_jsonb(OLD)-ARRAY['status','version','reviewed_at','reviewed_by','review_comment']) THEN
 RAISE EXCEPTION 'company correction history is immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER company_correction_immutable BEFORE UPDATE ON company_change_requests FOR EACH ROW EXECUTE FUNCTION protect_company_correction();
CREATE TRIGGER company_correction_no_delete BEFORE DELETE ON company_change_requests FOR EACH ROW EXECUTE FUNCTION immutable_data_audit();
CREATE FUNCTION protect_membership_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.id<>OLD.id OR NEW.user_id<>OLD.user_id OR NEW.company_id<>OLD.company_id OR NEW.created_at<>OLD.created_at OR NEW.version<>OLD.version+1 THEN
 RAISE EXCEPTION 'membership identity is immutable; update role and status with a new version' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER membership_identity_immutable BEFORE UPDATE ON user_company_memberships FOR EACH ROW EXECUTE FUNCTION protect_membership_identity();
CREATE TRIGGER membership_no_delete BEFORE DELETE ON user_company_memberships FOR EACH ROW EXECUTE FUNCTION immutable_data_audit();
CREATE OR REPLACE FUNCTION protect_submitted_application_snapshots() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.company_id IS DISTINCT FROM OLD.company_id THEN
 RAISE EXCEPTION 'application creator and company are immutable' USING ERRCODE='23514'; END IF;
 IF OLD.submitted_at IS NOT NULL AND (
 NEW.submitted_at IS DISTINCT FROM OLD.submitted_at OR NEW.financial_snapshot IS DISTINCT FROM OLD.financial_snapshot OR NEW.product_snapshot IS DISTINCT FROM OLD.product_snapshot
 OR NEW.product_id IS DISTINCT FROM OLD.product_id OR NEW.goal_id IS DISTINCT FROM OLD.goal_id OR NEW.goal_description IS DISTINCT FROM OLD.goal_description
 OR NEW.amount IS DISTINCT FROM OLD.amount OR NEW.term_months IS DISTINCT FROM OLD.term_months
 OR (to_jsonb(NEW)->'monthly_revenue') IS DISTINCT FROM (to_jsonb(OLD)->'monthly_revenue') OR (to_jsonb(NEW)->'monthly_expenses') IS DISTINCT FROM (to_jsonb(OLD)->'monthly_expenses')
 ) THEN RAISE EXCEPTION 'submitted application inputs are immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END; $$;
COMMIT;
