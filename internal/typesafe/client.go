package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxResponseBytes caps how much of a response body we will read. It is a
	// safety valve, not a protocol limit: real answers are a few kilobytes.
	maxResponseBytes = 8 << 20

	// baseRetryDelay is doubled on every attempt: 400ms, 800ms, 1.6s, ...
	baseRetryDelay = 400 * time.Millisecond

	// maxRetryDelay caps the backoff, including a server-supplied Retry-After.
	maxRetryDelay = 10 * time.Second

	// defaultMaxAttempts is the total number of tries, including the first.
	defaultMaxAttempts = 4

	// userAgent identifies this client to the API.
	userAgent = "jev-cli"
)

// APIError is a non-2xx response from the TypeSafe API.
type APIError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

// Error implements error.
func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	msg := fmt.Sprintf("typesafe: HTTP %d", e.Status)
	if body != "" {
		msg += ": " + body
	}
	// 422s carry the offending field; make it obvious it is our bug, not the
	// user's.
	if e.Status == http.StatusUnprocessableEntity {
		msg += " (request failed validation — this is a bug in the question shapes jev-cli sent)"
	}
	return msg
}

// Retryable reports whether retrying the same request could succeed.
//
// 429 and 529 are the documented transient failures; the 5xx family covers
// gateway hiccups. 401 and 422 are terminal: the key is wrong or the request
// is malformed and will stay malformed.
func (e *APIError) Retryable() bool {
	switch e.Status {
	case http.StatusTooManyRequests, 529,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Unauthorized reports whether the API rejected our credentials.
func (e *APIError) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// Result is one completed evaluation.
type Result struct {
	Response *SystemOneResponse
	// Raw is the response body exactly as received, for --explain and --json.
	Raw []byte
	// Latency is the wall-clock time of the request that succeeded.
	Latency time.Duration
	// Attempts is how many tries it took, including the successful one.
	Attempts int
}

// Client talks to the TypeSafe API. The zero value is not usable; call
// NewClient.
type Client struct {
	apiKey      string
	baseURL     string
	http        *http.Client
	maxAttempts int
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at a different host. Useful for tests and for
// self-hosted gateways.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient replaces the underlying transport.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithMaxAttempts sets the retry budget. Values below 1 are clamped to 1.
func WithMaxAttempts(n int) Option {
	return func(c *Client) {
		if n < 1 {
			n = 1
		}
		c.maxAttempts = n
	}
}

// NewClient returns a client authenticated with apiKey. The default timeout is
// 30s and the default retry budget is 4 attempts.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:      strings.TrimSpace(apiKey),
		baseURL:     DefaultBaseURL,
		http:        &http.Client{Timeout: 30 * time.Second},
		maxAttempts: defaultMaxAttempts,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// HasAPIKey reports whether the client was given a credential.
func (c *Client) HasAPIKey() bool { return c.apiKey != "" }

// BaseURL returns the configured host.
func (c *Client) BaseURL() string { return c.baseURL }

// SystemOne evaluates a state against a map of typed questions.
//
// Questions are evaluated in parallel by the model, so the cost of adding one
// is tokens, not latency. Prefer one request with many questions over several
// requests with few.
func (c *Client) SystemOne(ctx context.Context, req SystemOneRequest) (*Result, error) {
	if !c.HasAPIKey() {
		return nil, fmt.Errorf("typesafe: no API key (set %s, or write it to the jev config file)", EnvAPIKey)
	}
	if req.Model == "" {
		req.Model = DefaultModel
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: encode request: %w", err)
	}

	endpoint := c.baseURL + SystemOnePath
	started := time.Now()

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("typesafe: build request: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("User-Agent", userAgent)

		resp, err := c.http.Do(httpReq)
		if err != nil {
			// Transport-level failure: retry unless the caller gave up.
			lastErr = fmt.Errorf("typesafe: %w", err)
			if attempt < c.maxAttempts && ctx.Err() == nil {
				if err := sleepCtx(ctx, backoff(attempt, 0)); err != nil {
					return nil, lastErr
				}
				continue
			}
			return nil, lastErr
		}

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("typesafe: read response: %w", readErr)
			if attempt < c.maxAttempts && ctx.Err() == nil {
				continue
			}
			return nil, lastErr
		}

		if resp.StatusCode == http.StatusOK {
			var out SystemOneResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("typesafe: decode response: %w", err)
			}
			if err := completeAnswers(req, &out); err != nil {
				return nil, err
			}
			return &Result{
				Response: &out,
				Raw:      raw,
				Latency:  time.Since(started),
				Attempts: attempt,
			}, nil
		}

		apiErr := &APIError{
			Status:     resp.StatusCode,
			Body:       string(raw),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
		lastErr = apiErr
		if !apiErr.Retryable() || attempt == c.maxAttempts {
			return nil, apiErr
		}
		if err := sleepCtx(ctx, backoff(attempt, apiErr.RetryAfter)); err != nil {
			return nil, apiErr
		}
	}
	return nil, lastErr
}

// completeAnswers fails loudly when the response is missing an answer we asked
// for. Silently treating a missing answer as "no" would turn a protocol
// problem into a wrong decision, which is exactly what this design exists to
// avoid.
func completeAnswers(req SystemOneRequest, out *SystemOneResponse) error {
	if out.Answers == nil {
		out.Answers = map[string]Answer{}
	}
	var missing []string
	for id := range req.Questions {
		if _, ok := out.Answers[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("typesafe: response is missing %d answer(s): %s", len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// backoff returns how long to wait before the next attempt. A server-supplied
// Retry-After wins over our own schedule, and both are capped.
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxRetryDelay {
			return maxRetryDelay
		}
		return retryAfter
	}
	d := baseRetryDelay << (attempt - 1)
	if d > maxRetryDelay {
		d = maxRetryDelay
	}
	// Full jitter, so a fleet of jev invocations does not retry in lockstep.
	jitter := time.Duration(rand.Int64N(int64(d/2) + 1))
	return d/2 + jitter
}

// parseRetryAfter understands both forms of the header: delta-seconds and an
// HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// IsAuthError reports whether err is a TypeSafe authentication failure.
func IsAuthError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Unauthorized()
}
