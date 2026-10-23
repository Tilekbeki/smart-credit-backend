package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"smart-credit-backend/internal/scoring"
)

func storedCreditHistory() []byte {
	return []byte(`{"schemaVersion":1,"creditHistory":{"existingDebtRub":123,"existingMonthlyPaymentsRub":45,"overdueCount":2}}`)
}

func TestBuildScoringRequestUsesFrozenApplicationAndPersistedPrechecks(t *testing.T) {
	snapshot := simulatorSnapshot()
	snapshot.Company.Profile.Financials.ExistingDebtRub = 999
	snapshot.Company.Profile.Financials.ExistingMonthlyPaymentsRub = 999
	snapshot.Company.Profile.Financials.OverdueCount = 99
	request, err := BuildScoringRequest(snapshot, storedCreditHistory())
	if err != nil {
		t.Fatal(err)
	}
	if request.ApplicationID != snapshot.ApplicationID || request.MonthlyRevenue != *snapshot.Fields.MonthlyRevenueRub ||
		request.MonthlyExpenses != *snapshot.Fields.MonthlyExpensesRub || request.RequestedAmount != *snapshot.Fields.RequestedAmountRub ||
		request.TermMonths != *snapshot.Fields.TermMonths || request.ExistingDebt != 123 || request.MonthlyDebtPayments != 45 || request.OverdueCount != 2 {
		t.Fatalf("wrong immutable input mapping: %+v", request)
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 8 {
		t.Fatal("request must contain exactly the schema's eight fields")
	}
	for _, name := range []string{"application_id", "monthly_revenue", "monthly_expenses", "existing_debt", "monthly_debt_payments", "overdue_count", "requested_amount", "term_months"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestBuildScoringRequestRejectsCorruptPersistedData(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `not json`,
		`{"schemaVersion":2,"creditHistory":{"existingDebtRub":0,"existingMonthlyPaymentsRub":0,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingMonthlyPaymentsRub":0,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":null,"existingMonthlyPaymentsRub":0,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":0,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":0,"existingMonthlyPaymentsRub":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":-1,"existingMonthlyPaymentsRub":0,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":1000000000001,"existingMonthlyPaymentsRub":0,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":0,"existingMonthlyPaymentsRub":"0","overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":0,"existingMonthlyPaymentsRub":-1,"overdueCount":0}}`,
		`{"schemaVersion":1,"creditHistory":{"existingDebtRub":0,"existingMonthlyPaymentsRub":0,"overdueCount":101}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := BuildScoringRequest(simulatorSnapshot(), []byte(raw)); !errors.Is(err, ErrInvalidScoringData) {
				t.Fatalf("invalid persisted history accepted: %v", err)
			}
		})
	}
	for _, change := range []func(*FinancialSnapshot){
		func(s *FinancialSnapshot) { s.Fields.MonthlyRevenueRub = nil },
		func(s *FinancialSnapshot) { s.Fields.MonthlyExpensesRub = nil },
		func(s *FinancialSnapshot) { s.Fields.RequestedAmountRub = nil },
		func(s *FinancialSnapshot) { s.Fields.TermMonths = nil },
		func(s *FinancialSnapshot) { s.SchemaVersion = 2 },
	} {
		snapshot := simulatorSnapshot()
		change(&snapshot)
		if _, err := BuildScoringRequest(snapshot, storedCreditHistory()); !errors.Is(err, ErrInvalidScoringData) {
			t.Fatal("invalid snapshot accepted")
		}
	}
	if _, err := BuildScoringRequest(simulatorSnapshot(), []byte(`{"schemaVersion":1,"creditHistory":{"existingDebtRub":0,"existingMonthlyPaymentsRub":0,"overdueCount":0}}`)); err != nil {
		t.Fatal("legitimate zero financials rejected")
	}
}

type scoringQueueStub struct {
	*workerQueueStub
	job                *Job
	request            *scoring.Request
	completedScoring   int
	failedScoring      int
	code               string
	retryable          bool
	saveErr            error
	completeScoringErr error
}

func (q *scoringQueueStub) ClaimScoring(context.Context, time.Duration) (*Job, error) {
	job := q.job
	q.job = nil
	return job, nil
}
func (q *scoringQueueStub) SaveScoringRequest(_ context.Context, _ Job, request scoring.Request) error {
	q.request = &request
	return q.saveErr
}
func (q *scoringQueueStub) CompleteScoring(context.Context, Job, scoring.Response) error {
	q.completedScoring++
	return q.completeScoringErr
}
func (q *scoringQueueStub) FailScoring(_ context.Context, _ Job, code string, _ time.Duration, retryable bool) error {
	q.failedScoring++
	q.code = code
	q.retryable = retryable
	return nil
}

type scorerFunc func(context.Context, scoring.Request) (scoring.Response, error)

func (f scorerFunc) Score(ctx context.Context, r scoring.Request) (scoring.Response, error) {
	return f(ctx, r)
}

func TestWorkerScoringValidationAndFencedFailures(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		prechecks                []byte
		saveErr, errorOnComplete error
		panic                    bool
		wantCode                 string
		wantRetry                bool
	}{
		{name: "success", prechecks: storedCreditHistory()},
		{name: "invalid data", prechecks: []byte(`{}`), wantCode: "scoring_invalid_data"},
		{name: "storage retry", prechecks: storedCreditHistory(), saveErr: ErrStorage, wantCode: "scoring_storage_failed", wantRetry: true},
		{name: "stale lease before HTTP", prechecks: storedCreditHistory(), saveErr: ErrLeaseLost},
		{name: "stale completion", prechecks: storedCreditHistory(), errorOnComplete: ErrLeaseLost},
		{name: "processor panic", prechecks: storedCreditHistory(), panic: true, wantCode: "scoring_invalid_data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queue := &scoringQueueStub{workerQueueStub: newWorkerQueueStub(), saveErr: tc.saveErr, completeScoringErr: tc.errorOnComplete}
			calls := 0
			client := scorerFunc(func(context.Context, scoring.Request) (scoring.Response, error) {
				calls++
				if tc.panic {
					panic("private data")
				}
				return scoring.Response{Decision: "approved", Score: 0, Reasons: []string{"synthetic"}}, nil
			})
			worker, err := NewWorker(queue, NewSimulator(), workerTestConfig(), nil, WithScoring(client))
			if err != nil {
				t.Fatal(err)
			}
			worker.processScoring(context.Background(), Job{ApplicationID: simulatorSnapshot().ApplicationID, Snapshot: simulatorSnapshot(), PrecheckResults: tc.prechecks, Stage: ScoringStage})
			if tc.wantCode != "" {
				if queue.failedScoring != 1 || queue.code != tc.wantCode || queue.retryable != tc.wantRetry {
					t.Fatalf("unexpected failure: %+v", queue)
				}
			} else if queue.failedScoring != 0 {
				t.Fatal("successful or fenced attempt scheduled failure")
			}
			if (tc.saveErr != nil || tc.name == "invalid data") && calls != 0 {
				t.Fatal("HTTP called after validation/lease failure")
			}
		})
	}
	if _, err := NewWorker(newWorkerQueueStub(), NewSimulator(), workerTestConfig(), nil, WithScoring(scorerFunc(nil))); err == nil {
		t.Fatal("queue without scoring support accepted")
	}
}
