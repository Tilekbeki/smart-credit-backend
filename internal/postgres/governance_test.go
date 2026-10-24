package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http/httptest"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/document"
	"smart-credit-backend/internal/httpapi"
	"smart-credit-backend/internal/lending"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/review"
	"strings"
	"testing"
	"time"
)

func verifyFixtureFinancials(t *testing.T, pool *pgxpool.Pool, id string, f application.Fields) {
	t.Helper()
	if f.MonthlyRevenueRub == nil || f.MonthlyExpensesRub == nil {
		return
	}
	_, err := pool.Exec(context.Background(), `INSERT INTO company_financial_declarations(company_id,monthly_revenue,monthly_expenses,source,status,reason,verified_at,reviewed_by,reviewed_at) VALUES($1,$2,$3,'client_declared','verified','Подтверждённые тестовые финансовые факты',clock_timestamp(),$4,clock_timestamp())`, id, *f.MonthlyRevenueRub, *f.MonthlyExpensesRub, demoEmployeeID)
	if err != nil {
		t.Fatal(err)
	}
}

func managementService(pool *pgxpool.Pool) *company.ManagementService {
	return company.NewManagementService(NewManagementRepository(pool, 5*time.Second))
}
func dataCommand(action, companyID string, version int64) company.Command {
	return company.Command{ActorID: demoClientID, CompanyID: companyID, Action: action, Version: version, Key: uuid.NewString(), Reason: "Проверенные тестовые изменения"}
}
func executeData(t *testing.T, s *company.ManagementService, c company.Command) map[string]any {
	t.Helper()
	r, err := s.Execute(context.Background(), c)
	if err != nil {
		t.Fatalf("%s: %v", c.Action, err)
	}
	var obj map[string]any
	if json.Unmarshal(r.Body, &obj) != nil {
		t.Fatal("invalid management DTO")
	}
	return obj
}
func grantData(t *testing.T, s *company.ManagementService, user, companyID, role string, version int64) map[string]any {
	t.Helper()
	c := dataCommand("membership", companyID, version)
	c.ActorID = demoEmployeeID
	c.TargetID = user
	c.MembershipRole = role
	c.Status = "active"
	return executeData(t, s, c)
}

func TestMultiCompanyMembershipAuthorization(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	management := managementService(pool)
	apps := NewApplicationRepository(pool, 5*time.Second)
	// Tokens precede the membership change; middleware must read the current DB.
	handler, tokens := readHTTPFixture(t, pool)
	grantData(t, management, demoClientID, otherDraftOwner.CompanyID, "OWNER", 0)
	list, err := management.Memberships(ctx, demoClientID, "")
	if err != nil || len(list) != 2 {
		t.Fatalf("companies: %+v %v", list, err)
	}
	user, err := NewAuthRepository(pool, time.Second).UserByID(ctx, demoClientID)
	if err != nil || user.CompanyID != nil {
		t.Fatal("ambiguous company must be null")
	}
	if _, err := apps.Create(ctx, application.Scope{UserID: demoClientID}, draftFields()); !errors.Is(err, application.ErrCompanyRequired) {
		t.Fatalf("ambiguous creation: %v", err)
	}
	first := createDraft(t, apps, draftOwner, draftFields())
	second := createDraft(t, apps, application.Scope{UserID: demoClientID, CompanyID: otherDraftOwner.CompanyID}, draftFields())
	if first.CompanyID == second.CompanyID || second.UserID != demoClientID {
		t.Fatal("company and creator collapsed")
	}
	// Same-company representatives cannot discover the creator's private draft.
	grantData(t, management, otherDraftOwner.UserID, draftOwner.CompanyID, "VIEWER", 0)
	if _, err := apps.Get(ctx, application.Scope{UserID: otherDraftOwner.UserID}, first.ApplicationID); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("foreign private draft visible")
	}
	if _, err := apps.Create(ctx, application.Scope{UserID: otherDraftOwner.UserID, CompanyID: draftOwner.CompanyID}, draftFields()); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("viewer created draft")
	}
	if _, err := NewSubmissionRepository(pool, time.Second).Submit(ctx, application.Scope{UserID: otherDraftOwner.UserID, CompanyID: draftOwner.CompanyID}, first.ApplicationID, 1, nil); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("viewer submitted")
	}
	if _, err := NewSubmissionRepository(pool, time.Second).Submit(ctx, draftOwner, first.ApplicationID, 1, nil); err != nil {
		t.Fatal(err)
	}
	// A submitted application belongs to the company, so a viewer may read it.
	readGET(t, handler, "/api/v1/applications/"+first.ApplicationID+"/decision", tokens[otherDraftOwner.UserID], 200, nil)
	readGET(t, handler, "/api/v1/bank/applications/"+second.ApplicationID, tokens[demoEmployeeID], 404, nil)
	var before string
	if pool.QueryRow(ctx, `SELECT financial_snapshot::text FROM applications WHERE id=$1`, first.ApplicationID).Scan(&before) != nil {
		t.Fatal("snapshot")
	}
	c := dataCommand("membership", draftOwner.CompanyID, 1)
	c.ActorID = demoEmployeeID
	c.TargetID = otherDraftOwner.UserID
	c.Status = "revoked"
	c.MembershipRole = "VIEWER"
	executeData(t, management, c)
	readGET(t, handler, "/api/v1/applications/"+first.ApplicationID+"/decision", tokens[otherDraftOwner.UserID], 404, nil)
	var after string
	_ = pool.QueryRow(ctx, `SELECT financial_snapshot::text FROM applications WHERE id=$1`, first.ApplicationID).Scan(&after)
	if before != after {
		t.Fatal("membership changed immutable snapshot")
	}
	grantData(t, management, otherDraftOwner.UserID, draftOwner.CompanyID, "AUTHORIZED_REPRESENTATIVE", 2)
	readGET(t, handler, "/api/v1/applications/"+first.ApplicationID+"/decision", tokens[otherDraftOwner.UserID], 200, nil)
	if _, err := apps.Get(ctx, application.Scope{UserID: demoClientID, CompanyID: otherDraftOwner.CompanyID}, first.ApplicationID); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("selector rebound application")
	}
	var bankMemberships int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM user_company_memberships WHERE user_id=$1`, demoEmployeeID).Scan(&bankMemberships)
	if bankMemberships != 0 {
		t.Fatal("bank received fictitious membership")
	}
}

func TestContactProfileCorrectionsAndAudit(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	s := managementService(pool)
	c := dataCommand("profile", "", 1)
	c.DisplayName = draftValue("  Новое отображаемое имя  ")
	c.AdditionalEmail = draftValue("extra@example.test")
	p := executeData(t, s, c)
	if p["displayName"] != "Новое отображаемое имя" || p["legalName"] == p["displayName"] || p["version"] != float64(2) {
		t.Fatal(p)
	}
	if _, err := s.Execute(ctx, c); !errors.Is(err, company.ErrVersionConflict) {
		t.Fatal("stale profile update")
	}
	c = dataCommand("contact", draftOwner.CompanyID, 1)
	c.Contact = &company.Contact{Phone: "+79990009999", Email: "contact@example.test", Address: "Новый адрес"}
	before := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-before; _, err := s.Execute(ctx, c); results <- err }()
	}
	close(before)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, company.ErrVersionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("lost update")
	}
	reg := dataCommand("registration", draftOwner.CompanyID, 2)
	reg.Changes = &company.RegistrationChanges{Name: draftValue("Исправленное наименование")}
	created := executeData(t, s, reg)
	requestID := created["id"].(string)
	// Exact replay precedes version validation and cannot create a second request.
	repeated := executeData(t, s, reg)
	if repeated["id"] != requestID {
		t.Fatal("duplicate correction")
	}
	reg.Reason = "Иное тело"
	if _, err := s.Execute(ctx, reg); !errors.Is(err, company.ErrIdempotencyConflict) {
		t.Fatal("changed body accepted")
	}
	reviewCmd := dataCommand("review_registration", draftOwner.CompanyID, 3)
	reviewCmd.ActorID = demoEmployeeID
	reviewCmd.TargetID = requestID
	reviewCmd.Outcome = "approved"
	reviewed := executeData(t, s, reviewCmd)
	if reviewed["status"] != "approved" {
		t.Fatal(reviewed)
	}
	co, err := NewCompanyCreditRepository(pool, time.Second).CompanyByID(ctx, draftOwner.CompanyID)
	if err != nil || co.Name != "Исправленное наименование" || co.RegistrationDataSource != "bank_confirmed" || co.Version != 4 {
		t.Fatalf("company: %+v %v", co, err)
	}
	if _, err := s.Execute(ctx, company.Command{ActorID: demoClientID, CompanyID: draftOwner.CompanyID, Action: "review_registration", Version: 4, TargetID: requestID, Key: uuid.NewString(), Outcome: "rejected", Reason: "Попытка подменить проверяющего"}); !errors.Is(err, company.ErrForbidden) {
		t.Fatal("client reviewed correction")
	}
	var count int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM data_audit_events WHERE actor_id=$1`, demoClientID).Scan(&count)
	if count != 3 {
		t.Fatalf("audit missing: %d", count)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM data_audit_events`); err == nil {
		t.Fatal("audit deletion allowed")
	}
	history, err := s.History(ctx, demoClientID, draftOwner.CompanyID, "registration", false)
	if err != nil || len(history) != 1 || !strings.Contains(string(history[0]), "originalValues") {
		t.Fatalf("history: %s %v", history, err)
	}
}

func TestFinancialDeclarationsDoNotInheritTrust(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	s := managementService(pool)
	catalog := NewCompanyCreditRepository(pool, 5*time.Second)
	original, err := catalog.CompanyByID(ctx, draftOwner.CompanyID)
	if err != nil || !company.FinancialsVerified(original, "demo_bank_feed", 2400000, 1500000) {
		t.Fatal("initial evidence")
	}
	c := dataCommand("financial", draftOwner.CompanyID, 1)
	c.MonthlyRevenueRub = draftValue(int64(10000000))
	c.MonthlyExpensesRub = draftValue(int64(8800000))
	declared := executeData(t, s, c)
	if declared["source"] != "client_declared" || declared["status"] != "pending_review" || declared["verifiedAt"] != nil {
		t.Fatal(declared)
	}
	current, _ := catalog.CompanyByID(ctx, draftOwner.CompanyID)
	if current.Financials.MonthlyRevenueRub != 2400000 || current.FinancialProvenance == nil || current.FinancialProvenance.Status != "verified" || company.FinancialsVerified(current, "demo_bank_feed", 10000000, 8800000) {
		t.Fatal("declaration replaced confirmed data")
	}
	fields := draftFields()
	fields.MonthlyRevenueRub = c.MonthlyRevenueRub
	fields.MonthlyExpensesRub = c.MonthlyExpensesRub
	fields.RequestedAmountRub = draftValue(int64(1400000))
	apps := NewApplicationRepository(pool, 5*time.Second)
	a := createDraft(t, apps, draftOwner, fields)
	sub := NewSubmissionRepository(pool, 5*time.Second)
	if _, err := sub.Submit(ctx, draftOwner, a.ApplicationID, 1, nil); err == nil {
		t.Fatal("changed turnover submitted without statement")
	}
	// Technical readiness permits submit, but never verifies financial contents.
	docRepo := NewDocumentRepository(pool, time.Second)
	statement := document.Metadata{DocumentID: uuid.NewString(), ApplicationID: a.ApplicationID, DocumentType: document.BankStatement, FileName: "statement.pdf", ContentType: "application/pdf", SizeBytes: 10, Checksum: strings.Repeat("a", 64)}
	statement.StorageKey = statement.DocumentID
	scope := document.Scope{UserID: demoClientID, CompanyID: draftOwner.CompanyID, Role: auth.RoleClient}
	if _, err := docRepo.Reserve(ctx, scope, statement); err != nil {
		t.Fatal(err)
	}
	if _, err := docRepo.Activate(ctx, scope, a.ApplicationID, statement.DocumentID); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Submit(ctx, draftOwner, a.ApplicationID, 1, []string{statement.DocumentID}); err != nil {
		t.Fatal(err)
	}
	queue := NewPrecheckQueue(pool, 5*time.Second)
	job := claimPrecheck(t, queue)
	if err := queue.Complete(ctx, job, runPrecheck(t, job)); err != nil {
		t.Fatal(err)
	}
	job = claimScoring(t, queue)
	request := saveScoringRequest(t, queue, job)
	if request.MonthlyRevenue != 10000000 || request.MonthlyExpenses != 8800000 {
		t.Fatal("wrong financial snapshot")
	}
	response := scoringResponse(t)
	response.Score = 90
	response.ApprovedAmount = 1400000
	response.Factors.OperatingCashFlow = 1200000
	if err := queue.CompleteScoring(ctx, job, response); err != nil {
		t.Fatal(err)
	}
	job = claimDecision(t, queue)
	result := evaluateDecision(t, job)
	if result.Status != application.ManualReview {
		t.Fatalf("unverified credit auto-approved: %+v", result)
	}
	if err := queue.CompleteDecision(ctx, job, result); err != nil {
		t.Fatal(err)
	}
	manual := manualCommand(a.ApplicationID, "approved")
	manual.Offer.AmountRub = 1400000
	if _, err := lendingService(pool).Execute(ctx, manual); !errors.Is(err, lending.ErrValidation) {
		t.Fatalf("bank bypassed evidence: %v", err)
	}
	var snapshot string
	_ = pool.QueryRow(ctx, `SELECT financial_snapshot::text FROM applications WHERE id=$1`, a.ApplicationID).Scan(&snapshot)
	reviewCmd := dataCommand("review_financial", draftOwner.CompanyID, 2)
	reviewCmd.ActorID = demoEmployeeID
	reviewCmd.TargetID = declared["id"].(string)
	reviewCmd.Outcome = "approved"
	executeData(t, s, reviewCmd)
	manual.CustomerExplanation = "Подтверждённые обороты позволяют одобрить запрошенные условия."
	_, view := executeLending(t, lendingService(pool), manual, 201)
	if len(view.Reasons) != 1 || view.Reasons[0] != manual.CustomerExplanation {
		t.Fatal("customer explanation missing")
	}
	bank, err := review.NewService(NewReviewRepository(pool, time.Second)).BankAssessment(ctx, a.ApplicationID)
	if err != nil || bank.Scoring == nil || bank.Scoring.Score != 90 || bank.Scoring.Factors.OperatingCashFlow != 1200000 || bank.GoDecision == nil || bank.ManualDecision == nil || bank.ManualDecision.InternalComment == "" {
		t.Fatalf("explainable assessment: %+v %v", bank, err)
	}
	var after string
	_ = pool.QueryRow(ctx, `SELECT financial_snapshot::text FROM applications WHERE id=$1`, a.ApplicationID).Scan(&after)
	if snapshot != after {
		t.Fatal("financial correction rewrote submitted snapshot")
	}
	// A private draft declaration cannot be discovered through bank history.
	privateFields := fields
	privateFields.MonthlyRevenueRub = draftValue(int64(10000001))
	private := createDraft(t, apps, draftOwner, privateFields)
	_ = private
	history, err := s.History(ctx, demoEmployeeID, draftOwner.CompanyID, "financial", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range history {
		if strings.Contains(string(raw), private.ApplicationID) {
			t.Fatal("bank discovered private draft")
		}
	}
}

func TestFailedJobRecoveryPreservesCompletedStages(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	ctx := context.Background()
	job := claimScoring(t, queue)
	saveScoringRequest(t, queue, job)
	// A temporary dependency failure becomes a durable technical failure.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET scoring_attempt_count=max_attempts WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := queue.FailScoring(ctx, job, "scoring_unavailable", 0, true); err != nil {
		t.Fatal(err)
	}
	var version int64
	_ = pool.QueryRow(ctx, `SELECT version FROM applications WHERE id=$1`, id).Scan(&version)
	c := pipeline.RecoveryCommand{ActorID: demoEmployeeID, ApplicationID: id, Key: uuid.NewString(), Reason: "Зависимость восстановлена", Version: version}
	r, err := queue.RecoverJob(ctx, c)
	if err != nil || r.Status != 202 {
		t.Fatalf("recover: %v", err)
	}
	replay, err := queue.RecoverJob(ctx, c)
	if err != nil || string(replay.Body) != string(r.Body) {
		t.Fatal("recovery replay differs")
	}
	changed := c
	changed.Reason = "Другое основание"
	if _, err := queue.RecoverJob(ctx, changed); !errors.Is(err, company.ErrIdempotencyConflict) {
		t.Fatal("recovery hash conflict")
	}
	// Save the actual scoring response, then simulate a decision-stage failure.
	job = claimScoring(t, queue)
	saveScoringRequest(t, queue, job)
	response := scoringResponse(t)
	response.Score = 90
	if err := queue.CompleteScoring(ctx, job, response); err != nil {
		t.Fatal(err)
	}
	decisionJob := claimDecision(t, queue)
	if err := queue.FailDecision(ctx, decisionJob, "decision_storage_failed", 0, false); err != nil {
		t.Fatal(err)
	}
	var saved string
	var scoringAttempts int
	_ = pool.QueryRow(ctx, `SELECT scoring_response::text,scoring_attempt_count FROM jobs WHERE application_id=$1`, id).Scan(&saved, &scoringAttempts)
	_ = pool.QueryRow(ctx, `SELECT version FROM applications WHERE id=$1`, id).Scan(&version)
	c.Version = version
	c.Key = uuid.NewString()
	if _, err := queue.RecoverJob(ctx, c); err != nil {
		t.Fatal(err)
	}
	if repeated, err := queue.ClaimScoring(ctx, time.Minute); err != nil || repeated != nil {
		t.Fatal("successful scoring was scheduled again")
	}
	if err := queue.CompleteDecision(ctx, decisionJob, evaluateDecision(t, decisionJob)); !errors.Is(err, pipeline.ErrLeaseLost) {
		t.Fatal("expired lease committed after operator recovery")
	}
	decisionJob = claimDecision(t, queue)
	if err := queue.CompleteDecision(ctx, decisionJob, evaluateDecision(t, decisionJob)); err != nil {
		t.Fatal(err)
	}
	var after string
	var attempts, decisions, offers, events int
	if pool.QueryRow(ctx, `SELECT scoring_response::text,scoring_attempt_count,(SELECT count(*) FROM credit_decisions WHERE application_id=$1),(SELECT count(*) FROM offers WHERE application_id=$1),(SELECT count(*) FROM application_events WHERE application_id=$1 AND event_type='PROCESSING_RECOVERY_REQUESTED') FROM jobs WHERE application_id=$1`, id).Scan(&after, &attempts, &decisions, &offers, &events) != nil {
		t.Fatal("artifacts")
	}
	if saved != after || attempts != scoringAttempts || decisions != 1 || offers != 1 || events != 2 {
		t.Fatal("recovery duplicated persisted stages")
	}
}

func TestMembershipRevocationWaitsForTransaction(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	s := managementService(pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var allowed bool
	if tx.QueryRow(ctx, `SELECT lock_company_access($1,$2,true)`, demoClientID, draftOwner.CompanyID).Scan(&allowed) != nil || !allowed {
		t.Fatal("membership lock")
	}
	c := dataCommand("membership", draftOwner.CompanyID, 1)
	c.ActorID = demoEmployeeID
	c.TargetID = demoClientID
	c.Status = "revoked"
	c.MembershipRole = "OWNER"
	done := make(chan error, 1)
	go func() { _, err := s.Execute(ctx, c); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("revocation did not wait: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	if tx.Commit(ctx) != nil {
		t.Fatal("commit")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := NewApplicationRepository(pool, time.Second).Create(ctx, draftOwner, draftFields()); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("revoked membership wrote data")
	}
}

func TestManagementHTTPValidationRolesAndRevokedJWT(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	tokenService, err := auth.NewTokens(strings.Repeat("t", 32), "governance-http", "governance-http")
	if err != nil {
		t.Fatal(err)
	}
	authRepo := NewAuthRepository(pool, time.Second)
	middleware := auth.NewMiddleware(tokenService, authRepo, time.Second)
	management := httpapi.NewManagementHandler(managementService(pool), middleware)
	catalog := NewCompanyCreditRepository(pool, time.Second)
	creditHandler := httpapi.NewCreditHandler(company.NewService(catalog), credit.NewService(catalog, catalog), middleware)
	appHandler := httpapi.NewApplicationHandler(application.NewService(NewApplicationRepository(pool, time.Second), catalog, catalog), middleware)
	handler := httpapi.NewHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, nil, management.Register, creditHandler.Register, appHandler.Register)
	tokens := map[string]string{}
	for _, id := range []string{demoClientID, demoEmployeeID, otherDraftOwner.UserID} {
		u, e := authRepo.UserByID(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		token, _, e := tokenService.Issue(u)
		if e != nil {
			t.Fatal(e)
		}
		tokens[id] = token
	}
	request := func(method, path, body, actor string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if actor != "" {
			req.Header.Set("Authorization", "Bearer "+tokens[actor])
		}
		req.Header.Set("Idempotency-Key", uuid.NewString())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: %d want %d %s", method, path, w.Code, want, w.Body)
		}
		return w
	}
	request("GET", "/api/v1/me/companies", "", "", 401)
	request("GET", "/api/v1/me/companies", "", demoEmployeeID, 403)
	request("GET", "/api/v1/me/companies", "", demoClientID, 200)
	path := "/api/v1/companies/" + draftOwner.CompanyID
	for _, body := range []string{`null`, `{}`, `{"version":1,"inn":"1234567890"}`, `{"version":1,"contact":null}`, `{"version":1,"contact":{"phone":"+79990000001","email":"bad","address":"A"}}`, `{"version":1,"contact":{"phone":"+79990000001","email":"x@example.test","address":"A","inn":"123"}}`, `{"version":1,"version":1,"contact":{}}`, "{\"version\":1,\"contact\":{\"phone\":\"+79990000001\",\"email\":\"x@example.test\",\"address\":\"\xff\"}}"} {
		request("PATCH", path, body, demoClientID, 400)
	}
	for _, body := range []string{`{"version":1,"phone":"+79991111111"}`, `{"version":1,"role":"BANK_EMPLOYEE"}`, `{"version":1,"legalName":"Injected"}`, `{"version":1,"displayName":null}`, `{"version":1,"additionalEmail":"not-an-email"}`} {
		request("PATCH", "/api/v1/me", body, demoClientID, 400)
	}
	request("PATCH", path, `{"version":1,"contact":{"phone":"+79990000001","email":"x@example.test","address":"A"}}`, otherDraftOwner.UserID, 404)
	grantData(t, managementService(pool), demoClientID, otherDraftOwner.CompanyID, "OWNER", 0)
	request("POST", "/api/v1/applications", `{}`, demoClientID, 409)
	for _, id := range []string{draftOwner.CompanyID, otherDraftOwner.CompanyID} {
		r := request("POST", "/api/v1/applications", `{"companyId":"`+id+`"}`, demoClientID, 201)
		var a application.Application
		_ = json.Unmarshal(r.Body.Bytes(), &a)
		if a.CompanyID != id {
			t.Fatal("wrong selected company")
		}
	}
	// Foreign selectors never grant access even when the caller supplies a valid UUID.
	request("POST", "/api/v1/applications", `{"companyId":"10000000-0000-4000-8000-000000000003"}`, demoClientID, 404)
	c := dataCommand("membership", draftOwner.CompanyID, 1)
	c.ActorID = demoEmployeeID
	c.TargetID = demoClientID
	c.MembershipRole = "OWNER"
	c.Status = "revoked"
	executeData(t, managementService(pool), c)
	request("GET", path, "", demoClientID, 403)
	request("POST", "/api/v1/applications", `{"companyId":"`+draftOwner.CompanyID+`"}`, demoClientID, 404)
}

func TestRecoveryBoundedAndRoleProtected(t *testing.T) {
	pool, queue, id := scoringFixture(t)
	ctx := context.Background()
	if _, err := queue.RecoverJob(ctx, pipeline.RecoveryCommand{ActorID: demoClientID, ApplicationID: id, Key: uuid.NewString(), Version: 3, Reason: "Подмена оператора"}); !errors.Is(err, company.ErrForbidden) {
		t.Fatal("client restarted job")
	}
	for attempt := 0; attempt < 4; attempt++ {
		job := claimScoring(t, queue)
		saveScoringRequest(t, queue, job)
		if err := queue.FailScoring(ctx, job, "scoring_unavailable", 0, false); err != nil {
			t.Fatal(err)
		}
		var version int64
		if pool.QueryRow(ctx, `SELECT version FROM applications WHERE id=$1`, id).Scan(&version) != nil {
			t.Fatal("version")
		}
		_, err := queue.RecoverJob(ctx, pipeline.RecoveryCommand{ActorID: demoEmployeeID, ApplicationID: id, Key: uuid.NewString(), Version: version, Reason: "Контролируемое восстановление"})
		if attempt < 3 && err != nil {
			t.Fatal(err)
		}
		if attempt == 3 && !errors.Is(err, company.ErrInvalidStatus) {
			t.Fatal("unlimited recovery allowed")
		}
	}
}

func TestMultiCompanyDocumentsRemainBoundToApplication(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	s := managementService(pool)
	grantData(t, s, demoClientID, otherDraftOwner.CompanyID, "OWNER", 0)
	apps := NewApplicationRepository(pool, time.Second)
	a := createDraft(t, apps, draftOwner, application.Fields{})
	b := createDraft(t, apps, application.Scope{UserID: demoClientID, CompanyID: otherDraftOwner.CompanyID}, application.Fields{})
	repo := NewDocumentRepository(pool, time.Second)
	scope := document.Scope{UserID: demoClientID, Role: auth.RoleClient}
	d := document.Metadata{DocumentID: uuid.NewString(), ApplicationID: a.ApplicationID, DocumentType: document.BankStatement, FileName: "private.pdf", ContentType: "application/pdf", SizeBytes: 10, Checksum: strings.Repeat("b", 64)}
	d.StorageKey = d.DocumentID
	if _, err := repo.Reserve(ctx, scope, d); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Activate(ctx, scope, a.ApplicationID, d.DocumentID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, scope, b.ApplicationID, d.DocumentID); !errors.Is(err, document.ErrNotFound) {
		t.Fatal("cross-company document rebound")
	}
	if _, err := repo.Get(ctx, document.Scope{UserID: demoEmployeeID, Role: auth.RoleBankEmployee}, a.ApplicationID, d.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatal("bank accessed draft document")
	}
	c := dataCommand("membership", draftOwner.CompanyID, 1)
	c.ActorID = demoEmployeeID
	c.TargetID = demoClientID
	c.Status = "revoked"
	c.MembershipRole = "OWNER"
	executeData(t, s, c)
	if _, err := repo.Get(ctx, scope, a.ApplicationID, d.DocumentID); !errors.Is(err, document.ErrApplicationNotFound) {
		t.Fatal("revoked member accessed document")
	}
	if _, err := pool.Exec(ctx, `UPDATE documents SET company_id=$2 WHERE id=$1`, d.DocumentID, otherDraftOwner.CompanyID); err == nil {
		t.Fatal("document-company FK bypass")
	}
}

func TestRepresentativeOfferActionSeparatesCreatorAndActor(t *testing.T) {
	pool, id := lendingFixture(t, 90)
	ctx := context.Background()
	s := managementService(pool)
	grantData(t, s, otherDraftOwner.UserID, draftOwner.CompanyID, "AUTHORIZED_REPRESENTATIVE", 0)
	view, err := review.NewService(NewReviewRepository(pool, time.Second)).Decision(ctx, application.Scope{UserID: otherDraftOwner.UserID}, id)
	if err != nil || view.Offer == nil {
		t.Fatal("representative cannot read submitted company decision", err)
	}
	c := offerCommand(id, view.Offer.OfferID, lending.Accept)
	c.ActorID = otherDraftOwner.UserID
	c.CompanyID = ""
	response, signed := executeLending(t, lendingService(pool), c, 200)
	if signed.Status != application.Signed {
		t.Fatal("company representative action failed")
	}
	var creator, actor string
	if pool.QueryRow(ctx, `SELECT owner_id::text,signed_by::text FROM applications WHERE id=$1`, id).Scan(&creator, &actor) != nil || creator != demoClientID || actor != otherDraftOwner.UserID {
		t.Fatal("creator overwritten by acting representative")
	}
	replay, err := lendingService(pool).Execute(ctx, c)
	if err != nil || string(replay.Body) != string(response.Body) {
		t.Fatal("representative replay")
	}
	revoke := dataCommand("membership", draftOwner.CompanyID, 1)
	revoke.ActorID = demoEmployeeID
	revoke.TargetID = otherDraftOwner.UserID
	revoke.MembershipRole = "AUTHORIZED_REPRESENTATIVE"
	revoke.Status = "revoked"
	executeData(t, s, revoke)
	if _, err := lendingService(pool).Execute(ctx, c); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("revocation did not block idempotency replay")
	}
}

func TestNewConfirmationSupersedesHistoricalValues(t *testing.T) {
	pool := authTestPool(t)
	ctx := context.Background()
	s := managementService(pool)
	c := dataCommand("financial", draftOwner.CompanyID, 1)
	c.MonthlyRevenueRub = draftValue(int64(600000))
	c.MonthlyExpensesRub = draftValue(int64(450000))
	declaration := executeData(t, s, c)
	c = dataCommand("review_financial", draftOwner.CompanyID, 2)
	c.ActorID = demoEmployeeID
	c.TargetID = declaration["id"].(string)
	c.Outcome = "approved"
	executeData(t, s, c)
	companyNow, err := NewCompanyCreditRepository(pool, time.Second).CompanyByID(ctx, draftOwner.CompanyID)
	if err != nil || companyNow.FinancialProvenance == nil || companyNow.FinancialProvenance.MonthlyRevenueRub != 600000 {
		t.Fatal("latest confirmed values not selected", err)
	}
	// The old 2.4m record remains in history but cannot be reused as current proof.
	a := createDraft(t, NewApplicationRepository(pool, time.Second), draftOwner, draftFields())
	if _, err := NewSubmissionRepository(pool, time.Second).Submit(ctx, draftOwner, a.ApplicationID, 1, nil); err == nil {
		t.Fatal("historical higher turnover bypassed new confirmation")
	}
	var count int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM company_financial_declarations WHERE company_id=$1 AND status='verified'`, draftOwner.CompanyID).Scan(&count)
	if count != 2 {
		t.Fatal("history was erased")
	}
}
