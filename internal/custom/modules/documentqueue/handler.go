package documentqueue

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

type Handler struct {
	coordinator *Coordinator
}

type queueStatusRequest struct {
	KnowledgeIDs []string `json:"knowledge_ids"`
}

func NewHandler(coordinator *Coordinator) *Handler {
	return &Handler{coordinator: coordinator}
}

// Status returns a global waiting count and tenant-scoped positions for the
// requested document cards. Ranking is calculated globally by the service
// before tenant filtering, so position never leaks another document's details.
func (h *Handler) Status(c *gin.Context) {
	ids := make([]string, 0, 100)
	for _, raw := range c.QueryArray("knowledge_ids") {
		ids = append(ids, strings.Split(raw, ",")...)
	}
	if len(ids) == 0 {
		ids = strings.Split(c.Query("knowledge_ids"), ",")
	}
	h.status(c, ids)
}

// StatusBatch accepts all visible document IDs in one request body. Queue
// positions are a point-in-time global rank, so splitting a large card set
// across sequential GETs can legitimately observe different snapshots and
// produce duplicate merged positions. A single POST avoids that race and also
// stays below proxy request-line limits.
func (h *Handler) StatusBatch(c *gin.Context) {
	var request queueStatusRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError("knowledge_ids must be a JSON array"))
		return
	}
	h.status(c, request.KnowledgeIDs)
}

func (h *Handler) status(c *gin.Context, ids []string) {
	ctx := c.Request.Context()
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		c.Error(apperrors.NewBadRequestError("tenant context is required"))
		return
	}
	status, err := h.coordinator.QueueStatus(ctx, tenantID, ids)
	if err != nil {
		logger.Errorf(ctx, "[document queue] status query failed: %v", err)
		c.Error(apperrors.NewInternalServerError("failed to query document queue"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": status})
}

func (h *Handler) Instances(c *gin.Context) {
	instances, err := h.coordinator.ListInstances(c.Request.Context())
	if err != nil {
		c.Error(apperrors.NewInternalServerError("failed to query document parser instances"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"instances": instances}})
}

type terminationAttestationRequest struct {
	InstanceID string `json:"instance_id" binding:"required"`
	BootID     string `json:"boot_id" binding:"required"`
	Proof      string `json:"proof" binding:"required"`
}

// AttestTermination is an operator/orchestrator safety boundary, not a health
// detector. It may be called only after the exact container/Pod boot has been
// proved terminated (or its node fenced). Heartbeat, workflow lease and Redis
// inactivity remain independent gates in recovery.
func (h *Handler) AttestTermination(c *gin.Context) {
	var request terminationAttestationRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError("instance_id, boot_id and proof are required"))
		return
	}
	err := h.coordinator.ConfirmInstanceTermination(
		c.Request.Context(), request.InstanceID, request.BootID, request.Proof,
	)
	if err == nil {
		c.JSON(http.StatusOK, gin.H{"success": true})
		return
	}
	if errors.Is(err, ErrTerminationNotProven) {
		c.Error(apperrors.NewConflictError(err.Error()))
		return
	}
	if errors.Is(err, ErrStaleDelivery) || errors.Is(err, gorm.ErrRecordNotFound) {
		c.Error(apperrors.NewConflictError("instance boot changed or no longer exists"))
		return
	}
	logger.Errorf(c.Request.Context(), "[document queue] termination attestation failed: %v", err)
	c.Error(apperrors.NewInternalServerError("failed to record instance termination"))
}
