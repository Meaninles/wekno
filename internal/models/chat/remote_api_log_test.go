package chat

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/logger"
)

func TestRemoteAPIRequestLogContainsOnlyMetadataAndFingerprint(t *testing.T) {
	const documentCanary = "confidential-policy-document-canary"
	var logs bytes.Buffer
	logger.SetOutput(&logs)
	defer logger.ConfigureFromEnv()

	client := &RemoteAPIChat{modelName: "deepseek-v4-flash-int8"}
	client.logRequest(context.Background(), map[string]any{
		"messages": []map[string]string{{
			"role":    "user",
			"content": documentCanary,
		}},
		"api_key": "credential-canary",
	}, false)

	output := logs.String()
	for _, forbidden := range []string{documentCanary, "credential-canary", `"messages"`, `"content"`} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("LLM request log leaked %q: %s", forbidden, output)
		}
	}
	for _, required := range []string{
		"model=deepseek-v4-flash-int8",
		"stream=false",
		"request_bytes=",
		"request_sha256=",
	} {
		if !strings.Contains(output, required) {
			t.Fatalf("LLM request log missing %q: %s", required, output)
		}
	}
}
