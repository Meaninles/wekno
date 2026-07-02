package builtinagentdefaults

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	referenceUsername   = "37430534@qq.com"
	modelCloneManagedBy = "builtin_agent_defaults"
)

var modelCloneNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("weknora builtin agent default model clone"))

func (s *Service) ApplyReferenceModelDefaults(
	ctx context.Context,
	agent *types.CustomAgent,
	tenantID uint64,
) (*types.CustomAgent, error) {
	if s == nil || s.db == nil || agent == nil || !types.IsBuiltinAgentID(agent.ID) {
		return agent, nil
	}

	referenceTenantID, err := s.referenceTenantID(ctx)
	if err != nil {
		return nil, err
	}
	if referenceTenantID == 0 || tenantID == referenceTenantID {
		return agent, nil
	}

	personal, err := s.isPersonalTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if !personal {
		return agent, nil
	}

	referenceAgent, err := s.referenceBuiltinAgent(ctx, agent.ID, referenceTenantID)
	if err != nil {
		return nil, err
	}
	if referenceAgent == nil {
		return agent, nil
	}

	resolved := *agent
	resolved.Config, err = s.applyReferenceModelConfig(ctx, tenantID, resolved.Config, referenceAgent.Config)
	if err != nil {
		return nil, err
	}
	resolved.EnsureDefaults()
	return &resolved, nil
}

func (s *Service) referenceTenantID(ctx context.Context) (uint64, error) {
	var user types.User
	err := s.db.WithContext(ctx).
		Where("username = ? AND is_active = ?", referenceUsername, true).
		Where("deleted_at IS NULL").
		First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			logger.Warnf(ctx, "[builtin-agent-defaults] reference user %s not found", referenceUsername)
			return 0, nil
		}
		return 0, err
	}
	return user.TenantID, nil
}

func (s *Service) isPersonalTenant(ctx context.Context, tenantID uint64) (bool, error) {
	if tenantID == 0 {
		return false, nil
	}
	var count int64
	err := s.db.WithContext(ctx).
		Model(&types.User{}).
		Where("tenant_id = ? AND is_active = ?", tenantID, true).
		Where("deleted_at IS NULL").
		Count(&count).Error
	return count > 0, err
}

func (s *Service) referenceBuiltinAgent(
	ctx context.Context,
	agentID string,
	referenceTenantID uint64,
) (*types.CustomAgent, error) {
	referenceCtx := context.WithValue(ctx, types.TenantIDContextKey, referenceTenantID)
	defaultAgent := types.GetBuiltinAgentWithContext(referenceCtx, agentID, referenceTenantID)

	var agent types.CustomAgent
	err := s.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND is_builtin = ?", agentID, referenceTenantID, true).
		First(&agent).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return defaultAgent, nil
		}
		return nil, err
	}
	if defaultAgent != nil {
		if defaultAgent.Config.SystemPromptID != "" {
			agent.Config.SystemPromptID = defaultAgent.Config.SystemPromptID
			agent.Config.SystemPrompt = defaultAgent.Config.SystemPrompt
		}
		if defaultAgent.Config.ContextTemplateID != "" {
			agent.Config.ContextTemplateID = defaultAgent.Config.ContextTemplateID
			agent.Config.ContextTemplate = defaultAgent.Config.ContextTemplate
		}
		if agent.Config.Thinking == nil {
			agent.Config.Thinking = cloneBoolPtr(defaultAgent.Config.Thinking)
		}
	}
	agent.EnsureDefaults()
	return &agent, nil
}

func (s *Service) applyReferenceModelConfig(
	ctx context.Context,
	targetTenantID uint64,
	current types.CustomAgentConfig,
	reference types.CustomAgentConfig,
) (types.CustomAgentConfig, error) {
	cfg := current
	var err error

	cfg.ModelID, err = s.resolveTenantModelID(ctx, targetTenantID, reference.ModelID, types.ModelTypeKnowledgeQA)
	if err != nil {
		return cfg, err
	}
	cfg.RerankModelID, err = s.resolveTenantModelID(ctx, targetTenantID, reference.RerankModelID, types.ModelTypeRerank)
	if err != nil {
		return cfg, err
	}
	cfg.QueryUnderstandModelID, err = s.resolveTenantModelID(
		ctx, targetTenantID, reference.QueryUnderstandModelID, types.ModelTypeKnowledgeQA,
	)
	if err != nil {
		return cfg, err
	}
	cfg.VLMModelID, err = s.resolveTenantModelID(ctx, targetTenantID, reference.VLMModelID, types.ModelTypeVLLM)
	if err != nil {
		return cfg, err
	}
	cfg.ASRModelID, err = s.resolveTenantModelID(ctx, targetTenantID, reference.ASRModelID, types.ModelTypeASR)
	if err != nil {
		return cfg, err
	}

	cfg.Temperature = reference.Temperature
	cfg.MaxCompletionTokens = reference.MaxCompletionTokens
	cfg.Thinking = cloneBoolPtr(reference.Thinking)
	cfg.ImageUploadEnabled = reference.ImageUploadEnabled
	cfg.AudioUploadEnabled = reference.AudioUploadEnabled

	cfg.SystemPrompt = reference.SystemPrompt
	cfg.SystemPromptID = reference.SystemPromptID
	cfg.ContextTemplate = reference.ContextTemplate
	cfg.ContextTemplateID = reference.ContextTemplateID
	cfg.RewritePromptSystem = reference.RewritePromptSystem
	cfg.RewritePromptUser = reference.RewritePromptUser
	cfg.FallbackPrompt = reference.FallbackPrompt
	cfg.IntentPrompts = cloneStringMap(reference.IntentPrompts)

	return cfg, nil
}

func (s *Service) resolveTenantModelID(
	ctx context.Context,
	targetTenantID uint64,
	sourceModelID string,
	expectedType types.ModelType,
) (string, error) {
	if sourceModelID == "" {
		return "", nil
	}

	sourceModel, err := s.loadSourceModel(ctx, sourceModelID)
	if err != nil {
		return "", err
	}
	if sourceModel == nil {
		return "", fmt.Errorf("reference model %s not found", sourceModelID)
	}
	if expectedType != "" && sourceModel.Type != expectedType {
		return "", fmt.Errorf(
			"reference model %s has type %s, expected %s",
			sourceModelID, sourceModel.Type, expectedType,
		)
	}
	if sourceModel.IsBuiltin || sourceModel.TenantID == targetTenantID {
		return sourceModel.ID, nil
	}
	return s.cloneModelForTenant(ctx, sourceModel, targetTenantID)
}

func (s *Service) loadSourceModel(ctx context.Context, id string) (*types.Model, error) {
	var model types.Model
	err := s.db.WithContext(ctx).
		Where("id = ?", id).
		Where("deleted_at IS NULL").
		First(&model).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &model, nil
}

func (s *Service) cloneModelForTenant(
	ctx context.Context,
	source *types.Model,
	targetTenantID uint64,
) (string, error) {
	if source == nil {
		return "", nil
	}
	cloneID := deterministicModelCloneID(targetTenantID, source.ID)
	now := time.Now()
	clone := &types.Model{
		ID:          cloneID,
		TenantID:    targetTenantID,
		Name:        source.Name,
		DisplayName: source.DisplayName,
		Type:        source.Type,
		Source:      source.Source,
		Description: source.Description,
		Parameters:  cloneModelParameters(source.Parameters),
		IsDefault:   false,
		IsBuiltin:   false,
		ManagedBy:   modelCloneManagedBy,
		Status:      source.Status,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	action := ""
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing types.Model
		err := tx.Where("id = ?", cloneID).First(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			action = "created"
			return tx.Create(clone).Error
		}
		if err == nil {
			if existing.TenantID != targetTenantID {
				return fmt.Errorf("managed model clone %s belongs to tenant %d", cloneID, existing.TenantID)
			}
			clone.CreatedAt = existing.CreatedAt
			clone.UpdatedAt = existing.UpdatedAt
			if modelCloneMatches(&existing, clone) {
				return nil
			}
			clone.UpdatedAt = now
			action = "updated"
		}
		return tx.Save(clone).Error
	})
	if err != nil {
		return "", err
	}

	if action != "" {
		logger.Infof(ctx,
			"[builtin-agent-defaults] %s model clone source=%s target=%s tenant=%d",
			action, source.ID, cloneID, targetTenantID,
		)
	}
	return cloneID, nil
}

func deterministicModelCloneID(targetTenantID uint64, sourceModelID string) string {
	return uuid.NewSHA1(
		modelCloneNamespace,
		[]byte(fmt.Sprintf("%d:%s", targetTenantID, sourceModelID)),
	).String()
}

func cloneModelParameters(in types.ModelParameters) types.ModelParameters {
	out := in
	if in.ExtraConfig != nil {
		out.ExtraConfig = make(map[string]string, len(in.ExtraConfig))
		for k, v := range in.ExtraConfig {
			out.ExtraConfig[k] = v
		}
	}
	if in.CustomHeaders != nil {
		out.CustomHeaders = make(map[string]string, len(in.CustomHeaders))
		for k, v := range in.CustomHeaders {
			out.CustomHeaders[k] = v
		}
	}
	return out
}

func modelCloneMatches(existing *types.Model, desired *types.Model) bool {
	if existing == nil || desired == nil {
		return existing == desired
	}
	return existing.TenantID == desired.TenantID &&
		existing.Name == desired.Name &&
		existing.DisplayName == desired.DisplayName &&
		existing.Type == desired.Type &&
		existing.Source == desired.Source &&
		existing.Description == desired.Description &&
		reflect.DeepEqual(existing.Parameters, desired.Parameters) &&
		existing.IsDefault == desired.IsDefault &&
		existing.IsBuiltin == desired.IsBuiltin &&
		existing.ManagedBy == desired.ManagedBy &&
		existing.Status == desired.Status
}

func cloneBoolPtr(in *bool) *bool {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
