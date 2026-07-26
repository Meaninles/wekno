package session

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

func TestHandleAgentEventsForSSETerminalErrorReturns(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := &recordingStreamManager{
		events: []interfaces.StreamEvent{{
			ID:      "error-1",
			Type:    types.ResponseTypeError,
			Content: "model failed",
			Done:    true,
			Data: map[string]interface{}{
				"stage": "custom_agent_execution",
			},
		}},
	}
	handler := &Handler{streamManager: stream}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/stream", nil)
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = request

	done := make(chan struct{})
	go func() {
		handler.handleAgentEventsForSSE(
			context.Background(),
			ginContext,
			"session-1",
			"assistant-1",
			"request-1",
			event.NewEventBus(),
			false,
		)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal error did not close SSE stream")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `"response_type":"error"`) {
		t.Fatalf("SSE body does not contain terminal error: %s", body)
	}
	if strings.Contains(body, `"response_type":"complete"`) {
		t.Fatalf("failed SSE stream emitted a synthetic completion: %s", body)
	}
}
