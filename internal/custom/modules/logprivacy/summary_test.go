package logprivacy

import (
	"strings"
	"testing"
)

func TestSummarizeBytesDoesNotExposePayload(t *testing.T) {
	const secret = "制度正文-canary-api-token"
	summary := SummarizeBytes([]byte(secret))

	if summary.Bytes != len([]byte(secret)) {
		t.Fatalf("Bytes = %d, want %d", summary.Bytes, len([]byte(secret)))
	}
	if len(summary.SHA256) != 64 {
		t.Fatalf("SHA256 length = %d, want 64", len(summary.SHA256))
	}
	if strings.Contains(summary.SHA256, secret) {
		t.Fatal("payload content leaked into fingerprint")
	}
	if summary == SummarizeBytes([]byte(secret+"-changed")) {
		t.Fatal("different payloads produced the same summary")
	}
}

func TestSafeEndpointDropsCredentialsQueryAndFragment(t *testing.T) {
	got := SafeEndpoint("https://user:password@example.test/v1/chat/completions?api_key=secret#prompt")
	want := "https://example.test/v1/chat/completions"
	if got != want {
		t.Fatalf("SafeEndpoint() = %q, want %q", got, want)
	}
}

func TestSafeEndpointPreservesRelativeRoutingPath(t *testing.T) {
	if got := SafeEndpoint("/v1/chat/completions?token=secret"); got != "/v1/chat/completions" {
		t.Fatalf("SafeEndpoint() = %q", got)
	}
}
