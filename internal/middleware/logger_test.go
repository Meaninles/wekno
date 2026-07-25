package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/gin-gonic/gin"
)

func TestLoggerNeverWritesRequestResponseOrQueryBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var logs bytes.Buffer
	logger.SetOutput(&logs)
	defer logger.ConfigureFromEnv()

	router := gin.New()
	router.Use(RequestID(), Logger())
	router.POST("/privacy", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"document": "response-document-canary",
			"token":    "response-token-canary",
		})
	})

	request := httptest.NewRequest(
		http.MethodPost,
		"/privacy?access_token=query-token-canary",
		strings.NewReader(`{"document":"request-document-canary","api_key":"request-token-canary"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	output := logs.String()
	for _, forbidden := range []string{
		"request-document-canary",
		"request-token-canary",
		"response-document-canary",
		"response-token-canary",
		"query-token-canary",
		"request_body",
		"response_body",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("operational log leaked %q: %s", forbidden, output)
		}
	}
	for _, required := range []string{
		"path=/privacy",
		"request_size=",
		"response_size=",
		"status_code=200",
	} {
		if !strings.Contains(output, required) {
			t.Fatalf("operational log missing %q: %s", required, output)
		}
	}
}
