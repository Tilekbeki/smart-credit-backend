package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"smart-credit-backend/internal/decision"
)

type decisionQueueStub struct {
	*workerQueueStub
	job                               *Job
	completedDecision, failedDecision int
	completeErr                       error
	code                              string
	retryable                         bool
	delay                             time.Duration
}

func (q *decisionQueueStub) ClaimDecision(context.Context, time.Duration) (*Job, error) {
	job := q.job
	q.job = nil
	return job, nil
}
func (q *decisionQueueStub) CompleteDecision(context.Context, Job, decision.Result) error {
	q.completedDecision++
	return q.completeErr
}
func (q *decisionQueueStub) FailDecision(_ context.Context, _ Job, code string, delay time.Duration, retryable bool) error {
	q.failedDecision++
	q.code = code
	q.delay = delay
	q.retryable = retryable
	return nil
}

type decisionEngineFunc func(decision.Input) (decision.Result, error)

func (f decisionEngineFunc) Evaluate(input decision.Input) (decision.Result, error) { return f(input) }

func TestWorkerDecisionFailuresAndLeaseFencing(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		engineErr, completeErr error
		panic                  bool
		wantCode               string
		retry                  bool
	}{
		{name: "success"},
		{name: "invalid input", engineErr: decision.ErrInvalidData, wantCode: "decision_invalid_data"},
		{name: "transient storage", completeErr: ErrStorage, wantCode: "decision_storage_failed", retry: true},
		{name: "expired lease", completeErr: ErrLeaseLost},
		{name: "unexpected error", engineErr: errors.New("private details"), wantCode: "decision_failed", retry: true},
		{name: "panic", panic: true, wantCode: "decision_failed", retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queue := &decisionQueueStub{workerQueueStub: newWorkerQueueStub(), completeErr: tc.completeErr}
			engine := decisionEngineFunc(func(decision.Input) (decision.Result, error) {
				if tc.panic {
					panic("private details")
				}
				return decision.Result{}, tc.engineErr
			})
			worker, err := NewWorker(queue, NewSimulator(), workerTestConfig(), nil, WithDecisionEngine(engine))
			if err != nil {
				t.Fatal(err)
			}
			worker.processDecision(context.Background(), Job{ApplicationID: "test-app", DecisionInput: decision.Input{ApplicationID: "test-app"}, Stage: DecisionStage, Attempt: 3})
			if tc.wantCode != "" {
				if queue.failedDecision != 1 || queue.code != tc.wantCode || queue.retryable != tc.retry || queue.delay != 4*workerTestConfig().RetryBaseDelay {
					t.Fatalf("wrong failure: %+v", queue)
				}
			} else if queue.failedDecision != 0 {
				t.Fatal("success/lease loss wrote failure")
			}
		})
	}
	if _, err := NewWorker(newWorkerQueueStub(), NewSimulator(), workerTestConfig(), nil, WithDecisionEngine(decision.NewEngine())); err == nil {
		t.Fatal("queue without decision support accepted")
	}
}

func TestWorkerDecisionChecksIdentityAndCancellation(t *testing.T) {
	queue := &decisionQueueStub{workerQueueStub: newWorkerQueueStub()}
	calls := 0
	worker, err := NewWorker(queue, NewSimulator(), workerTestConfig(), nil, WithDecisionEngine(decisionEngineFunc(func(decision.Input) (decision.Result, error) { calls++; return decision.Result{}, nil })))
	if err != nil {
		t.Fatal(err)
	}
	worker.processDecision(context.Background(), Job{ApplicationID: "one", DecisionInput: decision.Input{ApplicationID: "other"}, Stage: DecisionStage})
	if calls != 0 || queue.code != "decision_invalid_data" || queue.retryable {
		t.Fatal("mismatched application processed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	queue.failedDecision = 0
	worker.processDecision(ctx, Job{ApplicationID: "one", DecisionInput: decision.Input{ApplicationID: "one"}, Stage: DecisionStage})
	if queue.completedDecision != 0 || queue.failedDecision != 0 {
		t.Fatal("shutdown must leave lease to recover")
	}
}

func TestWorkerClaimsAllStagesWithoutStarvation(t *testing.T) {
	queue := &decisionQueueStub{workerQueueStub: newWorkerQueueStub(), job: &Job{ID: "decision", Stage: DecisionStage}}
	queue.jobs <- Job{ID: "prechecks"}
	worker, err := NewWorker(queue, NewSimulator(), workerTestConfig(), nil, WithDecisionEngine(decision.NewEngine()))
	if err != nil {
		t.Fatal(err)
	}
	first, err := worker.claimNext(context.Background(), 1)
	if err != nil || first == nil || first.Stage != DecisionStage {
		t.Fatal("decision stage starved by preliminary checks")
	}
	second, err := worker.claimNext(context.Background(), 0)
	if err != nil || second == nil || second.ID != "prechecks" {
		t.Fatal("precheck stage starved")
	}
}
