package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) SearchSpaces(c *gin.Context) {
	limit, err := parseLimit(c.Query("limit"), 20)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	rows, err := h.service.SearchSpaces(c.Request.Context(), c.Query("q"), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func (h *Handler) SearchUsers(c *gin.Context) {
	limit, err := parseLimit(c.Query("limit"), 50)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	orgIDs := splitList(c.Query("iam_org_ids"))
	if len(orgIDs) == 0 {
		orgIDs = splitList(c.Query("iam_org_id"))
	}
	iamExternalIDs := splitList(c.Query("iam_external_ids"))
	rows, err := h.service.SearchUsers(c.Request.Context(), c.Query("q"), orgIDs, truthy(c.Query("direct")), limit, iamExternalIDs)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func (h *Handler) CreateLocalAccount(c *gin.Context) {
	var req CreateLocalAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "账号参数无效"})
		return
	}
	result, err := h.service.CreateLocalAccount(c.Request.Context(), req)
	if err != nil {
		writeError(c, err)
		return
	}
	actorID := c.GetString(types.UserIDContextKey.String())
	logger.Infof(
		c.Request.Context(),
		"[custom admin] local account created actor=%s target=%s",
		secutils.SanitizeForLog(actorID),
		secutils.SanitizeForLog(result.User.Username),
	)
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": result})
}

type setUserActiveRequest struct {
	Active *bool `json:"active"`
}

type batchSetUsersActiveRequest struct {
	Active         *bool    `json:"active"`
	Query          string   `json:"query"`
	IAMOrgIDs      []string `json:"iam_org_ids"`
	IAMExternalIDs []string `json:"iam_external_ids"`
	Direct         bool     `json:"direct"`
}

func (h *Handler) SetUserActive(c *gin.Context) {
	var req setUserActiveRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Active == nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": ErrInvalidActiveState.Error()})
		return
	}
	actorID := c.GetString(types.UserIDContextKey.String())
	row, err := h.service.SetUserActive(c.Request.Context(), c.Param("id"), *req.Active, actorID)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": row})
}

func (h *Handler) BatchSetUsersActive(c *gin.Context) {
	var req batchSetUsersActiveRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Active == nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": ErrInvalidActiveState.Error()})
		return
	}
	actorID := c.GetString(types.UserIDContextKey.String())
	result, err := h.service.BatchSetUsersActive(
		c.Request.Context(),
		req.Query,
		req.IAMOrgIDs,
		req.IAMExternalIDs,
		req.Direct,
		*req.Active,
		actorID,
	)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func writeError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := err.Error()
	switch {
	case errors.Is(err, ErrQueryRequired),
		errors.Is(err, ErrInvalidActiveState),
		errors.Is(err, ErrInvalidUsername),
		errors.Is(err, ErrDisplayNameTooLong):
		status = http.StatusBadRequest
	case errors.Is(err, ErrLocalUsernameExists), errors.Is(err, ErrIAMUsernameExists):
		status = http.StatusConflict
	case errors.Is(err, ErrCannotDisableSelf), errors.Is(err, ErrLastActiveSystemAdmin):
		status = http.StatusForbidden
	case errors.Is(err, ErrUserNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrAccountCreateFailed):
		logger.Errorf(c.Request.Context(), "[custom admin] local account creation failed: %v", err)
		message = ErrAccountCreateFailed.Error()
	}
	c.JSON(status, gin.H{"success": false, "message": message})
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

func truthy(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "1" || value == "true" || value == "yes"
}
