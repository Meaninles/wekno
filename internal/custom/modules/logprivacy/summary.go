package logprivacy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
)

// PayloadSummary is safe to write to operational logs. It preserves enough
// information to correlate retries without exposing prompts, document text,
// credentials, image data, or tool arguments.
type PayloadSummary struct {
	Bytes  int
	SHA256 string
}

// SummarizeBytes returns the byte length and a deterministic content
// fingerprint. The payload itself is never converted to a loggable string.
func SummarizeBytes(payload []byte) PayloadSummary {
	sum := sha256.Sum256(payload)
	return PayloadSummary{
		Bytes:  len(payload),
		SHA256: hex.EncodeToString(sum[:]),
	}
}

// SummarizeJSON serializes a request compactly and returns only its safe
// operational summary. Callers must not log the serialized bytes.
func SummarizeJSON(value any) (PayloadSummary, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return PayloadSummary{}, err
	}
	return SummarizeBytes(payload), nil
}

// SafeEndpoint removes every component that can carry credentials or
// user-supplied values. Scheme, host and path are retained for routing
// diagnostics; userinfo, query parameters and fragments are never logged.
func SafeEndpoint(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "[invalid-endpoint]"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	safe := parsed.String()
	if safe == "" {
		return "[empty-endpoint]"
	}
	return safe
}
