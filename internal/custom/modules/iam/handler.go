package iam

import (
	"fmt"
	"net"
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

func (h *Handler) SSOEntry(c *gin.Context) {
	redirectURI, err := ssoCallbackURLFromRequest(c.Request)
	if err != nil {
		c.Redirect(http.StatusFound, "/#oidc_error="+url.QueryEscape("entry_failed")+"&oidc_error_description="+url.QueryEscape(err.Error()))
		return
	}
	frontendRedirect, err := ssoFrontendRedirectFromRequest(c.Request)
	if err != nil {
		c.Redirect(http.StatusFound, "/#oidc_error="+url.QueryEscape("entry_failed")+"&oidc_error_description="+url.QueryEscape(err.Error()))
		return
	}
	resp, err := h.service.GetSSOAuthorizationURL(c.Request.Context(), redirectURI, frontendRedirect)
	if err != nil {
		c.Redirect(http.StatusFound, ssoBrowserRedirect(frontendRedirect, url.Values{
			"oidc_error":             {"entry_failed"},
			"oidc_error_description": {err.Error()},
		}))
		return
	}
	if resp == nil || strings.TrimSpace(resp.AuthorizationURL) == "" {
		c.Redirect(http.StatusFound, ssoBrowserRedirect(frontendRedirect, url.Values{
			"oidc_error":             {"entry_failed"},
			"oidc_error_description": {"IAM authorization URL is empty"},
		}))
		return
	}
	c.Redirect(http.StatusFound, resp.AuthorizationURL)
}

func (h *Handler) SSOCallback(c *gin.Context) {
	frontendRedirect := ssoFrontendRedirectFromState(strings.TrimSpace(c.Query("state")))
	if providerErr := strings.TrimSpace(c.Query("error")); providerErr != "" {
		c.Redirect(http.StatusFound, ssoBrowserRedirect(frontendRedirect, url.Values{"oidc_error": {providerErr}}))
		return
	}
	code := strings.TrimSpace(c.Query("code"))
	state := strings.TrimSpace(c.Query("state"))
	if code == "" || state == "" {
		c.Redirect(http.StatusFound, ssoBrowserRedirect(frontendRedirect, url.Values{"oidc_error": {"missing_code_or_state"}}))
		return
	}
	resp, err := h.service.LoginWithSSO(c.Request.Context(), code, state)
	if err != nil {
		c.Redirect(http.StatusFound, ssoBrowserRedirect(frontendRedirect, url.Values{
			"oidc_error":             {"login_failed"},
			"oidc_error_description": {err.Error()},
		}))
		return
	}
	payload, err := EncodeCallbackPayload(resp)
	if err != nil {
		c.Redirect(http.StatusFound, ssoBrowserRedirect(frontendRedirect, url.Values{"oidc_error": {"payload_encode_failed"}}))
		return
	}
	c.Redirect(http.StatusFound, ssoBrowserRedirect(frontendRedirect, url.Values{"oidc_result": {payload}}))
}

func ssoCallbackURLFromRequest(r *http.Request) (string, error) {
	origin, err := externalOriginFromRequest(r)
	if err != nil {
		return "", err
	}
	return origin + "/api/v1/custom/iam/sso/callback", nil
}

func ssoFrontendRedirectFromRequest(r *http.Request) (string, error) {
	origin, err := externalOriginFromRequest(r)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	host, port := splitRequestHost(parsed.Host)
	if isLocalRequestHost(parsed.Host) && port == "8080" {
		return parsed.Scheme + "://" + formatHostPort(host, "5177") + "/", nil
	}
	return origin + "/", nil
}

func externalOriginFromRequest(r *http.Request) (string, error) {
	if r == nil {
		return "", fmt.Errorf("request is nil")
	}
	host := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = forwardedHeaderParam(r.Header.Get("Forwarded"), "host")
	}
	if host == "" {
		host = r.Host
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("request host is empty")
	}
	proto := firstHeaderValue(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		proto = forwardedHeaderParam(r.Header.Get("Forwarded"), "proto")
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		if r.TLS != nil || !isLocalRequestHost(host) {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	if proto != "http" && proto != "https" {
		return "", fmt.Errorf("unsupported request scheme %q", proto)
	}
	return proto + "://" + host, nil
}

func ssoFrontendRedirectFromState(state string) string {
	if strings.TrimSpace(state) == "" {
		return ""
	}
	payload, err := decodeSSOState(state)
	if err != nil || payload == nil {
		return ""
	}
	return strings.TrimSpace(payload.FrontendRedirect)
}

func ssoBrowserRedirect(frontendRedirect string, params url.Values) string {
	target := strings.TrimSpace(frontendRedirect)
	if target == "" {
		target = "/"
	}
	if idx := strings.Index(target, "#"); idx >= 0 {
		target = target[:idx]
	}
	if target == "" {
		target = "/"
	}
	return target + "#" + params.Encode()
}

func firstHeaderValue(value string) string {
	if idx := strings.Index(value, ","); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func forwardedHeaderParam(header string, key string) string {
	header = firstHeaderValue(header)
	if header == "" {
		return ""
	}
	key = strings.ToLower(key)
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || strings.ToLower(strings.TrimSpace(name)) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}

func isLocalRequestHost(host string) bool {
	host, _ = splitRequestHost(host)
	host = strings.ToLower(host)
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func splitRequestHost(host string) (string, string) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", ""
	}
	if parsedHost, port, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(parsedHost, "[]"), port
	}
	if strings.HasPrefix(host, "[") {
		if idx := strings.Index(host, "]"); idx >= 0 {
			return host[1:idx], ""
		}
	}
	if strings.Count(host, ":") == 1 {
		parts := strings.SplitN(host, ":", 2)
		return parts[0], parts[1]
	}
	return strings.Trim(host, "[]"), ""
}

func formatHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
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
