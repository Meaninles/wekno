package builtinagentdefaults

import (
	stderrors "errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Reset(c *gin.Context) {
	ctx := c.Request.Context()
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.Error(apperrors.NewBadRequestError("Agent ID cannot be empty"))
		return
	}

	agent, err := h.service.Reset(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"agent_id": id,
		})
		switch {
		case stderrors.Is(err, ErrTenantContextMissing):
			c.Error(apperrors.NewUnauthorizedError("Missing tenant context"))
		case stderrors.Is(err, ErrBuiltinAgentNotFound):
			c.Error(apperrors.NewNotFoundError("Built-in agent not found"))
		default:
			c.Error(apperrors.NewInternalServerError(err.Error()))
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    agent,
	})
}
