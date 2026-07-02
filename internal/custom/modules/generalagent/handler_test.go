package generalagent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCallToolRequiresInternalAPIKey(t *testing.T) {
	t.Setenv("CUSTOM_GENERAL_AGENT_API_KEY", "test-internal-key")
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		auth       string
		wantStatus int
	}{
		{name: "missing key", wantStatus: http.StatusUnauthorized},
		{name: "wrong key", auth: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		// With the correct internal key the request reaches request validation.
		// The empty JSON body is intentionally invalid for ToolCallRequest, so a
		// 400 here proves global auth/key validation did not stop the handler.
		{name: "correct key reaches handler", auth: "Bearer test-internal-key", wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			router.POST("/api/v1/custom/general-agent/internal/tools/call", NewHandler(nil).CallTool)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/custom/general-agent/internal/tools/call", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}
