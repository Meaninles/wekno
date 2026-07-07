package sessionstate

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

type statusRequest struct {
	SessionIDs []string `json:"session_ids"`
}

func (h *Handler) ListStatus(c *gin.Context) {
	ctx := c.Request.Context()
	if h == nil || h.service == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{}})
		return
	}

	var req statusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	statuses, err := h.service.ListStatus(ctx, req.SessionIDs)
	if err != nil {
		logger.Warnf(ctx, "[sessionstate] failed to list status: %v", err)
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": statuses})
}

func (h *Handler) MarkRead(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := strings.TrimSpace(c.Param("session_id"))
	if sessionID == "" {
		c.Error(apperrors.NewBadRequestError("session_id is required"))
		return
	}
	if h == nil || h.service == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": Status{SessionID: sessionID}})
		return
	}

	status, err := h.service.MarkRead(ctx, sessionID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.Error(apperrors.NewNotFoundError("session not found"))
			return
		}
		logger.Warnf(ctx, "[sessionstate] failed to mark read: %v", err)
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": status})
}
