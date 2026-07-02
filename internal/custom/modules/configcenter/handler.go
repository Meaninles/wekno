package configcenter

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

type saveRefsRequest struct {
	Grants []ResourceRef `json:"grants"`
}

func (h *Handler) ListUsers(c *gin.Context) {
	users, err := h.service.ListUsers(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": users})
}

func (h *Handler) ListResources(c *gin.Context) {
	sourceUserID := c.Query("source_user_id")
	items, err := h.service.ListSourceResourcesForUser(c.Request.Context(), sourceUserID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items})
}

func (h *Handler) GetDefaults(c *gin.Context) {
	grants, err := h.service.GetDefaults(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": grants})
}

func (h *Handler) SaveDefaults(c *gin.Context) {
	var req saveRefsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, err)
		return
	}
	if err := h.service.SaveDefaults(c.Request.Context(), req.Grants); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) GetUserGrants(c *gin.Context) {
	grants, err := h.service.GetUserGrants(c.Request.Context(), c.Param("user_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": grants})
}

func (h *Handler) SaveUserGrants(c *gin.Context) {
	var req saveRefsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, err)
		return
	}
	if err := h.service.SaveUserGrants(c.Request.Context(), c.Param("user_id"), req.Grants); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) ApplyAll(c *gin.Context) {
	result, err := h.service.ApplyAll(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func (h *Handler) ApplyUser(c *gin.Context) {
	result, err := h.service.ApplyUserByID(c.Request.Context(), c.Param("user_id"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func writeError(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{
		"success": false,
		"message": err.Error(),
	})
}
