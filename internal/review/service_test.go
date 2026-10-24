package review

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/scoring"
)

type memoryRepository struct {
	record Record
	err    error
	calls  int
}

func (m *memoryRepository) Read(_ context.Context, _ *application.Scope, _ string) (Record, error) {
	m.calls++
	return m.record, m.err
}
func (m *memoryRepository) List(_ context.Context, _ Filter) ([]Record, int64, error) {
	m.calls++
	return []Record{m.record}, 1, m.err
}
func pointer[T any](v T) *T { return &v }

func TestProcessingStatesAndNullableResults(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name         string
		status       application.Status
		job          *Job
		stage, state string
	}{
		{"draft", application.Draft, nil, "", ""},
		{"submitted", application.Submitted, nil, "prechecks", "waiting"},
		{"waiting scoring", application.Processing, &Job{Status: pipeline.AwaitingScoring, Stage: "prechecks"}, "scoring", "waiting"},
		{"waiting decision", application.Processing, &Job{Status: pipeline.AwaitingDecision, Stage: "scoring"}, "decision", "waiting"},
		{"running", application.Processing, &Job{Status: "running", Stage: "scoring", ReadAt: now, LeaseUntil: pointer(now.Add(time.Minute))}, "scoring", "running"},
		{"expired lease", application.Processing, &Job{Status: "running", Stage: "decision", ReadAt: now, LeaseUntil: pointer(now.Add(-time.Minute))}, "decision", "retrying"},
		{"retry", application.Processing, &Job{Status: pipeline.AwaitingScoring, Stage: "scoring", ReadAt: now, AvailableAt: now.Add(time.Minute), HasError: true}, "scoring", "retrying"},
		{"failure", application.Processing, &Job{Status: "failed", Stage: "scoring", HasError: true}, "scoring", "failed"},
		{"manual", application.ManualReview, nil, "", ""},
		{"final", application.Rejected, nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memoryRepository{record: Record{Application: application.Application{ApplicationID: "test", Status: tc.status, Version: 3}, Job: tc.job}}
			v, err := NewService(repo).Decision(context.Background(), application.Scope{}, "test")
			if err != nil || v.Decision != nil || v.Score != nil || v.RiskLevel != nil || v.Offer != nil || v.AssessedAt != nil || v.ReviewedBy != nil {
				t.Fatalf("manufactured result: %+v %v", v, err)
			}
			if tc.stage == "" {
				if v.Processing != nil {
					t.Fatal("unexpected processing")
				}
			} else if v.Processing == nil || v.Processing.Stage != tc.stage || v.Processing.State != tc.state || v.Processing.Message == nil {
				t.Fatalf("processing: %+v", v.Processing)
			}
			if tc.name == "retry" && (v.Processing.RetryAt == nil || !v.Processing.RetryAt.Equal(now.Add(time.Minute))) {
				t.Fatal("scheduled retry missing")
			}
			if tc.status == application.ManualReview && len(v.Reasons) == 0 {
				t.Fatal("manual review explanation absent")
			}
			a, err := NewService(repo).BankAssessment(context.Background(), "test")
			if tc.status == application.Draft {
				if !errors.Is(err, application.ErrNotFound) {
					t.Fatal("bank draft visible")
				}
				return
			}
			if err != nil || a.CompanyCheck != nil || a.CreditHistory != nil || a.Scoring != nil || a.RiskLevel != nil || a.ManualDecision != nil || a.Recommendations == nil || a.ManualReviewReasons == nil {
				t.Fatalf("missing results must be null: %+v %v", a, err)
			}
		})
	}
}

func TestSavedZeroScoreAndAssessmentAreNotRecomputed(t *testing.T) {
	now := time.Now().UTC()
	repo := &memoryRepository{record: Record{Application: application.Application{Status: application.Processing, Version: 3}, Job: &Job{Status: pipeline.AwaitingDecision, Scoring: &scoring.Response{Score: 0}, ScoringAt: &now}}}
	v, err := NewService(repo).Decision(context.Background(), application.Scope{}, "test")
	if err != nil || v.Score == nil || *v.Score != 0 || v.RiskLevel != nil {
		t.Fatalf("zero score: %+v %v", v, err)
	}
	a, err := NewService(repo).BankAssessment(context.Background(), "test")
	if err != nil || a.Scoring == nil || a.Scoring.Score != 0 || !a.Scoring.AssessedAt.Equal(now) {
		t.Fatal("stored scoring absent")
	}
	if repo.calls != 2 {
		t.Fatal("extra repository/scoring requests")
	}
}

func TestManualReviewUsesPersistedPolicyWithoutFinalDecision(t *testing.T) {
	result := decision.Result{Status: application.ManualReview, Score: 62, RiskLevel: "MEDIUM", Reasons: []string{"Требуется ручное рассмотрение."}, ManualReviewReasons: []decision.Reason{{Code: "medium_risk", Message: "Средний риск."}}, PolicyVersion: decision.PolicyVersion}
	repo := &memoryRepository{record: Record{Application: application.Application{Status: application.ManualReview}, Job: &Job{Assessment: &Assessment{SchemaVersion: 1, Source: "automatic", AssessedAt: time.Now().UTC(), Result: result}}}}
	s := NewService(repo)
	v, err := s.Decision(context.Background(), application.Scope{}, "test")
	if err != nil || v.Decision != nil || v.Offer != nil || v.AssessedAt != nil || v.Processing != nil || v.Score == nil || *v.Score != 62 || !reflect.DeepEqual(v.Reasons, result.Reasons) {
		t.Fatalf("manual result %+v %v", v, err)
	}
	a, err := s.BankAssessment(context.Background(), "test")
	if err != nil || !reflect.DeepEqual(a.ManualReviewReasons, result.ManualReviewReasons) || len(a.Recommendations) != 0 {
		t.Fatal("manual reasons/recommendations")
	}
}

func TestBankViewsUseFrozenFieldsAndActions(t *testing.T) {
	now := time.Now().UTC()
	f := application.Fields{ProductID: pointer("turnover"), Purpose: pointer("stock"), RequestedAmountRub: pointer(int64(150000)), TermMonths: pointer(12), MonthlyRevenueRub: pointer(int64(200000)), MonthlyExpensesRub: pointer(int64(100000))}
	snapshot := pipeline.FinancialSnapshot{SchemaVersion: 1, ApplicationID: "a", UserID: "u", CompanyID: "c", Fields: f, Company: pipeline.CompanySnapshot{Profile: company.Company{ID: "c", Name: "Frozen company", INN: "123"}}}
	product := credit.Product{ID: "turnover", Name: "Frozen product", AnnualRatePercent: json.Number("22")}
	raw, _ := json.Marshal(snapshot)
	terms, _ := json.Marshal(product)
	for _, status := range []application.Status{application.Submitted, application.Processing, application.ManualReview, application.Approved, application.CounterOffer, application.Rejected, application.Signed, application.Closed} {
		repo := &memoryRepository{record: Record{Application: application.Application{ApplicationID: "a", UserID: "u", CompanyID: "c", Status: status, SubmittedAt: &now, Fields: application.Fields{RequestedAmountRub: pointer(int64(999))}}, FinancialSnapshot: raw, ProductSnapshot: terms}}
		s := NewService(repo)
		d, err := s.BankDetail(context.Background(), "a")
		if err != nil || *d.RequestedAmountRub != 150000 || d.Company.Name != "Frozen company" || d.Product.Name != "Frozen product" || d.ProductTerms.AnnualRatePercent != "22" {
			t.Fatalf("frozen detail %+v %v", d, err)
		}
		if d.ManualReviewRequired != (status == application.ManualReview) || !reflect.DeepEqual(d.AvailableActions, bankActions(status)) {
			t.Fatal("bank actions")
		}
		page, err := s.BankList(context.Background(), Filter{Limit: 20})
		if err != nil || len(page.Items) != 1 || page.Items[0].RequestedAmountRub != 150000 {
			t.Fatal("frozen list")
		}
	}
}

func TestFilterValidationAndErrors(t *testing.T) {
	repo := &memoryRepository{err: application.ErrStorage}
	s := NewService(repo)
	for _, f := range []Filter{{Limit: 0}, {Limit: 101}, {Limit: 1, Offset: -1}, {Limit: 1, Status: pointer(application.Draft)}, {Limit: 1, Status: pointer(application.Status("bogus"))}, {Limit: 1, CompanyID: pointer("' OR true--")}} {
		if _, err := s.BankList(context.Background(), f); !errors.Is(err, application.ErrInvalidInput) {
			t.Fatal("bad filter accepted")
		}
	}
	if repo.calls != 0 {
		t.Fatal("invalid filters reached storage")
	}
	if _, err := s.BankDetail(context.Background(), "a"); !errors.Is(err, application.ErrStorage) {
		t.Fatal("storage error hidden")
	}
}

func TestBankAssessmentKeepsPythonAndGoResultsDistinct(t *testing.T) {
	assessedAt := time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC)
	goAt := assessedAt.Add(time.Second)
	response := scoring.Response{Decision: "approved", Score: 65, ApprovedAmount: 1_234_567,
		Reasons: []string{"PYTHON_ONLY_REASON"}, Factors: scoring.Factors{
			OperatingCashFlow: 321_123.25, OperatingMargin: 0.23, EstimatedNewMonthlyPayment: 89_456.75,
			DebtServiceShare: pointer(0.37), ExistingDebtToAnnualRevenue: pointer(0.45), OverdueCount: 2,
		}}
	result := decision.Result{Status: application.ManualReview, Score: 65, RiskLevel: "MEDIUM",
		Reasons: []string{"GO_POLICY_REASON"}, ManualReviewReasons: []decision.Reason{{Code: "medium_risk", Message: "Средний риск."}}}
	repo := &memoryRepository{record: Record{Application: application.Application{ApplicationID: "saved", Status: application.ManualReview, Version: 4},
		Job: &Job{Scoring: &response, ScoringAt: &assessedAt, Assessment: &Assessment{SchemaVersion: 1, Source: "automatic", Result: result, AssessedAt: goAt}}}}
	view, err := NewService(repo).BankAssessment(context.Background(), "saved")
	if err != nil || view.Scoring == nil || view.GoDecision == nil {
		t.Fatalf("assessment %+v %v", view, err)
	}
	if view.Scoring.Decision != response.Decision || view.Scoring.ApprovedAmountRub != response.ApprovedAmount ||
		view.Scoring.Score != response.Score || !view.Scoring.AssessedAt.Equal(assessedAt) ||
		!reflect.DeepEqual(view.Scoring.Reasons, response.Reasons) || !reflect.DeepEqual(view.Scoring.Factors, &response.Factors) {
		t.Fatalf("Python result changed in projection: %+v", view.Scoring)
	}
	if view.GoDecision.Decision != result.Status || view.GoDecision.RiskLevel != result.RiskLevel ||
		!reflect.DeepEqual(view.GoDecision.Reasons, result.Reasons) || !view.GoDecision.AssessedAt.Equal(goAt) || len(view.Recommendations) != 0 {
		t.Fatalf("Go policy confused with Python recommendation: %+v", view)
	}
	if repo.calls != 1 {
		t.Fatal("assessment must be one persisted read")
	}
	client, err := NewService(repo).Decision(context.Background(), application.Scope{}, "saved")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(client)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PYTHON_ONLY_REASON", "operating_cash_flow", "approvedAmountRub", "goDecision", "manualReviewReasons"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("bank-only assessment leaked %s", forbidden)
		}
	}
	if repo.calls != 2 {
		t.Fatal("client decision must not recalculate scoring")
	}
}

func TestBankAssessmentMissingScoringAndNullableFactors(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name        string
		job         *Job
		wantScoring bool
	}{
		{"missing job", nil, false},
		{"missing scoring", &Job{ScoringAt: &now}, false},
		{"missing timestamp", &Job{Scoring: &scoring.Response{Score: 0}}, false},
		{"nullable ratios", &Job{ScoringAt: &now, Scoring: &scoring.Response{Score: 0, Decision: "rejected", Reasons: []string{"Сохранённая причина."}, Factors: scoring.Factors{}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memoryRepository{record: Record{Application: application.Application{Status: application.Processing}, Job: tc.job}}
			got, err := NewService(repo).BankAssessment(context.Background(), "saved")
			if err != nil || (got.Scoring != nil) != tc.wantScoring || got.GoDecision != nil {
				t.Fatalf("unexpected absent assessment %+v %v", got, err)
			}
			body, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantScoring {
				if got.Scoring.Score != 0 || got.Scoring.ApprovedAmountRub != 0 || got.Scoring.Factors == nil ||
					!strings.Contains(string(body), `"debt_service_share":null`) || !strings.Contains(string(body), `"existing_debt_to_annual_revenue":null`) {
					t.Fatal("zero and nullable factors were fabricated or omitted", string(body))
				}
			} else if !strings.Contains(string(body), `"scoring":null`) {
				t.Fatal("missing scoring must remain null")
			}
		})
	}
}
