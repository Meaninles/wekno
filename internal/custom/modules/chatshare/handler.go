package chatshare

import (
	stderrors "errors"
	"io"
	"mime"
	"net/http"
	"strconv"
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

func (h *Handler) Create(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := strings.TrimSpace(c.Param("session_id"))
	if sessionID == "" {
		c.Error(apperrors.NewBadRequestError("session_id is required"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("chat share service unavailable"))
		return
	}

	result, err := h.service.CreateShare(ctx, sessionID)
	if err != nil {
		h.writeServiceError(c, err, "failed to create chat share")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) Get(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		c.Error(apperrors.NewBadRequestError("token is required"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("chat share service unavailable"))
		return
	}

	result, err := h.service.GetShare(ctx, token)
	if err != nil {
		h.writeServiceError(c, err, "failed to load chat share")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) File(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))
	filePath := strings.TrimSpace(c.Query("file_path"))
	if token == "" || filePath == "" {
		c.Error(apperrors.NewBadRequestError("token and file_path are required"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("chat share service unavailable"))
		return
	}

	reader, contentType, err := h.service.GetSharedFile(ctx, token, filePath)
	if err != nil {
		h.writeServiceError(c, err, "failed to load shared file")
		return
	}
	defer reader.Close()

	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "private, max-age=300")
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		logger.Warnf(ctx, "[chatshare] failed to write shared file: %v", err)
	}
}

func (h *Handler) Artifact(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))
	artifactID := strings.TrimSpace(c.Param("artifact_id"))
	if token == "" || artifactID == "" {
		c.Error(apperrors.NewBadRequestError("token and artifact_id are required"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("chat share service unavailable"))
		return
	}

	file, err := h.service.GetSharedArtifact(ctx, token, artifactID)
	if err != nil {
		h.writeServiceError(c, err, "failed to load shared artifact")
		return
	}
	defer file.Reader.Close()

	fileName := strings.TrimSpace(file.FileName)
	if fileName == "" {
		fileName = "artifact"
	}
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fileName}))
	c.Header("Content-Type", file.ContentType)
	c.Header("Cache-Control", "private, max-age=300")
	if file.FileSize > 0 {
		c.Header("Content-Length", strconv.FormatInt(file.FileSize, 10))
	}
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, file.Reader); err != nil {
		logger.Warnf(ctx, "[chatshare] failed to write shared artifact: %v", err)
	}
}

func (h *Handler) writeServiceError(c *gin.Context, err error, fallback string) {
	switch {
	case stderrors.Is(err, ErrWebLoginRequired):
		c.Error(apperrors.NewUnauthorizedError("web login required"))
	case stderrors.Is(err, gorm.ErrRecordNotFound):
		c.Error(apperrors.NewNotFoundError("chat share not found"))
	default:
		msg := strings.TrimSpace(err.Error())
		if msg == "" {
			msg = fallback
		}
		if strings.Contains(strings.ToLower(msg), "required") ||
			strings.Contains(strings.ToLower(msg), "invalid") ||
			strings.Contains(strings.ToLower(msg), "forbidden") ||
			strings.Contains(strings.ToLower(msg), "tenant") {
			c.Error(apperrors.NewBadRequestError(msg))
			return
		}
		logger.Warnf(c.Request.Context(), "[chatshare] %s: %v", fallback, err)
		c.Error(apperrors.NewInternalServerError(fallback))
	}
}
