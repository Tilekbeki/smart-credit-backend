package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type Config struct {
	Concurrency    int
	PollInterval   time.Duration
	LeaseDuration  time.Duration
	AttemptTimeout time.Duration
	RetryBaseDelay time.Duration
}

func DefaultConfig() Config {
	return Config{Concurrency: 2, PollInterval: 500 * time.Millisecond,
		LeaseDuration: 30 * time.Second, AttemptTimeout: 10 * time.Second, RetryBaseDelay: time.Second}
}

type Worker struct {
	queue          Queue
	processor      Processor
	config         Config
	logger         *slog.Logger
	scoringQueue   ScoringQueue
	scorer         Scorer
	decisionQueue  DecisionQueue
	decisionEngine DecisionEngine
}

func NewWorker(queue Queue, processor Processor, config Config, logger *slog.Logger, options ...WorkerOption) (*Worker, error) {
	if queue == nil || processor == nil || config.Concurrency < 1 || config.Concurrency > 64 ||
		config.PollInterval <= 0 || config.LeaseDuration < time.Millisecond ||
		config.AttemptTimeout <= 0 || config.AttemptTimeout >= config.LeaseDuration ||
		config.RetryBaseDelay <= 0 || config.RetryBaseDelay > 30*time.Second {
		return nil, errors.New("invalid pipeline worker configuration")
	}
	if logger == nil {
		logger = slog.Default()
	}
	worker := &Worker{queue: queue, processor: processor, config: config, logger: logger}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("invalid worker option")
		}
		if err := option(worker); err != nil {
			return nil, err
		}
	}
	return worker, nil
}

// Run owns a fixed number of processing goroutines. PostgreSQL remains the only
// authoritative queue; cancellation leaves in-flight leases for safe recovery.
func (w *Worker) Run(ctx context.Context) error {
	var workers sync.WaitGroup
	for range w.config.Concurrency {
		workers.Go(func() { w.loop(ctx) })
	}
	workers.Wait()
	return nil
}

func (w *Worker) loop(ctx context.Context) {
	nextStage := 0
	for ctx.Err() == nil {
		job, err := w.claimNext(ctx, nextStage)
		nextStage++
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.logger.Warn("pipeline queue temporarily unavailable")
			if !waitForPoll(ctx, w.config.PollInterval) {
				return
			}
			continue
		}
		if job == nil {
			if !waitForPoll(ctx, w.config.PollInterval) {
				return
			}
			continue
		}
		if job.Stage == DecisionStage {
			w.processDecision(ctx, *job)
		} else if job.Stage == ScoringStage {
			w.processScoring(ctx, *job)
		} else {
			w.process(ctx, *job)
		}
	}
}

func (w *Worker) claimNext(ctx context.Context, next int) (*Job, error) {
	claims := []func(context.Context, time.Duration) (*Job, error){w.queue.Claim}
	if w.scoringQueue != nil {
		claims = append(claims, w.scoringQueue.ClaimScoring)
	}
	if w.decisionQueue != nil {
		claims = append(claims, w.decisionQueue.ClaimDecision)
	}
	for offset := range len(claims) {
		job, err := claims[(next+offset)%len(claims)](ctx, w.config.LeaseDuration)
		if err != nil || job != nil {
			return job, err
		}
	}
	return nil, nil
}

func (w *Worker) process(ctx context.Context, job Job) {
	attemptCtx, cancel := context.WithTimeout(ctx, w.config.AttemptTimeout)
	var results Results
	var err error
	if job.ApplicationID != job.Snapshot.ApplicationID {
		err = ErrInvalidSnapshot
	} else {
		results, err = w.runProcessor(attemptCtx, job.Snapshot)
	}
	if err == nil {
		err = attemptCtx.Err()
	}
	if err == nil {
		err = w.queue.Complete(attemptCtx, job, results)
	}
	cancel()
	if ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
		return
	}
	if err != nil {
		code := "prechecks_failed"
		if errors.Is(err, ErrInvalidSnapshot) {
			code = "invalid_snapshot"
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = "prechecks_timeout"
		}
		// Completion may have consumed the attempt context. A fresh, bounded
		// context lets us persist a retry; a failed commit is still fenced by SQL.
		cleanupCtx, cleanupCancel := context.WithTimeout(ctx, min(5*time.Second, w.config.AttemptTimeout))
		failErr := w.queue.Fail(cleanupCtx, job, code, retryDelay(w.config.RetryBaseDelay, job.Attempt))
		cleanupCancel()
		if failErr != nil && !errors.Is(failErr, ErrLeaseLost) && ctx.Err() == nil {
			w.logger.Warn("precheck retry could not be persisted", "job_id", job.ID)
		}
	}
}

func (w *Worker) runProcessor(ctx context.Context, snapshot FinancialSnapshot) (results Results, err error) {
	defer func() {
		if recover() != nil {
			results, err = Results{}, errors.New("preliminary check processor failed")
		}
	}()
	if !validPrecheckSnapshot(snapshot) {
		return Results{}, ErrInvalidSnapshot
	}
	return w.processor.Run(ctx, snapshot)
}

func retryDelay(base time.Duration, attempt int) time.Duration {
	delay := min(base, 30*time.Second)
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay = min(delay*2, 30*time.Second)
	}
	return delay
}

func waitForPoll(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
