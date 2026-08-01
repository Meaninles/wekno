package modeladmission

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
)

type Handler struct {
	manager *Manager
}

func NewHandler(manager *Manager) *Handler {
	return &Handler{manager: manager}
}

func (h *Handler) ListResourcePools(c *gin.Context) {
	var rows []ResourcePool
	if err := h.db().WithContext(c).Order("resource_kind, name, id").Find(&rows).Error; err != nil {
		h.internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func (h *Handler) ListQuotaPools(c *gin.Context) {
	var rows []QuotaPool
	if err := h.db().WithContext(c).Order("name, id").Find(&rows).Error; err != nil {
		h.internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func (h *Handler) CreateQuotaPool(c *gin.Context) {
	var row QuotaPool
	if err := c.ShouldBindJSON(&row); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := validateQuotaPool(&row); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	now := time.Now().UTC()
	row.PolicyVersion = 1
	row.CreatedAt, row.UpdatedAt = now, now
	if err := h.db().WithContext(c).Create(&row).Error; err != nil {
		h.internal(c, err)
		return
	}
	h.audit(c, "create", "quota_pool", row.ID, nil, row, row.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": row})
}

func (h *Handler) UpdateQuotaPool(c *gin.Context) {
	expected, err := ifMatchVersion(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var input QuotaPool
	if err := c.ShouldBindJSON(&input); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	input.ID = c.Param("id")
	if err := validateQuotaPool(&input); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var old QuotaPool
	if err := h.db().WithContext(c).Where("id = ?", input.ID).First(&old).Error; err != nil {
		h.internal(c, err)
		return
	}
	input.PolicyVersion = expected + 1
	input.CreatedAt = old.CreatedAt
	input.UpdatedAt = time.Now().UTC()
	result := h.db().WithContext(c).Model(&QuotaPool{}).
		Where("id = ? AND policy_version = ?", input.ID, expected).
		Select("name", "rpm", "tpm", "token_burst", "state", "policy_version", "updated_at").
		Updates(&input)
	if result.Error != nil {
		h.internal(c, result.Error)
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
		return
	}
	h.audit(c, "update", "quota_pool", input.ID, old, input, input.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": input})
}

func (h *Handler) ListGatewayPools(c *gin.Context) {
	var rows []GatewayPool
	if err := h.db().WithContext(c).Order("name, id").Find(&rows).Error; err != nil {
		h.internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func (h *Handler) CreateGatewayPool(c *gin.Context) {
	var row GatewayPool
	if err := c.ShouldBindJSON(&row); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := validateGatewayPool(&row); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	now := time.Now().UTC()
	row.PolicyVersion = 1
	row.CreatedAt, row.UpdatedAt = now, now
	if err := h.db().WithContext(c).Create(&row).Error; err != nil {
		h.internal(c, err)
		return
	}
	h.audit(c, "create", "gateway_pool", row.ID, nil, row, row.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": row})
}

func (h *Handler) UpdateGatewayPool(c *gin.Context) {
	expected, err := ifMatchVersion(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var input GatewayPool
	if err := c.ShouldBindJSON(&input); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	input.ID = c.Param("id")
	if err := validateGatewayPool(&input); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var old GatewayPool
	if err := h.db().WithContext(c).Where("id = ?", input.ID).First(&old).Error; err != nil {
		h.internal(c, err)
		return
	}
	input.PolicyVersion = expected + 1
	input.CreatedAt = old.CreatedAt
	input.UpdatedAt = time.Now().UTC()
	result := h.db().WithContext(c).Model(&GatewayPool{}).
		Where("id = ? AND policy_version = ?", input.ID, expected).
		Select("name", "max_inflight", "state", "policy_version", "updated_at").
		Updates(&input)
	if result.Error != nil {
		h.internal(c, result.Error)
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
		return
	}
	h.audit(c, "update", "gateway_pool", input.ID, old, input, input.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": input})
}

func (h *Handler) CreateResourcePool(c *gin.Context) {
	var pool ResourcePool
	if err := c.ShouldBindJSON(&pool); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := NormalizePool(&pool); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	now := time.Now().UTC()
	pool.PolicyVersion = 1
	pool.CreatedAt, pool.UpdatedAt = now, now
	if err := h.db().WithContext(c).Create(&pool).Error; err != nil {
		h.internal(c, err)
		return
	}
	h.audit(c, "create", "resource_pool", pool.ID, nil, pool, pool.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": pool})
}

func (h *Handler) UpdateResourcePool(c *gin.Context) {
	expected, err := ifMatchVersion(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var input ResourcePool
	if err := c.ShouldBindJSON(&input); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	input.ID = c.Param("id")
	if err := NormalizePool(&input); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var old ResourcePool
	if err := h.db().WithContext(c).Where("id = ?", input.ID).First(&old).Error; err != nil {
		h.internal(c, err)
		return
	}
	input.PolicyVersion = expected + 1
	input.CreatedAt = old.CreatedAt
	input.UpdatedAt = time.Now().UTC()
	result := h.db().WithContext(c).Model(&ResourcePool{}).
		Where("id = ? AND policy_version = ?", input.ID, expected).
		Select(
			"name", "resource_kind", "chat_max_concurrent", "chat_max_waiting",
			"max_inflight", "max_background_inflight",
			"interactive_reserve", "tenant_guaranteed", "tenant_burst",
			"document_guaranteed", "document_burst", "rpm", "tpm", "token_burst",
			"request_timeout_seconds", "circuit_threshold", "circuit_window_seconds",
			"circuit_open_seconds", "state", "policy_version", "updated_at",
		).Updates(&input)
	if result.Error != nil {
		h.internal(c, result.Error)
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
		return
	}
	h.audit(c, "update", "resource_pool", input.ID, old, input, input.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": input})
}

func (h *Handler) DrainResourcePool(c *gin.Context) {
	h.setPoolState(c, "draining")
}

func (h *Handler) DeleteResourcePool(c *gin.Context) {
	expected, err := ifMatchVersion(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var deleted ResourcePool
	err = h.db().WithContext(c).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND policy_version = ?", c.Param("id"), expected).
			First(&deleted).Error; err != nil {
			return err
		}
		if deleted.State != "draining" {
			return apperrors.NewBadRequestError("resource pool must be draining before deletion")
		}
		var bindings int64
		if err := tx.Model(&ResourceBinding{}).
			Where("resource_pool_id = ?", deleted.ID).Count(&bindings).Error; err != nil {
			return err
		}
		if bindings > 0 {
			return apperrors.NewBadRequestError("resource pool still has model bindings")
		}
		if tx.Migrator().HasTable("custom_derivative_work_items") {
			var active int64
			if err := tx.Table("custom_derivative_work_items").
				Where("resource_pool_id = ? AND state NOT IN ?",
					deleted.ID, []string{"completed", "cancelled", "failed"}).
				Count(&active).Error; err != nil {
				return err
			}
			if active > 0 {
				return apperrors.NewBadRequestError("resource pool still has active derivative work")
			}
		}
		return tx.Delete(&ResourcePool{}, "id = ?", deleted.ID).Error
	})
	if err != nil {
		if appErr, ok := apperrors.IsAppError(err); ok {
			c.Error(appErr)
			return
		}
		h.internal(c, err)
		return
	}
	h.audit(c, "delete", "resource_pool", deleted.ID, deleted, nil, expected)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) ResetResourcePool(c *gin.Context) {
	expected, err := ifMatchVersion(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var old ResourcePool
	if err := h.db().WithContext(c).Where("id = ?", c.Param("id")).First(&old).Error; err != nil {
		h.internal(c, err)
		return
	}
	before := old
	policy := builtinPolicy(
		Kind(old.ResourceKind), old.ResourceKind == string(KindDerivative),
	)
	if h.manager != nil && h.manager.store != nil {
		if template, ok := h.manager.store.template(c.Request.Context(), old.ResourceKind); ok {
			policy = templatePolicy(template, Kind(old.ResourceKind))
			old.RequestTimeoutSeconds = template.RequestTimeoutSeconds
			old.CircuitThreshold = template.CircuitThreshold
			old.CircuitWindowSeconds = template.CircuitWindowSeconds
			old.CircuitOpenSeconds = template.CircuitOpenSeconds
		}
	}
	old.MaxInflight = policy.limit.Concurrency
	old.ChatMaxConcurrent = nil
	old.ChatMaxWaiting = nil
	old.MaxBackgroundInflight = policy.limit.Background
	old.InteractiveReserve = policy.reserve
	old.TenantGuaranteed = 1
	old.TenantBurst = policy.limit.PerTenant
	old.DocumentGuaranteed = 1
	old.DocumentBurst = policy.limit.PerDocument
	old.RPM = policy.limit.RPM
	old.TPM = policy.tpm
	old.TokenBurst = 0
	old.State = "enabled"
	old.PolicyVersion = expected + 1
	old.UpdatedAt = time.Now().UTC()
	result := h.db().WithContext(c).Model(&ResourcePool{}).
		Where("id = ? AND policy_version = ?", old.ID, expected).Updates(map[string]any{
		"chat_max_concurrent":     nil,
		"chat_max_waiting":        nil,
		"max_inflight":            old.MaxInflight,
		"max_background_inflight": old.MaxBackgroundInflight,
		"interactive_reserve":     old.InteractiveReserve,
		"tenant_guaranteed":       old.TenantGuaranteed,
		"tenant_burst":            old.TenantBurst,
		"document_guaranteed":     old.DocumentGuaranteed,
		"document_burst":          old.DocumentBurst,
		"rpm":                     old.RPM,
		"tpm":                     old.TPM,
		"token_burst":             old.TokenBurst,
		"request_timeout_seconds": old.RequestTimeoutSeconds,
		"circuit_threshold":       old.CircuitThreshold,
		"circuit_window_seconds":  old.CircuitWindowSeconds,
		"circuit_open_seconds":    old.CircuitOpenSeconds,
		"state":                   old.State,
		"policy_version":          old.PolicyVersion,
		"updated_at":              old.UpdatedAt,
	})
	if result.Error != nil {
		h.internal(c, result.Error)
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
		return
	}
	h.audit(c, "reset", "resource_pool", old.ID, before, old, old.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": old})
}

func (h *Handler) ListBindings(c *gin.Context) {
	var rows []ResourceBinding
	if err := h.db().WithContext(c).Order("model_tenant_id, model_id").Find(&rows).Error; err != nil {
		h.internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

type bindingRequest struct {
	ModelTenantID  uint64 `json:"model_tenant_id" binding:"required"`
	ResourcePoolID string `json:"resource_pool_id" binding:"required"`
	QuotaPoolID    string `json:"quota_pool_id"`
	GatewayPoolID  string `json:"gateway_pool_id"`
}

func (h *Handler) PutBinding(c *gin.Context) {
	expected, err := ifMatchVersionAllowCreate(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var request bindingRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	request.ResourcePoolID = strings.TrimSpace(request.ResourcePoolID)
	request.QuotaPoolID = strings.TrimSpace(request.QuotaPoolID)
	request.GatewayPoolID = strings.TrimSpace(request.GatewayPoolID)
	var model types.Model
	if err := h.db().WithContext(c).
		Where("id = ? AND tenant_id = ? AND deleted_at IS NULL", c.Param("model_id"), request.ModelTenantID).
		First(&model).Error; err != nil {
		h.internal(c, err)
		return
	}
	var pool ResourcePool
	if err := h.db().WithContext(c).Where("id = ?", request.ResourcePoolID).First(&pool).Error; err != nil {
		h.internal(c, err)
		return
	}
	if request.QuotaPoolID != "" {
		var quota QuotaPool
		if err := h.db().WithContext(c).Where("id = ?", request.QuotaPoolID).First(&quota).Error; err != nil {
			h.internal(c, err)
			return
		}
	}
	if request.GatewayPoolID != "" {
		var gateway GatewayPool
		if err := h.db().WithContext(c).Where("id = ?", request.GatewayPoolID).First(&gateway).Error; err != nil {
			h.internal(c, err)
			return
		}
	}
	var old ResourceBinding
	err = h.db().WithContext(c).
		Where("model_id = ? AND model_tenant_id = ?", model.ID, model.TenantID).
		First(&old).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		h.internal(c, err)
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if expected != 0 {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "binding_version conflict"})
			return
		}
	} else if expected != old.BindingVersion {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "binding_version conflict"})
		return
	}
	version := expected + 1
	now := time.Now().UTC()
	row := ResourceBinding{
		ModelID: model.ID, ModelTenantID: model.TenantID,
		ResourcePoolID:   request.ResourcePoolID,
		QuotaPoolID:      strings.TrimSpace(request.QuotaPoolID),
		GatewayPoolID:    strings.TrimSpace(request.GatewayPoolID),
		RouteFingerprint: fingerprintDigest(RouteFingerprint(&model)),
		BindingVersion:   version, CreatedAt: old.CreatedAt, UpdatedAt: now,
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if expected == 0 {
		if err := h.db().WithContext(c).Create(&row).Error; err != nil {
			h.internal(c, err)
			return
		}
	} else {
		result := h.db().WithContext(c).Model(&ResourceBinding{}).
			Where(
				"model_id = ? AND model_tenant_id = ? AND binding_version = ?",
				row.ModelID, row.ModelTenantID, expected,
			).
			Updates(map[string]any{
				"resource_pool_id":  row.ResourcePoolID,
				"quota_pool_id":     row.QuotaPoolID,
				"gateway_pool_id":   row.GatewayPoolID,
				"route_fingerprint": row.RouteFingerprint,
				"binding_version":   row.BindingVersion,
				"updated_at":        row.UpdatedAt,
			})
		if result.Error != nil {
			h.internal(c, result.Error)
			return
		}
		if result.RowsAffected != 1 {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "binding_version conflict"})
			return
		}
	}
	h.audit(c, "bind", "model_binding", model.ID, old, row, row.BindingVersion)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": row})
}

func (h *Handler) ListTemplates(c *gin.Context) {
	var rows []AdmissionTemplate
	if err := h.db().WithContext(c).Order("kind").Find(&rows).Error; err != nil {
		h.internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func (h *Handler) GetSchedulerPolicy(c *gin.Context) {
	// Management reads bypass the one-second worker hot-path cache so a write
	// followed through a load balancer is immediately consistent on every API
	// replica.
	var policy SchedulerPolicy
	if err := h.db().WithContext(c).Where("id = ?", 1).First(&policy).Error; err != nil {
		h.internal(c, err)
		return
	}
	if err := NormalizeSchedulerPolicy(&policy); err != nil {
		h.internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": policy})
}

func (h *Handler) PutSchedulerPolicy(c *gin.Context) {
	expected, err := ifMatchVersion(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var input SchedulerPolicy
	if err := c.ShouldBindJSON(&input); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	if err := NormalizeSchedulerPolicy(&input); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var old SchedulerPolicy
	if err := h.db().WithContext(c).Where("id = ?", 1).First(&old).Error; err != nil {
		h.internal(c, err)
		return
	}
	if old.PolicyVersion != expected {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
		return
	}
	input.ID = 1
	input.PolicyVersion = expected + 1
	input.CreatedAt = old.CreatedAt
	input.UpdatedAt = time.Now().UTC()
	input.UpdatedBy, _ = types.UserIDFromContext(c.Request.Context())
	result := h.db().WithContext(c).Model(&SchedulerPolicy{}).
		Where("id = ? AND policy_version = ?", 1, expected).
		Updates(map[string]any{
			"prefetch_factor":             input.PrefetchFactor,
			"derivative_weight":           input.DerivativeWeight,
			"wiki_weight":                 input.WikiWeight,
			"background_max_wait_seconds": input.BackgroundMaxWaitSeconds,
			"dispatch_lease_seconds":      input.DispatchLeaseSeconds,
			"policy_version":              input.PolicyVersion,
			"updated_by":                  input.UpdatedBy,
			"updated_at":                  input.UpdatedAt,
		})
	if result.Error != nil {
		h.internal(c, result.Error)
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
		return
	}
	h.audit(c, "update", "scheduler_policy", "1", old, input, input.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": input})
}

func (h *Handler) PutTemplate(c *gin.Context) {
	expected, err := ifMatchVersionAllowCreate(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var row AdmissionTemplate
	if err := c.ShouldBindJSON(&row); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	row.Kind = strings.TrimSpace(c.Param("kind"))
	if err := NormalizeTemplate(&row); err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	var old AdmissionTemplate
	findErr := h.db().WithContext(c).Where("kind = ?", row.Kind).First(&old).Error
	if errors.Is(findErr, gorm.ErrRecordNotFound) {
		if expected != 0 {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
			return
		}
	} else if findErr != nil {
		h.internal(c, findErr)
		return
	} else if expected != old.PolicyVersion {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
		return
	}
	row.PolicyVersion = expected + 1
	row.CreatedAt = old.CreatedAt
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	row.UpdatedAt = time.Now().UTC()
	row.UpdatedBy, _ = types.UserIDFromContext(c.Request.Context())
	if expected == 0 {
		if err := h.db().WithContext(c).Create(&row).Error; err != nil {
			h.internal(c, err)
			return
		}
	} else {
		result := h.db().WithContext(c).Model(&AdmissionTemplate{}).
			Where("kind = ? AND policy_version = ?", row.Kind, expected).
			Updates(&row)
		if result.Error != nil {
			h.internal(c, result.Error)
			return
		}
		if result.RowsAffected != 1 {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
			return
		}
	}
	if h.manager != nil {
		_ = h.manager.Reconcile(c.Request.Context())
	}
	h.audit(c, "upsert", "template", row.Kind, old, row, row.PolicyVersion)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true, "data": row})
}

func (h *Handler) QueueStatus(c *gin.Context) {
	stats := Stats{}
	if h.manager != nil {
		stats = h.manager.Snapshot()
	}
	var pools int64
	var bindings int64
	_ = h.db().WithContext(c).Model(&ResourcePool{}).Count(&pools).Error
	_ = h.db().WithContext(c).Model(&ResourceBinding{}).Count(&bindings).Error
	data := gin.H{
		"admission": stats, "resource_pools": pools, "bindings": bindings,
		"work_items": []any{}, "oldest_wait_seconds": 0,
	}
	if h.db().Migrator().HasTable("custom_derivative_work_items") {
		type queueCount struct {
			State          string     `json:"state"`
			WorkKind       string     `json:"work_kind"`
			ResourcePoolID string     `json:"resource_pool_id"`
			Count          int64      `json:"count"`
			Oldest         *time.Time `json:"-"`
		}
		var counts []queueCount
		if err := h.db().WithContext(c).
			Table("custom_derivative_work_items").
			Select("state, work_kind, resource_pool_id, COUNT(*) AS count, MIN(created_at) AS oldest").
			Group("state, work_kind, resource_pool_id").
			Order("state, work_kind, resource_pool_id").
			Scan(&counts).Error; err == nil {
			data["work_items"] = counts
			oldest := 0.0
			now := time.Now().UTC()
			for _, row := range counts {
				switch row.State {
				case "completed", "cancelled", "failed":
					continue
				}
				if row.Oldest != nil && now.After(*row.Oldest) {
					seconds := now.Sub(*row.Oldest).Seconds()
					if seconds > oldest {
						oldest = seconds
					}
				}
			}
			data["oldest_wait_seconds"] = oldest
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

func (h *Handler) ListAudits(c *gin.Context) {
	var rows []AdmissionAudit
	if err := h.db().WithContext(c).Order("id DESC").Limit(500).Find(&rows).Error; err != nil {
		h.internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func (h *Handler) Reconcile(c *gin.Context) {
	if err := h.manager.Reconcile(c.Request.Context()); err != nil {
		h.internal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) setPoolState(c *gin.Context, state string) {
	expected, err := ifMatchVersion(c)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	result := h.db().WithContext(c).Model(&ResourcePool{}).
		Where("id = ? AND policy_version = ?", c.Param("id"), expected).
		Updates(map[string]any{
			"state": state, "policy_version": expected + 1, "updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		h.internal(c, result.Error)
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "policy_version conflict"})
		return
	}
	h.audit(c, "drain", "resource_pool", c.Param("id"), nil, map[string]any{"state": state}, expected+1)
	h.invalidate()
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (h *Handler) db() *gorm.DB {
	if h == nil || h.manager == nil || h.manager.store == nil {
		return nil
	}
	return h.manager.store.db
}

func (h *Handler) invalidate() {
	if h != nil && h.manager != nil && h.manager.store != nil {
		h.manager.store.Invalidate()
	}
}

func (h *Handler) audit(
	c *gin.Context,
	action, resourceType, resourceID string,
	oldValue, newValue any,
	version uint64,
) {
	oldJSON, _ := json.Marshal(oldValue)
	newJSON, _ := json.Marshal(newValue)
	actor, _ := types.UserIDFromContext(c.Request.Context())
	_ = h.db().WithContext(c).Create(&AdmissionAudit{
		ActorID: actor, Action: action, ResourceType: resourceType, ResourceID: resourceID,
		OldValue: string(oldJSON), NewValue: string(newJSON),
		PolicyVersion: version, CreatedAt: time.Now().UTC(),
	}).Error
}

func (h *Handler) internal(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.Error(apperrors.NewNotFoundError(err.Error()))
		return
	}
	c.Error(apperrors.NewInternalServerError(err.Error()))
}

func ifMatchVersion(c *gin.Context) (uint64, error) {
	value, err := ifMatchVersionAllowCreate(c)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, errors.New("If-Match must be a positive policy_version")
	}
	return value, nil
}

func ifMatchVersionAllowCreate(c *gin.Context) (uint64, error) {
	raw := strings.Trim(strings.TrimSpace(c.GetHeader("If-Match")), `"`)
	if raw == "" {
		return 0, errors.New("If-Match policy_version is required")
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.New("If-Match must be a non-negative policy_version")
	}
	return value, nil
}

func validateQuotaPool(row *QuotaPool) error {
	if row == nil {
		return errors.New("quota pool is required")
	}
	row.ID = strings.TrimSpace(row.ID)
	row.Name = strings.TrimSpace(row.Name)
	if !safePoolID.MatchString(row.ID) || row.Name == "" {
		return errors.New("valid quota pool id and name are required")
	}
	if row.RPM < 0 || row.TPM < 0 || row.TokenBurst < 0 {
		return errors.New("quota pool limits cannot be negative")
	}
	switch row.State {
	case "", "enabled":
		row.State = "enabled"
	case "draining", "disabled":
	default:
		return errors.New("state must be enabled, draining, or disabled")
	}
	return nil
}

func validateGatewayPool(row *GatewayPool) error {
	if row == nil {
		return errors.New("gateway pool is required")
	}
	row.ID = strings.TrimSpace(row.ID)
	row.Name = strings.TrimSpace(row.Name)
	if !safePoolID.MatchString(row.ID) || row.Name == "" {
		return errors.New("valid gateway pool id and name are required")
	}
	if row.MaxInflight < 1 || row.MaxInflight > 4096 {
		return errors.New("max_inflight must be between 1 and 4096")
	}
	switch row.State {
	case "", "enabled":
		row.State = "enabled"
	case "draining", "disabled":
	default:
		return errors.New("state must be enabled, draining, or disabled")
	}
	return nil
}
