package derivativecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	appservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const singletonConfigID uint = 1

type Config struct {
	ID             uint      `gorm:"primaryKey"`
	DefaultModelID string    `gorm:"type:varchar(64);not null;default:''"`
	Version        uint64    `gorm:"not null;default:0"`
	UpdatedBy      string    `gorm:"type:varchar(36);not null;default:''"`
	CreatedAt      time.Time `gorm:"not null"`
	UpdatedAt      time.Time `gorm:"not null"`
}

func (Config) TableName() string { return "custom_derivative_control_configs" }

type Assignment struct {
	ModelID       string    `gorm:"type:varchar(64);primaryKey"`
	ModelTenantID uint64    `gorm:"not null;index"`
	PublishedBy   string    `gorm:"type:varchar(36);not null;default:''"`
	CreatedAt     time.Time `gorm:"not null"`
	UpdatedAt     time.Time `gorm:"not null"`
}

func (Assignment) TableName() string { return "custom_derivative_model_assignments" }

type PublishedModel struct {
	ID            string                   `json:"id"`
	TenantID      uint64                   `json:"tenant_id"`
	Name          string                   `json:"name"`
	DisplayName   string                   `json:"display_name"`
	Type          types.ModelType          `json:"type"`
	Source        types.ModelSource        `json:"source"`
	Status        types.ModelStatus        `json:"status"`
	WorkloadScope types.ModelWorkloadScope `json:"workload_scope"`
	IsDefault     bool                     `json:"is_derivative_default"`
	Parameters    PublishedModelParameters `json:"parameters"`
}

type PublishedModelParameters struct {
	Provider       string `json:"provider,omitempty"`
	SupportsVision bool   `json:"supports_vision"`
}

type Candidate struct {
	PublishedModel
	Assigned bool   `json:"assigned"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}

type Status struct {
	Configured     bool             `json:"configured"`
	DefaultModelID string           `json:"default_model_id"`
	TPM            int64            `json:"tpm"`
	Models         []PublishedModel `json:"models"`
	Limiter        LimiterSnapshot  `json:"limiter"`
}

type AdminConfig struct {
	Status
	Candidates []Candidate `json:"candidates"`
}

type Service struct {
	db        *gorm.DB
	audit     interfaces.AuditLogService
	kbRepo    interfaces.KnowledgeBaseRepository
	agentRepo interfaces.CustomAgentRepository
	limiter   *Limiter
}

func NewService(
	db *gorm.DB,
	rdb *redis.Client,
	settings interfaces.SystemSettingService,
	audit interfaces.AuditLogService,
	kbRepo interfaces.KnowledgeBaseRepository,
	agentRepo interfaces.CustomAgentRepository,
	admissionManagers ...*modeladmission.Manager,
) *Service {
	var admissionManager *modeladmission.Manager
	if len(admissionManagers) > 0 {
		admissionManager = admissionManagers[0]
	}
	return &Service{
		db: db, audit: audit,
		kbRepo: kbRepo, agentRepo: agentRepo,
		limiter: NewLimiterWithAdmission(rdb, settings, admissionManager),
	}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("derivative control database is unavailable")
	}
	migrator := s.db.WithContext(ctx).Migrator()
	if !migrator.HasColumn(&types.Model{}, "WorkloadScope") {
		if err := migrator.AddColumn(&types.Model{}, "WorkloadScope"); err != nil {
			return fmt.Errorf("add models.workload_scope: %w", err)
		}
	}
	if !migrator.HasColumn(&types.KnowledgeBase{}, "DerivativeModelID") {
		if err := migrator.AddColumn(&types.KnowledgeBase{}, "DerivativeModelID"); err != nil {
			return fmt.Errorf("add knowledge_bases.derivative_model_id: %w", err)
		}
	}
	if err := s.db.WithContext(ctx).Exec(
		"UPDATE models SET workload_scope = ? WHERE workload_scope IS NULL OR workload_scope = ''",
		types.ModelWorkloadInteractive,
	).Error; err != nil {
		return fmt.Errorf("backfill model workload scope: %w", err)
	}
	if err := s.db.WithContext(ctx).AutoMigrate(&Config{}, &Assignment{}); err != nil {
		return fmt.Errorf("migrate derivative control tables: %w", err)
	}
	now := time.Now().UTC()
	row := &Config{ID: singletonConfigID, CreatedAt: now, UpdatedAt: now}
	if err := s.db.WithContext(ctx).
		Where("id = ?", singletonConfigID).
		FirstOrCreate(row).Error; err != nil {
		return fmt.Errorf("ensure derivative control singleton: %w", err)
	}
	return nil
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	config, err := s.loadConfig(ctx, s.db)
	if err != nil {
		return Status{}, err
	}
	models, err := s.listPublished(ctx, s.db, config.DefaultModelID)
	if err != nil {
		return Status{}, err
	}
	configured := false
	for _, model := range models {
		if model.ID == config.DefaultModelID &&
			model.WorkloadScope == types.ModelWorkloadDerivativeOnly &&
			model.Status == types.ModelStatusActive {
			configured = true
			break
		}
	}
	return Status{
		Configured:     configured,
		DefaultModelID: config.DefaultModelID,
		TPM:            s.limiter.TPM(ctx),
		Models:         models,
		Limiter:        s.limiter.Snapshot(ctx),
	}, nil
}

func (s *Service) AdminConfig(ctx context.Context) (AdminConfig, error) {
	status, err := s.Status(ctx)
	if err != nil {
		return AdminConfig{}, err
	}
	candidates, err := s.listCandidates(ctx, status.DefaultModelID)
	if err != nil {
		return AdminConfig{}, err
	}
	return AdminConfig{Status: status, Candidates: candidates}, nil
}

func (s *Service) Publish(
	ctx context.Context,
	modelID string,
	modelTenantID uint64,
) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" || modelTenantID == 0 {
		return apperrors.NewBadRequestError("model_id and model_tenant_id are required")
	}
	actor, _ := types.UserIDFromContext(ctx)
	var becameDefault bool
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		config, err := s.lockConfig(ctx, tx)
		if err != nil {
			return err
		}
		var existing Assignment
		err = tx.WithContext(ctx).Where("model_id = ?", modelID).First(&existing).Error
		if err == nil {
			if existing.ModelTenantID != modelTenantID {
				return apperrors.NewBadRequestError("model assignment tenant mismatch")
			}
			model, loadErr := s.loadExactModel(ctx, tx, modelID, modelTenantID, true)
			if loadErr != nil {
				return loadErr
			}
			if reason := candidateReason(model); reason != "" {
				return apperrors.NewBadRequestError(reason)
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		model, err := s.loadExactModel(ctx, tx, modelID, modelTenantID, true)
		if err != nil {
			return err
		}
		if reason := candidateReason(model); reason != "" {
			return apperrors.NewBadRequestError(reason)
		}
		assignment := &Assignment{
			ModelID: modelID, ModelTenantID: modelTenantID,
			PublishedBy: actor, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := tx.WithContext(ctx).Create(assignment).Error; err != nil {
			return fmt.Errorf("publish derivative model: %w", err)
		}
		if config.DefaultModelID == "" {
			config.DefaultModelID = modelID
			config.Version++
			config.UpdatedBy = actor
			config.UpdatedAt = time.Now().UTC()
			if err := tx.WithContext(ctx).Save(config).Error; err != nil {
				return fmt.Errorf("set initial derivative default: %w", err)
			}
			becameDefault = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.auditChange(ctx, "publish", modelID, map[string]any{
		"model_tenant_id": modelTenantID, "became_default": becameDefault,
	})
	return nil
}

func (s *Service) Unpublish(ctx context.Context, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return apperrors.NewBadRequestError("model_id is required")
	}
	actor, _ := types.UserIDFromContext(ctx)
	var newDefault string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		config, err := s.lockConfig(ctx, tx)
		if err != nil {
			return err
		}
		var assignment Assignment
		if err := tx.WithContext(ctx).Where("model_id = ?", modelID).
			First(&assignment).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var references int64
		if err := tx.WithContext(ctx).Model(&types.KnowledgeBase{}).
			Where("derivative_model_id = ?", modelID).
			Count(&references).Error; err != nil {
			return err
		}
		if references > 0 {
			return apperrors.NewBadRequestError(fmt.Sprintf(
				"衍生模型仍被 %d 个知识库使用，请先改绑", references,
			))
		}
		if err := tx.WithContext(ctx).Delete(
			&Assignment{}, "model_id = ?", modelID,
		).Error; err != nil {
			return err
		}
		if config.DefaultModelID == modelID {
			var replacement Assignment
			findErr := tx.WithContext(ctx).Order("created_at ASC").
				First(&replacement).Error
			if findErr == nil {
				newDefault = replacement.ModelID
			} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
				return findErr
			}
			config.DefaultModelID = newDefault
			config.Version++
			config.UpdatedBy = actor
			config.UpdatedAt = time.Now().UTC()
			if err := tx.WithContext(ctx).Save(config).Error; err != nil {
				return err
			}
		} else {
			newDefault = config.DefaultModelID
		}
		// workload_scope intentionally remains derivative_only. Revocation
		// removes it from KB choices but never silently turns a dedicated
		// endpoint back into an interactive model.
		return nil
	})
	if err != nil {
		return err
	}
	s.auditChange(ctx, "unpublish", modelID, map[string]any{
		"default_model_id": newDefault,
	})
	return nil
}

func (s *Service) SetDefault(ctx context.Context, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return apperrors.NewBadRequestError("model_id is required")
	}
	actor, _ := types.UserIDFromContext(ctx)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		config, err := s.lockConfig(ctx, tx)
		if err != nil {
			return err
		}
		var assignment Assignment
		if err := tx.WithContext(ctx).Where("model_id = ?", modelID).
			First(&assignment).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperrors.NewBadRequestError("model is not published for derivative work")
			}
			return err
		}
		model, err := s.loadExactModel(
			ctx, tx, assignment.ModelID, assignment.ModelTenantID, true,
		)
		if err != nil {
			return err
		}
		if reason := candidateReason(model); reason != "" {
			return apperrors.NewBadRequestError(reason)
		}
		if config.DefaultModelID == modelID {
			return nil
		}
		config.DefaultModelID = modelID
		config.Version++
		config.UpdatedBy = actor
		config.UpdatedAt = time.Now().UTC()
		return tx.WithContext(ctx).Save(config).Error
	})
	if err != nil {
		return err
	}
	s.auditChange(ctx, "set_default", modelID, nil)
	return nil
}

type derivativeAuthorization struct {
	ModelID  string
	TenantID uint64
}

type derivativeAuthorizationKey struct{}

func (s *Service) ResolveChatModel(
	ctx context.Context,
	modelService interfaces.ModelService,
	requestedModelID string,
) (chat.Chat, error) {
	if modelService == nil {
		return nil, &DeferredError{
			Reason:     "derivative model service is unavailable",
			RetryAfter: time.Minute,
		}
	}
	config, err := s.loadConfig(ctx, s.db)
	if err != nil {
		return nil, &DeferredError{
			Reason:     "derivative model configuration cannot be read",
			RetryAfter: 30 * time.Second, Cause: err,
		}
	}
	modelID := strings.TrimSpace(requestedModelID)
	if modelID == "" {
		modelID = config.DefaultModelID
	}
	if modelID == "" {
		return nil, &DeferredError{
			Reason:     "no derivative model is configured",
			RetryAfter: time.Minute,
		}
	}
	var assignment Assignment
	if err := s.db.WithContext(ctx).Where("model_id = ?", modelID).
		First(&assignment).Error; err != nil {
		return nil, &DeferredError{
			Reason:     "requested derivative model is not published",
			RetryAfter: time.Minute, Cause: err,
		}
	}
	model, err := s.loadExactModel(
		ctx, s.db, assignment.ModelID, assignment.ModelTenantID, false,
	)
	if err != nil {
		return nil, &DeferredError{
			Reason:     "published derivative model cannot be loaded",
			RetryAfter: time.Minute,
			Cause:      err,
		}
	}
	if reason := candidateReason(model); reason != "" {
		return nil, &DeferredError{
			Reason:     "published derivative model is not eligible: " + reason,
			RetryAfter: time.Minute,
		}
	}
	modelCtx := context.WithValue(ctx, types.TenantIDContextKey, assignment.ModelTenantID)
	modelCtx = context.WithValue(modelCtx, derivativeAuthorizationKey{}, derivativeAuthorization{
		ModelID: modelID, TenantID: assignment.ModelTenantID,
	})
	instance, err := modelService.GetChatModel(modelCtx, modelID)
	if err != nil {
		return nil, &DeferredError{
			Reason:     "published derivative model is unavailable",
			RetryAfter: time.Minute, Cause: err,
		}
	}
	return s.limiter.WrapForModel(instance, model), nil
}

func (s *Service) GuardChatModel(ctx context.Context, model *types.Model) error {
	if model == nil {
		return nil
	}
	if model.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly {
		authorization, ok := ctx.Value(derivativeAuthorizationKey{}).(derivativeAuthorization)
		if ok && authorization.ModelID == model.ID &&
			authorization.TenantID == model.TenantID {
			var count int64
			if err := s.db.WithContext(ctx).Model(&Assignment{}).
				Where("model_id = ? AND model_tenant_id = ?", model.ID, model.TenantID).
				Count(&count).Error; err == nil && count == 1 {
				return nil
			}
		}
		return apperrors.NewBadRequestError(
			"衍生任务专用模型不能用于对话、Agent 或普通模型调试",
		)
	}

	return nil
}

func (s *Service) GuardModelMutation(
	ctx context.Context,
	operation appservice.ModelMutation,
	existing *types.Model,
	proposed *types.Model,
) error {
	target := proposed
	if target == nil {
		target = existing
	}
	if target == nil {
		return nil
	}

	if target.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly {
		if !types.IsSystemAdminFromContext(ctx) {
			return apperrors.NewForbiddenError("衍生任务专用模型只能由系统管理员维护")
		}
		if operation == appservice.ModelMutationCreate {
			if reason := candidateReason(target); reason != "" {
				return apperrors.NewBadRequestError(reason)
			}
			return nil
		}
		if existing == nil ||
			existing.WorkloadScope.Normalize() != types.ModelWorkloadDerivativeOnly {
			return apperrors.NewBadRequestError("模型类型不能在对话模型与衍生任务模型之间切换")
		}
		if operation == appservice.ModelMutationDelete {
			var count int64
			if err := s.db.WithContext(ctx).Model(&Assignment{}).
				Where("model_id = ?", existing.ID).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return apperrors.NewBadRequestError("请先从平台衍生模型池撤回该模型")
			}
			return nil
		}
		if operation == appservice.ModelMutationUpdate {
			if reason := candidateReason(target); reason != "" {
				return apperrors.NewBadRequestError(reason)
			}
		}
		return nil
	}
	return nil
}

func (s *Service) ValidateKnowledgeBase(
	ctx context.Context,
	kb *types.KnowledgeBase,
) error {
	if kb == nil {
		return nil
	}
	if kb.SummaryModelID != "" {
		var online types.Model
		err := s.db.WithContext(ctx).Where(
			"id = ? AND (tenant_id = ? OR is_builtin = true)",
			kb.SummaryModelID, kb.TenantID,
		).First(&online).Error
		if err != nil {
			return apperrors.NewBadRequestError("知识库对话模型不存在")
		}
		if online.WorkloadScope.Normalize() != types.ModelWorkloadInteractive {
			return apperrors.NewBadRequestError("衍生任务专用模型不能作为知识库对话模型")
		}
	}
	wikiModelID := ""
	if kb.WikiConfig != nil {
		wikiModelID = strings.TrimSpace(kb.WikiConfig.SynthesisModelID)
	}
	if kb.DerivativeModelID == "" && wikiModelID != "" {
		kb.DerivativeModelID = wikiModelID
	}
	if wikiModelID != "" && wikiModelID != kb.DerivativeModelID {
		return apperrors.NewBadRequestError("Wiki 模型必须与知识库衍生任务模型一致")
	}
	if kb.DerivativeModelID != "" {
		var count int64
		if err := s.db.WithContext(ctx).Model(&Assignment{}).
			Where("model_id = ?", kb.DerivativeModelID).
			Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return apperrors.NewBadRequestError("只能选择系统管理员已下发的衍生任务模型")
		}
	}
	if kb.WikiConfig != nil {
		// Keep the old JSON field coherent for older readers, but it is never
		// consulted as a fallback by the execution path.
		kb.WikiConfig.SynthesisModelID = kb.DerivativeModelID
	}
	return nil
}

func (s *Service) IsPublished(ctx context.Context, modelID string) (bool, error) {
	if strings.TrimSpace(modelID) == "" {
		return false, nil
	}
	var count int64
	err := s.db.WithContext(ctx).Model(&Assignment{}).
		Where("model_id = ?", modelID).Count(&count).Error
	return count == 1, err
}

func (s *Service) loadConfig(ctx context.Context, db *gorm.DB) (*Config, error) {
	var config Config
	err := db.WithContext(ctx).Where("id = ?", singletonConfigID).First(&config).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.New("derivative control migration has not been applied")
	}
	return &config, err
}

func (s *Service) lockConfig(ctx context.Context, tx *gorm.DB) (*Config, error) {
	var config Config
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", singletonConfigID).
		First(&config).Error
	return &config, err
}

func (s *Service) listPublished(
	ctx context.Context,
	db *gorm.DB,
	defaultModelID string,
) ([]PublishedModel, error) {
	var assignments []Assignment
	if err := db.WithContext(ctx).Order("created_at ASC").Find(&assignments).Error; err != nil {
		return nil, err
	}
	out := make([]PublishedModel, 0, len(assignments))
	for _, assignment := range assignments {
		model, err := s.loadExactModel(
			ctx, db, assignment.ModelID, assignment.ModelTenantID, false,
		)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, publishedModel(model, model.ID == defaultModelID))
	}
	return out, nil
}

func (s *Service) listCandidates(ctx context.Context, defaultModelID string) ([]Candidate, error) {
	var models []types.Model
	query := s.db.WithContext(ctx).
		Where(
			"type = ? AND workload_scope = ?",
			types.ModelTypeKnowledgeQA,
			types.ModelWorkloadDerivativeOnly,
		)
	// Model management itself is tenant-scoped. A system administrator can
	// switch tenants when publishing another tenant's endpoint, while the
	// resulting assignment remains platform-global. Keeping candidates scoped
	// here avoids an N+1 eligibility scan and thousands of disabled dropdown
	// options in installations with many tenants.
	if tenantID, ok := types.TenantIDFromContext(ctx); ok && tenantID != 0 {
		query = query.Where("tenant_id = ?", tenantID)
	}
	if err := query.
		Order("tenant_id ASC, created_at ASC").
		Find(&models).Error; err != nil {
		return nil, err
	}
	var assignments []Assignment
	if err := s.db.WithContext(ctx).Find(&assignments).Error; err != nil {
		return nil, err
	}
	assigned := make(map[string]bool, len(assignments))
	for _, assignment := range assignments {
		assigned[assignment.ModelID] = true
	}
	out := make([]Candidate, 0, len(models))
	for index := range models {
		model := &models[index]
		reason := candidateReason(model)
		out = append(out, Candidate{
			PublishedModel: publishedModel(model, model.ID == defaultModelID),
			Assigned:       assigned[model.ID],
			Eligible:       reason == "",
			Reason:         reason,
		})
	}
	return out, nil
}

func candidateReason(model *types.Model) string {
	if model == nil {
		return "模型不存在"
	}
	if model.Type != types.ModelTypeKnowledgeQA {
		return "衍生任务模型必须使用对话兼容接口"
	}
	if model.Status != "" && model.Status != types.ModelStatusActive {
		return "模型当前不是 active 状态"
	}
	if model.WorkloadScope.Normalize() != types.ModelWorkloadDerivativeOnly {
		return "只能下发在“衍生任务”分类中创建的模型"
	}
	return ""
}

func (s *Service) loadExactModel(
	ctx context.Context,
	db *gorm.DB,
	modelID string,
	tenantID uint64,
	lock bool,
) (*types.Model, error) {
	var model types.Model
	query := db.WithContext(ctx).Where("id = ? AND tenant_id = ?", modelID, tenantID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.First(&model).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperrors.NewBadRequestError("模型不存在或不属于指定租户")
		}
		return nil, err
	}
	return &model, nil
}

func publishedModel(model *types.Model, isDefault bool) PublishedModel {
	return PublishedModel{
		ID: model.ID, TenantID: model.TenantID,
		Name: model.Name, DisplayName: model.DisplayName,
		Type: model.Type, Source: model.Source, Status: model.Status,
		WorkloadScope: model.WorkloadScope.Normalize(),
		IsDefault:     isDefault,
		Parameters: PublishedModelParameters{
			Provider:       model.Parameters.Provider,
			SupportsVision: model.Parameters.SupportsVision,
		},
	}
}

func (s *Service) auditChange(
	ctx context.Context,
	operation string,
	modelID string,
	details map[string]any,
) {
	if s.audit == nil {
		return
	}
	if details == nil {
		details = map[string]any{}
	}
	details["operation"] = operation
	details["model_id"] = modelID
	raw, _ := json.Marshal(details)
	actor, _ := types.UserIDFromContext(ctx)
	_ = s.audit.Log(ctx, &types.AuditLog{
		TenantID: 0, ActorUserID: actor, ActorRole: "system_admin",
		Action:     types.AuditActionSystemSettingChanged,
		TargetType: "derivative_model_pool",
		TargetID:   modelID,
		Outcome:    types.AuditOutcomeSuccess,
		Details:    types.JSON(raw),
	})
}
