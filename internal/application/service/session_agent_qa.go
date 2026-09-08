package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/agentconfig"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// AgentQA and KnowledgeQA share the registered durable runtime.
func (s *sessionService) AgentQA(ctx context.Context, req *types.QARequest, bus *event.EventBus) error {
	if req == nil || req.Session == nil {
		return errors.New("QA request and session are required")
	}
	copyReq := *req
	if copyReq.CustomAgent == nil {
		kbIDs, knowledgeIDs := s.resolveKnowledgeBases(ctx, req)
		modelID, err := s.resolveChatModelID(ctx, req, kbIDs, knowledgeIDs)
		if err != nil {
			return err
		}
		tenantID, _ := types.TenantIDFromContext(ctx)
		builtinAgent := types.GetBuiltinAgent(types.BuiltinKnowledgeQAID, tenantID)
		if builtinAgent == nil {
			return errors.New("default QA profile is unavailable")
		}
		// The legacy agent-less endpoint still resolves a model from the
		// request/session fallback. Once the built-in QA profile is materialized,
		// put that effective value on the profile so the agent-authoritative
		// model rule does not erase the legacy path.
		builtinAgent.Config.ModelID = modelID
		resolvedAgent, resolveErr := resolveBuiltinAgentConfig(ctx, builtinAgent, tenantID)
		if resolveErr != nil {
			return resolveErr
		}
		copyReq.CustomAgent = resolvedAgent
		copyReq.CustomAgent.Config.KBSelectionMode = "selected"
		copyReq.CustomAgent.Config.KnowledgeBases = kbIDs
		copyReq.KnowledgeIDs = knowledgeIDs
		copyReq.SummaryModelID = modelID
	}
	return runUnifiedAgent(ctx, &copyReq, bus)
}

// BuildAgentRuntimeConfig creates the same runtime AgentConfig used by AgentQA,
// including shared-agent tenant scoping, resolved KB/search targets, selected
// model ID, VLM model ID and request attachments.
func (s *sessionService) BuildAgentRuntimeConfig(
	ctx context.Context,
	req *types.QARequest,
) (*types.AgentConfig, error) {
	if req == nil || req.CustomAgent == nil {
		return nil, errors.New("custom agent configuration is required for agent runtime config")
	}
	req.CustomAgent.EnsureDefaults()

	agentTenantID := s.resolveRetrievalTenantID(ctx, req)
	logger.Infof(ctx, "Building agent runtime config, session ID: %s, agent tenant ID: %d, agent ID: %s",
		req.Session.ID, agentTenantID, req.CustomAgent.ID)

	var tenantInfo *types.Tenant
	if v := ctx.Value(types.TenantInfoContextKey); v != nil {
		tenantInfo, _ = v.(*types.Tenant)
	}
	// When agent belongs to another tenant (shared agent), use the agent's
	// tenant for model/KB/MCP scope; load tenantInfo if needed.
	if tenantInfo == nil || tenantInfo.ID != agentTenantID {
		if s.tenantService != nil {
			if agentTenant, err := s.tenantService.GetTenantByID(ctx, agentTenantID); err == nil && agentTenant != nil {
				tenantInfo = agentTenant
				logger.Infof(ctx, "Using agent tenant info for runtime scope, tenant ID: %d", agentTenantID)
			}
		}
	}
	if tenantInfo == nil {
		logger.Warnf(ctx, "Tenant info not available for agent tenant %d, proceeding with defaults", agentTenantID)
		tenantInfo = &types.Tenant{ID: agentTenantID}
	}

	agentConfig, err := s.buildAgentConfig(ctx, req, tenantInfo, agentTenantID)
	if err != nil {
		return nil, err
	}
	if req.CustomAgent.Config.VLMModelID != "" {
		agentConfig.VLMModelID = req.CustomAgent.Config.VLMModelID
	}

	effectiveModelID, err := s.resolveChatModelID(ctx, req, agentConfig.KnowledgeBases, agentConfig.KnowledgeIDs)
	if err != nil {
		return nil, err
	}
	if effectiveModelID == "" {
		return nil, errors.New("summary model (model_id) is not configured in custom agent settings")
	}
	agentConfig.RuntimeModelID = effectiveModelID
	return agentConfig, nil
}

// buildAgentConfig creates a runtime AgentConfig from the QARequest's custom agent configuration,
// tenant info, and resolved knowledge bases / search targets.
func (s *sessionService) buildAgentConfig(
	ctx context.Context,
	req *types.QARequest,
	tenantInfo *types.Tenant,
	agentTenantID uint64,
) (*types.AgentConfig, error) {
	customAgent := req.CustomAgent
	resolved, resolveErr := agentconfig.ResolvePrompts(customAgent.Config, s.cfg.PromptTemplates)
	if resolveErr != nil {
		return nil, resolveErr
	}
	agentCopy := *customAgent
	agentCopy.Config = resolved
	customAgent = &agentCopy
	agentConfig := &types.AgentConfig{
		RetrievalBudget:             customAgent.Config.RetrievalBudget,
		AgentID:                     customAgent.ID,
		AgentTenantID:               agentTenantID,
		MaxIterations:               customAgent.Config.MaxIterations,
		AgentType:                   customAgent.Config.AgentType,
		Temperature:                 customAgent.Config.Temperature,
		MaxCompletionTokens:         customAgent.Config.MaxCompletionTokens,
		WebSearchEnabled:            customAgent.Config.WebSearchEnabled && req.WebSearchEnabled,
		WebSearchMaxResults:         customAgent.Config.WebSearchMaxResults,
		WebSearchProviderID:         customAgent.Config.WebSearchProviderID,
		WebFetchEnabled:             customAgent.Config.WebFetchEnabled,
		WebFetchTopN:                customAgent.Config.WebFetchTopN,
		MultiTurnEnabled:            customAgent.Config.MultiTurnEnabled,
		HistoryTurns:                customAgent.Config.HistoryTurns,
		DocumentTemplate:            customAgent.Config.DocumentTemplate,
		MCPSelectionMode:            customAgent.Config.MCPSelectionMode,
		MCPServices:                 customAgent.Config.MCPServices,
		MCPAuthWaitTimeout:          customAgent.Config.MCPAuthWaitTimeout,
		Thinking:                    customAgent.Config.Thinking,
		RetrieveKBOnlyWhenMentioned: customAgent.Config.RetrieveKBOnlyWhenMentioned,
		LLMCallTimeout:              customAgent.Config.LLMCallTimeout,
		RetainRetrievalHistory:      customAgent.Config.RetainRetrievalHistory,
		DBDataSources:               append([]string(nil), customAgent.Config.DBDataSources...),
		RuntimeAttachments:          append(types.MessageAttachments(nil), req.Attachments...),
		EmbeddingTopK:               customAgent.Config.EmbeddingTopK,
		KeywordThreshold:            customAgent.Config.KeywordThreshold,
		VectorThreshold:             customAgent.Config.VectorThreshold,
		RerankTopK:                  customAgent.Config.RerankTopK,
		RerankThreshold:             customAgent.Config.RerankThreshold,
		FAQPriorityEnabled:          customAgent.Config.FAQPriorityEnabled,
		FAQDirectAnswerThreshold:    customAgent.Config.FAQDirectAnswerThreshold,
		EnableArtifacts:             customAgent.Config.EnableArtifacts,
	}
	proMode, proNames := professionalSkillSelection(customAgent)
	agentConfig.ProfessionalSkillsEnabled = proMode == "all" || (proMode == "selected" && len(proNames) > 0)
	agentConfig.AllowedProfessionalSkills = proNames

	// Falls back to global configuration if no specific timeout is set for the agent.
	if agentConfig.LLMCallTimeout == 0 && s.cfg.Agent != nil && s.cfg.Agent.LLMCallTimeout > 0 {
		agentConfig.LLMCallTimeout = s.cfg.Agent.LLMCallTimeout
	}

	// Configure skills based on CustomAgentConfig
	if err := configureRuntimeSkills(ctx, req, agentConfig, customAgent); err != nil {
		return nil, err
	}

	// Resolve knowledge bases using shared helper
	agentConfig.KnowledgeBases, agentConfig.KnowledgeIDs = s.resolveKnowledgeBases(ctx, req)
	if err := applyAgentRuntimeConfigHooks(ctx, req, agentConfig); err != nil {
		return nil, err
	}

	// Use custom agent's allowed tools if specified, otherwise use defaults
	if len(customAgent.Config.AllowedTools) > 0 {
		agentConfig.AllowedTools = customAgent.Config.AllowedTools
	} else {
		agentConfig.AllowedTools = tools.DefaultAllowedTools()
	}
	// Apply per-turn @Skill / @MCP scope. Each helper narrows the agent's
	// whitelist to the mentioned items and records the pinned set used for the
	// <must_use> hint, keeping all scope logic in one place per resource type.
	isSharedAgent := req.Session != nil && req.Session.TenantID != customAgent.TenantID
	applyPerRequestMCPScope(ctx, agentConfig, customAgent.Config.MCPServices, isSharedAgent, req.MCPServiceIDs)

	// Use custom agent's system prompt if specified
	if customAgent.Config.SystemPrompt != "" {
		agentConfig.UseCustomSystemPrompt = true
		agentConfig.SystemPrompt = customAgent.Config.SystemPrompt
	}

	logger.Infof(ctx, "Custom agent config applied: MaxIterations=%d, Temperature=%.2f, AllowedTools=%v, WebSearchEnabled=%v",
		agentConfig.MaxIterations, agentConfig.Temperature, agentConfig.AllowedTools, agentConfig.WebSearchEnabled)

	// Set web search max results from tenant config if not set (default: 5)
	if agentConfig.WebSearchMaxResults == 0 {
		agentConfig.WebSearchMaxResults = 5
		if tenantInfo.WebSearchConfig != nil && tenantInfo.WebSearchConfig.MaxResults > 0 {
			agentConfig.WebSearchMaxResults = tenantInfo.WebSearchConfig.MaxResults
		}
	}

	// Resolve web search provider ID: agent-level > tenant default (is_default=true)
	if agentConfig.WebSearchProviderID == "" {
		if defaultProvider, err := s.webSearchProviderRepo.GetDefault(ctx, tenantInfo.ID); err == nil && defaultProvider != nil {
			agentConfig.WebSearchProviderID = defaultProvider.ID
		}
	}

	logger.Infof(ctx, "Merged agent config from tenant %d and session %s", tenantInfo.ID, req.Session.ID)

	// Log knowledge bases if present
	if len(agentConfig.KnowledgeBases) > 0 || len(req.TagScopes) > 0 {
		if len(agentConfig.KnowledgeBases) > 0 {
			logger.Infof(ctx, "Agent configured with %d knowledge base(s): %v",
				len(agentConfig.KnowledgeBases), agentConfig.KnowledgeBases)
		} else {
			logger.Infof(ctx, "Agent configured with %d tag-scoped search target(s)", len(req.TagScopes))
		}
	} else {
		logger.Infof(ctx, "No knowledge bases specified for agent, running in pure agent mode")
	}

	// Build search targets using agent's tenant (handler has validated access for shared agent)
	searchTargets, err := s.buildSearchTargets(ctx, agentTenantID, agentConfig.KnowledgeBases, agentConfig.KnowledgeIDs, req.TagScopes)
	if err != nil {
		return nil, fmt.Errorf("build authorized search targets: %w", err)
	}
	agentConfig.SearchTargets = searchTargets
	logger.Infof(ctx, "Agent search targets built: %d targets", len(searchTargets))

	if agentConfig.MaxContextTokens <= 0 {
		agentConfig.MaxContextTokens = types.DefaultMaxContextTokens
	}

	return agentConfig, nil
}

// applyPerRequestMCPScope narrows the agent's MCP services to the @MCP mentions
// for this turn and records the pinned set for the <must_use> hint. It is a
// no-op when no services were mentioned or MCP selection is disabled.
func applyPerRequestMCPScope(
	ctx context.Context,
	agentConfig *types.AgentConfig,
	agentPresetMCPs []string,
	isSharedAgent bool,
	requested []string,
) {
	if len(requested) == 0 || agentConfig == nil {
		return
	}
	if agentConfig.MCPSelectionMode == "none" {
		logger.Warnf(ctx, "Ignoring @MCP mention: agent MCP selection is disabled (mode=none)")
		return
	}
	mentioned := dedupPreservingOrder(requested)
	effective, mode := resolvePerRequestMCPScope(mentioned, agentPresetMCPs, agentConfig.MCPSelectionMode, isSharedAgent)
	if len(effective) == 0 {
		logger.Warnf(ctx, "Ignoring @MCP scope outside agent preset: requested=%v agent=%v shared=%v",
			requested, agentPresetMCPs, isSharedAgent)
		return
	}
	agentConfig.MCPSelectionMode = mode
	agentConfig.MCPServices = effective
	agentConfig.PinnedMCPServiceIDs = intersectPreservingRequestOrder(requested, agentConfig.MCPServices)
	logger.Infof(ctx, "Applied per-request @MCP scope: requested=%v mode=%s effective=%v",
		requested, agentConfig.MCPSelectionMode, agentConfig.MCPServices)
}

// resolvePerRequestMCPScope narrows MCP registration for a per-turn @mention.
// selectionMode "none" rejects all mentions. Shared agents never register MCP
// services outside the agent preset.
func resolvePerRequestMCPScope(
	mentioned, agentMCPs []string,
	selectionMode string,
	isSharedAgent bool,
) (effective []string, mode string) {
	if len(mentioned) == 0 {
		return nil, selectionMode
	}
	if isSharedAgent {
		mentioned = intersectPreservingRequestOrder(mentioned, agentMCPs)
		if len(mentioned) == 0 {
			return nil, selectionMode
		}
	}
	switch selectionMode {
	case "none":
		return nil, selectionMode
	case "selected":
		effective = intersectPreservingRequestOrder(mentioned, agentMCPs)
	case "all", "":
		effective = mentioned
	default:
		effective = mentioned
	}
	if len(effective) == 0 {
		return nil, selectionMode
	}
	return effective, "selected"
}

func intersectPreservingRequestOrder(requested []string, allowed []string) []string {
	allowedSet := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		if value != "" {
			allowedSet[value] = true
		}
	}
	result := make([]string, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, value := range requested {
		if value == "" || seen[value] || !allowedSet[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func dedupPreservingOrder(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func lightweightSkillSelection(customAgent *types.CustomAgent) (string, []string) {
	if customAgent == nil {
		return "none", nil
	}
	if customAgent.Config.LightweightSkillsSelectionMode != "" {
		return customAgent.Config.LightweightSkillsSelectionMode, append([]string(nil), customAgent.Config.SelectedLightweightSkills...)
	}
	if customAgent.Config.SkillsSelectionMode != "" {
		return customAgent.Config.SkillsSelectionMode, append([]string(nil), customAgent.Config.SelectedSkills...)
	}
	return "none", nil
}

func professionalSkillSelection(customAgent *types.CustomAgent) (string, []string) {
	if customAgent == nil {
		return "none", nil
	}
	mode := customAgent.Config.ProfessionalSkillsSelectionMode
	if mode == "" {
		mode = "none"
	}
	return mode, append([]string(nil), customAgent.Config.SelectedProfessionalSkills...)
}
