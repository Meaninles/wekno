package middleware

import "testing"

func TestGeneralAgentInternalToolCallbackBypassesGlobalAuth(t *testing.T) {
	paths := []string{
		"/api/v1/custom/general-agent/internal/tools/call",
		"/api/v1/custom/general-agent/internal/artifacts/upload",
	}

	for _, path := range paths {
		if !isNoAuthAPI(path, "POST") {
			t.Fatalf("expected %s POST to bypass global auth and reach internal key validation", path)
		}
		if isNoAuthAPI(path, "GET") {
			t.Fatalf("expected %s GET to remain protected", path)
		}
		if isNoAuthAPI(path+"/extra", "POST") {
			t.Fatalf("expected only the exact general-agent callback path to bypass global auth")
		}
	}
}
