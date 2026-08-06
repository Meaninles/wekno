package middleware

import "testing"

func TestIMReferenceRoutesAreNotAddedToGlobalAuthBypass(t *testing.T) {
	path := "/api/v1/custom/im-output/reference"
	for _, candidate := range []string{path, path + "/data", path + "/original", path + "/extra"} {
		if isNoAuthAPI(candidate, "GET") || isNoAuthAPI(candidate, "HEAD") || isNoAuthAPI(candidate, "POST") {
			t.Fatalf("IM capability route %q must be isolated before Auth, not broaden the global bypass", candidate)
		}
	}
}
