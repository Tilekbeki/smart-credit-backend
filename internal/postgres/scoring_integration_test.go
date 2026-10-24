package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/review"
	"smart-credit-backend/internal/scoring"
)

// This opt-in test calls the actual Python logreg_v1 HTTP service. It never
// starts an httptest scorer or replaces its recommendation or score.
func TestRealScoringPipeline(t *testing.T) {
	url := os.Getenv("TEST_SCORING_URL")
	if url == "" {
		t.Skip("TEST_SCORING_URL is not set; real Python scoring is not requested")
	}
	client, err := scoring.NewClient(url, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(url + "/health")
	if err != nil {
		t.Fatalf("real scoring health unavailable: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("real scoring health is not OK")
	}
	for _, tc := range []struct {
		name                      string
		revenue, expenses, amount int64
		score                     int
		pythonDecision            string
		pythonAmount, goAmount    int64
		want                      application.Status
	}{
		{"approved", 2_400_000, 1_500_000, 1_500_000, 97, "approved", 1_500_000, 1_500_000, application.Approved},
		{"low risk financial rejection", 600_000, 450_000, 100_000, 85, "rejected", 0, 0, application.Rejected},
		{"counter offer", 600_000, 300_000, 1_500_000, 96, "counter_offer", 809_633, 641_064, application.CounterOffer},
		{"manual review", 10_000_000, 8_800_000, 1_400_000, 78, "counter_offer", 1_190_000, 0, application.ManualReview},
		{"high risk", 600_000, 570_000, 100_000, 7, "rejected", 0, 0, application.Rejected},
		{"mandatory margin with affordable payment", 10_000_000, 9_100_000, 1_400_000, 71, "rejected", 0, 0, application.Rejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := draftFields()
			fields.MonthlyRevenueRub = draftValue(tc.revenue)
			fields.MonthlyExpensesRub = draftValue(tc.expenses)
			fields.RequestedAmountRub = draftValue(tc.amount)
			pool, submit, app := submissionFixture(t, fields)
			if _, err := submit.Submit(context.Background(), draftOwner, app.ApplicationID, 1, nil); err != nil {
				t.Fatal(err)
			}
			queue := NewPrecheckQueue(pool, time.Second)
			cancel, done := startDecisionWorker(t, queue, pipeline.WithScoring(client))
			waitScoringState(t, pool, app.ApplicationID, "completed")
			cancel()
			<-done
			assertDecisionArtifacts(t, pool, app.ApplicationID, tc.want, tc.score)
			var requestJSON, responseJSON []byte
			var attempts int
			if err := pool.QueryRow(context.Background(), `SELECT scoring_request,scoring_response,scoring_attempt_count FROM jobs WHERE application_id=$1`, app.ApplicationID).Scan(&requestJSON, &responseJSON, &attempts); err != nil {
				t.Fatal(err)
			}
			var input scoring.Request
			if json.Unmarshal(requestJSON, &input) != nil || input.MonthlyRevenue != tc.revenue || input.MonthlyExpenses != tc.expenses || input.ExistingDebt != 500_000 || input.MonthlyDebtPayments != 90_000 || input.OverdueCount != 0 || attempts != 1 {
				t.Fatal("saved request or number of scoring calls differs")
			}
			result, err := scoring.DecodeResponse(responseJSON)
			if err != nil || result.Score != tc.score || result.Decision != tc.pythonDecision || result.ApprovedAmount != tc.pythonAmount {
				t.Fatalf("real logreg_v1 result changed; review model/artifact compatibility: %+v %v", result, err)
			}
			h, tokens := readHTTPFixture(t, pool)
			var view review.CreditDecision
			readGET(t, h, "/api/v1/applications/"+app.ApplicationID+"/decision", tokens[demoClientID], 200, &view)
			if view.Status != tc.want || view.Score == nil || *view.Score != tc.score {
				t.Fatalf("GET decision differs: %+v", view)
			}
			if tc.goAmount > 0 && (view.Offer == nil || view.Offer.AmountRub != tc.goAmount) {
				t.Fatalf("backend offer differs: %+v", view.Offer)
			}
			before := readFingerprint(t, pool)
			var assessment review.BankAssessment
			readGET(t, h, "/api/v1/bank/applications/"+app.ApplicationID+"/assessment", tokens[demoEmployeeID], 200, &assessment)
			if assessment.Scoring == nil || assessment.Scoring.Decision != result.Decision || assessment.Scoring.Score != result.Score ||
				assessment.Scoring.ApprovedAmountRub != result.ApprovedAmount || !reflect.DeepEqual(assessment.Scoring.Reasons, result.Reasons) ||
				!reflect.DeepEqual(assessment.Scoring.Factors, &result.Factors) || assessment.GoDecision == nil || assessment.GoDecision.Decision != tc.want {
				t.Fatalf("bank assessment differs from persisted real Python/Go results: %+v", assessment)
			}
			if readFingerprint(t, pool) != before {
				t.Fatal("assessment GET mutated persisted scoring or decisions")
			}
			if job, err := NewPrecheckQueue(pool, time.Second).ClaimScoring(context.Background(), time.Minute); err != nil || job != nil {
				t.Fatal("saved scoring scheduled again after repository recreation")
			}
			if readFingerprint(t, pool) != before {
				t.Fatal("completed result changed after restart check")
			}
			exportReadContract(t, "CreditDecision", view)
			t.Logf("logreg_v1: revenue=%d expenses=%d debt=500000 payments=90000 overdue=0 amount=%d term=12 python=%s score=%d recommended=%d risk=%s Go=%s offer=%d", tc.revenue, tc.expenses, tc.amount, result.Decision, result.Score, result.ApprovedAmount, *view.RiskLevel, view.Status, tc.goAmount)
		})
	}
}
