package builtinagentpolicy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	appservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/custom/modules/builtinagentdefaults"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	modelGroupQwen     = "qwen"
	modelGroupDeepSeek = "deepseek"
)

var (
	ErrPolicyAgentNotBuiltin = errors.New("agent is not a built-in agent")
	ErrPolicyTenantRequired  = errors.New("tenant ID is required for built-in agent policy")
)

// Service owns the tenant-wide policy for built-in-agent model bindings and
// conversation visibility. It deliberately does not own the rest of the
// CustomAgent JSON, so knowledge bases, Skills, MCP and tool configuration
// remain ordinary editable agent configuration.
type Service struct {
	db            *gorm.DB
	modelDefaults *builtinagentdefaults.Service
}

func NewService(db *gorm.DB, modelDefaults *builtinagentdefaults.Service) *Service {
	return &Service{db: db, modelDefaults: modelDefaults}
}

// Migrate is called by the maintenance role before API replicas are started.
// AutoMigrate is safe to retry and the composite primary keys make concurrent
// policy initialization idempotent.
func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.WithContext(ctx).AutoMigrate(
		&BuiltinAgentModelPolicy{},
		&BuiltinAgentChatVisibility{},
	)
}

var defaultVisibleBuiltins = map[string]struct{}{
	types.BuiltinKnowledgeQAID:        {},
	"builtin-quick-answer":            {},
	types.BuiltinDocumentProcessingID: {},
	types.BuiltinGeneralAgentID:       {},
}

var qwenBuiltins = map[string]struct{}{
	types.BuiltinKnowledgeQAID:          {},
	"builtin-quick-answer":              {},
	"builtin-smart-reasoning":           {},
	"builtin-wiki-researcher":           {},
	types.BuiltinKnowledgeGraphExpertID: {},
}

var deepSeekBuiltins = map[string]struct{}{
	"builtin-simple-chat":             {},
	types.BuiltinDeepResearcherID:     {},
	types.BuiltinDataAnalystID:        {},
	types.BuiltinTableAnalystID:       {},
	types.BuiltinGeneralAgentID:       {},
	types.BuiltinDocumentProcessingID: {},
	types.BuiltinWikiFixerID:          {},
}

func isBuiltinAgent(agent *types.CustomAgent) bool {
	if agent == nil {
		return false
	}
	return agent.IsBuiltin || types.IsBuiltinAgentID(agent.ID) || strings.HasPrefix(agent.ID, "builtin-")
}

func isBuiltinID(agentID string) bool {
	return strings.HasPrefix(strings.TrimSpace(agentID), "builtin-") || types.IsBuiltinAgentID(agentID)
}

func modelGroupForAgent(agentID string) string {
	if _, ok := qwenBuiltins[agentID]; ok {
		return modelGroupQwen
	}
	// The explicit map documents the intended DeepSeek set. Unknown future
	// built-ins default to DeepSeek so adding a built-in cannot accidentally
	// expose a user-selected model family before policy is configured.
	if _, ok := deepSeekBuiltins[agentID]; ok {
		return modelGroupDeepSeek
	}
	return modelGroupDeepSeek
}

func (s *Service) Apply(
	ctx context.Context,
	agent *types.CustomAgent,
	tenantID uint64,
) (*types.CustomAgent, error) {
	if agent == nil || !isBuiltinAgent(agent) {
		return agent, nil
	}
	if tenantID == 0 {
		tenantID = agent.TenantID
	}
	if tenantID == 0 {
		return nil, ErrPolicyTenantRequired
	}

	policy, err := s.ensureModelPolicy(ctx, agent, tenantID)
	if err != nil {
		return nil, err
	}
	visible, err := s.ResolveVisibility(ctx, agent.ID, tenantID)
	if err != nil {
		return nil, err
	}

	resolved := *agent
	resolved.TenantID = tenantID
	resolved.Config.ModelID = policy.ModelID
	resolved.Config.RerankModelID = policy.RerankModelID
	resolved.Config.VLMModelID = policy.VLMModelID
	resolved.Config.ASRModelID = policy.ASRModelID
	// QueryUnderstandModelID is reserved in the policy schema for future agent
	// config fields; current CustomAgentConfig has no corresponding field.
	resolved.ModelFieldsLocked = true
	resolved.ModelGroup = policy.ModelGroup
	resolved.VisibleInChat = boolPtr(visible)
	return &resolved, nil
}

// Mutate is called from the native built-in-agent update path. Only tenant
// administrators/system administrators may change model bindings. All other
// requested fields are left untouched and continue through the native update
// and custom normalizer pipeline.
func (s *Service) Mutate(
	ctx context.Context,
	previous *types.CustomAgent,
	requested *types.CustomAgent,
	tenantID uint64,
) error {
	if requested == nil || !isBuiltinAgent(requested) {
		return nil
	}
	if tenantID == 0 {
		tenantID = requested.TenantID
	}
	if tenantID == 0 {
		return fmt.Errorf("%w: %v", appservice.ErrBuiltinAgentModelPolicyInvalid, ErrPolicyTenantRequired)
	}
	seed := requested
	if previous != nil && seed.Config.ModelID == "" {
		copyRequested := *requested
		copyRequested.Config = requested.Config
		copyRequested.Config.ModelID = previous.Config.ModelID
		seed = &copyRequested
	}
	policy, err := s.ensureModelPolicy(ctx, seed, tenantID)
	if err != nil {
		return err
	}
	if !isTenantAdmin(ctx) {
		// The native route normally keeps built-in updates admin-only. This
		// fail-closed service guard also covers RBAC-disabled deployments and
		// direct callers: non-admins may still update non-model configuration,
		// but every centrally controlled model field is restored from policy.
		requested.Config.ModelID = policy.ModelID
		requested.Config.RerankModelID = policy.RerankModelID
		requested.Config.VLMModelID = policy.VLMModelID
		requested.Config.ASRModelID = policy.ASRModelID
		return nil
	}

	mainModelID := strings.TrimSpace(requested.Config.ModelID)
	if mainModelID == "" {
		mainModelID = strings.TrimSpace(policy.ModelID)
	}
	if mainModelID == "" && previous != nil {
		mainModelID = strings.TrimSpace(previous.Config.ModelID)
	}
	mainModel, err := s.loadModel(ctx, mainModelID)
	if err != nil {
		return fmt.Errorf("%w: model_id: %v", appservice.ErrBuiltinAgentModelPolicyInvalid, err)
	}
	if !matchesModelGroup(mainModel, modelGroupForAgent(requested.ID)) {
		return fmt.Errorf(
			"%w: model_id must belong to the %s model group",
			appservice.ErrBuiltinAgentModelPolicyInvalid,
			modelGroupForAgent(requested.ID),
		)
	}
	resolvedMain, err := s.validateAndResolveModel(ctx, tenantID, mainModelID, types.ModelTypeKnowledgeQA, true)
	if err != nil {
		return fmt.Errorf("%w: model_id: %v", appservice.ErrBuiltinAgentModelPolicyInvalid, err)
	}

	resolvedRerank, err := s.validateAndResolveModel(ctx, tenantID, requested.Config.RerankModelID, types.ModelTypeRerank, false)
	if err != nil {
		return fmt.Errorf("%w: rerank_model_id: %v", appservice.ErrBuiltinAgentModelPolicyInvalid, err)
	}
	resolvedVLM, err := s.validateAndResolveModel(ctx, tenantID, requested.Config.VLMModelID, types.ModelTypeVLLM, false)
	if err != nil {
		return fmt.Errorf("%w: vlm_model_id: %v", appservice.ErrBuiltinAgentModelPolicyInvalid, err)
	}
	resolvedASR, err := s.validateAndResolveModel(ctx, tenantID, requested.Config.ASRModelID, types.ModelTypeASR, false)
	if err != nil {
		return fmt.Errorf("%w: asr_model_id: %v", appservice.ErrBuiltinAgentModelPolicyInvalid, err)
	}

	policy.ModelID = resolvedMain
	policy.RerankModelID = resolvedRerank
	policy.VLMModelID = resolvedVLM
	policy.ASRModelID = resolvedASR
	policy.ModelGroup = modelGroupForAgent(requested.ID)
	policy.UpdatedBy, _ = types.AccountUserIDFromContext(ctx)
	policy.UpdatedAt = time.Now()
	if err := s.saveModelPolicy(ctx, policy); err != nil {
		return err
	}

	// Persist tenant-local IDs in the native CustomAgent row as well. The
	// policy overlay remains authoritative on every read, so this also keeps
	// legacy code paths and exports coherent.
	requested.Config.ModelID = resolvedMain
	requested.Config.RerankModelID = resolvedRerank
	requested.Config.VLMModelID = resolvedVLM
	requested.Config.ASRModelID = resolvedASR
	return nil
}

func (s *Service) ConfigureRuntime(ctx context.Context, req *types.QARequest, config *types.AgentConfig) error {
	if req == nil || req.CustomAgent == nil || !isBuiltinAgent(req.CustomAgent) {
		return nil
	}
	tenantID := req.CustomAgent.TenantID
	if tenantID == 0 && config != nil {
		tenantID = config.AgentTenantID
	}
	if tenantID == 0 {
		tenantID, _ = types.TenantIDFromContext(ctx)
	}
	resolved, err := s.Apply(ctx, req.CustomAgent, tenantID)
	if err != nil {
		return err
	}
	*req.CustomAgent = *resolved
	if config != nil {
		config.VLMModelID = resolved.Config.VLMModelID
	}
	return nil
}

func (s *Service) ResolveVisibility(ctx context.Context, agentID string, tenantID uint64) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("%w: policy storage is unavailable", appservice.ErrBuiltinAgentModelPolicyInvalid)
	}
	if !isBuiltinID(agentID) {
		return false, ErrPolicyAgentNotBuiltin
	}
	if tenantID == 0 {
		return false, ErrPolicyTenantRequired
	}
	var row BuiltinAgentChatVisibility
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND agent_id = ?", tenantID, agentID).
		First(&row).Error
	if err == nil {
		return row.Visible, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	_, ok := defaultVisibleBuiltins[agentID]
	return ok, nil
}

func (s *Service) SetVisibility(ctx context.Context, agentID string, tenantID uint64, visible bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: policy storage is unavailable", appservice.ErrBuiltinAgentModelPolicyInvalid)
	}
	if !isTenantAdmin(ctx) {
		return fmt.Errorf("%w: tenant administrator role is required", appservice.ErrBuiltinAgentModelPolicyInvalid)
	}
	if !isBuiltinID(agentID) {
		return fmt.Errorf("%w: %s", appservice.ErrBuiltinAgentModelPolicyInvalid, ErrPolicyAgentNotBuiltin)
	}
	if tenantID == 0 {
		return fmt.Errorf("%w: %v", appservice.ErrBuiltinAgentModelPolicyInvalid, ErrPolicyTenantRequired)
	}
	updatedBy, _ := types.AccountUserIDFromContext(ctx)
	now := time.Now()
	row := &BuiltinAgentChatVisibility{
		TenantID:  tenantID,
		AgentID:   agentID,
		Visible:   visible,
		UpdatedBy: updatedBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "agent_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"visible", "updated_by", "updated_at"}),
	}).Create(row).Error
}

func (s *Service) ensureModelPolicy(ctx context.Context, agent *types.CustomAgent, tenantID uint64) (*BuiltinAgentModelPolicy, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: policy storage is unavailable", appservice.ErrBuiltinAgentModelPolicyInvalid)
	}
	if agent == nil || !isBuiltinAgent(agent) {
		return nil, ErrPolicyAgentNotBuiltin
	}
	var policy BuiltinAgentModelPolicy
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND agent_id = ?", tenantID, agent.ID).
		First(&policy).Error
	if err == nil {
		if policy.ModelGroup == "" {
			policy.ModelGroup = modelGroupForAgent(agent.ID)
		}
		return &policy, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	group := modelGroupForAgent(agent.ID)
	mainID, err := s.selectInitialMainModel(ctx, tenantID, agent, group)
	if err != nil {
		return nil, err
	}
	rerankID, err := s.preserveInitialModel(ctx, tenantID, agent.Config.RerankModelID, types.ModelTypeRerank)
	if err != nil {
		return nil, err
	}
	vlmID, err := s.preserveInitialModel(ctx, tenantID, agent.Config.VLMModelID, types.ModelTypeVLLM)
	if err != nil {
		return nil, err
	}
	asrID, err := s.preserveInitialModel(ctx, tenantID, agent.Config.ASRModelID, types.ModelTypeASR)
	if err != nil {
		return nil, err
	}

	policy = BuiltinAgentModelPolicy{
		TenantID:      tenantID,
		AgentID:       agent.ID,
		ModelGroup:    group,
		ModelID:       mainID,
		RerankModelID: rerankID,
		VLMModelID:    vlmID,
		ASRModelID:    asrID,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if updatedBy, ok := types.AccountUserIDFromContext(ctx); ok {
		policy.UpdatedBy = updatedBy
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&policy).Error; err != nil {
		return nil, err
	}
	// Re-read the winner after a concurrent first request or administrator
	// update. This makes all API replicas converge without process-local cache.
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND agent_id = ?", tenantID, agent.ID).
		First(&policy).Error; err != nil {
		return nil, err
	}
	return &policy, nil
}

func (s *Service) saveModelPolicy(ctx context.Context, policy *BuiltinAgentModelPolicy) error {
	if policy == nil {
		return fmt.Errorf("%w: empty model policy", appservice.ErrBuiltinAgentModelPolicyInvalid)
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "agent_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"model_group", "model_id", "rerank_model_id", "query_understand_model_id",
			"vlm_model_id", "asr_model_id", "updated_by", "updated_at",
		}),
	}).Create(policy).Error
}

func (s *Service) selectInitialMainModel(
	ctx context.Context,
	tenantID uint64,
	agent *types.CustomAgent,
	group string,
) (string, error) {
	// Preserve a previously configured model only when it already belongs to
	// the requested family. Otherwise the initial policy actively selects a
	// Qwen/DeepSeek model instead of silently retaining a mismatched model.
	if current := strings.TrimSpace(agent.Config.ModelID); current != "" {
		if model, err := s.loadModel(ctx, current); err != nil {
			return "", err
		} else if modelEligible(model, types.ModelTypeKnowledgeQA) && matchesModelGroup(model, group) && modelVisibleToTenant(model, tenantID) {
			resolved, resolveErr := s.resolveModelForTenant(ctx, tenantID, current, types.ModelTypeKnowledgeQA)
			if resolveErr == nil {
				return resolved, nil
			}
			logger.Warnf(ctx, "[builtin-agent-policy] failed to resolve existing %s model %s for tenant %d: %v", group, current, tenantID, resolveErr)
		}
	}

	var models []types.Model
	err := s.db.WithContext(ctx).
		Where("type = ?", types.ModelTypeKnowledgeQA).
		Where("status = ?", types.ModelStatusActive).
		Where("deleted_at IS NULL").
		Where("workload_scope <> ? OR workload_scope IS NULL OR workload_scope = ''", types.ModelWorkloadDerivativeOnly).
		Where("tenant_id = ? OR tenant_id = ? OR is_builtin = ?", tenantID, types.DefaultBuiltinModelTenantID, true).
		Find(&models).Error
	if err != nil {
		return "", err
	}
	candidates := make([]types.Model, 0, len(models))
	for _, model := range models {
		if modelEligible(&model, types.ModelTypeKnowledgeQA) && matchesModelGroup(&model, group) {
			candidates = append(candidates, model)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		leftTenant := left.TenantID == tenantID
		rightTenant := right.TenantID == tenantID
		if leftTenant != rightTenant {
			return leftTenant
		}
		if left.IsDefault != right.IsDefault {
			return left.IsDefault
		}
		return left.ID < right.ID
	})
	if len(candidates) == 0 {
		// Keep the policy row visible and locked even when an installation has
		// not yet provisioned the requested family. Runtime then reports the
		// missing model clearly, while an administrator can choose one later.
		logger.Warnf(ctx, "[builtin-agent-policy] no active %s chat model for built-in agent %s in tenant %d", group, agent.ID, tenantID)
		return "", nil
	}
	return s.resolveModelForTenant(ctx, tenantID, candidates[0].ID, types.ModelTypeKnowledgeQA)
}

func (s *Service) preserveInitialModel(ctx context.Context, tenantID uint64, id string, modelType types.ModelType) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil
	}
	model, err := s.loadModel(ctx, id)
	if err != nil {
		return "", err
	}
	if !modelEligible(model, modelType) || !modelVisibleToTenant(model, tenantID) {
		logger.Warnf(ctx, "[builtin-agent-policy] preserving unavailable auxiliary model ID %s for tenant %d as empty", id, tenantID)
		return "", nil
	}
	resolved, err := s.resolveModelForTenant(ctx, tenantID, id, modelType)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func (s *Service) validateAndResolveModel(
	ctx context.Context,
	tenantID uint64,
	id string,
	modelType types.ModelType,
	required bool,
) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		if required {
			return "", errors.New("an active model is required")
		}
		return "", nil
	}
	model, err := s.loadModel(ctx, id)
	if err != nil {
		return "", err
	}
	if !modelVisibleToTenant(model, tenantID) {
		return "", errors.New("model is not visible to the current tenant")
	}
	if !modelEligible(model, modelType) {
		return "", fmt.Errorf("model %s is not an active %s model", id, modelType)
	}
	return s.resolveModelForTenant(ctx, tenantID, id, modelType)
}

func (s *Service) resolveModelForTenant(ctx context.Context, tenantID uint64, id string, modelType types.ModelType) (string, error) {
	if s.modelDefaults != nil {
		return s.modelDefaults.ResolveModelForTenant(ctx, tenantID, id, modelType)
	}
	model, err := s.loadModel(ctx, id)
	if err != nil {
		return "", err
	}
	if model == nil {
		return "", gorm.ErrRecordNotFound
	}
	if model.TenantID == tenantID || model.IsBuiltin {
		return model.ID, nil
	}
	return "", errors.New("model cannot be resolved to the target tenant")
}

func (s *Service) loadModel(ctx context.Context, id string) (*types.Model, error) {
	var model types.Model
	err := s.db.WithContext(ctx).Where("id = ?", id).Where("deleted_at IS NULL").First(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &model, nil
}

func modelVisibleToTenant(model *types.Model, tenantID uint64) bool {
	return model != nil && (model.TenantID == tenantID || model.TenantID == types.DefaultBuiltinModelTenantID || model.IsBuiltin)
}

func modelEligible(model *types.Model, expectedType types.ModelType) bool {
	return model != nil && model.Type == expectedType && model.Status == types.ModelStatusActive &&
		(model.WorkloadScope.Normalize() == types.ModelWorkloadInteractive || expectedType != types.ModelTypeKnowledgeQA)
}

func matchesModelGroup(model *types.Model, group string) bool {
	if model == nil {
		return false
	}
	parts := []string{
		model.ID,
		model.Name,
		model.DisplayName,
		string(model.Source),
		model.Parameters.Provider,
		model.Parameters.BaseURL,
		model.Parameters.InterfaceType,
		model.Parameters.ParameterSize,
	}
	// Some tenant model rows use opaque UUIDs for ID/Name and put the
	// upstream model name in provider-specific extra_config. Include both keys
	// and values so the family check remains useful without exposing secrets.
	for key, value := range model.Parameters.ExtraConfig {
		parts = append(parts, key, value)
	}
	fingerprint := strings.ToLower(strings.Join(parts, " "))
	switch group {
	case modelGroupQwen:
		return strings.Contains(fingerprint, "qwen")
	case modelGroupDeepSeek:
		return strings.Contains(fingerprint, "deepseek")
	default:
		return false
	}
}

func isTenantAdmin(ctx context.Context) bool {
	return types.IsSystemAdminFromContext(ctx) || types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin)
}

func boolPtr(value bool) *bool {
	return &value
}
