package postgres

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"smart-credit-backend/internal/application"
)

var draftOwner = application.Scope{UserID: demoClientID, CompanyID: "10000000-0000-4000-8000-000000000001"}
var otherDraftOwner = application.Scope{UserID: "20000000-0000-4000-8000-000000000002", CompanyID: "10000000-0000-4000-8000-000000000002"}

func draftValue[T any](value T) *T { return &value }

func draftFields() application.Fields {
	return application.Fields{
		ProductID: draftValue("turnover"), Purpose: draftValue("stock"),
		PurposeDescription: draftValue("Закупка товара"), RequestedAmountRub: draftValue(int64(1_500_000)),
		TermMonths: draftValue(12), MonthlyRevenueRub: draftValue(int64(2_400_000)), MonthlyExpensesRub: draftValue(int64(1_500_000)),
	}
}

func applicationFixture(t *testing.T) (*pgxpool.Pool, *ApplicationRepository) {
	t.Helper()
	pool := authTestPool(t)
	return pool, NewApplicationRepository(pool, 5*time.Second)
}

func createDraft(t *testing.T, repo *ApplicationRepository, scope application.Scope, fields application.Fields) application.Application {
	t.Helper()
	draft, err := repo.Create(context.Background(), scope, fields)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	return draft
}

// Future states are fixtures only; stage 4 does not expose any transition API.
func draftStatusFixture(t *testing.T, pool *pgxpool.Pool, id string, status application.Status) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE applications SET status=$2,
		product_id=(SELECT id FROM credit_products WHERE code='turnover'), goal_id='stock',
		amount=1500000, term_months=12, contact_name='Synthetic user', contact_role='Owner',
		submitted_at=clock_timestamp(), financial_snapshot='{}', product_snapshot='{}' WHERE id=$1`, id, string(status))
	if err != nil {
		t.Fatalf("set application state fixture: %v", err)
	}
}

func TestApplicationRepositoryRejectsMalformedScopeWithoutQuery(t *testing.T) {
	repo := NewApplicationRepository(nil, time.Second)
	ctx := context.Background()
	for _, scope := range []application.Scope{{}, {UserID: demoClientID}, {UserID: "' OR true--", CompanyID: draftOwner.CompanyID}} {
		if _, err := repo.Create(ctx, scope, application.Fields{}); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("create malformed scope: %v", err)
		}
		if _, err := repo.Get(ctx, scope, demoClientID); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("get malformed scope: %v", err)
		}
		if _, err := repo.List(ctx, scope, application.ListFilter{Limit: 20}); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("list malformed scope: %v", err)
		}
		if _, err := repo.UpdateDraft(ctx, scope, demoClientID, 1, application.Fields{}); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("update malformed scope: %v", err)
		}
		if _, err := repo.Timeline(ctx, scope, demoClientID); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("timeline malformed scope: %v", err)
		}
	}
}

func TestApplicationRepositoryCreate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields application.Fields
	}{
		{"empty draft", application.Fields{}},
		{"all draft fields", draftFields()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, repo := applicationFixture(t)
			ctx := context.Background()
			draft := createDraft(t, repo, draftOwner, tc.fields)
			if !validUUID(draft.ApplicationID) || draft.UserID != draftOwner.UserID || draft.CompanyID != draftOwner.CompanyID ||
				draft.Status != application.Draft || draft.Version != 1 || draft.SubmittedAt != nil ||
				draft.CreatedAt.IsZero() || !draft.CreatedAt.Equal(draft.UpdatedAt) ||
				!reflect.DeepEqual(draft.Fields, tc.fields) || !reflect.DeepEqual(draft.AvailableActions, []string{"edit", "submit"}) {
				t.Fatalf("draft differs from submitted fields or expected defaults: %+v", draft)
			}
			if draft.ProductID != nil && (draft.ProductName == nil || *draft.ProductName != "Оборотный кредит") {
				t.Fatal("public product code and name were not joined from UUID relation")
			}
			got, err := repo.Get(ctx, draftOwner, draft.ApplicationID)
			if err != nil || !reflect.DeepEqual(got, draft) {
				t.Fatalf("saved draft cannot be reopened: %+v, %v", got, err)
			}
			events, err := repo.Timeline(ctx, draftOwner, draft.ApplicationID)
			if err != nil || len(events) != 1 || events[0].EventType != "draft_created" ||
				events[0].Status != application.Draft || events[0].Sequence != 1 ||
				events[0].ActorID == nil || *events[0].ActorID != draftOwner.UserID ||
				events[0].Title != "Черновик создан" || !validUUID(events[0].EventID) || events[0].CreatedAt.IsZero() {
				t.Fatalf("first event: %+v, %v", events, err)
			}
			for _, table := range []string{"jobs", "credit_decisions", "offers"} {
				var count int
				if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("draft creation unexpectedly wrote %s", table)
				}
			}
		})
	}
}

func TestApplicationRepositoryOwnership(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, draftFields())
	const teammateID = "20000000-0000-4000-8000-000000000005"
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, profile_key, company_id, phone, full_name, role)
		VALUES ($1,'teammate',$2,'+79990000005','Synthetic teammate','CLIENT')`, teammateID, draftOwner.CompanyID); err != nil {
		t.Fatal("cannot create second owner in same company")
	}
	for _, scope := range []application.Scope{
		otherDraftOwner,
		{UserID: teammateID, CompanyID: draftOwner.CompanyID},
		{UserID: draftOwner.UserID, CompanyID: otherDraftOwner.CompanyID},
	} {
		if _, err := repo.Get(ctx, scope, draft.ApplicationID); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("foreign application read: %v", err)
		}
		if _, err := repo.UpdateDraft(ctx, scope, draft.ApplicationID, 1, application.Fields{}); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("foreign application write: %v", err)
		}
		if _, err := repo.Timeline(ctx, scope, draft.ApplicationID); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("foreign timeline read: %v", err)
		}
		page, err := repo.List(ctx, scope, application.ListFilter{Limit: 20})
		if err != nil || page.Total != 0 || page.Items == nil || len(page.Items) != 0 {
			t.Fatalf("foreign application appeared in list: %+v, %v", page, err)
		}
	}
	for _, id := range []string{"not-uuid", "' OR true--", "00000000-0000-4000-8000-000000000000"} {
		if _, err := repo.Get(ctx, draftOwner, id); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("malformed or unknown application read: %v", err)
		}
		if _, err := repo.UpdateDraft(ctx, draftOwner, id, 1, application.Fields{}); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("malformed or unknown application write: %v", err)
		}
		if _, err := repo.Timeline(ctx, draftOwner, id); !errors.Is(err, application.ErrNotFound) {
			t.Fatalf("malformed or unknown application timeline: %v", err)
		}
	}
}

func TestApplicationRepositoryPaginationAndStatus(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	var ids []string
	for range 5 {
		draft := createDraft(t, repo, draftOwner, draftFields())
		ids = append(ids, draft.ApplicationID)
	}
	createDraft(t, repo, otherDraftOwner, application.Fields{})
	if _, err := pool.Exec(ctx, `UPDATE applications SET created_at='2026-01-01T00:00:00Z' WHERE owner_id=$1`, draftOwner.UserID); err != nil {
		t.Fatal("cannot set deterministic timestamp fixture")
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	for offset := 0; offset < 7; offset += 2 {
		page, err := repo.List(ctx, draftOwner, application.ListFilter{Limit: 2, Offset: offset})
		if err != nil || page.Total != 5 || page.Limit != 2 || page.Offset != offset || page.Items == nil {
			t.Fatalf("pagination metadata: %+v, %v", page, err)
		}
		end := min(offset+2, len(ids))
		want := ids[min(offset, len(ids)):end]
		if len(page.Items) != len(want) {
			t.Fatalf("page length: got %d, want %d", len(page.Items), len(want))
		}
		for i, item := range page.Items {
			if item.ApplicationID != want[i] || item.ProductName == nil || *item.ProductName != "Оборотный кредит" ||
				item.ProductID == nil || *item.ProductID != "turnover" || item.Version != 1 || item.Status != application.Draft {
				t.Fatalf("pagination order or fields: %+v", page.Items)
			}
		}
	}
	draftStatusFixture(t, pool, ids[0], application.Submitted)
	for _, tc := range []struct {
		status application.Status
		count  int64
	}{{application.Draft, 4}, {application.Submitted, 1}, {application.Rejected, 0}} {
		page, err := repo.List(ctx, draftOwner, application.ListFilter{Limit: 20, Status: &tc.status})
		if err != nil || page.Total != tc.count || int64(len(page.Items)) != tc.count || page.Items == nil {
			t.Fatalf("filter %s: %+v, %v", tc.status, page, err)
		}
		for _, item := range page.Items {
			if item.Status != tc.status {
				t.Fatal("status filter returned wrong state")
			}
		}
	}
}

func TestApplicationRepositoryUpdateAndClear(t *testing.T) {
	_, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, application.Fields{})
	fields := draftFields()
	updated, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 1, fields)
	if err != nil || updated.Version != 2 || !reflect.DeepEqual(updated.Fields, fields) ||
		!updated.CreatedAt.Equal(draft.CreatedAt) || !updated.UpdatedAt.After(draft.UpdatedAt) ||
		updated.UserID != draft.UserID || updated.CompanyID != draft.CompanyID || updated.Status != application.Draft {
		t.Fatalf("update draft: %+v, %v", updated, err)
	}
	if _, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 1, application.Fields{}); !errors.Is(err, application.ErrVersionConflict) {
		t.Fatalf("stale version overwrote saved data: %v", err)
	}
	current, err := repo.Get(ctx, draftOwner, draft.ApplicationID)
	if err != nil || !reflect.DeepEqual(current, updated) {
		t.Fatalf("failed stale write changed draft: %+v, %v", current, err)
	}
	cleared, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 2, application.Fields{})
	if err != nil || cleared.Version != 3 || !reflect.DeepEqual(cleared.Fields, application.Fields{}) || cleared.ProductName != nil {
		t.Fatalf("clear nullable draft fields: %+v, %v", cleared, err)
	}
	events, err := repo.Timeline(ctx, draftOwner, draft.ApplicationID)
	if err != nil || len(events) != 3 {
		t.Fatalf("draft history: %+v, %v", events, err)
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) || event.ActorID == nil || *event.ActorID != draftOwner.UserID || event.Status != application.Draft {
			t.Fatalf("timeline sequence or owner: %+v", events)
		}
		if i > 0 && (event.EventType != "draft_updated" || event.Title != "Черновик обновлён" || event.CreatedAt.Before(events[i-1].CreatedAt)) {
			t.Fatalf("update event details: %+v", event)
		}
	}
}

func TestApplicationRepositoryConcurrentUpdate(t *testing.T) {
	_, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, application.Fields{})
	results := concurrently(20, func() error {
		_, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 1, draftFields())
		return err
	})
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, application.ErrVersionConflict) {
			t.Fatalf("unexpected concurrent write error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent successful writes = %d, want 1", winners)
	}
	got, err := repo.Get(ctx, draftOwner, draft.ApplicationID)
	if err != nil || got.Version != 2 || !reflect.DeepEqual(got.Fields, draftFields()) {
		t.Fatalf("concurrent final draft: %+v, %v", got, err)
	}
	events, err := repo.Timeline(ctx, draftOwner, draft.ApplicationID)
	if err != nil || len(events) != 2 || events[1].Sequence != 2 {
		t.Fatalf("concurrent writes duplicated history: %+v, %v", events, err)
	}
}

func TestApplicationRepositoryNonDraftCannotBeEdited(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, draftFields())
	draftStatusFixture(t, pool, draft.ApplicationID, application.Submitted)
	for _, version := range []int64{1, 99} {
		if _, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, version, application.Fields{}); !errors.Is(err, application.ErrInvalidStatus) {
			t.Fatalf("submitted application edit: %v", err)
		}
	}
	got, err := repo.Get(ctx, draftOwner, draft.ApplicationID)
	if err != nil || got.Status != application.Submitted || got.Version != 1 || got.SubmittedAt == nil ||
		got.AvailableActions == nil || len(got.AvailableActions) != 0 {
		t.Fatalf("read-only future state: %+v, %v", got, err)
	}
	events, err := repo.Timeline(ctx, draftOwner, draft.ApplicationID)
	if err != nil || len(events) != 1 {
		t.Fatalf("rejected edit added events: %+v, %v", events, err)
	}
}

func TestApplicationRepositoryProductResolution(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, draftFields())
	if _, err := pool.Exec(ctx, `UPDATE credit_products SET active=false WHERE code='turnover'`); err != nil {
		t.Fatal("cannot deactivate product fixture")
	}
	got, err := repo.Get(ctx, draftOwner, draft.ApplicationID)
	if err != nil || got.ProductID == nil || *got.ProductID != "turnover" || got.ProductName == nil {
		t.Fatalf("inactive product disappeared from saved draft: %+v, %v", got, err)
	}
	for _, code := range []string{"turnover", "missing", "' OR true--"} {
		fields := application.Fields{ProductID: &code}
		if _, err := repo.Create(ctx, draftOwner, fields); !errors.Is(err, application.ErrProductNotFound) {
			t.Fatalf("create with unavailable product: %v", err)
		}
		if _, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 1, fields); !errors.Is(err, application.ErrProductNotFound) {
			t.Fatalf("update with unavailable product: %v", err)
		}
	}
	page, err := repo.List(ctx, draftOwner, application.ListFilter{Limit: 20})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ProductName == nil {
		t.Fatalf("unavailable product writes affected data: %+v, %v", page, err)
	}
}

func TestApplicationRepositoryRechecksConcurrentProductChange(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE credit_products SET max_amount=1000000 WHERE code='turnover'`); err != nil {
		t.Fatal("cannot change product terms in locking transaction")
	}
	result := make(chan error, 1)
	go func() {
		_, err := repo.Create(ctx, draftOwner, draftFields())
		result <- err
	}()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("cannot commit product term change")
	}
	if err := <-result; !errors.Is(err, application.ErrInvalidInput) {
		t.Fatalf("concurrent product change accepted stale terms: %v", err)
	}
	page, err := repo.List(ctx, draftOwner, application.ListFilter{Limit: 20})
	if err != nil || page.Total != 0 {
		t.Fatalf("invalid product terms left a draft: %+v, %v", page, err)
	}
}

func TestApplicationRepositoryConcurrentStateChange(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, draftFields())
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE applications SET status='submitted', contact_name='Synthetic user',
		contact_role='Owner', submitted_at=clock_timestamp(), financial_snapshot='{}', product_snapshot='{}'
		WHERE id=$1`, draft.ApplicationID); err != nil {
		t.Fatal("cannot set concurrent state fixture")
	}
	result := make(chan error, 1)
	go func() {
		_, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 1, draftFields())
		result <- err
	}()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("cannot commit state change")
	}
	if err := <-result; !errors.Is(err, application.ErrInvalidStatus) {
		t.Fatalf("concurrent state change allowed draft edit: %v", err)
	}
	got, err := repo.Get(ctx, draftOwner, draft.ApplicationID)
	if err != nil || got.Status != application.Submitted || got.Version != 1 {
		t.Fatalf("state change lost or draft version altered: %+v, %v", got, err)
	}
}

func installEventFailure(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `CREATE FUNCTION fail_draft_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic event write failure'; END $$;
		CREATE TRIGGER fail_draft_event BEFORE INSERT ON application_events
		FOR EACH ROW EXECUTE FUNCTION fail_draft_event()`); err != nil {
		t.Fatalf("cannot create transaction failure fixture: %v", err)
	}
}

func TestApplicationRepositoryCreateRollsBackOnEventFailure(t *testing.T) {
	pool, repo := applicationFixture(t)
	installEventFailure(t, pool)
	if _, err := repo.Create(context.Background(), draftOwner, draftFields()); !errors.Is(err, application.ErrStorage) {
		t.Fatalf("event failure not sanitized: %v", err)
	}
	var applications, events int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM applications),
		(SELECT count(*) FROM application_events)`).Scan(&applications, &events); err != nil || applications != 0 || events != 0 {
		t.Fatalf("partial create survived rollback: applications=%d events=%d err=%v", applications, events, err)
	}
}

func TestApplicationRepositoryUpdateRollsBackOnEventFailure(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, application.Fields{})
	installEventFailure(t, pool)
	if _, err := repo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 1, draftFields()); !errors.Is(err, application.ErrStorage) {
		t.Fatalf("event failure not sanitized: %v", err)
	}
	got, err := repo.Get(ctx, draftOwner, draft.ApplicationID)
	if err != nil || !reflect.DeepEqual(got, draft) {
		t.Fatalf("partial patch survived rollback: %+v, %v", got, err)
	}
	events, err := repo.Timeline(ctx, draftOwner, draft.ApplicationID)
	if err != nil || len(events) != 1 {
		t.Fatalf("partial patch wrote events: %+v, %v", events, err)
	}
}

func TestApplicationRepositoryTimeoutAndCancellation(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, application.Fields{})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	cases := []struct {
		name string
		call func() error
	}{
		{"create", func() error { _, err := repo.Create(canceled, draftOwner, application.Fields{}); return err }},
		{"get", func() error { _, err := repo.Get(canceled, draftOwner, draft.ApplicationID); return err }},
		{"list", func() error { _, err := repo.List(canceled, draftOwner, application.ListFilter{Limit: 20}); return err }},
		{"update", func() error {
			_, err := repo.UpdateDraft(canceled, draftOwner, draft.ApplicationID, 1, application.Fields{})
			return err
		}},
		{"timeline", func() error { _, err := repo.Timeline(canceled, draftOwner, draft.ApplicationID); return err }},
	}
	for _, tc := range cases {
		if err := tc.call(); err != application.ErrStorage {
			t.Fatalf("%s cancellation: %v", tc.name, err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal("cannot begin locking transaction")
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM applications WHERE id=$1 FOR UPDATE`, draft.ApplicationID); err != nil {
		t.Fatal("cannot lock application fixture")
	}
	shortRepo := NewApplicationRepository(pool, 50*time.Millisecond)
	started := time.Now()
	if _, err := shortRepo.UpdateDraft(ctx, draftOwner, draft.ApplicationID, 1, application.Fields{}); err != application.ErrStorage {
		t.Fatalf("locked application patch: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("application update exceeded bounded lock wait")
	}
}

func applyDraftMigration(t *testing.T, pool *pgxpool.Pool, direction string) {
	t.Helper()
	data, err := os.ReadFile("../../migrations/000006_application_drafts." + direction + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(data)); err != nil {
		t.Fatalf("application draft migration %s: %v", direction, err)
	}
}

func TestApplicationDraftMigrationCycle(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, application.Fields{})
	applyDraftMigration(t, pool, "down")
	applyDraftMigration(t, pool, "up")
	got, err := repo.Get(ctx, draftOwner, draft.ApplicationID)
	if err != nil || !reflect.DeepEqual(got, draft) {
		t.Fatalf("schema cycle changed original draft data: %+v, %v", got, err)
	}
	for _, query := range []string{
		`UPDATE applications SET monthly_revenue=-1`,
		`UPDATE applications SET monthly_expenses=-1`,
		`UPDATE applications SET monthly_revenue=1000000000001`,
		`UPDATE applications SET monthly_expenses=1000000000001`,
		`UPDATE applications SET status='confirmed'`,
		`UPDATE applications SET status='processing_failed'`,
		`UPDATE application_events SET status='confirmed'`,
		`UPDATE application_events SET status='processing_failed'`,
	} {
		if _, err := pool.Exec(ctx, query); err == nil {
			t.Fatalf("migration allowed invalid financial data or old status: %s", query)
		}
	}
}

func TestApplicationDraftMigrationPreservesLegacyFailure(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, draftFields())
	draftStatusFixture(t, pool, draft.ApplicationID, application.Processing)
	applyDraftMigration(t, pool, "down")
	if _, err := pool.Exec(ctx, `UPDATE applications SET status='processing_failed' WHERE id=$1`, draft.ApplicationID); err != nil {
		t.Fatal(err)
	}
	applyDraftMigration(t, pool, "up")
	var status, processingError string
	if err := pool.QueryRow(ctx, `SELECT status,processing_error FROM applications WHERE id=$1`, draft.ApplicationID).
		Scan(&status, &processingError); err != nil || status != "processing" || processingError == "" {
		t.Fatalf("technical failure lost during upgrade: %s %s %v", status, processingError, err)
	}
	applyDraftMigration(t, pool, "down")
	if err := pool.QueryRow(ctx, `SELECT status FROM applications WHERE id=$1`, draft.ApplicationID).Scan(&status); err != nil || status != "processing_failed" {
		t.Fatalf("technical failure not restored on downgrade: %s %v", status, err)
	}
	applyDraftMigration(t, pool, "up")
}

func TestApplicationDraftMigrationRenamesConfirmation(t *testing.T) {
	pool, repo := applicationFixture(t)
	ctx := context.Background()
	draft := createDraft(t, repo, draftOwner, draftFields())
	draftStatusFixture(t, pool, draft.ApplicationID, application.Approved)
	var decisionID, offerID string
	if err := pool.QueryRow(ctx, `INSERT INTO credit_decisions
		(application_id,version,source,outcome,approved_amount,reasons,policy_version,decided_by,internal_comment)
		VALUES ($1,1,'MANUAL','approved',1500000,'[]','synthetic-test',$2,'Synthetic legacy confirmation') RETURNING id::text`,
		draft.ApplicationID, demoEmployeeID).Scan(&decisionID); err != nil {
		t.Fatal("cannot create legacy confirmation decision fixture")
	}
	if err := pool.QueryRow(ctx, `INSERT INTO offers
		(application_id,decision_id,product_id,version,status,amount,term_months,
		annual_rate_percent,monthly_payment,total_payment,overpayment)
		VALUES ($1,$2,(SELECT id FROM credit_products WHERE code='turnover'),1,'accepted',1500000,12,22,150000,1800000,300000)
		RETURNING id::text`, draft.ApplicationID, decisionID).Scan(&offerID); err != nil {
		t.Fatal("cannot create legacy confirmation offer fixture")
	}
	if _, err := pool.Exec(ctx, `UPDATE applications SET status='signed', accepted_offer_id=$2,
		signed_by=$3, signed_at=clock_timestamp() WHERE id=$1`, draft.ApplicationID, offerID, draftOwner.UserID); err != nil {
		t.Fatal("cannot create signature fixture")
	}
	if _, err := pool.Exec(ctx, `UPDATE application_events SET status='signed' WHERE application_id=$1`, draft.ApplicationID); err != nil {
		t.Fatal("cannot create signature event fixture")
	}
	var initialTime time.Time
	if err := pool.QueryRow(ctx, `SELECT signed_at FROM applications WHERE id=$1`, draft.ApplicationID).Scan(&initialTime); err != nil {
		t.Fatal(err)
	}
	applyDraftMigration(t, pool, "down")
	var status, signedBy, acceptedOffer string
	var signedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT status, confirmed_by::text, accepted_offer_id::text, confirmed_at
		FROM applications WHERE id=$1`, draft.ApplicationID).Scan(&status, &signedBy, &acceptedOffer, &signedAt); err != nil ||
		status != "confirmed" || signedBy != draftOwner.UserID || acceptedOffer != offerID || !initialTime.Equal(signedAt) {
		t.Fatalf("downgrade lost existing confirmation: %s %s %s %v %v", status, signedBy, acceptedOffer, signedAt, err)
	}
	applyDraftMigration(t, pool, "up")
	if err := pool.QueryRow(ctx, `SELECT status, signed_by::text, accepted_offer_id::text, signed_at
		FROM applications WHERE id=$1`, draft.ApplicationID).Scan(&status, &signedBy, &acceptedOffer, &signedAt); err != nil ||
		status != "signed" || signedBy != draftOwner.UserID || acceptedOffer != offerID || !initialTime.Equal(signedAt) {
		t.Fatalf("upgrade lost legacy confirmation: %s %s %s %v %v", status, signedBy, acceptedOffer, signedAt, err)
	}
	events, err := repo.Timeline(ctx, draftOwner, draft.ApplicationID)
	if err != nil || len(events) != 1 || events[0].Status != application.Signed {
		t.Fatalf("upgrade lost confirmation history: %+v, %v", events, err)
	}
}

func TestApplicationDraftMigrationRefusesLossyDowngrade(t *testing.T) {
	for _, state := range []string{"closed", "new processing error", "ambiguous legacy timeline"} {
		t.Run(state, func(t *testing.T) {
			pool, repo := applicationFixture(t)
			ctx := context.Background()
			draft := createDraft(t, repo, draftOwner, draftFields())
			switch state {
			case "closed":
				draftStatusFixture(t, pool, draft.ApplicationID, application.Closed)
			case "new processing error":
				draftStatusFixture(t, pool, draft.ApplicationID, application.Processing)
				if _, err := pool.Exec(ctx, `UPDATE applications SET processing_error='Synthetic new failure' WHERE id=$1`, draft.ApplicationID); err != nil {
					t.Fatal(err)
				}
			case "ambiguous legacy timeline":
				draftStatusFixture(t, pool, draft.ApplicationID, application.Processing)
				applyDraftMigration(t, pool, "down")
				if _, err := pool.Exec(ctx, `UPDATE applications SET status='processing_failed' WHERE id=$1`, draft.ApplicationID); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `UPDATE application_events SET status='processing_failed' WHERE application_id=$1`, draft.ApplicationID); err != nil {
					t.Fatal(err)
				}
				applyDraftMigration(t, pool, "up")
			}
			data, err := os.ReadFile("../../migrations/000006_application_drafts.down.sql")
			if err != nil {
				t.Fatal(err)
			}
			// A dedicated connection is needed to roll back the deliberately failed
			// migration transaction before returning the connection to the pool.
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, string(data)); err == nil {
				t.Fatal("lossy downgrade unexpectedly succeeded")
			}
			if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
				t.Fatal("cannot roll back rejected downgrade")
			}
			if _, err := repo.Get(ctx, draftOwner, draft.ApplicationID); err != nil {
				t.Fatalf("rejected downgrade changed schema or application: %v", err)
			}
		})
	}
}
