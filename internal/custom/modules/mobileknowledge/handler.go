package mobileknowledge

import (
	"errors"
	"net/http"
	"strconv"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func queryInt(c *gin.Context, key string, fallback int) int {
	value, err := strconv.Atoi(c.Query(key))
	if err != nil {
		return fallback
	}
	return value
}

func (h *Handler) ListShareTargets(c *gin.Context) {
	page, err := h.service.ListShareTargets(
		c.Request.Context(),
		c.Param("id"),
		c.GetString(types.UserIDContextKey.String()),
		c.GetUint64(types.TenantIDContextKey.String()),
		c.Query("q"),
		queryInt(c, "page", 1),
		queryInt(c, "page_size", defaultShareTargetPageSize),
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrKnowledgeBaseNotFound):
			c.Error(apperrors.NewNotFoundError("知识库不存在"))
		case errors.Is(err, ErrShareTargetForbidden):
			c.Error(apperrors.NewForbiddenError("当前账号没有管理该知识库共享的权限"))
		default:
			c.Error(apperrors.NewInternalServerError("加载共享空间失败").WithDetails(err.Error()))
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": page})
}
