package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/httpapi"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/review"
	"smart-credit-backend/internal/scoring"
)

func readFingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var hash string
	err := pool.QueryRow(context.Background(), `SELECT md5(jsonb_build_object(
 'applications',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM applications a),
 'jobs',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM jobs j),
 'decisions',(SELECT jsonb_agg(to_jsonb(d) ORDER BY id) FROM credit_decisions d),
 'offers',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM offers o),
 'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM application_events e))::text)`).Scan(&hash)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
func readHTTPFixture(t *testing.T, pool *pgxpool.Pool) (http.Handler, map[string]string) {
	t.Helper()
	tokens, err := auth.NewTokens(strings.Repeat("t", 32), "read-pg-test", "read-pg-test")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewAuthRepository(pool, time.Second)
	h := httpapi.NewReviewHandler(review.NewService(NewReviewRepository(pool, time.Second)), auth.NewMiddleware(tokens, repo, time.Second))
	jwt := map[string]string{}
	for _, id := range []string{demoClientID, demoEmployeeID, otherDraftOwner.UserID} {
		u, err := repo.UserByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		token, _, err := tokens.Issue(u)
		if err != nil {
			t.Fatal(err)
		}
		jwt[id] = token
	}
	return httpapi.NewHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, nil, h.Register), jwt
}
func readGET(t *testing.T, h http.Handler, path, token string, want int, dst any) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body)
	}
	if dst != nil && json.Unmarshal(w.Body.Bytes(), dst) != nil {
		t.Fatal("invalid response JSON")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("response caching")
	}
}
func exportReadContract(t *testing.T, schema string, payload any) {
	t.Helper()
	dir := os.Getenv("TEST_READ_CONTRACT_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(struct {
		Schema  string `json:"schema"`
		Payload any    `json:"payload"`
	}{schema, payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, schema+"-"+uuid.NewString()+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadAPIFullPipelineAllOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name             string
		score            int
		recommendation   string
		amount, expenses int64
		want             application.Status
	}{
		{"approved", 90, "approved", 1500000, 1500000, application.Approved},
		{"rejected", 0, "rejected", 0, 1500000, application.Rejected},
		{"counter_offer", 90, "counter_offer", 500000, 2100000, application.CounterOffer},
		{"manual_review", 65, "approved", 1500000, 1500000, application.ManualReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fields := draftFields()
			fields.MonthlyExpensesRub = draftValue(tc.expenses)
			pool, submit, app := submissionFixture(t, fields)
			if _, err := submit.Submit(ctx, draftOwner, app.ApplicationID, 1, nil); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, decisionResponseJSON(tc.score, tc.recommendation, tc.amount, 2400000-tc.expenses))
			}))
			defer server.Close()
			cancel, done := startDecisionWorker(t, NewPrecheckQueue(pool, time.Second), pipeline.WithScoring(scoringClientForDecision(t, server.URL)))
			waitScoringState(t, pool, app.ApplicationID, "completed")
			cancel()
			<-done
			assertDecisionArtifacts(t, pool, app.ApplicationID, tc.want, tc.score)
			h, tokens := readHTTPFixture(t, pool)
			before := readFingerprint(t, pool)
			var client review.CreditDecision
			readGET(t, h, "/api/v1/applications/"+app.ApplicationID+"/decision", tokens[demoClientID], 200, &client)
			if client.Status != tc.want || client.Version != 4 || client.Score == nil || *client.Score != tc.score || client.RiskLevel == nil || client.Processing != nil || len(client.Reasons) == 0 {
				t.Fatalf("client result %+v", client)
			}
			if tc.want == application.ManualReview {
				if client.Decision != nil || client.AssessedAt != nil || client.Offer != nil {
					t.Fatal("fake final manual-review decision")
				}
			} else if client.Decision == nil || *client.Decision != string(tc.want) || client.AssessedAt == nil || client.ReviewedBy != nil {
				t.Fatal("persisted final decision missing")
			}
			if tc.want == application.Approved || tc.want == application.CounterOffer {
				if client.Offer == nil || client.Offer.AmountRub != tc.amount || client.Offer.MonthlyPaymentRub < 1 || client.Offer.Status != "active" || client.Offer.ExpiresAt != nil || client.Offer.AnnualRatePercent != "22.0000" {
					t.Fatalf("offer %+v", client.Offer)
				}
			} else if client.Offer != nil {
				t.Fatal("unexpected offer")
			}
			exportReadContract(t, "CreditDecision", client)
			readGET(t, h, "/api/v1/applications/"+app.ApplicationID+"/decision", tokens[otherDraftOwner.UserID], 404, nil)
			readGET(t, h, "/api/v1/applications/"+app.ApplicationID+"/decision", tokens[demoEmployeeID], 403, nil)
			var detail review.BankDetail
			var assessment review.BankAssessment
			var page review.BankPage
			readGET(t, h, "/api/v1/bank/applications/"+app.ApplicationID, tokens[demoEmployeeID], 200, &detail)
			readGET(t, h, "/api/v1/bank/applications/"+app.ApplicationID+"/assessment", tokens[demoEmployeeID], 200, &assessment)
			readGET(t, h, "/api/v1/bank/applications", tokens[demoEmployeeID], 200, &page)
			if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ApplicationID != app.ApplicationID || detail.Status != tc.want || detail.ManualReviewRequired != (tc.want == application.ManualReview) {
				t.Fatal("bank list/detail")
			}
			if assessment.CompanyCheck == nil || assessment.CreditHistory == nil || assessment.Scoring == nil || assessment.Scoring.Score != tc.score || assessment.RiskLevel == nil || *assessment.RiskLevel != *client.RiskLevel || assessment.Processing != nil || assessment.ManualDecision != nil {
				t.Fatalf("assessment %+v", assessment)
			}
			var storedScoring, storedAssessment []byte
			var storedAt time.Time
			if err := pool.QueryRow(ctx, `SELECT scoring_response,scoring_received_at,assessment_result FROM jobs WHERE application_id=$1`, app.ApplicationID).Scan(&storedScoring, &storedAt, &storedAssessment); err != nil {
				t.Fatal(err)
			}
			var python scoring.Response
			var goResult review.Assessment
			if json.Unmarshal(storedScoring, &python) != nil || json.Unmarshal(storedAssessment, &goResult) != nil {
				t.Fatal("persisted assessment invalid")
			}
			if assessment.Scoring.Decision != tc.recommendation || assessment.Scoring.ApprovedAmountRub != tc.amount ||
				!reflect.DeepEqual(assessment.Scoring.Reasons, python.Reasons) || !reflect.DeepEqual(assessment.Scoring.Factors, &python.Factors) || !assessment.Scoring.AssessedAt.Equal(storedAt) {
				t.Fatalf("bank projection differs from saved Python output: %+v", assessment.Scoring)
			}
			if assessment.GoDecision == nil || assessment.GoDecision.Decision != tc.want || assessment.GoDecision.RiskLevel != goResult.Result.RiskLevel ||
				!reflect.DeepEqual(assessment.GoDecision.Reasons, goResult.Result.Reasons) || !assessment.GoDecision.AssessedAt.Equal(goResult.AssessedAt) {
				t.Fatalf("saved Go policy is confused with Python recommendation: %+v", assessment.GoDecision)
			}
			if tc.want == application.ManualReview {
				if len(assessment.ManualReviewReasons) == 0 || !reflect.DeepEqual(detail.AvailableActions, []string{"decide"}) || len(assessment.Recommendations) != 0 {
					t.Fatal("manual review bank view")
				}
			} else if len(assessment.Recommendations) != 1 || assessment.Recommendations[0].Decision != string(tc.want) || len(detail.AvailableActions) != 0 {
				t.Fatal("policy recommendations or bank actions")
			}
			exportReadContract(t, "BankAssessment", assessment)
			exportReadContract(t, "BankApplicationDetail", detail)
			exportReadContract(t, "BankApplicationPage", page)
			if after := readFingerprint(t, pool); after != before || calls.Load() != 1 {
				t.Fatal("GET changed data or repeated scoring")
			}
			// Live catalog/company edits do not rewrite historical bank information.
			if _, err := pool.Exec(ctx, `UPDATE companies SET name='Changed live company',monthly_revenue=1 WHERE id=$1`, draftOwner.CompanyID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE credit_products SET name='Changed live product',annual_rate_percent=99 WHERE code='turnover'`); err != nil {
				t.Fatal(err)
			}
			var frozen review.BankDetail
			readGET(t, h, "/api/v1/bank/applications/"+app.ApplicationID, tokens[demoEmployeeID], 200, &frozen)
			if !reflect.DeepEqual(detail, frozen) {
				t.Fatal("bank detail used mutable company/product data")
			}
			if tc.want == application.Approved {
				for _, status := range []application.Status{application.Signed, application.Closed} {
					offerStatus := "accepted"
					if _, err := pool.Exec(ctx, `UPDATE offers SET status=$2 WHERE id=$1`, client.Offer.OfferID, offerStatus); err != nil {
						t.Fatal(err)
					}
					if _, err := pool.Exec(ctx, `UPDATE applications SET status=$2,accepted_offer_id=$3,signed_by=owner_id,signed_at=clock_timestamp() WHERE id=$1`, app.ApplicationID, string(status), client.Offer.OfferID); err != nil {
						t.Fatal(err)
					}
					var terminal review.CreditDecision
					readGET(t, h, "/api/v1/applications/"+app.ApplicationID+"/decision", tokens[demoClientID], 200, &terminal)
					if terminal.Status != status || terminal.Decision == nil || *terminal.Decision != "approved" || terminal.Offer == nil || terminal.Offer.Status != "accepted" {
						t.Fatal("terminal state lost decision or actual offer status")
					}
					exportReadContract(t, "CreditDecision", terminal)
				}
			}
		})
	}
}

func TestReadAPIPendingStagesAndTechnicalFailure(t *testing.T) {
	ctx := context.Background()
	pool, submit, app := submissionFixture(t, draftFields())
	h, tokens := readHTTPFixture(t, pool)
	read := func(wantStage, wantState string, wantChecks, wantScoring bool) {
		t.Helper()
		before := readFingerprint(t, pool)
		var client review.CreditDecision
		readGET(t, h, "/api/v1/applications/"+app.ApplicationID+"/decision", tokens[demoClientID], 200, &client)
		if client.Decision != nil || client.Offer != nil || client.AssessedAt != nil || (client.Score != nil) != wantScoring {
			t.Fatalf("pending result %+v", client)
		}
		if wantStage == "" {
			if client.Processing != nil {
				t.Fatal("draft processing")
			}
		} else if client.Processing == nil || client.Processing.Stage != wantStage || client.Processing.State != wantState {
			t.Fatalf("processing %+v", client.Processing)
		}
		exportReadContract(t, "CreditDecision", client)
		if wantStage != "" {
			var a review.BankAssessment
			readGET(t, h, "/api/v1/bank/applications/"+app.ApplicationID+"/assessment", tokens[demoEmployeeID], 200, &a)
			if (a.CompanyCheck != nil) != wantChecks || (a.CreditHistory != nil) != wantChecks || (a.Scoring != nil) != wantScoring || a.RiskLevel != nil {
				t.Fatal("missing results were manufactured")
			}
			exportReadContract(t, "BankAssessment", a)
		}
		if readFingerprint(t, pool) != before {
			t.Fatal("GET mutated pending records")
		}
	}
	read("", "", false, false)
	for _, suffix := range []string{"", "/assessment"} {
		readGET(t, h, "/api/v1/bank/applications/"+app.ApplicationID+suffix, tokens[demoEmployeeID], 404, nil)
	}
	var empty review.BankPage
	readGET(t, h, "/api/v1/bank/applications", tokens[demoEmployeeID], 200, &empty)
	if empty.Total != 0 || len(empty.Items) != 0 {
		t.Fatal("draft count leaked")
	}
	exportReadContract(t, "BankApplicationPage", empty)
	if _, err := submit.Submit(ctx, draftOwner, app.ApplicationID, 1, nil); err != nil {
		t.Fatal(err)
	}
	read("prechecks", "waiting", false, false)
	queue := NewPrecheckQueue(pool, time.Second)
	job := claimPrecheck(t, queue)
	read("prechecks", "running", false, false)
	if err := queue.Complete(ctx, job, runPrecheck(t, job)); err != nil {
		t.Fatal(err)
	}
	read("scoring", "waiting", true, false)
	job = claimScoring(t, queue)
	read("scoring", "running", true, false)
	if err := queue.FailScoring(ctx, job, "scoring_dns", time.Second, true); err != nil {
		t.Fatal(err)
	}
	read("scoring", "retrying", true, false)
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE application_id=$1`, app.ApplicationID); err != nil {
		t.Fatal(err)
	}
	job = claimScoring(t, queue)
	if err := queue.FailScoring(ctx, job, "scoring_invalid_response", 0, false); err != nil {
		t.Fatal(err)
	}
	read("scoring", "failed", true, false)
	// Resume only in test data to inspect a saved score before policy evaluation.
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='awaiting_scoring',finished_at=NULL,available_at=clock_timestamp() WHERE application_id=$1`, app.ApplicationID); err != nil {
		t.Fatal(err)
	}
	job = claimScoring(t, queue)
	saveScoringRequest(t, queue, job)
	if err := queue.CompleteScoring(ctx, job, scoringResponse(t)); err != nil {
		t.Fatal(err)
	}
	read("decision", "waiting", true, true)
	claimDecision(t, queue)
	read("decision", "running", true, true)
}

func TestBankListPrivacyFiltersSortingAndPagination(t *testing.T) {
	ctx := context.Background()
	pool, apps := applicationFixture(t)
	if _, err := pool.Exec(ctx, `UPDATE companies SET financial_data_source='demo_bank_feed'`); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []application.Scope{draftOwner, otherDraftOwner} {
		createDraft(t, apps, scope, application.Fields{})
	}
	ids := []string{}
	for range 3 {
		for _, scope := range []application.Scope{draftOwner, otherDraftOwner} {
			a := createDraft(t, apps, scope, draftFields())
			ids = append(ids, a.ApplicationID)
			if _, err := pool.Exec(ctx, `UPDATE applications SET created_at='2026-10-24T09:00:00Z' WHERE id=$1`, a.ApplicationID); err != nil {
				t.Fatal(err)
			}
			if _, err := NewSubmissionRepository(pool, time.Second).Submit(ctx, scope, a.ApplicationID, 1, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	s := review.NewService(NewReviewRepository(pool, time.Second))
	before := readFingerprint(t, pool)
	page, err := s.BankList(ctx, review.Filter{Limit: 2, Offset: 1})
	if err != nil || page.Total != 6 || len(page.Items) != 2 || page.Items[0].ApplicationID != ids[1] || page.Items[1].ApplicationID != ids[2] {
		t.Fatalf("pagination %+v %v", page, err)
	}
	empty, err := s.BankList(ctx, review.Filter{Limit: 2, Offset: 100})
	if err != nil || empty.Total != 6 || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatal("empty page lost total")
	}
	for _, scope := range []application.Scope{draftOwner, otherDraftOwner} {
		filtered, err := s.BankList(ctx, review.Filter{Limit: 100, CompanyID: &scope.CompanyID, Status: draftValue(application.Submitted)})
		if err != nil || filtered.Total != 3 || len(filtered.Items) != 3 {
			t.Fatal("company/status filter")
		}
		for _, a := range filtered.Items {
			if a.Company.ID != scope.CompanyID || a.Status == application.Draft {
				t.Fatal("filter leaked company/draft")
			}
		}
	}
	h, tokens := readHTTPFixture(t, pool)
	readGET(t, h, "/api/v1/bank/applications?status=draft", tokens[demoEmployeeID], 400, nil)
	for _, path := range []string{"/api/v1/bank/applications", "/api/v1/bank/applications/" + ids[0], "/api/v1/bank/applications/" + ids[0] + "/assessment"} {
		readGET(t, h, path, tokens[demoClientID], 403, nil)
	}
	for _, suffix := range []string{"", "/assessment"} {
		readGET(t, h, "/api/v1/bank/applications/"+uuid.NewString()+suffix, tokens[demoEmployeeID], 404, nil)
	}
	if readFingerprint(t, pool) != before {
		t.Fatal("bank GET changed DB")
	}
	exportReadContract(t, "BankApplicationPage", page)
	if _, err := NewReviewRepository(pool, time.Second).Read(ctx, &application.Scope{}, ids[0]); !errors.Is(err, application.ErrNotFound) {
		t.Fatal("invalid owner scope accepted")
	}
}
