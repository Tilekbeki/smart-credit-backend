package scoring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const maxResponseBytes = 1 << 20

type Client struct {
	endpoint  string
	http      *http.Client
	transport *http.Transport
}

func NewClient(baseURL string, timeout time.Duration) (*Client, error) {
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint == nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || timeout <= 0 {
		return nil, failure("invalid_configuration", false)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/score"
	endpoint.RawPath = ""
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		endpoint:  endpoint.String(),
		transport: transport,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (client *Client) CloseIdleConnections() {
	client.transport.CloseIdleConnections()
}

// Score performs one HTTP call. The PostgreSQL job owns retry and backoff policy.
func (client *Client) Score(ctx context.Context, input Request) (Response, error) {
	if err := ValidateRequest(input); err != nil {
		return Response{}, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return Response{}, failure("invalid_request", false)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, failure("invalid_request", false)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return Response{}, transportFailure(ctx, err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		return Response{}, failure("http429", true)
	case response.StatusCode >= 500 && response.StatusCode <= 599:
		return Response{}, failure("http5xx", true)
	case response.StatusCode != http.StatusOK:
		return Response{}, failure("unexpected_status", false)
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Response{}, transportFailure(ctx, err)
	}
	if len(result) > maxResponseBytes {
		return Response{}, failure("invalid_response", false)
	}
	return DecodeResponse(result)
}

type clientError struct {
	code      string
	retryable bool
}

// Only a stable code is exposed; wrapped URL/transport errors can contain service
// addresses or credentials and must not reach application logs or API responses.
func (err *clientError) Error() string { return "scoring: " + err.code }

func failure(code string, retryable bool) error {
	return &clientError{code: code, retryable: retryable}
}

func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var scoringError *clientError
	if errors.As(err, &scoringError) {
		return scoringError.code
	}
	return "connectivity"
}

func IsRetryable(err error) bool {
	var scoringError *clientError
	return errors.As(err, &scoringError) && scoringError.retryable
}

func transportFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return failure("canceled", false)
	}
	var networkError net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
		return failure("timeout", true)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return failure("connection_refused", true)
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return failure("dns", true)
	}
	return failure("connectivity", true)
}
