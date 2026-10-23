package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"smart-credit-backend/internal/scoring"
)

var ErrInvalidScoringData = errors.New("invalid persisted scoring inputs")

// BuildScoringRequest deliberately uses application fields frozen at submit and
// the persisted credit-history result, never the live company profile.
func BuildScoringRequest(snapshot FinancialSnapshot, prechecks []byte) (scoring.Request, error) {
	if !validPrecheckSnapshot(snapshot) {
		return scoring.Request{}, ErrInvalidScoringData
	}
	var stored struct {
		SchemaVersion int `json:"schemaVersion"`
		CreditHistory struct {
			Debt     *int64 `json:"existingDebtRub"`
			Payments *int64 `json:"existingMonthlyPaymentsRub"`
			Overdue  *int   `json:"overdueCount"`
		} `json:"creditHistory"`
	}
	if err := json.Unmarshal(prechecks, &stored); err != nil || stored.SchemaVersion != 1 ||
		stored.CreditHistory.Debt == nil || stored.CreditHistory.Payments == nil || stored.CreditHistory.Overdue == nil {
		return scoring.Request{}, ErrInvalidScoringData
	}
	fields, history := snapshot.Fields, stored.CreditHistory
	request := scoring.Request{
		ApplicationID: snapshot.ApplicationID, MonthlyRevenue: *fields.MonthlyRevenueRub,
		MonthlyExpenses: *fields.MonthlyExpensesRub, ExistingDebt: *history.Debt,
		MonthlyDebtPayments: *history.Payments, OverdueCount: *history.Overdue,
		RequestedAmount: *fields.RequestedAmountRub, TermMonths: *fields.TermMonths,
	}
	if err := scoring.ValidateRequest(request); err != nil {
		return scoring.Request{}, ErrInvalidScoringData
	}
	return request, nil
}

type WorkerOption func(*Worker) error

func WithScoring(client Scorer) WorkerOption {
	return func(worker *Worker) error {
		queue, ok := worker.queue.(ScoringQueue)
		if !ok || client == nil {
			return errors.New("scoring requires a compatible durable queue and client")
		}
		worker.scoringQueue, worker.scorer = queue, client
		return nil
	}
}

func (w *Worker) processScoring(ctx context.Context, job Job) {
	attemptCtx, cancel := context.WithTimeout(ctx, w.config.AttemptTimeout)
	request, err := BuildScoringRequest(job.Snapshot, job.PrecheckResults)
	if err == nil && job.ApplicationID != request.ApplicationID {
		err = ErrInvalidScoringData
	}
	if err == nil {
		err = w.scoringQueue.SaveScoringRequest(attemptCtx, job, request)
	}
	if err == nil {
		var response scoring.Response
		response, err = w.runScorer(attemptCtx, request)
		if err == nil {
			err = attemptCtx.Err()
		}
		if err == nil {
			err = w.scoringQueue.CompleteScoring(attemptCtx, job, response)
		}
	}
	cancel()
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
		return
	}
	code, retryable := "scoring_"+scoring.ErrorCode(err), scoring.IsRetryable(err)
	if errors.Is(err, ErrInvalidScoringData) {
		code, retryable = "scoring_invalid_data", false
	} else if errors.Is(err, ErrStorage) {
		code, retryable = "scoring_storage_failed", true
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, retryable = "scoring_timeout", true
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, min(5*time.Second, w.config.AttemptTimeout))
	failErr := w.scoringQueue.FailScoring(cleanupCtx, job, code, retryDelay(w.config.RetryBaseDelay, job.Attempt), retryable)
	cleanupCancel()
	if failErr != nil && !errors.Is(failErr, ErrLeaseLost) && ctx.Err() == nil {
		w.logger.Warn("scoring retry could not be persisted", "job_id", job.ID)
	}
}

func (w *Worker) runScorer(ctx context.Context, request scoring.Request) (response scoring.Response, err error) {
	defer func() {
		if recover() != nil {
			response, err = scoring.Response{}, ErrInvalidScoringData
		}
	}()
	response, err = w.scorer.Score(ctx, request)
	if err == nil {
		err = scoring.ValidateResponse(response)
	}
	return response, err
}
