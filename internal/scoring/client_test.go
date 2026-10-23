package scoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestClientScore(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/prefix/v1/score" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
			t.Error("incorrect scoring request method, path or headers")
		}
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request != validRequest() {
			t.Error("incorrect scoring request body")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, validResponseJSON)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/prefix/", time.Second)
	response, err := client.Score(context.Background(), validRequest())
	if err != nil || response.Decision != "approved" || calls.Load() != 1 {
		t.Fatalf("expected one successful call, got decision %s, calls %d, error %v", response.Decision, calls.Load(), err)
	}
}

func TestClientHTTPFailuresNoRetries(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		code      string
		retryable bool
	}{
		{"internal", 500, "financial-secret", "http5xx", true},
		{"unavailable", 503, "financial-secret", "http5xx", true},
		{"rate limited", 429, "financial-secret", "http429", true},
		{"bad request", 400, "financial-secret", "unexpected_status", false},
		{"unprocessable", 422, "financial-secret", "unexpected_status", false},
		{"unexpected success", 201, validResponseJSON, "unexpected_status", false},
		{"empty success", 204, "", "unexpected_status", false},
		{"malformed", 200, "financial-secret", "invalid_json", false},
		{"bad contract", 200, `{}`, "invalid_response", false},
		{"oversized", 200, strings.Repeat(" ", maxResponseBytes+1), "invalid_response", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, time.Second)
			_, err := client.Score(context.Background(), validRequest())
			if ErrorCode(err) != test.code || IsRetryable(err) != test.retryable || calls.Load() != 1 {
				t.Fatalf("expected %s retry=%v once, got %v retry=%v calls=%d", test.code, test.retryable, err, IsRetryable(err), calls.Load())
			}
			if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "financial-secret") {
				t.Fatal("error contains private request details")
			}
		})
	}
}

func TestClientRejectsRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		fmt.Fprint(w, validResponseJSON)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	_, err := client.Score(context.Background(), validRequest())
	if ErrorCode(err) != "unexpected_status" || redirected.Load() != 0 {
		t.Fatalf("redirect was followed or incorrectly reported: %v", err)
	}
}

func TestClientTimeoutAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      bool
		cancel    bool
		deadline  bool
		code      string
		retryable bool
	}{
		{name: "response timeout", code: "timeout", retryable: true},
		{name: "body timeout", body: true, code: "timeout", retryable: true},
		{name: "context canceled", cancel: true, code: "canceled"},
		{name: "context deadline", deadline: true, code: "timeout", retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				if test.body {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{`)
					w.(http.Flusher).Flush()
				}
				close(started)
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, 100*time.Millisecond)
			ctx := context.Background()
			var cancel context.CancelFunc
			if test.cancel {
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				go func() { <-started; cancel() }()
			} else if test.deadline {
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			_, err := client.Score(ctx, validRequest())
			if ErrorCode(err) != test.code || IsRetryable(err) != test.retryable {
				t.Fatalf("expected %s retry=%v, got %v", test.code, test.retryable, err)
			}
		})
	}
}

func TestClientConnectionRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, "http://"+address, time.Second)
	_, err = client.Score(context.Background(), validRequest())
	if ErrorCode(err) != "connection_refused" || !IsRetryable(err) || strings.Contains(err.Error(), address) {
		t.Fatalf("expected safe retryable connection_refused: %v", err)
	}
}

func TestClientDoesNotSendInvalidRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	request := validRequest()
	request.TermMonths = 0
	_, err := client.Score(context.Background(), request)
	if ErrorCode(err) != "invalid_request" || calls.Load() != 0 {
		t.Fatalf("invalid request sent to service: %v", err)
	}
}

func TestNewClientRejectsInvalidConfiguration(t *testing.T) {
	for _, address := range []string{"", "host:8000", "ftp://host", "http://", "http://:8000", "http://user:secret@host", "http://host?token=secret", "http://host#fragment", "http://[invalid"} {
		if _, err := NewClient(address, time.Second); ErrorCode(err) != "invalid_configuration" || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid address accepted or leaked: %v", err)
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := NewClient("http://localhost", timeout); ErrorCode(err) != "invalid_configuration" {
			t.Fatalf("invalid timeout accepted: %v", err)
		}
	}
}

func TestTransportFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		code      string
		retryable bool
	}{
		{"canceled", context.Canceled, "canceled", false},
		{"deadline", context.DeadlineExceeded, "timeout", true},
		{"refused", fmt.Errorf("private details: %w", syscall.ECONNREFUSED), "connection_refused", true},
		{"dns", &net.DNSError{Err: "private details", Name: "private service"}, "dns", true},
		{"body truncated", io.ErrUnexpectedEOF, "connectivity", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := transportFailure(context.Background(), test.err)
			if ErrorCode(err) != test.code || IsRetryable(err) != test.retryable || strings.Contains(err.Error(), "private") {
				t.Fatalf("incorrect safe classification: %v", err)
			}
			wrapped := fmt.Errorf("outer: %w", err)
			if ErrorCode(wrapped) != test.code || IsRetryable(wrapped) != test.retryable {
				t.Fatal("classification lost after wrapping")
			}
		})
	}
	if ErrorCode(nil) != "" || IsRetryable(nil) || IsRetryable(errors.New("unknown")) {
		t.Fatal("unexpected nil or unknown error classification")
	}
}

func newTestClient(t *testing.T, baseURL string, timeout time.Duration) *Client {
	t.Helper()
	client, err := NewClient(baseURL, timeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}
