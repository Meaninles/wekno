package middleware

import "testing"

func TestGeneralAgentInternalToolCallbackBypassesGlobalAuth(t *testing.T) {
	const path = "/api/v1/custom/general-agent/internal/tools/call"

	if !isNoAuthAPI(path, "POST") {
		t.Fatalf("expected %s POST to bypass global auth and reach internal key validation", path)
	}
	if isNoAuthAPI(path, "GET") {
		t.Fatalf("expected %s GET to remain protected", path)
	}
	if isNoAuthAPI("/api/v1/custom/general-agent/internal/tools/call/extra", "POST") {
		t.Fatalf("expected only the exact general-agent callback path to bypass global auth")
	}
}
