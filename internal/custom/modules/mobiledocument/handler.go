package mobiledocument

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

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

func (h *Handler) CreateDownloadLink(c *gin.Context) {
	knowledgeID := strings.TrimSpace(c.Param("knowledge_id"))
	if knowledgeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "knowledge_id is required"})
		return
	}
	downloadURL, expiresAt, err := h.service.Issue(c.Request.Context(), knowledgeID)
	if err != nil {
		logger.Warnf(c.Request.Context(), "[mobile document] failed to issue download link: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "暂时无法创建下载链接，请稍后重试",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"url":        downloadURL,
			"expires_at": expiresAt.Format(timeLayout),
		},
	})
}

func (h *Handler) CreateArtifactDownloadLink(c *gin.Context) {
	artifactID := strings.TrimSpace(c.Param("artifact_id"))
	if artifactID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "artifact_id is required"})
		return
	}
	tenantID, tenantOK := types.TenantIDFromContext(c.Request.Context())
	userID, userOK := types.UserIDFromContext(c.Request.Context())
	if !tenantOK || tenantID == 0 || !userOK || strings.TrimSpace(userID) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "登录状态无效"})
		return
	}
	downloadURL, expiresAt, err := h.service.IssueArtifact(
		c.Request.Context(),
		artifactID,
		tenantID,
		userID,
	)
	if err != nil {
		status := http.StatusInternalServerError
		message := "暂时无法创建下载链接，请稍后重试"
		if errors.Is(err, ErrArtifactUnavailable) {
			status = http.StatusNotFound
			message = "产物文件暂时不可用"
		}
		logger.Warnf(c.Request.Context(), "[mobile document] failed to issue artifact download link: %v", err)
		c.JSON(status, gin.H{"success": false, "message": message})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"url":        downloadURL,
			"expires_at": expiresAt.Format(timeLayout),
		},
	})
}

func (h *Handler) Download(c *gin.Context) {
	ctx := c.Request.Context()
	record, err := h.service.Resolve(ctx, c.Request.URL.Query())
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, ErrExpiredTicket) {
			status = http.StatusGone
		}
		logger.Warnf(ctx, "[mobile document] rejected signed download: %v", err)
		c.JSON(status, gin.H{"success": false, "message": "下载链接无效或已过期"})
		return
	}

	reader, filename, err := h.service.Open(ctx, record)
	if err != nil {
		logger.Warnf(ctx, "[mobile document] failed to open knowledge file id=%s: %v", record.ID, err)
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "文件暂时不可用"})
		return
	}
	defer reader.Close()

	filename = cleanDownloadFilename(filename)
	fileSize := record.FileSize
	if record.IsManual() {
		fileSize = 0
	}
	setDownloadHeaders(c, filename, fileSize)
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		logger.Warnf(ctx, "[mobile document] failed to stream knowledge file id=%s: %v", record.ID, err)
	}
}

func (h *Handler) DownloadArtifact(c *gin.Context) {
	ctx := c.Request.Context()
	file, err := h.service.ResolveArtifact(ctx, c.Request.URL.Query())
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, ErrExpiredTicket) {
			status = http.StatusGone
		}
		logger.Warnf(ctx, "[mobile document] rejected signed artifact download: %v", err)
		c.JSON(status, gin.H{"success": false, "message": "下载链接无效或已过期"})
		return
	}

	reader, err := h.service.OpenArtifact(ctx, file)
	if err != nil {
		logger.Warnf(ctx, "[mobile document] failed to open artifact id=%s: %v", file.ID, err)
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "产物文件暂时不可用"})
		return
	}
	defer reader.Close()

	filename := cleanDownloadFilename(file.FileName)
	setDownloadHeaders(c, filename, file.FileSize)
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		logger.Warnf(ctx, "[mobile document] failed to stream artifact id=%s: %v", file.ID, err)
	}
}

func setDownloadHeaders(c *gin.Context, filename string, fileSize int64) {
	c.Header("Content-Disposition", contentDisposition(filename))
	c.Header("Content-Type", contentTypeForDownload(filename))
	c.Header("Content-Description", "File Transfer")
	c.Header("Content-Transfer-Encoding", "binary")
	c.Header("Cache-Control", "private, no-store, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	c.Header("X-Content-Type-Options", "nosniff")
	if fileSize > 0 {
		c.Header("Content-Length", strconv.FormatInt(fileSize, 10))
	}
}

const timeLayout = "2006-01-02T15:04:05Z07:00"

func cleanDownloadFilename(filename string) string {
	filename = strings.TrimSpace(strings.ReplaceAll(filename, "\\", "/"))
	filename = filepath.Base(filename)
	filename = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, filename)
	if filename == "" || filename == "." {
		return "download"
	}
	return filename
}

func contentDisposition(filename string) string {
	fallback := asciiFilenameFallback(filename)
	encoded := url.PathEscape(filename)
	return fmt.Sprintf(
		`attachment; filename="%s"; filename*=UTF-8''%s`,
		strings.ReplaceAll(fallback, `"`, "_"),
		encoded,
	)
}

func asciiFilenameFallback(filename string) string {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	var builder strings.Builder
	for _, r := range base {
		switch {
		case r >= 0x20 && r <= 0x7e && r != '"' && r != '\\' && r != ';':
			builder.WriteRune(r)
		}
	}
	value := strings.Trim(builder.String(), " ._")
	if value == "" {
		value = "download"
	}
	if len(ext) > 12 || strings.IndexFunc(ext, func(r rune) bool {
		return r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == ';'
	}) >= 0 {
		ext = ""
	}
	return value + ext
}

func contentTypeForDownload(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	known := map[string]string{
		".pdf":  "application/pdf",
		".doc":  "application/msword",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".ppt":  "application/vnd.ms-powerpoint",
		".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		".xls":  "application/vnd.ms-excel",
		".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".csv":  "text/csv; charset=utf-8",
		".txt":  "text/plain; charset=utf-8",
		".md":   "text/markdown; charset=utf-8",
		".json": "application/json; charset=utf-8",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".webp": "image/webp",
		".svg":  "image/svg+xml",
		".mp3":  "audio/mpeg",
		".wav":  "audio/wav",
		".m4a":  "audio/mp4",
	}
	if value := known[ext]; value != "" {
		return value
	}
	if value := mime.TypeByExtension(ext); value != "" {
		return value
	}
	return "application/octet-stream"
}
