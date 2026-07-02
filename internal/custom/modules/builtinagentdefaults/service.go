package builtinagentdefaults

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var (
	ErrBuiltinAgentNotFound = errors.New("builtin agent not found")
	ErrTenantContextMissing = errors.New("tenant context missing")
)

type Service struct {
	db           *gorm.DB
	agentService interfaces.CustomAgentService
}

func NewService(db *gorm.DB, agentService interfaces.CustomAgentService) *Service {
	return &Service{db: db, agentService: agentService}
}

func (s *Service) Reset(ctx context.Context, id string) (*types.CustomAgent, error) {
	if s == nil || s.db == nil || s.agentService == nil {
		return nil, errors.New("builtin agent defaults service is not initialized")
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok {
		return nil, ErrTenantContextMissing
	}
	if !isPublicBuiltinAgent(id) {
		return nil, ErrBuiltinAgentNotFound
	}

	defaultAgent := types.GetBuiltinAgentWithContext(ctx, id, tenantID)
	if defaultAgent == nil {
		return nil, ErrBuiltinAgentNotFound
	}

	currentAgent, err := s.agentService.GetAgentByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if currentAgent == nil || !currentAgent.IsBuiltin {
		return nil, ErrBuiltinAgentNotFound
	}

	now := time.Now()
	resetAgent := &types.CustomAgent{
		ID:               defaultAgent.ID,
		Name:             defaultAgent.Name,
		Description:      defaultAgent.Description,
		Avatar:           defaultAgent.Avatar,
		IsBuiltin:        true,
		TenantID:         tenantID,
		Config:           mergeResetConfig(defaultAgent.Config, currentAgent.Config),
		RunnableByViewer: currentAgent.RunnableByViewer,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	var existing types.CustomAgent
	err = s.db.WithContext(ctx).Where("id = ? AND tenant_id = ?", id, tenantID).First(&existing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err == nil {
		resetAgent.CreatedAt = existing.CreatedAt
		resetAgent.CreatedBy = existing.CreatedBy
		resetAgent.RunnableByViewer = existing.RunnableByViewer
	}

	resetAgent.EnsureDefaults()
	enableThinking := true
	resetAgent.Config.Thinking = &enableThinking
	if err := types.NormalizeCustomAgentDocumentTemplateConfig(&resetAgent.Config); err != nil {
		return nil, err
	}
	resolvedAgent, err := s.ApplyReferenceModelDefaults(ctx, resetAgent, tenantID)
	if err != nil {
		return nil, err
	}
	resetAgent = resolvedAgent

	if err := s.db.WithContext(ctx).Save(resetAgent).Error; err != nil {
		return nil, err
	}
	return resetAgent, nil
}

func isPublicBuiltinAgent(id string) bool {
	if id == "" {
		return false
	}
	for _, builtinID := range types.GetBuiltinAgentIDs() {
		if builtinID == id && types.IsBuiltinAgentID(id) {
			return true
		}
	}
	return false
}

func mergeResetConfig(defaultConfig, currentConfig types.CustomAgentConfig) types.CustomAgentConfig {
	cfg := defaultConfig

	cfg.ModelID = currentConfig.ModelID
	cfg.RerankModelID = currentConfig.RerankModelID
	cfg.QueryUnderstandModelID = currentConfig.QueryUnderstandModelID
	cfg.VLMModelID = currentConfig.VLMModelID
	cfg.ASRModelID = currentConfig.ASRModelID
	cfg.ImageStorageProvider = currentConfig.ImageStorageProvider

	if defaultConfig.AgentType == types.AgentTypeDataAnalysis {
		cfg.DBDataSources = cloneStringSlice(currentConfig.DBDataSources)
	}

	cfg.MCPSelectionMode = currentConfig.MCPSelectionMode
	cfg.MCPServices = cloneStringSlice(currentConfig.MCPServices)
	cfg.MCPAuthWaitTimeout = currentConfig.MCPAuthWaitTimeout

	enableThinking := true
	cfg.Thinking = &enableThinking

	return cfg
}

func cloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
