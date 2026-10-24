package pipeline

import (
	"context"
	"errors"
	"time"

	"smart-credit-backend/internal/decision"
)

func WithDecisionEngine(engine DecisionEngine) WorkerOption {
	return func(worker *Worker) error {
		queue, ok := worker.queue.(DecisionQueue)
		if !ok || engine == nil {
			return errors.New("decision engine requires a compatible durable queue and engine")
		}
		worker.decisionQueue, worker.decisionEngine = queue, engine
		return nil
	}
}

func (w *Worker) processDecision(ctx context.Context, job Job) {
	attemptCtx, cancel := context.WithTimeout(ctx, w.config.AttemptTimeout)
	result, err := w.runDecision(job)
	if err == nil {
		err = attemptCtx.Err()
	}
	if err == nil {
		err = w.decisionQueue.CompleteDecision(attemptCtx, job, result)
	}
	cancel()
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
		return
	}
	code, retryable := "decision_failed", true
	if errors.Is(err, decision.ErrInvalidData) {
		code, retryable = "decision_invalid_data", false
	} else if errors.Is(err, ErrStorage) {
		code = "decision_storage_failed"
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = "decision_timeout"
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, min(5*time.Second, w.config.AttemptTimeout))
	failErr := w.decisionQueue.FailDecision(cleanupCtx, job, code, retryDelay(w.config.RetryBaseDelay, job.Attempt), retryable)
	cleanupCancel()
	if failErr != nil && !errors.Is(failErr, ErrLeaseLost) && ctx.Err() == nil {
		w.logger.Warn("decision retry could not be persisted", "job_id", job.ID)
	}
}

func (w *Worker) runDecision(job Job) (result decision.Result, err error) {
	defer func() {
		if recover() != nil {
			result, err = decision.Result{}, errors.New("decision processor failed")
		}
	}()
	if job.ApplicationID != job.DecisionInput.ApplicationID {
		return decision.Result{}, decision.ErrInvalidData
	}
	return w.decisionEngine.Evaluate(job.DecisionInput)
}
