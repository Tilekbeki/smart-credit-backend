package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type workerQueueStub struct {
	jobs        chan Job
	completed   chan Job
	failed      chan workerFailure
	claimErrors atomic.Int32
	completeErr error
}

type workerFailure struct {
	job   Job
	code  string
	delay time.Duration
	err   error
}

func (q *workerQueueStub) Claim(context.Context, time.Duration) (*Job, error) {
	if q.claimErrors.Add(-1) >= 0 {
		return nil, ErrStorage
	}
	select {
	case job := <-q.jobs:
		return &job, nil
	default:
		return nil, nil
	}
}

func (q *workerQueueStub) Complete(_ context.Context, job Job, _ Results) error {
	q.completed <- job
	return q.completeErr
}

func (q *workerQueueStub) Fail(ctx context.Context, job Job, code string, delay time.Duration) error {
	q.failed <- workerFailure{job, code, delay, ctx.Err()}
	return nil
}

type processorFunc func(context.Context, FinancialSnapshot) (Results, error)

func (f processorFunc) Run(ctx context.Context, snapshot FinancialSnapshot) (Results, error) {
	return f(ctx, snapshot)
}

func newWorkerQueueStub() *workerQueueStub {
	return &workerQueueStub{jobs: make(chan Job, 32), completed: make(chan Job, 32), failed: make(chan workerFailure, 32)}
}

func workerTestConfig() Config {
	return Config{Concurrency: 2, PollInterval: time.Millisecond, LeaseDuration: time.Second,
		AttemptTimeout: 50 * time.Millisecond, RetryBaseDelay: time.Millisecond}
}

func startTestWorker(t *testing.T, queue Queue, processor Processor, config Config) (context.CancelFunc, <-chan error) {
	t.Helper()
	worker, err := NewWorker(queue, processor, config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop")
		}
	})
	return cancel, done
}

func TestWorkerBoundedConcurrencyAndDatabaseRecovery(t *testing.T) {
	queue := newWorkerQueueStub()
	queue.claimErrors.Store(2)
	for index := range 20 {
		queue.jobs <- Job{ID: fmt.Sprint(index), ApplicationID: simulatorSnapshot().ApplicationID, Attempt: 1, Snapshot: simulatorSnapshot()}
	}
	var active, peak atomic.Int32
	started := make(chan struct{}, 20)
	release := make(chan struct{})
	processor := processorFunc(func(ctx context.Context, _ FinancialSnapshot) (Results, error) {
		value := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); value > old && !peak.CompareAndSwap(old, value); old = peak.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
			return Results{SchemaVersion: 1}, nil
		case <-ctx.Done():
			return Results{}, ctx.Err()
		}
	})
	config := workerTestConfig()
	config.AttemptTimeout = 500 * time.Millisecond
	startTestWorker(t, queue, processor, config)
	for range config.Concurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("worker failed to recover after a database error")
		}
	}
	if peak.Load() != int32(config.Concurrency) {
		t.Fatalf("expected %d workers, got %d", config.Concurrency, peak.Load())
	}
	close(release)
	for range 20 {
		select {
		case <-queue.completed:
		case <-time.After(time.Second):
			t.Fatal("job was not completed")
		}
	}
	if peak.Load() > int32(config.Concurrency) {
		t.Fatal("processor concurrency exceeded the limit")
	}
}

func TestWorkerErrorsPersistSanitizedRetries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		processor processorFunc
		snapshot  FinancialSnapshot
		code      string
	}{
		{"technical", func(context.Context, FinancialSnapshot) (Results, error) {
			return Results{}, errors.New("private database or file details")
		}, simulatorSnapshot(), "prechecks_failed"},
		{"invalid snapshot", func(context.Context, FinancialSnapshot) (Results, error) { panic("must not call") }, FinancialSnapshot{}, "invalid_snapshot"},
		{"panic", func(context.Context, FinancialSnapshot) (Results, error) { panic("private details") }, simulatorSnapshot(), "prechecks_failed"},
		{"timeout", func(ctx context.Context, _ FinancialSnapshot) (Results, error) {
			<-ctx.Done()
			return Results{}, ctx.Err()
		}, simulatorSnapshot(), "prechecks_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queue := newWorkerQueueStub()
			queue.jobs <- Job{ID: "test-job", ApplicationID: tc.snapshot.ApplicationID, Attempt: 3, Snapshot: tc.snapshot}
			config := workerTestConfig()
			startTestWorker(t, queue, tc.processor, config)
			select {
			case failure := <-queue.failed:
				if failure.code != tc.code || failure.err != nil || failure.delay != 4*config.RetryBaseDelay {
					t.Fatalf("unexpected retry: %+v", failure)
				}
			case <-time.After(time.Second):
				t.Fatal("failure was not persisted")
			}
		})
	}
}

func TestWorkerLeaseLossAndShutdownDoNotFinishOldAttempt(t *testing.T) {
	t.Run("lost lease", func(t *testing.T) {
		queue := newWorkerQueueStub()
		queue.completeErr = ErrLeaseLost
		worker, err := NewWorker(queue, processorFunc(func(context.Context, FinancialSnapshot) (Results, error) {
			return Results{SchemaVersion: 1}, nil
		}), workerTestConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		worker.process(context.Background(), Job{ApplicationID: simulatorSnapshot().ApplicationID, Snapshot: simulatorSnapshot()})
		if len(queue.completed) != 1 || len(queue.failed) != 0 {
			t.Fatal("lost lease must not schedule a second completion or failure")
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		queue := newWorkerQueueStub()
		queue.jobs <- Job{ID: "in-flight", ApplicationID: simulatorSnapshot().ApplicationID, Snapshot: simulatorSnapshot()}
		started := make(chan struct{})
		cancel, done := startTestWorker(t, queue, processorFunc(func(ctx context.Context, _ FinancialSnapshot) (Results, error) {
			close(started)
			<-ctx.Done()
			return Results{}, ctx.Err()
		}), workerTestConfig())
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("worker never started")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown did not cancel processing")
		}
		if len(queue.completed) != 0 || len(queue.failed) != 0 {
			t.Fatal("shutdown must leave the existing lease for recovery")
		}
	})
}

func TestWorkerConfigurationAndBackoff(t *testing.T) {
	queue := newWorkerQueueStub()
	for _, change := range []func(*Config){
		func(c *Config) { c.Concurrency = 0 }, func(c *Config) { c.Concurrency = 65 },
		func(c *Config) { c.PollInterval = 0 }, func(c *Config) { c.LeaseDuration = 0 },
		func(c *Config) { c.AttemptTimeout = c.LeaseDuration }, func(c *Config) { c.AttemptTimeout = 0 },
		func(c *Config) { c.RetryBaseDelay = 0 },
	} {
		config := DefaultConfig()
		change(&config)
		if _, err := NewWorker(queue, NewSimulator(), config, nil); err == nil {
			t.Fatal("invalid configuration was accepted")
		}
	}
	if _, err := NewWorker(queue, NewSimulator(), DefaultConfig(), nil); err != nil {
		t.Fatal(err)
	}
	if retryDelay(time.Second, 1) != time.Second || retryDelay(time.Second, 4) != 8*time.Second ||
		retryDelay(time.Second, 1_000_000) != 30*time.Second {
		t.Fatal("retry backoff is not bounded")
	}
}
