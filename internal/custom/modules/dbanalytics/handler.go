package dbanalytics

import (
	"errors"
	"net/http"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func tenantID(c *gin.Context) uint64 {
	return c.GetUint64(types.TenantIDContextKey.String())
}

func userID(c *gin.Context) string {
	if v, ok := c.Get(types.UserIDContextKey.String()); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func tenantRole(c *gin.Context) types.TenantRole {
	return types.TenantRoleFromContext(c.Request.Context())
}

func (h *Handler) ListSources(c *gin.Context) {
	items, err := h.service.ListSources(c.Request.Context(), tenantID(c), tenantRole(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "total": len(items)})
}

func (h *Handler) CreateSource(c *gin.Context) {
	var req CreateSourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	src, err := h.service.CreateSource(c.Request.Context(), tenantID(c), userID(c), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": src.Response(true)})
}

func (h *Handler) TestSourceConfig(c *gin.Context) {
	var req TestSourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	if err := h.service.TestSourceConfig(c.Request.Context(), req); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) GetSource(c *gin.Context) {
	src, err := h.service.GetAccessibleSourceWithTables(c.Request.Context(), tenantID(c), c.Param("id"), tenantRole(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	includeConfig := src.TenantID == tenantID(c)
	resp := src.Response(includeConfig)
	resp.Shared = src.TenantID != tenantID(c)
	resp.SourceTenantID = src.TenantID
	resp.IsMine = src.TenantID == tenantID(c)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp, "tables": src.Tables})
}

func (h *Handler) UpdateSource(c *gin.Context) {
	var req UpdateSourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	src, err := h.service.UpdateSource(c.Request.Context(), tenantID(c), c.Param("id"), tenantRole(c), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": src.Response(true)})
}

func (h *Handler) DeleteSource(c *gin.Context) {
	if err := h.service.DeleteSource(c.Request.Context(), tenantID(c), c.Param("id")); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) TestSource(c *gin.Context) {
	if err := h.service.TestConnection(c.Request.Context(), tenantID(c), c.Param("id"), tenantRole(c)); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) ListSchemas(c *gin.Context) {
	items, err := h.service.ListSchemas(c.Request.Context(), tenantID(c), c.Param("id"), tenantRole(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "total": len(items)})
}

func (h *Handler) ListTables(c *gin.Context) {
	items, err := h.service.ListTables(c.Request.Context(), tenantID(c), c.Param("id"), c.Query("schema"), tenantRole(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "total": len(items)})
}

func (h *Handler) RefreshMetadata(c *gin.Context) {
	if err := h.service.RefreshMetadata(c.Request.Context(), tenantID(c), c.Param("id"), tenantRole(c)); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) SetTableScope(c *gin.Context) {
	var req SetTableScopeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	if err := h.service.SetTableScope(c.Request.Context(), tenantID(c), c.Param("id"), tenantRole(c), req.TableIDs); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) UpdateColumn(c *gin.Context) {
	var req UpdateColumnRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	col, err := h.service.UpdateColumn(c.Request.Context(), tenantID(c), c.Param("column_id"), tenantRole(c), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": col})
}

func (h *Handler) GetAgentBindings(c *gin.Context) {
	ids, err := h.service.GetAgentBindings(c.Request.Context(), tenantID(c), c.Param("agent_id"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": ids})
}

func (h *Handler) SetAgentBindings(c *gin.Context) {
	var req AgentBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	if err := h.service.SetAgentBindings(c.Request.Context(), tenantID(c), c.Param("agent_id"), req.SourceIDs); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) ShareSource(c *gin.Context) {
	var req ShareSourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	share, err := h.service.ShareSource(c.Request.Context(), c.Param("id"), req.OrganizationID, userID(c), tenantID(c), req.Permission)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": share})
}

func (h *Handler) ListSourceShares(c *gin.Context) {
	shares, err := h.service.ListSharesBySource(c.Request.Context(), c.Param("id"), tenantID(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"shares": shares, "total": len(shares)}})
}

func (h *Handler) UpdateSourceSharePermission(c *gin.Context) {
	var req UpdateSourceSharePermissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	if err := h.service.UpdateSharePermission(c.Request.Context(), c.Param("share_id"), req.Permission, userID(c), tenantID(c)); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) RemoveSourceShare(c *gin.Context) {
	if err := h.service.RemoveShare(c.Request.Context(), c.Param("share_id"), userID(c), tenantID(c)); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) ListSharedSources(c *gin.Context) {
	items, err := h.service.ListSharedSources(c.Request.Context(), tenantID(c), tenantRole(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "total": len(items)})
}

func (h *Handler) ListOrganizationSharedSources(c *gin.Context) {
	items, err := h.service.ListOrganizationSourcesIncludingAgent(c.Request.Context(), c.Param("id"), tenantID(c), tenantRole(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "total": len(items)})
}

func (h *Handler) fail(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, gorm.ErrRecordNotFound) {
		status = http.StatusNotFound
	}
	switch {
	case errors.Is(err, ErrSourceNotFound), errors.Is(err, ErrSourceShareNotFound), errors.Is(err, ErrSourceOrgNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrNotSourceOwner), errors.Is(err, ErrSourceShareDenied), errors.Is(err, ErrSourceTenantNotInOrg), errors.Is(err, ErrSourceOrgRoleCannotShare):
		status = http.StatusForbidden
	case errors.Is(err, ErrInvalidSharePermission), errors.Is(err, ErrSourceConnectionInvalid), errors.Is(err, ErrSourceReadOnlyRequired):
		status = http.StatusBadRequest
	}
	c.JSON(status, gin.H{"success": false, "error": err.Error()})
}
