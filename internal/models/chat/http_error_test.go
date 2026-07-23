package chat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.July, 21, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		present bool
	}{
		{name: "delta seconds", value: "45", want: 45 * time.Second, present: true},
		{name: "zero is valid", value: "0", want: 0, present: true},
		{name: "future HTTP date", value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second, present: true},
		{name: "past HTTP date", value: now.Add(-time.Minute).Format(http.TimeFormat), want: 0, present: true},
		{name: "negative rejected", value: "-1", present: false},
		{name: "malformed rejected", value: "later", present: false},
		{name: "missing", value: "", present: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, present := parseRetryAfter(tt.value, now)
			if present != tt.present || got != tt.want {
				t.Fatalf("parseRetryAfter(%q) = (%s, %v), want (%s, %v)", tt.value, got, present, tt.want, tt.present)
			}
		})
	}
}

func TestResponseMetadataRoundTripperPreservesRetryAfter(t *testing.T) {
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{"Retry-After": []string{"37"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"limited"}`)),
			Request:    req,
		}, nil
	})
	client := wrapHTTPClientForResponseMetadata(&http.Client{Transport: base})
	metadata := &responseMetadata{}
	ctx := context.WithValue(context.Background(), responseMetadataContextKey{}, metadata)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	resp.Body.Close()

	cause := errors.New("sdk consumed response")
	got := enrichRemoteAPIError(cause, metadata)
	if !errors.Is(got, cause) {
		t.Fatal("enriched error no longer unwraps to its original cause")
	}
	status, ok := HTTPStatusCode(got)
	if !ok || status != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatusCode() = (%d, %v), want (429, true)", status, ok)
	}
	retryAfter, ok := RetryAfterDuration(got)
	if !ok || retryAfter != 37*time.Second {
		t.Fatalf("RetryAfterDuration() = (%s, %v), want (37s, true)", retryAfter, ok)
	}
}

func TestRemoteAPIChatSDKPathReturnsStructuredRetryAfter(t *testing.T) {
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"Retry-After":  []string{"29"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"error":{"message":"limited","type":"rate_limit"}}`)),
			Request: req,
		}, nil
	})
	sdkConfig := openai.DefaultConfig("test-key")
	sdkConfig.BaseURL = "https://example.com/v1"
	sdkConfig.HTTPClient = wrapHTTPClientForResponseMetadata(&http.Client{Transport: base})
	remote := newTestRemoteChat(t)
	remote.client = openai.NewClientWithConfig(sdkConfig)

	_, err := remote.Chat(context.Background(), []Message{{Role: "user", Content: "hello"}}, &ChatOptions{})
	if err == nil {
		t.Fatal("Chat() error = nil, want HTTP 429")
	}
	status, ok := HTTPStatusCode(err)
	if !ok || status != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatusCode() = (%d, %v), want (429, true): %v", status, ok, err)
	}
	retryAfter, ok := RetryAfterDuration(err)
	if !ok || retryAfter != 29*time.Second {
		t.Fatalf("RetryAfterDuration() = (%s, %v), want (29s, true)", retryAfter, ok)
	}
	var sdkErr *openai.APIError
	if !errors.As(err, &sdkErr) {
		t.Fatalf("structured error does not unwrap to *openai.APIError: %v", err)
	}
}

func TestEnrichRemoteAPIErrorSDKFallbackKeepsWrapping(t *testing.T) {
	cause := &openai.APIError{
		HTTPStatusCode: http.StatusServiceUnavailable,
		HTTPStatus:     "503 Service Unavailable",
		Message:        "overloaded",
	}
	got := enrichRemoteAPIError(cause, nil)
	if !errors.Is(got, cause) {
		t.Fatal("enriched SDK error no longer unwraps to the SDK error")
	}
	status, ok := HTTPStatusCode(got)
	if !ok || status != http.StatusServiceUnavailable {
		t.Fatalf("HTTPStatusCode() = (%d, %v), want (503, true)", status, ok)
	}
	if _, ok := RetryAfterDuration(got); ok {
		t.Fatal("RetryAfterDuration() reported a missing SDK header as present")
	}
}

func TestNewHTTPStatusErrorPreservesRawResponseMetadata(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Header:     http.Header{"Retry-After": []string{"12"}},
	}
	err := newHTTPStatusError(resp, `{"error":"quota"}`, nil, time.Now())
	if status, ok := HTTPStatusCode(err); !ok || status != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatusCode() = (%d, %v), want (429, true)", status, ok)
	}
	if retryAfter, ok := RetryAfterDuration(err); !ok || retryAfter != 12*time.Second {
		t.Fatalf("RetryAfterDuration() = (%s, %v), want (12s, true)", retryAfter, ok)
	}
	if !strings.Contains(err.Error(), "status 429") || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("Error() lost compatible status/body detail: %q", err.Error())
	}
}
