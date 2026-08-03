package imoutput

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/gin-gonic/gin"
)

// ReferenceRedirectPath is the single device-neutral capability URL emitted
// into IM messages. It is registered before the normal Web authentication
// middleware and is never used by ordinary Web-chat citations.
const ReferenceRedirectPath = "/api/v1/custom/im-output/reference"

const (
	ReferenceDataPath     = ReferenceRedirectPath + "/data"
	ReferenceOriginalPath = ReferenceRedirectPath + "/original"
)

var mobileReferenceUserAgentRE = regexp.MustCompile(`(?i)(android|iphone|ipad|ipod|windows phone|harmonyos|openharmony|\bmobile\b)`)

type ReferenceHandler struct {
	service *ReferenceService
}

func NewReferenceHandler(service *ReferenceService) *ReferenceHandler {
	return &ReferenceHandler{service: service}
}

// Redirect verifies the IM-only capability before selecting a separate public
// reader for the current device. It never accepts raw source coordinates or an
// arbitrary redirect target.
func (h *ReferenceHandler) Redirect(c *gin.Context) {
	token := strings.TrimSpace(c.Query("token"))
	if token == "" {
		writeReferenceError(c, http.StatusBadRequest)
		return
	}
	if h == nil || h.service == nil {
		writeReferenceError(c, http.StatusServiceUnavailable)
		return
	}
	if _, err := h.service.VerifyToken(token); err != nil {
		writeReferenceServiceError(c, err)
		return
	}
	query := url.Values{"token": []string{token}}
	destination := "/im-reference?" + query.Encode()
	if isMobileReferenceRequest(c.Request) {
		destination = "/mobile/reference?" + query.Encode()
	}
	setPublicReferenceHeaders(c)
	c.Header("Vary", "User-Agent, Sec-CH-UA-Mobile")
	c.Redirect(http.StatusFound, destination)
}

func (h *ReferenceHandler) Data(c *gin.Context) {
	token := strings.TrimSpace(c.Query("token"))
	if token == "" {
		writeReferenceError(c, http.StatusBadRequest)
		return
	}
	if h == nil || h.service == nil {
		writeReferenceError(c, http.StatusServiceUnavailable)
		return
	}
	view, err := h.service.Resolve(c.Request.Context(), token)
	if err != nil {
		writeReferenceServiceError(c, err)
		return
	}
	setPublicReferenceHeaders(c)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": view})
}

func (h *ReferenceHandler) Original(c *gin.Context) {
	token := strings.TrimSpace(c.Query("token"))
	if token == "" {
		writeReferenceError(c, http.StatusBadRequest)
		return
	}
	if h == nil || h.service == nil {
		writeReferenceError(c, http.StatusServiceUnavailable)
		return
	}
	var file *OriginalReferenceFile
	var err error
	if c.Request.Method == http.MethodHead {
		file, err = h.service.DescribeOriginal(c.Request.Context(), token)
	} else {
		file, err = h.service.OpenOriginal(c.Request.Context(), token)
	}
	if err != nil {
		writeReferenceServiceError(c, err)
		return
	}
	if file.Reader != nil {
		defer file.Reader.Close()
	}

	fileName := cleanReferenceFilename(file.FileName)
	contentType := referenceContentType(fileName, file.FileType)
	disposition := "inline"
	if strings.TrimSpace(c.Query("download")) == "1" {
		disposition = "attachment"
	}
	setPublicReferenceHeaders(c)
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": fileName}))
	c.Header("Content-Security-Policy", "sandbox")
	c.Header("X-Content-Type-Options", "nosniff")
	if file.FileSize > 0 {
		c.Header("X-Reference-File-Size", strconv.FormatInt(file.FileSize, 10))
	}
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, file.Reader); err != nil {
		logger.Warnf(c.Request.Context(), "[IM reference] failed to stream original document: %v", err)
	}
}

func writeReferenceServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrReferenceSigningKeyUnavailable):
		writeReferenceError(c, http.StatusServiceUnavailable)
	case errors.Is(err, ErrInvalidReferenceCapability), isReferenceNotFound(err):
		writeReferenceError(c, http.StatusNotFound)
	default:
		logger.Warnf(c.Request.Context(), "[IM reference] public reference failed: %v", err)
		writeReferenceError(c, http.StatusNotFound)
	}
}

func writeReferenceError(c *gin.Context, status int) {
	setPublicReferenceHeaders(c)
	message := "引用链接无效或内容已不可用"
	if status == http.StatusServiceUnavailable {
		message = "引用服务暂时不可用"
	}
	c.JSON(status, gin.H{"success": false, "message": message})
}

func setPublicReferenceHeaders(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Robots-Tag", "noindex, nofollow, noarchive")
}

func isMobileReferenceRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-CH-UA-Mobile")), "?1") {
		return true
	}
	return mobileReferenceUserAgentRE.MatchString(r.UserAgent())
}

func cleanReferenceFilename(value string) string {
	value = filepath.Base(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
	value = strings.Map(func(r rune) rune {
		if r == 0 || r == '\r' || r == '\n' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if value == "" || value == "." {
		return "document"
	}
	return value
}

func referenceContentType(fileName, fileType string) string {
	ext := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fileType)), ".")
	if ext == "" {
		ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(fileName)), ".")
	}
	known := map[string]string{
		"pdf": "application/pdf", "doc": "application/msword",
		"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"ppt":  "application/vnd.ms-powerpoint",
		"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"xls":  "application/vnd.ms-excel",
		"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"csv":  "text/csv; charset=utf-8", "txt": "text/plain; charset=utf-8",
		"md": "text/markdown; charset=utf-8", "markdown": "text/markdown; charset=utf-8",
		"json": "application/json; charset=utf-8", "png": "image/png", "jpg": "image/jpeg",
		"jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp", "svg": "image/svg+xml",
		"mp3": "audio/mpeg", "wav": "audio/wav", "m4a": "audio/mp4",
	}
	if value := known[ext]; value != "" {
		return value
	}
	if value := mime.TypeByExtension("." + ext); value != "" {
		return value
	}
	return "application/octet-stream"
}
