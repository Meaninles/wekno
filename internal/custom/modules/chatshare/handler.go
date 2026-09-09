package chatshare

import (
	stderrors "errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func (h *Handler) Candidates(c *gin.Context) {
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

	result, err := h.service.GetCandidates(ctx, sessionID)
	if err != nil {
		h.writeServiceError(c, err, "failed to load chat share candidates")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
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
	var req CreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError("message_ids is required"))
		return
	}

	result, err := h.service.CreateShare(ctx, sessionID, req.MessageIDs)
	if err != nil {
		h.writeServiceError(c, err, "failed to create chat share")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) CreateArtifactShare(c *gin.Context) {
	ctx := c.Request.Context()
	artifactID := strings.TrimSpace(c.Param("artifact_id"))
	if artifactID == "" {
		c.Error(apperrors.NewBadRequestError("artifact_id is required"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("artifact share service unavailable"))
		return
	}

	result, err := h.service.CreateArtifactShare(ctx, artifactID)
	if err != nil {
		h.writeArtifactShareError(c, err, "failed to create artifact share")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) SetArtifactSharePassword(c *gin.Context) {
	ctx := c.Request.Context()
	artifactID := strings.TrimSpace(c.Param("artifact_id"))
	if artifactID == "" {
		c.Error(apperrors.NewBadRequestError("artifact_id is required"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("artifact share service unavailable"))
		return
	}
	var req ArtifactSharePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError("share password is required"))
		return
	}
	result, err := h.service.SetArtifactSharePassword(ctx, artifactID, req.Password)
	if err != nil {
		h.writeArtifactShareError(c, err, "failed to set artifact share password")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// ArtifactShare serves metadata for the public password-protected share flow.
// Preview requests use ArtifactSharePreview so they can go through the normal
// authenticated route boundary and enforce creator ownership.
func (h *Handler) ArtifactShare(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))
	accessToken := strings.TrimSpace(c.Query("access"))
	if token == "" {
		c.Error(apperrors.NewNotFoundError("artifact share not found"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("artifact share service unavailable"))
		return
	}
	if strings.TrimSpace(c.Query("preview")) != "" {
		c.Error(apperrors.NewUnauthorizedError("artifact preview requires login"))
		return
	}

	result, err := h.service.GetArtifactShare(ctx, token, "", accessToken, c.ClientIP())
	if err != nil {
		h.writeArtifactShareError(c, err, "failed to load artifact share")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// ArtifactSharePreview serves the creator-only preview metadata. It is
// registered after the global auth middleware and additionally checks that the
// authenticated web user owns the preview capability.
func (h *Handler) ArtifactSharePreview(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))
	previewToken := strings.TrimSpace(c.Query("preview"))
	if token == "" || previewToken == "" {
		c.Error(apperrors.NewNotFoundError("artifact share not found"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("artifact share service unavailable"))
		return
	}

	result, err := h.service.GetArtifactShare(ctx, token, previewToken, "", "")
	if err != nil {
		h.writeArtifactShareError(c, err, "failed to load artifact preview")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) ArtifactShareAccess(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		c.Error(apperrors.NewNotFoundError("artifact share not found"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("artifact share service unavailable"))
		return
	}
	var req ArtifactSharePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewUnauthorizedError("share password is incorrect"))
		return
	}
	result, err := h.service.VerifyArtifactSharePassword(ctx, token, req.Password, c.ClientIP())
	if err != nil {
		h.writeArtifactShareError(c, err, "failed to verify artifact share password")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

// ArtifactShareContent streams the HTML body for the public password share.
// Creator preview content uses ArtifactSharePreviewContent so it can require
// the authenticated creator without changing the public password flow.
func (h *Handler) ArtifactShareContent(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))
	accessToken := strings.TrimSpace(c.Query("access"))
	if token == "" {
		c.Error(apperrors.NewNotFoundError("artifact share not found"))
		return
	}
	if strings.TrimSpace(c.Query("preview")) != "" {
		c.Error(apperrors.NewUnauthorizedError("artifact preview requires login"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("artifact share service unavailable"))
		return
	}

	file, err := h.service.GetArtifactShareContent(ctx, token, "", accessToken)
	if err != nil {
		h.writeArtifactShareError(c, err, "failed to load artifact share content")
		return
	}
	defer file.Reader.Close()
	h.writeArtifactShareContent(c, file)
}

func (h *Handler) writeArtifactShareContent(c *gin.Context, file *SharedArtifactFile) {
	if file == nil || file.Reader == nil {
		c.Error(apperrors.NewInternalServerError("artifact share content unavailable"))
		return
	}
	fileName := strings.TrimSpace(file.FileName)
	if fileName == "" {
		fileName = "artifact.html"
	}
	c.Header("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": fileName}))
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Content-Length", strconv.FormatInt(file.FileSize, 10))
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	// The HTML is rendered inside the isolated share view iframe. Inline
	// scripts are allowed for generated charts, while network, forms, plugins
	// and parent-window access remain disabled by default.
	// Keep the artifact isolated even if somebody opens the content URL
	// directly instead of through ArtifactShareView's sandboxed iframe.
	c.Header("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data: blob:; connect-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'")
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, file.Reader); err != nil {
		logger.Warnf(c.Request.Context(), "[chatshare] failed to write artifact share content: %v", err)
	}
}

// ArtifactSharePreviewContent streams creator-only preview content. The
// service repeats the ownership check so the content endpoint cannot be used
// to bypass the metadata endpoint.
func (h *Handler) ArtifactSharePreviewContent(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))
	previewToken := strings.TrimSpace(c.Query("preview"))
	if token == "" || previewToken == "" {
		c.Error(apperrors.NewNotFoundError("artifact share not found"))
		return
	}
	if h == nil || h.service == nil {
		c.Error(apperrors.NewInternalServerError("artifact share service unavailable"))
		return
	}

	file, err := h.service.GetArtifactShareContent(ctx, token, previewToken, "")
	if err != nil {
		h.writeArtifactShareError(c, err, "failed to load artifact preview content")
		return
	}
	defer file.Reader.Close()
	h.writeArtifactShareContent(c, file)
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
	case stderrors.Is(err, ErrInvalidMessageSelection):
		c.Error(apperrors.NewBadRequestError("invalid message selection"))
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

func (h *Handler) writeArtifactShareError(c *gin.Context, err error, fallback string) {
	var attemptErr *ArtifactSharePasswordAttemptError
	if stderrors.As(err, &attemptErr) {
		details := gin.H{}
		if attemptErr.Status != nil {
			details["remaining_attempts"] = attemptErr.Status.Remaining
			details["max_attempts"] = attemptErr.Status.MaxFailures
			if attemptErr.Status.LockedUntil != nil {
				details["locked_until"] = attemptErr.Status.LockedUntil.UTC().Format(time.RFC3339)
				retryAfter := int(time.Until(*attemptErr.Status.LockedUntil).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				details["retry_after_seconds"] = retryAfter
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
		}
		if stderrors.Is(err, ErrArtifactSharePasswordLocked) {
			c.Error(apperrors.NewTooManyRequestsError("share password attempts are temporarily locked").WithDetails(details))
			return
		}
		c.Error(apperrors.NewUnauthorizedError("share password is incorrect").WithDetails(details))
		return
	}

	switch {
	case stderrors.Is(err, ErrWebLoginRequired):
		c.Error(apperrors.NewUnauthorizedError("web login required"))
	case stderrors.Is(err, ErrArtifactSharePreviewForbidden):
		c.Error(apperrors.NewForbiddenError("artifact preview is only available to its creator"))
	case stderrors.Is(err, ErrArtifactSharePreviewInvalid):
		c.Error(apperrors.NewNotFoundError("artifact preview not found or expired"))
	case stderrors.Is(err, ErrArtifactShareUnsupported):
		c.Error(apperrors.NewBadRequestError("only HTML artifacts can be shared"))
	case stderrors.Is(err, ErrArtifactSharePasswordRequired):
		c.Error(apperrors.NewUnauthorizedError("share password is required"))
	case stderrors.Is(err, ErrArtifactSharePasswordInvalid):
		c.Error(apperrors.NewUnauthorizedError("share password is incorrect"))
	case stderrors.Is(err, ErrArtifactSharePasswordLocked):
		c.Error(apperrors.NewTooManyRequestsError("share password attempts are temporarily locked"))
	case stderrors.Is(err, ErrArtifactSharePasswordTooShort):
		c.Error(apperrors.NewBadRequestError("share password must be at least 6 characters"))
	case stderrors.Is(err, ErrArtifactSharePasswordTooLong):
		c.Error(apperrors.NewBadRequestError("share password is too long"))
	case stderrors.Is(err, ErrArtifactShareNotPublished):
		c.Error(apperrors.NewNotFoundError("artifact share not found"))
	case stderrors.Is(err, ErrArtifactShareNotFound),
		stderrors.Is(err, ErrArtifactShareRevoked),
		stderrors.Is(err, gorm.ErrRecordNotFound):
		c.Error(apperrors.NewNotFoundError("artifact share not found"))
	default:
		logger.Warnf(c.Request.Context(), "[chatshare] %s: %v", fallback, err)
		c.Error(apperrors.NewInternalServerError(fallback))
	}
}
