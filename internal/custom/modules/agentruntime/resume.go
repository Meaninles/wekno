package agentruntime

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	sessionhandler "github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Message ownership is checked by the session handler before these hooks.
// Reconnect reads the durable outbox directly, independent of the API process
// that accepted the question. Disconnecting a reader does not stop execution.
func (h *Handler) Resume(c *gin.Context, message *types.Message) bool {
	var row RunRecord
	err := h.service.db.WithContext(c.Request.Context()).Where("message_id = ? AND session_id = ?", message.ID, message.SessionID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false
	}
	if err != nil {
		c.JSON(503, gin.H{"error": "Runtime replay unavailable"})
		return true
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	bus := event.NewEventBus()
	sink := &replayStream{c: c, requestID: message.RequestID}
	copy := *message
	copy.Content = ""
	sessionhandler.NewAgentStreamHandler(c.Request.Context(), row.SessionID, row.MessageID, message.RequestID, row.CreatedAt, &copy, sink, bus).Subscribe()
	if err = h.service.replayRun(c.Request.Context(), &row, bus); err != nil && c.Request.Context().Err() == nil {
		_ = sink.AppendEvent(c.Request.Context(), row.SessionID, row.MessageID, interfaces.StreamEvent{Type: types.ResponseTypeError, Content: err.Error(), Done: true, Timestamp: time.Now()})
	}
	return true
}

func (h *Handler) Stop(c *gin.Context, message *types.Message) error {
	return h.service.db.WithContext(c.Request.Context()).Model(&RunRecord{}).
		Where("message_id = ? AND session_id = ? AND status IN ?", message.ID, message.SessionID, []string{"queued", "running"}).
		Updates(map[string]any{"status": "cancelled", "error": "cancelled by caller"}).Error
}

type replayStream struct {
	c          *gin.Context
	requestID  string
	answerSeen bool
}

func (s *replayStream) AppendEvent(ctx context.Context, _, _ string, item interfaces.StreamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if item.Type == types.ResponseTypeAnswer && item.Content != "" {
		s.answerSeen = true
	}
	s.c.SSEvent("message", sessionhandler.BuildStreamResponse(item, s.requestID))
	s.c.Writer.Flush()
	return nil
}
func (s *replayStream) GetEvents(context.Context, string, string, int) ([]interfaces.StreamEvent, int, error) {
	if s.answerSeen {
		return []interfaces.StreamEvent{{Type: types.ResponseTypeAnswer, Content: "streamed"}}, 1, nil
	}
	return nil, 0, nil
}
