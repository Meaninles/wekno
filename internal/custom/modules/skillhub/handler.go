package skillhub

import (
	"errors"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

type Handler struct {
	service *Service
	db      *gorm.DB
}

func NewHandler(service *Service, db *gorm.DB) *Handler {
	return &Handler{service: service, db: db}
}

func (h *Handler) List(c *gin.Context) {
	items, err := h.service.ListForManage(c.Request.Context())
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "total": len(items)})
}

func (h *Handler) ListProfessional(c *gin.Context) {
	items, err := h.service.ListProfessionalForManage(c.Request.Context())
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "total": len(items)})
}

func (h *Handler) ImportProfessional(c *gin.Context) {
	header, err := c.FormFile("package")
	if err != nil {
		c.Error(apperrors.NewBadRequestError("professional skill package is required"))
		return
	}
	file, err := header.Open()
	if err != nil {
		h.fail(c, err)
		return
	}
	defer file.Close()

	item, err := h.service.ImportProfessionalSkill(c.Request.Context(), ProfessionalSkillImportRequest{
		Name:        c.PostForm("name"),
		DisplayName: c.PostForm("display_name"),
		Description: c.PostForm("description"),
		File:        file,
		Filename:    header.Filename,
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": item})
}

func (h *Handler) UpdateProfessional(c *gin.Context) {
	var file multipart.File
	var filename string
	header, err := c.FormFile("package")
	if err == nil && header != nil {
		opened, err := header.Open()
		if err != nil {
			h.fail(c, err)
			return
		}
		defer opened.Close()
		file = opened
		filename = header.Filename
	} else if err != nil && !errors.Is(err, http.ErrMissingFile) {
		h.fail(c, err)
		return
	}
	description, descriptionProvided := c.GetPostForm("description")
	item, err := h.service.UpdateProfessionalSkill(c.Request.Context(), c.Param("id"), ProfessionalSkillUpdateRequest{
		Name:                c.PostForm("name"),
		DisplayName:         c.PostForm("display_name"),
		Description:         description,
		DescriptionProvided: descriptionProvided,
		File:                file,
		Filename:            filename,
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": item})
}

func (h *Handler) DownloadProfessional(c *gin.Context) {
	download, err := h.service.DownloadProfessionalSkill(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.fail(c, err)
		return
	}
	if download.Cleanup != nil {
		defer download.Cleanup()
	}
	c.FileAttachment(download.Path, download.Filename)
}

func (h *Handler) DeleteProfessional(c *gin.Context) {
	if err := h.service.DeleteProfessionalSkill(c.Request.Context(), c.Param("id")); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) ListProfessionalShares(c *gin.Context) {
	shares, err := h.service.ListProfessionalShares(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": shares})
}

func (h *Handler) ShareProfessionalOrganization(c *gin.Context) {
	var req ShareOrganizationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	share, err := h.service.ShareProfessionalToOrganization(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": share})
}

func (h *Handler) ShareProfessionalUser(c *gin.Context) {
	var req ShareUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	share, err := h.service.ShareProfessionalToUser(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": share})
}

func (h *Handler) RemoveProfessionalOrganizationShare(c *gin.Context) {
	if err := h.service.RemoveProfessionalOrganizationShare(c.Request.Context(), c.Param("id"), c.Param("share_id")); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) RemoveProfessionalUserShare(c *gin.Context) {
	if err := h.service.RemoveProfessionalUserShare(c.Request.Context(), c.Param("id"), c.Param("share_id")); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) ListByOrganization(c *gin.Context) {
	items, err := h.service.ListOrganization(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "total": len(items)})
}

func (h *Handler) Create(c *gin.Context) {
	var req SkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	skill, err := h.service.Create(c.Request.Context(), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": skill})
}

func (h *Handler) Update(c *gin.Context) {
	var req SkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	skill, err := h.service.Update(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": skill})
}

func (h *Handler) Delete(c *gin.Context) {
	if err := h.service.Delete(c.Request.Context(), c.Param("id")); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) ListShares(c *gin.Context) {
	shares, err := h.service.ListShares(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": shares})
}

func (h *Handler) ShareOrganization(c *gin.Context) {
	var req ShareOrganizationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	share, err := h.service.ShareToOrganization(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": share})
}

func (h *Handler) ShareUser(c *gin.Context) {
	var req ShareUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	share, err := h.service.ShareToUser(c.Request.Context(), c.Param("id"), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": share})
}

func (h *Handler) RemoveOrganizationShare(c *gin.Context) {
	if err := h.service.RemoveOrganizationShare(c.Request.Context(), c.Param("id"), c.Param("share_id")); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) RemoveUserShare(c *gin.Context) {
	if err := h.service.RemoveUserShare(c.Request.Context(), c.Param("id"), c.Param("share_id")); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) SearchUsers(c *gin.Context) {
	query := strings.ToLower(strings.TrimSpace(c.Query("q")))
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	currentUserID := c.GetString(types.UserIDContextKey.String())
	db := h.db.WithContext(c.Request.Context()).
		Model(&types.User{}).
		Where("is_active = ?", true)
	if currentUserID != "" {
		db = db.Where("id <> ?", currentUserID)
	}
	if query != "" {
		db = db.Where("LOWER(username) LIKE ?", "%"+query+"%")
	}
	var users []types.User
	if err := db.Order("username ASC").Limit(limit).Find(&users).Error; err != nil {
		h.fail(c, err)
		return
	}
	type userOption struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Avatar   string `json:"avatar,omitempty"`
		TenantID uint64 `json:"tenant_id"`
	}
	out := make([]userOption, 0, len(users))
	for _, user := range users {
		out = append(out, userOption{ID: user.ID, Username: user.DisplayNameOrUsername(), Avatar: user.Avatar, TenantID: user.TenantID})
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}

func (h *Handler) fail(c *gin.Context, err error) {
	if err == nil {
		return
	}
	logger.Warnf(c.Request.Context(), "[custom skillhub] request failed: %v", err)
	msg := err.Error()
	switch {
	case isProfessionalSkillNameExistsError(err):
		c.Error(apperrors.NewConflictError(duplicateProfessionalSkillNameMessage))
	case isLightweightSkillNameExistsError(err):
		c.Error(apperrors.NewConflictError(duplicateLightweightSkillNameMessage))
	case strings.Contains(msg, "permission denied"):
		c.Error(apperrors.NewForbiddenError(msg))
	case strings.Contains(msg, "record not found"):
		c.Error(apperrors.NewNotFoundError("skill not found"))
	default:
		c.Error(apperrors.NewBadRequestError(msg))
	}
}
