package middleware

import "testing"

func TestAgentRuntimeInternalCallbacksReachInternalKeyValidation(t *testing.T) {
	paths := []string{
		"/api/v1/custom/agent-runtime/internal/tools/call",
		"/api/v1/custom/agent-runtime/internal/artifacts/upload",
	}
	for _, operation := range []string{"claim", "heartbeat", "checkpoint", "prefetch", "reuse-evidence", "events", "validate", "commit", "fail", "status", "budget", "baseline", "finalize"} {
		paths = append(paths, "/api/v1/custom/agent-runtime/internal/runs/"+operation)
	}

	for _, path := range paths {
		if !isNoAuthAPI(path, "POST") {
			t.Fatalf("expected %s POST to bypass global auth and reach internal key validation", path)
		}
		if isNoAuthAPI(path, "GET") {
			t.Fatalf("expected %s GET to remain protected", path)
		}
		if isNoAuthAPI(path+"/extra", "POST") {
			t.Fatalf("expected only the exact runtime callback path to bypass global auth")
		}
	}
	lease := "/api/v1/custom/agent-runtime/internal/models/lease"
	if !isNoAuthAPI(lease, "GET") || isNoAuthAPI(lease, "POST") || isNoAuthAPI(lease+"/extra", "GET") {
		t.Fatal("only the exact model lease GET may reach internal authentication")
	}
}
