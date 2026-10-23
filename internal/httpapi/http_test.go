package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

func testLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func responseJSON(t *testing.T, response *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		t.Fatal("response is not JSON")
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	return body
}

func TestHealthWorksWithoutDatabase(t *testing.T) {
	response := httptest.NewRecorder()
	NewHandler(nil, testLogger(), time.Second, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusOK || responseJSON(t, response)["status"] != "ok" {
		t.Fatalf("liveness failed without a database: %d %s", response.Code, response.Body.String())
	}
}

func TestReadinessUsesDatabaseAndDoesNotLeakErrors(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(map[bool]string{true: "available", false: "unavailable"}[available], func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			calls := 0
			var pingContext context.Context
			db := pingFunc(func(ctx context.Context) error {
				calls++
				pingContext = ctx
				if !available {
					return errors.New("postgres://user:private-password@database:5432/private-db")
				}
				return nil
			})
			response := httptest.NewRecorder()
			NewHandler(db, logger, time.Second, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
			wantCode, wantStatus := http.StatusOK, "ready"
			if !available {
				wantCode, wantStatus = http.StatusServiceUnavailable, "not_ready"
			}
			if response.Code != wantCode || responseJSON(t, response)["status"] != wantStatus || calls != 1 {
				t.Fatalf("unexpected readiness result: status=%d body=%s calls=%d", response.Code, response.Body.String(), calls)
			}
			if pingContext.Err() != context.Canceled {
				t.Fatal("readiness did not cancel its ping context after returning")
			}
			if strings.Contains(response.Body.String()+logs.String(), "private-") {
				t.Fatal("database error leaked into the response or request logs")
			}
		})
	}
}

func TestReadinessEnforcesPingDeadline(t *testing.T) {
	const timeout = 20 * time.Millisecond
	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deadlineWasSet bool
	db := pingFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		deadlineWasSet = ok && time.Until(deadline) <= timeout
		<-ctx.Done()
		return ctx.Err()
	})
	response := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/ready", nil).WithContext(requestContext)
		NewHandler(db, testLogger(), timeout, nil).ServeHTTP(response, request)
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("readiness did not stop waiting for PostgreSQL")
	}
	if !deadlineWasSet || response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing bounded readiness deadline or wrong status: deadline=%t status=%d", deadlineWasSet, response.Code)
	}
}

func TestReadinessPropagatesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	db := pingFunc(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	response := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/ready", nil).WithContext(ctx)
		NewHandler(db, testLogger(), time.Minute, nil).ServeHTTP(response, request)
		close(completed)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("readiness ping did not start")
	}
	cancel()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("readiness ignored request cancellation")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled ping returned %d", response.Code)
	}
}

func TestHandlerRecoversPanicsWithoutLeakingDetails(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	db := pingFunc(func(context.Context) error { panic("private-database-password") })
	response := httptest.NewRecorder()
	NewHandler(db, logger, time.Second, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if response.Code != http.StatusInternalServerError || responseJSON(t, response)["error"] != "internal_error" {
		t.Fatalf("unexpected panic response: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String()+logs.String(), "private-database-password") {
		t.Fatal("panic details leaked")
	}
	if !strings.Contains(logs.String(), "HTTP panic") {
		t.Fatal("panic was not logged")
	}
}

func TestUnknownRoutesAndMethodsReturnJSONErrors(t *testing.T) {
	handler := NewHandler(nil, testLogger(), time.Second, nil)
	for _, test := range []struct {
		method string
		path   string
		status int
		error  string
	}{
		{method: http.MethodGet, path: "/missing", status: http.StatusNotFound, error: "not_found"},
		{method: http.MethodPost, path: "/health", status: http.StatusMethodNotAllowed, error: "method_not_allowed"},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.status || responseJSON(t, response)["error"] != test.error {
				t.Fatalf("unexpected HTTP error: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
