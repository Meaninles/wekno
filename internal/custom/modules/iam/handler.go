package iam

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	coretypes "github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type Handler struct {
	service    *Service
	orgService interfaces.OrganizationService
}

func NewHandler(service *Service, orgService interfaces.OrganizationService) *Handler {
	return &Handler{service: service, orgService: orgService}
}

func (h *Handler) GetSetting(c *gin.Context) {
	setting, err := h.service.GetSetting(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": MaskSetting(setting)})
}

func (h *Handler) SaveSetting(c *gin.Context) {
	var req SyncSetting
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, err)
		return
	}
	if err := ValidateLoginBaseURL(req.BaseURL); err != nil {
		writeError(c, err)
		return
	}
	setting, err := h.service.SaveSetting(c.Request.Context(), &req)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": MaskSetting(setting)})
}

func (h *Handler) RunSync(c *gin.Context) {
	run, err := h.service.RunSync(c.Request.Context(), "manual")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error(), "data": run})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": run})
}

func (h *Handler) ListRuns(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	runs, err := h.service.ListRuns(c.Request.Context(), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": runs})
}

func (h *Handler) ListSpaceMemberCandidateOrganizations(c *gin.Context) {
	spaceID, ok := h.requireSpaceAdmin(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "1000"))
	orgs, err := h.service.ListSpaceMemberCandidateOrganizations(
		c.Request.Context(),
		spaceID,
		c.Query("q"),
		limit,
	)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": orgs})
}

func (h *Handler) ListSpaceMemberCandidateUsers(c *gin.Context) {
	spaceID, ok := h.requireSpaceAdmin(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	orgIDs := splitQueryList(c.Query("iam_org_ids"))
	if len(orgIDs) == 0 {
		orgIDs = splitQueryList(c.Query("iam_org_id"))
	}
	users, err := h.service.ListSpaceMemberCandidateUsers(
		c.Request.Context(),
		spaceID,
		c.Query("q"),
		orgIDs,
		limit,
	)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": users})
}

func (h *Handler) GetSSOConfig(c *gin.Context) {
	resp, err := h.service.GetSSOConfig(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) GetSSOAuthorizationURL(c *gin.Context) {
	resp, err := h.service.GetSSOAuthorizationURL(c.Request.Context(), strings.TrimSpace(c.Query("redirect_uri")))
	if err != nil {
		c.JSON(http.StatusBadRequest, SSOAuthURLResponse{Success: false, Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) SSOCallback(c *gin.Context) {
	if providerErr := strings.TrimSpace(c.Query("error")); providerErr != "" {
		c.Redirect(http.StatusFound, "/#oidc_error="+url.QueryEscape(providerErr))
		return
	}
	code := strings.TrimSpace(c.Query("code"))
	state := strings.TrimSpace(c.Query("state"))
	if code == "" || state == "" {
		c.Redirect(http.StatusFound, "/#oidc_error="+url.QueryEscape("missing_code_or_state"))
		return
	}
	resp, err := h.service.LoginWithSSO(c.Request.Context(), code, state)
	if err != nil {
		c.Redirect(http.StatusFound, "/#oidc_error="+url.QueryEscape("login_failed")+"&oidc_error_description="+url.QueryEscape(err.Error()))
		return
	}
	payload, err := EncodeCallbackPayload(resp)
	if err != nil {
		c.Redirect(http.StatusFound, "/#oidc_error="+url.QueryEscape("payload_encode_failed"))
		return
	}
	c.Redirect(http.StatusFound, "/#oidc_result="+url.QueryEscape(payload))
}

func (h *Handler) requireSpaceAdmin(c *gin.Context) (string, bool) {
	spaceID := strings.TrimSpace(c.Query("space_id"))
	if spaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "space_id is required"})
		return "", false
	}
	if h.orgService == nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "organization service is unavailable"})
		return "", false
	}
	tenantID := c.GetUint64(coretypes.TenantIDContextKey.String())
	isAdmin, err := h.orgService.IsTenantOrgAdmin(c.Request.Context(), spaceID, tenantID)
	if err != nil || !isAdmin {
		c.JSON(http.StatusForbidden, gin.H{"success": false, "message": "Only space admins can list IAM member candidates"})
		return "", false
	}
	return spaceID, true
}

func splitQueryList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func writeError(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
}
