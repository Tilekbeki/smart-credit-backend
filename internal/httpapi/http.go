// Package httpapi implements the HTTP transport. Stage 1 exposes probes only.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"smart-credit-backend/internal/config"
)

type Pinger interface {
	Ping(context.Context) error
}

func NewHandler(db Pinger, logger *slog.Logger, pingTimeout time.Duration) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			started := time.Now()
			ww := middleware.NewWrapResponseWriter(w, req.ProtoMajor)
			defer func() {
				if recover() != nil {
					logger.ErrorContext(req.Context(), "HTTP panic", "request_id", middleware.GetReqID(req.Context()))
					if ww.Status() == 0 {
						writeJSON(ww, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
					}
				}
				logger.InfoContext(req.Context(), "HTTP request", "method", req.Method,
					"status", ww.Status(), "duration", time.Since(started).String(),
					"request_id", middleware.GetReqID(req.Context()))
			}()
			next.ServeHTTP(ww, req)
		})
	})
	r.Get("/health", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "backend"})
	})
	r.Get("/ready", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), pingTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			logger.WarnContext(req.Context(), "PostgreSQL readiness check failed", "request_id", middleware.GetReqID(req.Context()))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "error": "database_unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
	})
	return r
}

func NewServer(cfg config.HTTP, handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr: cfg.Address(), Handler: handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout,
		IdleTimeout: cfg.IdleTimeout, MaxHeaderBytes: 1 << 20,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

func writeJSON(w http.ResponseWriter, status int, payload map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
