package scheduledchat

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) ListTasks(c *gin.Context) {
	tasks, err := h.service.ListTasks(c.Request.Context())
	if err != nil {
		writeScheduledChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": tasks})
}

func (h *Handler) CreateTask(c *gin.Context) {
	var req TaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeScheduledChatError(c, err)
		return
	}
	task, err := h.service.CreateTask(c.Request.Context(), req)
	if err != nil {
		writeScheduledChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": task})
}

func (h *Handler) GetTask(c *gin.Context) {
	task, err := h.service.GetTask(c.Request.Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		writeScheduledChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": task})
}

func (h *Handler) UpdateTask(c *gin.Context) {
	var req TaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeScheduledChatError(c, err)
		return
	}
	task, err := h.service.UpdateTask(c.Request.Context(), strings.TrimSpace(c.Param("id")), req)
	if err != nil {
		writeScheduledChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": task})
}

func (h *Handler) DeleteTask(c *gin.Context) {
	if err := h.service.DeleteTask(c.Request.Context(), strings.TrimSpace(c.Param("id"))); err != nil {
		writeScheduledChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) RunTaskNow(c *gin.Context) {
	run, err := h.service.RunTaskNow(c.Request.Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		writeScheduledChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": run})
}

func (h *Handler) ListRuns(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	runs, err := h.service.ListRuns(c.Request.Context(), strings.TrimSpace(c.Param("id")), limit)
	if err != nil {
		writeScheduledChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": runs})
}

func (h *Handler) Variables(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": Variables()})
}

func (h *Handler) PromptTemplates(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": PromptTemplates()})
}

func (h *Handler) RenderPreview(c *gin.Context) {
	var req RenderPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeScheduledChatError(c, err)
		return
	}
	rendered, err := h.service.RenderPreview(c.Request.Context(), req)
	if err != nil {
		writeScheduledChatError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"content": rendered}})
}

func writeScheduledChatError(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
}
