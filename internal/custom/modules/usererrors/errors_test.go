package usererrors

import "testing"

func TestPublicFailures(t *testing.T) {
	cases := []struct{ detail, code string }{
		{"Delivery requirements could not be satisfied: Answer must not be empty.", "empty_response"},
		{"provider: maximum context length is 131072 tokens", "input_too_long"},
		{"HTTP 413 request entity too large", "file_too_large"},
		{"RateLimitError: 429 secret request id", "busy"},
		{"context deadline exceeded", "timeout"},
		{"unexpected EOF", "connection"},
		{"run membership unavailable", "permission_denied"},
		{"private path /control/token credential=secret", "unknown"},
		{"", "unknown"},
		{"workspace python error at line 413, input id 5842967", "unknown"},
	}
	for _, tc := range cases {
		got := Classify("", tc.detail)
		if got.Code != tc.code {
			t.Errorf("%q: %s != %s", tc.detail, got.Code, tc.code)
		}
		if Classify("", got.Message).Code != got.Code {
			t.Errorf("public classification must be idempotent: %s", got.Code)
		}
	}
	if got := Classify("empty_response", "private stack"); got.Code != "empty_response" {
		t.Fatal(got)
	}
	if got := Classify("untrusted_code", "private stack"); got.Code != "unknown" {
		t.Fatal(got)
	}
}
