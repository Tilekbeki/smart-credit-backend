package postgres

import (
	"context"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"smart-credit-backend/internal/application"
	"testing"
)

func TestUpgradeFromElevenPreservesExistingData(t *testing.T) {
	pool := authTestPoolThrough(t, 11)
	ctx := context.Background()
	id := uuid.NewString()
	jobID := uuid.NewString()
	decisionID := uuid.NewString()
	documentID := uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO applications(id,owner_id,company_id,product_id,goal_id,amount,term_months,monthly_revenue,monthly_expenses,contact_name,contact_role,status,submitted_at,financial_snapshot,product_snapshot)
 VALUES($1,$2,$3,(SELECT id FROM credit_products WHERE code='turnover'),'stock',1500000,12,2400000,1500000,'Исторический пользователь','Предприниматель','rejected',clock_timestamp(),'{"schemaVersion":1,"historical":true}','{"historical":true}')`, id, demoClientID, draftOwner.CompanyID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO jobs(id,application_id,type,status,finished_at,scoring_request,scoring_response,scoring_received_at) VALUES($1,$2,'prechecks','failed',clock_timestamp(),'{"saved":"request"}','{"saved":"response"}',clock_timestamp())`, jobID, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO credit_decisions(id,application_id,version,source,outcome,risk_category,approved_amount,reasons,policy_version,scoring_job_id) VALUES($1,$2,1,'AUTO','rejected','HIGH',0,'["Исторический отказ"]','go-demo-v1',$3)`, decisionID, id, jobID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO documents(id,application_id,uploaded_by,original_name,content_type,size_bytes,checksum_sha256,storage_key,status,document_type) VALUES($1::uuid,$2,$3,'historical.pdf','application/pdf',12,repeat('a',64),$1::text,'ready','BANK_STATEMENT')`, documentID, id, demoClientID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO application_events(application_id,sequence,event_type,status,title,actor_id) VALUES($1,1,'historical_event','rejected','Историческое событие',$2)`, id, demoClientID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO idempotency_keys(user_id,operation,key,request_hash,status,response_status,response_body,expires_at) VALUES($1,'manual:historical','old-key',repeat('a',64),'completed',201,'{"historical":"replay"}',clock_timestamp()+interval '1 day')`, demoClientID); err != nil {
		t.Fatal(err)
	}
	var before string
	const fingerprints = `SELECT md5(jsonb_build_object('applications',(SELECT jsonb_agg(to_jsonb(a)) FROM applications a),'jobs',(SELECT jsonb_agg(to_jsonb(j)-'recovery_count') FROM jobs j),'decisions',(SELECT jsonb_agg(to_jsonb(d)-'customer_explanation') FROM credit_decisions d),'documents',(SELECT jsonb_agg(to_jsonb(d)-'company_id') FROM documents d),'events',(SELECT jsonb_agg(to_jsonb(e)) FROM application_events e),'keys',(SELECT jsonb_agg(to_jsonb(k)) FROM idempotency_keys k))::text)`
	if pool.QueryRow(ctx, fingerprints).Scan(&before) != nil {
		t.Fatal("before fingerprint")
	}
	for _, name := range []string{"000012_company_memberships.up.sql", "000013_data_governance.up.sql", "000014_history_integrity.up.sql"} {
		b, err := os.ReadFile(filepath.Join("../../migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	var after string
	if pool.QueryRow(ctx, fingerprints).Scan(&after) != nil || before != after {
		t.Fatal("upgrade rewrote historical state")
	}
	var memberships, users, bankMemberships int
	if pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM user_company_memberships),(SELECT count(*) FROM users),(SELECT count(*) FROM user_company_memberships WHERE user_id=$1)`, demoEmployeeID).Scan(&memberships, &users, &bankMemberships) != nil || memberships != 3 || users != 4 || bankMemberships != 0 {
		t.Fatal("identity backfill lost data")
	}
	// Historical creator/company relations survive and permit adding a second company.
	if _, err = pool.Exec(ctx, `INSERT INTO user_company_memberships(user_id,company_id,membership_role,status) VALUES($1,$2,'OWNER','active')`, demoClientID, otherDraftOwner.CompanyID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewApplicationRepository(pool, 5e9).Get(ctx, application.Scope{UserID: demoClientID}, id); err != nil {
		t.Fatal("old application inaccessible", err)
	}
	for _, name := range []string{"000012_company_memberships.down.sql", "000013_data_governance.down.sql", "000014_history_integrity.down.sql"} {
		b, _ := os.ReadFile(filepath.Join("../../migrations", name))
		if _, err = pool.Exec(ctx, string(b)); err == nil {
			t.Fatal("unsafe downgrade accepted")
		}
	}
}
