package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"smart-credit-backend/internal/application"
	"smart-credit-backend/internal/auth"
	"smart-credit-backend/internal/company"
	"smart-credit-backend/internal/config"
	"smart-credit-backend/internal/credit"
	"smart-credit-backend/internal/decision"
	"smart-credit-backend/internal/document"
	"smart-credit-backend/internal/httpapi"
	"smart-credit-backend/internal/pipeline"
	"smart-credit-backend/internal/postgres"
	"smart-credit-backend/internal/scoring"
)

func main() {
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("application stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	tokens, err := auth.NewTokens(cfg.JWTSecret, cfg.JWTIssuer, cfg.JWTAudience)
	if err != nil {
		return errors.New("JWT configuration is invalid")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer pool.Close()
	authRepo := postgres.NewAuthRepository(pool, cfg.AuthDBTimeout)
	authHandler := httpapi.NewAuthHandler(authRepo, tokens, cfg.DemoOTPCode, cfg.AuthDBTimeout, logger)
	catalogRepo := postgres.NewCompanyCreditRepository(pool, cfg.AuthDBTimeout)
	creditHandler := httpapi.NewCreditHandler(company.NewService(catalogRepo), credit.NewService(catalogRepo, catalogRepo), auth.NewMiddleware(tokens, authRepo, cfg.AuthDBTimeout))
	applicationHandler := httpapi.NewApplicationHandler(application.NewService(postgres.NewApplicationRepository(pool, cfg.AuthDBTimeout), catalogRepo, catalogRepo), auth.NewMiddleware(tokens, authRepo, cfg.AuthDBTimeout))
	storage, err := document.NewLocalStorage(cfg.DocumentStoragePath)
	if err != nil {
		return errors.New("document storage initialization failed")
	}
	defer storage.Close()
	documents := document.NewService(postgres.NewDocumentRepository(pool, cfg.AuthDBTimeout), storage)
	if err := documents.Recover(ctx); err != nil {
		return errors.New("document storage recovery failed")
	}
	documentHandler := httpapi.NewDocumentHandler(documents, auth.NewMiddleware(tokens, authRepo, cfg.AuthDBTimeout))
	submissions := pipeline.NewSubmissionService(postgres.NewApplicationRepository(pool, cfg.AuthDBTimeout), documents, postgres.NewSubmissionRepository(pool, cfg.AuthDBTimeout))
	submissionHandler := httpapi.NewSubmissionHandler(submissions, auth.NewMiddleware(tokens, authRepo, cfg.AuthDBTimeout))
	scoringClient, err := scoring.NewClient(cfg.ScoringURL, cfg.ScoringTimeout)
	if err != nil {
		return errors.New("scoring HTTP client configuration is invalid")
	}
	defer scoringClient.CloseIdleConnections()
	workerConfig := pipeline.DefaultConfig()
	// Leave room for bounded database writes around the HTTP call; the lease
	// outlives the entire attempt, including its final fenced transaction.
	workerConfig.AttemptTimeout = max(workerConfig.AttemptTimeout, cfg.ScoringTimeout+10*time.Second)
	workerConfig.LeaseDuration = max(workerConfig.LeaseDuration, workerConfig.AttemptTimeout+15*time.Second)
	worker, err := pipeline.NewWorker(postgres.NewPrecheckQueue(pool, cfg.AuthDBTimeout), pipeline.NewSimulator(), workerConfig, logger, pipeline.WithScoring(scoringClient), pipeline.WithDecisionEngine(decision.NewEngine()))
	if err != nil {
		return errors.New("background worker configuration is invalid")
	}
	server := httpapi.NewServer(cfg.HTTP, httpapi.NewHandler(pool, logger, cfg.Database.PingTimeout, authHandler, creditHandler.Register, applicationHandler.Register, documentHandler.Register, submissionHandler.Register), logger)
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	// Stop and join the worker before the document store and PostgreSQL pool close.
	defer func() { cancelWorker(); <-workerDone }()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	logger.Info("HTTP server started", "address", listener.Addr().String())
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	case <-ctx.Done():
		stop() // A second signal may terminate an unresponsive shutdown.
		logger.Info("shutting down HTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown HTTP: %w", err)
		}
		if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP during shutdown: %w", err)
		}
		logger.Info("HTTP server stopped")
		return nil
	}
}
