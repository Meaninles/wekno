package generalagent

import (
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"

	embedservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) CallTool(c *gin.Context) {
	if !validInternalAPIKey(c) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	var req ToolCallRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	result, err := executeRuntimeTool(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, ToolCallResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) DownloadArtifact(c *gin.Context) {
	if h == nil || h.service == nil || h.service.db == nil {
		c.Error(errors.NewInternalServerError("general agent service unavailable"))
		return
	}
	ctx := logger.CloneContext(c.Request.Context())
	tenantID, _ := types.TenantIDFromContext(ctx)
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.Error(errors.NewBadRequestError("artifact id is required"))
		return
	}
	var row Artifact
	if err := h.service.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		First(&row).Error; err != nil {
		c.Error(errors.NewNotFoundError("artifact not found"))
		return
	}
	streamArtifact(c, row)
}

func (h *Handler) DownloadEmbedArtifact(c *gin.Context) {
	if h == nil || h.service == nil || h.service.db == nil {
		c.Error(errors.NewInternalServerError("general agent service unavailable"))
		return
	}
	requestCtx := c.Request.Context()
	ctx := logger.CloneContext(requestCtx)
	ch, ok := middleware.EmbedChannelFromContext(requestCtx)
	if !ok {
		c.Error(errors.NewUnauthorizedError("embed channel is required"))
		return
	}
	sessionID := strings.TrimSpace(c.Param("session_id"))
	if sessionID == "" {
		c.Error(errors.NewBadRequestError("session id is required"))
		return
	}
	if !embedservice.VerifyEmbedSessionHandle(ch, sessionID, c.GetHeader("X-Embed-Session")) {
		c.Error(errors.NewForbiddenError("session signature invalid"))
		return
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		c.Error(errors.NewUnauthorizedError("tenant context is required"))
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.Error(errors.NewBadRequestError("artifact id is required"))
		return
	}
	var row Artifact
	if err := h.service.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND session_id = ?", id, tenantID, sessionID).
		First(&row).Error; err != nil {
		c.Error(errors.NewNotFoundError("artifact not found"))
		return
	}
	streamArtifact(c, row)
}

func streamArtifact(c *gin.Context, row Artifact) {
	f, err := os.Open(row.FilePath)
	if err != nil {
		c.Error(errors.NewNotFoundError("artifact file not found"))
		return
	}
	defer f.Close()
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": row.FileName}))
	c.Header("Content-Type", row.ContentType)
	c.Header("Content-Length", stringInt(row.FileSize))
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}

func validInternalAPIKey(c *gin.Context) bool {
	expected := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_API_KEY"))
	if expected == "" {
		logger.Errorf(c.Request.Context(), "CUSTOM_GENERAL_AGENT_API_KEY is not configured; refusing general-agent internal callback")
		return false
	}
	auth := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		auth = strings.TrimSpace(auth[len("Bearer "):])
	}
	return auth == expected || strings.TrimSpace(c.GetHeader("X-API-Key")) == expected
}

func stringInt(v int64) string {
	if v <= 0 {
		return "0"
	}
	return strconvFormatInt(v)
}

func strconvFormatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}
