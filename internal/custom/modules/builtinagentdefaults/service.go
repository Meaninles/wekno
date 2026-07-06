package builtinagentdefaults

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

// EnsureAllUsers backfills built-in agent records for existing active personal
// tenants. Missing rows are created with tenant-local model IDs resolved from
// the reference tenant defaults; existing rows are left untouched.
func (s *Service) EnsureAllUsers(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var users []types.User
	if err := s.db.WithContext(ctx).
		Where("is_active = ?", true).
		Where("tenant_id <> ?", referenceTenantID).
		Find(&users).Error; err != nil {
		return err
	}

	seenTenants := make(map[uint64]bool, len(users))
	errs := make([]string, 0)
	for i := range users {
		user := &users[i]
		if user.TenantID == 0 || seenTenants[user.TenantID] {
			continue
		}
		seenTenants[user.TenantID] = true
		if err := s.EnsureUserProvisioned(ctx, user); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%d: %v", user.Username, user.TenantID, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// EnsureUserProvisioned makes the user's home tenant own concrete built-in
// agent rows before the frontend loads models/agents on first login.
func (s *Service) EnsureUserProvisioned(ctx context.Context, user *types.User) error {
	if s == nil || s.db == nil || user == nil || user.TenantID == 0 || !user.IsActive {
		return nil
	}
	if user.TenantID == referenceTenantID {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	provisionCtx := context.WithValue(ctx, types.TenantIDContextKey, user.TenantID)
	if user.ID != "" {
		provisionCtx = context.WithValue(provisionCtx, types.UserIDContextKey, user.ID)
	}
	return s.ensureTenantBuiltinAgents(provisionCtx, user.TenantID)
}

func (s *Service) ensureTenantBuiltinAgents(ctx context.Context, tenantID uint64) error {
	errs := make([]string, 0)
	for _, builtinID := range types.GetBuiltinAgentIDs() {
		if !isPublicBuiltinAgent(builtinID) {
			continue
		}
		if err := s.ensureTenantBuiltinAgent(ctx, tenantID, builtinID); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", builtinID, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *Service) ensureTenantBuiltinAgent(ctx context.Context, tenantID uint64, id string) error {
	var existingCount int64
	err := s.db.WithContext(ctx).
		Model(&types.CustomAgent{}).
		Where("id = ? AND tenant_id = ?", id, tenantID).
		Count(&existingCount).Error
	if err != nil {
		return err
	}
	if existingCount > 0 {
		return nil
	}

	defaultAgent := types.GetBuiltinAgentWithContext(ctx, id, tenantID)
	if defaultAgent == nil {
		return ErrBuiltinAgentNotFound
	}

	now := time.Now()
	agent := &types.CustomAgent{
		ID:               defaultAgent.ID,
		Name:             defaultAgent.Name,
		Description:      defaultAgent.Description,
		Avatar:           defaultAgent.Avatar,
		IsBuiltin:        true,
		TenantID:         tenantID,
		RunnableByViewer: defaultAgent.RunnableByViewer,
		Config:           defaultAgent.Config,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	agent.EnsureDefaults()
	if err := types.NormalizeCustomAgentDocumentTemplateConfig(&agent.Config); err != nil {
		return err
	}
	resolvedAgent, err := s.ApplyReferenceModelDefaults(ctx, agent, tenantID)
	if err != nil {
		return err
	}
	agent = resolvedAgent

	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(agent).Error
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
