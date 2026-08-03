package wikiaccess

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) GetCurrent(c *gin.Context) {
	userID := c.GetString(types.UserIDContextKey.String())
	enabled, err := h.service.IsEnabled(c.Request.Context(), userID)
	if err != nil {
		logger.Errorf(c.Request.Context(), "[custom wiki-access] resolve current permission: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Wiki权限读取失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": CurrentPermission{WikiEnabled: enabled}})
}

func (h *Handler) SearchUsers(c *gin.Context) {
	page, err := parsePositiveInt(c.Query("page"), 1, 0)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "page必须是正整数"})
		return
	}
	pageSize, err := parsePositiveInt(c.Query("page_size"), 20, 100)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "page_size必须在1到100之间"})
		return
	}
	result, err := h.service.SearchUsers(c.Request.Context(), c.Query("q"), page, pageSize)
	if err != nil {
		logger.Errorf(c.Request.Context(), "[custom wiki-access] search users: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "用户Wiki权限读取失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func parsePositiveInt(raw string, fallback int, maximum int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || (maximum > 0 && value > maximum) {
		return 0, errors.New("invalid positive integer")
	}
	return value, nil
}

type setPermissionRequest struct {
	WikiEnabled *bool `json:"wiki_enabled"`
}

func (h *Handler) SetUser(c *gin.Context) {
	var req setPermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.WikiEnabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "wiki_enabled参数无效"})
		return
	}
	actorUserID := c.GetString(types.UserIDContextKey.String())
	row, err := h.service.SetUserPermission(
		c.Request.Context(),
		c.Param("user_id"),
		*req.WikiEnabled,
		actorUserID,
	)
	if errors.Is(err, ErrUserNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": err.Error()})
		return
	}
	if err != nil {
		logger.Errorf(c.Request.Context(), "[custom wiki-access] update user permission: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Wiki权限更新失败"})
		return
	}
	logger.Infof(
		c.Request.Context(),
		"[custom wiki-access] actor=%s target=%s enabled=%t",
		actorUserID,
		row.ID,
		row.WikiEnabled,
	)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": row})
}
