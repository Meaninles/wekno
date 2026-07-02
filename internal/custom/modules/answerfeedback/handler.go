package answerfeedback

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type Handler struct {
	service        *Service
	messageService interfaces.MessageService
}

type setFeedbackRequest struct {
	Feedback string `json:"feedback"`
}

func NewHandler(service *Service, messageService interfaces.MessageService) *Handler {
	return &Handler{service: service, messageService: messageService}
}

func (h *Handler) SetMessageFeedback(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := c.Param("session_id")
	messageID := c.Param("message_id")
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(messageID) == "" {
		c.Error(apperrors.NewBadRequestError("session_id and message_id are required"))
		return
	}

	var req setFeedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	feedback, ok := normalizeFeedback(req.Feedback)
	if !ok {
		c.Error(apperrors.NewBadRequestError("feedback must be like, dislike, or none"))
		return
	}

	message, err := h.messageService.GetMessage(ctx, sessionID, messageID)
	if err != nil {
		c.Error(apperrors.NewNotFoundError("message not found"))
		return
	}
	if message.Role != "assistant" {
		c.Error(apperrors.NewBadRequestError("feedback can only be set on assistant messages"))
		return
	}
	if h.service == nil || !h.service.SetMessageFeedback(ctx, message, feedback) {
		logger.Warnf(ctx, "[answerfeedback] feedback was accepted but not queued, message=%s", messageID)
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"message_id": messageID,
			"feedback":   feedback,
		},
	})
}

func (h *Handler) ListMessageFeedback(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := c.Query("session_id")
	rawIDs := c.Query("message_ids")
	if strings.TrimSpace(rawIDs) == "" {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{}})
		return
	}
	messageIDs := splitIDs(rawIDs)
	if len(messageIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{}})
		return
	}
	if h.service == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{}})
		return
	}
	data, err := h.service.ListFeedback(ctx, sessionID, messageIDs)
	if err != nil {
		logger.Warnf(ctx, "[answerfeedback] failed to list feedback: %v", err)
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func splitIDs(raw string) []string {
	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}
