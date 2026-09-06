package chatuploads

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

type AgentResolver func(*gin.Context, string, string) (*types.CustomAgent, error)
type Handler struct {
	service      *Service
	resolveAgent AgentResolver
}

func NewHandler(service *Service, resolve AgentResolver) *Handler {
	return &Handler{service: service, resolveAgent: resolve}
}

func (h *Handler) Register(group *gin.RouterGroup) {
	r := group.Group("/chat-uploads/sessions/:session_id")
	r.GET("", h.List)
	r.POST("", h.Upload)
	r.GET("/:id", h.Get)
	r.GET("/:id/download", h.Download)
	r.POST("/:id/retry", h.Retry)
	r.POST("/:id/cancel", h.Cancel)
	r.DELETE("/:id", h.Delete)
}

func descriptor(k *types.Knowledge) gin.H {
	return gin.H{"id": k.ID, "upload_id": k.GetMetadata()["chat_upload_id"], "upload_ids": uploadIDs(k), "file_name": k.FileName, "file_type": k.FileType,
		"file_size": k.FileSize, "knowledge_id": k.ID, "knowledge_base_id": k.KnowledgeBaseID,
		"parse_status": k.ParseStatus, "core_status": k.CoreStatus, "ready": ready(k), "error": k.ErrorMessage,
		"processing_generation": k.ProcessingGeneration, "published_generation": k.PublishedGeneration, "updated_at": k.UpdatedAt}
}
func fail(c *gin.Context, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, ErrBusy) {
		status = http.StatusConflict
		c.Header("Retry-After", "2")
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || errors.Is(err, ErrTooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	c.JSON(status, gin.H{"success": false, "error": err.Error()})
}
func (h *Handler) Upload(c *gin.Context) {
	// Authentication/session validation happens before reading a large body.
	if _, err := h.service.session(c.Request.Context(), c.Param("session_id")); err != nil {
		fail(c, err)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxFileBytes+(1<<20))
	if err := c.Request.ParseMultipartForm(4 << 20); err != nil {
		fail(c, err)
		return
	}
	defer c.Request.MultipartForm.RemoveAll()
	files := c.Request.MultipartForm.File["file"]
	if len(files) != 1 || len(c.Request.MultipartForm.File) != 1 {
		fail(c, errors.New("exactly one file per upload request is required"))
		return
	}
	agent, err := h.resolveAgent(c, c.PostForm("agent_id"), files[0].Filename)
	if err != nil {
		fail(c, err)
		return
	}
	k, err := h.service.Upload(c.Request.Context(), c.Param("session_id"), c.PostForm("upload_id"), files[0], agent)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": descriptor(k)})
}
func (h *Handler) List(c *gin.Context) {
	rows, err := h.service.List(c.Request.Context(), c.Param("session_id"))
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		out = append(out, descriptor(row))
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out, "max_file_bytes": MaxFileBytes})
}
func (h *Handler) Get(c *gin.Context) {
	k, err := h.service.Get(c.Request.Context(), c.Param("session_id"), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": descriptor(k)})
}
func (h *Handler) Download(c *gin.Context) {
	k, err := h.service.Get(c.Request.Context(), c.Param("session_id"), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	f, name, _, err := h.service.knowledge.GetKnowledgeFileWithSharedAccess(c.Request.Context(), k.TenantID, k.ID)
	if err != nil {
		fail(c, err)
		return
	}
	defer f.Close()
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	contentType := mime.TypeByExtension(filepath.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}
func (h *Handler) Retry(c *gin.Context) {
	k, err := h.service.Get(c.Request.Context(), c.Param("session_id"), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	if k.ParseStatus != types.ParseStatusFailed && k.ParseStatus != types.ParseStatusCancelled {
		fail(c, errors.New("only failed or cancelled processing can be retried"))
		return
	}
	k, err = h.service.knowledge.ReparseKnowledge(c.Request.Context(), k.ID, nil)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": descriptor(k)})
}
func (h *Handler) Cancel(c *gin.Context) {
	k, err := h.service.Get(c.Request.Context(), c.Param("session_id"), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	k, err = h.service.knowledge.CancelKnowledgeParse(c.Request.Context(), k.ID)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": descriptor(k)})
}
func (h *Handler) Delete(c *gin.Context) {
	k, err := h.service.Get(c.Request.Context(), c.Param("session_id"), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	if err = h.service.knowledge.DeleteKnowledge(c.Request.Context(), k.ID); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true})
}
