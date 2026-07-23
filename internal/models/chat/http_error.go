package chat

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// HTTPStatusError preserves transport metadata that callers need in order to
// make a responsible retry decision. In particular, many model gateways use
// Retry-After with HTTP 429; reducing the response to a formatted string makes
// every caller retry too early and creates a synchronized retry storm.
//
// Cause is deliberately retained through Unwrap. Code which previously used
// errors.Is/errors.As against an SDK or transport error therefore continues to
// work when the error is enriched with HTTP metadata.
type HTTPStatusError struct {
	StatusCode        int
	Status            string
	RetryAfter        time.Duration
	RetryAfterPresent bool
	Body              string
	cause             error
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "<nil>"
	}
	detail := strings.TrimSpace(e.Body)
	if detail == "" && e.cause != nil {
		detail = e.cause.Error()
	}
	if detail == "" {
		return fmt.Sprintf("API request failed with status %d", e.StatusCode)
	}
	return fmt.Sprintf("API request failed with status %d: %s", e.StatusCode, detail)
}

func (e *HTTPStatusError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// HTTPStatusCode returns a structured status from an enriched remote-chat
// error. It follows the wrapping chain through errors.As.
func HTTPStatusCode(err error) (int, bool) {
	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) || httpErr.StatusCode <= 0 {
		return 0, false
	}
	return httpErr.StatusCode, true
}

// RetryAfterDuration returns the parsed Retry-After value and whether the
// upstream supplied a valid header. A valid value may be zero (for example an
// HTTP-date equal to the current time), hence the separate boolean.
func RetryAfterDuration(err error) (time.Duration, bool) {
	var httpErr *HTTPStatusError
	if !errors.As(err, &httpErr) || !httpErr.RetryAfterPresent {
		return 0, false
	}
	return httpErr.RetryAfter, true
}

// responseMetadata is populated by responseMetadataRoundTripper before the
// go-openai SDK consumes (and otherwise discards) response headers.
type responseMetadata struct {
	mu                sync.Mutex
	statusCode        int
	status            string
	retryAfter        time.Duration
	retryAfterPresent bool
}

func (m *responseMetadata) capture(resp *http.Response, now time.Time) {
	if m == nil || resp == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Clear metadata for every response. This matters when a caller performs
	// an in-call fallback (for example retrying a multimodal request without
	// images): a later successful/decoding response must not inherit the first
	// response's 4xx metadata.
	m.statusCode = 0
	m.status = ""
	m.retryAfter = 0
	m.retryAfterPresent = false
	if resp.StatusCode < http.StatusBadRequest {
		return
	}
	retryAfter, present := parseRetryAfter(resp.Header.Get("Retry-After"), now)
	m.statusCode = resp.StatusCode
	m.status = resp.Status
	m.retryAfter = retryAfter
	m.retryAfterPresent = present
}

func (m *responseMetadata) reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.statusCode = 0
	m.status = ""
	m.retryAfter = 0
	m.retryAfterPresent = false
	m.mu.Unlock()
}

func (m *responseMetadata) snapshot() (int, string, time.Duration, bool) {
	if m == nil {
		return 0, "", 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusCode, m.status, m.retryAfter, m.retryAfterPresent
}

type responseMetadataContextKey struct{}

type responseMetadataRoundTripper struct {
	base http.RoundTripper
}

func (t *responseMetadataRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if resp != nil {
		if metadata, ok := req.Context().Value(responseMetadataContextKey{}).(*responseMetadata); ok {
			metadata.capture(resp, time.Now())
		}
	}
	return resp, err
}

func wrapHTTPClientForResponseMetadata(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	wrapped := *client
	wrapped.Transport = &responseMetadataRoundTripper{base: base}
	return &wrapped
}

func newHTTPStatusError(resp *http.Response, body string, cause error, now time.Time) *HTTPStatusError {
	e := &HTTPStatusError{Body: body, cause: cause}
	if resp == nil {
		return e
	}
	e.StatusCode = resp.StatusCode
	e.Status = resp.Status
	e.RetryAfter, e.RetryAfterPresent = parseRetryAfter(resp.Header.Get("Retry-After"), now)
	return e
}

// enrichRemoteAPIError adds status/retry metadata to errors emitted by the
// go-openai path. The response metadata wins because it includes headers; SDK
// status fields are a fallback for custom HTTPDoer implementations that do not
// use responseMetadataRoundTripper.
func enrichRemoteAPIError(err error, metadata *responseMetadata) error {
	if err == nil {
		return nil
	}
	statusCode, status, retryAfter, retryAfterPresent := metadata.snapshot()
	if statusCode == 0 {
		var apiErr *openai.APIError
		if errors.As(err, &apiErr) {
			statusCode = apiErr.HTTPStatusCode
			status = apiErr.HTTPStatus
		}
	}
	if statusCode == 0 {
		var requestErr *openai.RequestError
		if errors.As(err, &requestErr) {
			statusCode = requestErr.HTTPStatusCode
			status = requestErr.HTTPStatus
		}
	}
	if statusCode == 0 {
		return err
	}
	return &HTTPStatusError{
		StatusCode:        statusCode,
		Status:            status,
		RetryAfter:        retryAfter,
		RetryAfterPresent: retryAfterPresent,
		cause:             err,
	}
}

// parseRetryAfter supports both RFC forms: delta-seconds and an HTTP-date.
// Malformed and negative delta values are rejected instead of being treated as
// an immediate retry. Past dates are valid and normalize to zero.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 || seconds > int64((time.Duration(1<<63-1))/time.Second) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := date.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}
