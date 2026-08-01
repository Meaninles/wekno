package derivativecontrol

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type Handler struct {
	service      *Service
	modelService interfaces.ModelService
}

func NewHandler(service *Service, modelService interfaces.ModelService) *Handler {
	return &Handler{service: service, modelService: modelService}
}

func (h *Handler) Status(c *gin.Context) {
	status, err := h.service.Status(c.Request.Context())
	if err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": status})
}

func (h *Handler) AdminConfig(c *gin.Context) {
	config, err := h.service.AdminConfig(c.Request.Context())
	if err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": config})
}

type publishRequest struct {
	ModelID       string `json:"model_id" binding:"required"`
	ModelTenantID uint64 `json:"model_tenant_id" binding:"required"`
}

func (h *Handler) Publish(c *gin.Context) {
	var request publishRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := h.service.Publish(
		c.Request.Context(), request.ModelID, request.ModelTenantID,
	); err != nil {
		h.writeError(c, err)
		return
	}
	config, err := h.service.AdminConfig(c.Request.Context())
	if err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": config})
}

func (h *Handler) Unpublish(c *gin.Context) {
	if err := h.service.Unpublish(c.Request.Context(), c.Param("model_id")); err != nil {
		h.writeError(c, err)
		return
	}
	config, err := h.service.AdminConfig(c.Request.Context())
	if err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": config})
}

type defaultRequest struct {
	ModelID string `json:"model_id" binding:"required"`
}

func (h *Handler) SetDefault(c *gin.Context) {
	var request defaultRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := h.service.SetDefault(c.Request.Context(), request.ModelID); err != nil {
		h.writeError(c, err)
		return
	}
	config, err := h.service.AdminConfig(c.Request.Context())
	if err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": config})
}

type testRequest struct {
	Prompt string `json:"prompt"`
}

func (h *Handler) Test(c *gin.Context) {
	var request testRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		prompt = "只回复 OK"
	}
	if len([]rune(prompt)) > 200 {
		c.Error(apperrors.NewBadRequestError("prompt is too long"))
		return
	}
	instance, err := h.service.ResolveChatModel(
		c.Request.Context(), h.modelService, c.Param("model_id"),
	)
	if err != nil {
		h.writeError(c, err)
		return
	}
	thinking := false
	started := time.Now()
	response, err := instance.Chat(c.Request.Context(), []chat.Message{
		{Role: "user", Content: prompt},
	}, &chat.ChatOptions{
		Temperature: 0,
		MaxTokens:   8,
		Thinking:    &thinking,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"ok":         true,
			"elapsed_ms": time.Since(started).Milliseconds(),
			"model_id":   instance.GetModelID(),
			"content":    response.Content,
			"usage":      response.Usage,
		},
	})
}

func (h *Handler) writeError(c *gin.Context, err error) {
	if appError, ok := apperrors.IsAppError(err); ok {
		c.Error(appError)
		return
	}
	if _, ok := err.(*DeferredError); ok {
		c.Error(apperrors.NewServiceUnavailableError(err.Error()))
		return
	}
	c.Error(apperrors.NewInternalServerError(err.Error()))
}
