package service

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	chatpipeline "github.com/Tencent/WeKnora/internal/application/service/chat_pipeline"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
)

// KnowledgeQA performs knowledge base question answering with LLM summarization
// Events are emitted through eventBus (references, answer chunks, completion)
// customAgent is optional - if provided, uses custom agent configuration for multiTurnEnabled and historyTurns
func (s *sessionService) KnowledgeQA(ctx context.Context, req *types.QARequest, eventBus *event.EventBus) error {
	return s.AgentQA(ctx, req, eventBus)
}

// selectChatModelID selects the appropriate chat model ID with priority for Remote models
// Priority order:
// 1. Session's SummaryModelID if it's a Remote model
// 2. First knowledge base with a Remote model (from knowledgeBaseIDs or derived from knowledgeIDs)
// 3. Session's SummaryModelID (if not Remote)
// 4. First knowledge base's SummaryModelID
func (s *sessionService) selectChatModelID(
	ctx context.Context,
	session *types.Session,
	knowledgeBaseIDs []string,
	knowledgeIDs []string,
) (string, error) {
	// If no knowledge base IDs but have knowledge IDs, derive KB IDs from knowledge IDs (include shared KB files)
	if len(knowledgeBaseIDs) == 0 && len(knowledgeIDs) > 0 {
		tenantID := types.MustTenantIDFromContext(ctx)
		knowledgeList, err := s.knowledgeService.GetKnowledgeBatchWithSharedAccess(ctx, tenantID, knowledgeIDs)
		if err != nil {
			logger.Warnf(ctx, "Failed to get knowledge batch for model selection: %v", err)
		} else {
			// Collect unique KB IDs from knowledge items
			kbIDSet := make(map[string]bool)
			for _, k := range knowledgeList {
				if k != nil && k.KnowledgeBaseID != "" {
					kbIDSet[k.KnowledgeBaseID] = true
				}
			}
			for kbID := range kbIDSet {
				knowledgeBaseIDs = append(knowledgeBaseIDs, kbID)
			}
			logger.Infof(ctx, "Derived %d knowledge base IDs from %d knowledge IDs for model selection",
				len(knowledgeBaseIDs), len(knowledgeIDs))
		}
	}
	// Check knowledge bases for models
	if len(knowledgeBaseIDs) > 0 {
		// Try to find a knowledge base with Remote model
		for _, kbID := range knowledgeBaseIDs {
			kb, err := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, kbID)
			if err != nil {
				logger.Warnf(ctx, "Failed to get knowledge base: %v", err)
				continue
			}
			if kb != nil && kb.SummaryModelID != "" {
				model, err := s.modelService.GetModelByID(ctx, kb.SummaryModelID)
				if err == nil && model != nil && model.Source == types.ModelSourceRemote {
					logger.Info(ctx, "Using Remote summary model from knowledge base")
					return kb.SummaryModelID, nil
				}
			}
		}

		// If no Remote model found, use first knowledge base's model
		kb, err := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, knowledgeBaseIDs[0])
		if err != nil {
			logger.Errorf(ctx, "Failed to get knowledge base for model ID: %v", err)
			return "", fmt.Errorf("failed to get knowledge base %s: %w", knowledgeBaseIDs[0], err)
		}
		if kb != nil && kb.SummaryModelID != "" {
			logger.Infof(
				ctx,
				"Using summary model from first knowledge base %s: %s",
				knowledgeBaseIDs[0],
				kb.SummaryModelID,
			)
			return kb.SummaryModelID, nil
		}
	}

	// No knowledge bases - try to find any available chat model
	models, err := s.modelService.ListModels(ctx)
	if err != nil {
		logger.Errorf(ctx, "Failed to list models: %v", err)
		return "", fmt.Errorf("failed to list models: %w", err)
	}
	for _, model := range models {
		if model.IsInteractiveChatModel() {
			logger.Infof(ctx, "Using first available KnowledgeQA model: %s", model.ID)
			return model.ID, nil
		}
	}

	logger.Error(ctx, "No chat model ID available")
	return "", fmt.Errorf("no chat model ID available: no knowledge bases configured and no available models")
}

// resolveKnowledgeBasesFromAgent resolves knowledge base IDs based on agent's KBSelectionMode.
// sessionTenantID is the tenant of the current session (caller); it is compared with
// customAgent.TenantID to detect the shared-agent scenario and avoid leaking the
// current user's personal shared KBs into the agent's retrieval scope.
//
// Returns the resolved knowledge base IDs based on the selection mode:
//   - "all": fetches all knowledge bases for the tenant
//   - "selected": uses the explicitly configured knowledge bases
//   - "none": returns empty slice
//   - default: falls back to configured knowledge bases for backward compatibility
func (s *sessionService) resolveKnowledgeBasesFromAgent(
	ctx context.Context,
	customAgent *types.CustomAgent,
	sessionTenantID uint64,
) []string {
	if customAgent == nil {
		return nil
	}

	switch customAgent.Config.KBSelectionMode {
	case "all":
		// Authoritative capability filter for the runtime path. The frontend
		// editor and @mention dropdown apply the same filter, but we don't
		// trust the client here: a stale session payload or API caller could
		// still ask us to retrieve against an incompatible KB and we'd rather
		// just drop it (and log) than feed it to tools that would no-op.
		capFilter := tools.DeriveKBFilterForAgent(customAgent.Config.AgentMode, customAgent.Config.AllowedTools)
		accept := func(kb *types.KnowledgeBase) bool {
			if kb == nil {
				return false
			}
			if capFilter.IsEmpty() {
				return true
			}
			return tools.KBSatisfiesAgentRequirements(kb.Capabilities(), customAgent.Config.AgentMode, customAgent.Config.AllowedTools)
		}

		// Get own knowledge bases (uses ctx TenantID = agent's tenant)
		allKBs, err := s.knowledgeBaseService.ListKnowledgeBases(ctx)
		if err != nil {
			logger.Warnf(ctx, "Failed to list all knowledge bases: %v", err)
		}
		kbIDSet := make(map[string]bool)
		kbIDs := make([]string, 0, len(allKBs))
		ownSkipped := 0
		for _, kb := range allKBs {
			if !accept(kb) {
				ownSkipped++
				continue
			}
			kbIDs = append(kbIDs, kb.ID)
			kbIDSet[kb.ID] = true
		}

		// For shared agents (session tenant != agent tenant), only use the agent
		// tenant's own KBs. Including the current user's shared KBs would leak
		// unrelated KBs from other organisations into the agent's retrieval scope.
		isSharedAgent := sessionTenantID != 0 && sessionTenantID != customAgent.TenantID
		sharedSkipped := 0
		if !isSharedAgent {
			tenantID := types.MustTenantIDFromContext(ctx)
			userIDVal := ctx.Value(types.UserIDContextKey)
			if userIDVal != nil {
				if userID, ok := userIDVal.(string); ok && userID != "" && s.kbShareService != nil {
					callerTenantRole := types.TenantRoleFromContext(ctx)
					sharedList, err := s.kbShareService.ListSharedKnowledgeBases(ctx, tenantID, callerTenantRole)
					if err != nil {
						logger.Warnf(ctx, "Failed to list shared knowledge bases: %v", err)
					} else {
						for _, info := range sharedList {
							if info == nil || info.KnowledgeBase == nil || kbIDSet[info.KnowledgeBase.ID] {
								continue
							}
							if !accept(info.KnowledgeBase) {
								sharedSkipped++
								continue
							}
							kbIDs = append(kbIDs, info.KnowledgeBase.ID)
							kbIDSet[info.KnowledgeBase.ID] = true
						}
					}
				}
			}
		} else {
			logger.Infof(ctx, "Shared agent detected (session tenant %d != agent tenant %d): skipping user's shared KBs",
				sessionTenantID, customAgent.TenantID)
		}

		if ownSkipped+sharedSkipped > 0 {
			logger.Infof(ctx,
				"KBSelectionMode=all: tool-capability filter removed %d own + %d shared KBs (agent=%s, tools=%v)",
				ownSkipped, sharedSkipped, customAgent.ID, customAgent.Config.AllowedTools)
		}
		logger.Infof(ctx, "KBSelectionMode=all: loaded %d knowledge bases (own + shared)", len(kbIDs))
		return kbIDs
	case "selected":
		logger.Infof(ctx, "KBSelectionMode=selected: using %d configured knowledge bases", len(customAgent.Config.KnowledgeBases))
		return customAgent.Config.KnowledgeBases
	case "none":
		logger.Infof(ctx, "KBSelectionMode=none: no knowledge bases configured")
		return nil
	default:
		// Default to "selected" behavior for backward compatibility
		if len(customAgent.Config.KnowledgeBases) > 0 {
			logger.Infof(ctx, "KBSelectionMode not set: using %d configured knowledge bases", len(customAgent.Config.KnowledgeBases))
		}
		return customAgent.Config.KnowledgeBases
	}
}

// buildSearchTargets computes the unified search targets from knowledgeBaseIDs and knowledgeIDs.
// tenantID is the retrieval scope: session.TenantID or effective tenant from shared agent (set by handler).
// This is called once at the request entry point to avoid repeated queries later in the pipeline.
// Logic:
//   - For each knowledgeBaseID: resolve actual TenantID (own, org-shared, or in retrieval-tenant scope for shared agent)
//   - For each knowledgeID: find its knowledgeBaseID; if the KB is already in the list, skip; otherwise add SearchTargetTypeKnowledge
func (s *sessionService) buildSearchTargets(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseIDs []string,
	knowledgeIDs []string,
	tagScopes []types.TagScope,
) (types.SearchTargets, error) {
	var targets types.SearchTargets

	// Build a map from KB ID to TenantID for all KBs we need to process
	kbTenantMap := make(map[string]uint64)

	// Track which KBs are fully searched
	fullKBSet := make(map[string]bool)

	// First pass: batch-fetch KBs, then resolve tenant per ID (tenant scope already set by caller)
	callerTenantRole := types.TenantRoleFromContext(ctx)
	if len(knowledgeBaseIDs) > 0 {
		kbs, _ := s.knowledgeBaseService.GetKnowledgeBasesByIDsOnly(ctx, knowledgeBaseIDs)
		kbByID := make(map[string]*types.KnowledgeBase, len(kbs))
		for _, kb := range kbs {
			if kb != nil {
				kbByID[kb.ID] = kb
			}
		}
		userID, _ := types.UserIDFromContext(ctx)
		for _, kbID := range knowledgeBaseIDs {
			fullKBSet[kbID] = true
			kb := kbByID[kbID]
			if kb == nil {
				kbTenantMap[kbID] = tenantID
			} else if kb.TenantID == tenantID {
				kbTenantMap[kbID] = tenantID
			} else if s.kbShareService != nil && userID != "" {
				hasAccess, _ := s.kbShareService.HasTenantKBPermission(ctx, kbID, tenantID, callerTenantRole, types.OrgRoleViewer)
				if hasAccess {
					kbTenantMap[kbID] = kb.TenantID
				} else {
					kbTenantMap[kbID] = tenantID
				}
			} else {
				kbTenantMap[kbID] = tenantID
			}
			targets = append(targets, &types.SearchTarget{
				Type:            types.SearchTargetTypeKnowledgeBase,
				KnowledgeBaseID: kbID,
				TenantID:        kbTenantMap[kbID],
			})
		}
	}

	// Process individual knowledge IDs (include shared KB files the user has access to)
	if len(knowledgeIDs) > 0 {
		knowledgeList, err := s.knowledgeService.GetKnowledgeBatchWithSharedAccess(ctx, tenantID, knowledgeIDs)
		if err != nil {
			logger.Warnf(ctx, "Failed to get knowledge batch for search targets: %v", err)
			return targets, nil // Return what we have, don't fail
		}

		// Group knowledge IDs by their KB, excluding those already covered by full KB search
		// Also track KB tenant IDs from knowledge items
		kbToKnowledgeIDs := make(map[string][]string)
		for _, k := range knowledgeList {
			if k == nil || k.KnowledgeBaseID == "" {
				continue
			}
			// Track KB -> TenantID mapping from knowledge items
			if kbTenantMap[k.KnowledgeBaseID] == 0 {
				kbTenantMap[k.KnowledgeBaseID] = k.TenantID
			}
			// Skip if this KB is already fully searched
			if fullKBSet[k.KnowledgeBaseID] {
				continue
			}
			kbToKnowledgeIDs[k.KnowledgeBaseID] = append(kbToKnowledgeIDs[k.KnowledgeBaseID], k.ID)
		}

		// Create SearchTargetTypeKnowledge targets for each KB with specific files
		for kbID, kidList := range kbToKnowledgeIDs {
			kbTenant := kbTenantMap[kbID]
			if kbTenant == 0 {
				kbTenant = tenantID // fallback
			}
			targets = append(targets, &types.SearchTarget{
				Type:            types.SearchTargetTypeKnowledge,
				KnowledgeBaseID: kbID,
				TenantID:        kbTenant,
				KnowledgeIDs:    kidList,
			})
		}
	}

	if len(tagScopes) > 0 {
		tagKBIDs := make([]string, 0, len(tagScopes))
		for _, scope := range tagScopes {
			if scope.KnowledgeBaseID != "" && !fullKBSet[scope.KnowledgeBaseID] {
				tagKBIDs = append(tagKBIDs, scope.KnowledgeBaseID)
			}
		}
		kbByID := make(map[string]*types.KnowledgeBase, len(tagKBIDs))
		if len(tagKBIDs) > 0 {
			if kbs, err := s.knowledgeBaseService.GetKnowledgeBasesByIDsOnly(ctx, tagKBIDs); err == nil {
				for _, kb := range kbs {
					if kb != nil {
						kbByID[kb.ID] = kb
					}
				}
			}
		}
		userID, _ := types.UserIDFromContext(ctx)
		for _, scope := range tagScopes {
			if scope.KnowledgeBaseID == "" || len(scope.TagIDs) == 0 || fullKBSet[scope.KnowledgeBaseID] {
				continue
			}
			kbTenant := kbTenantMap[scope.KnowledgeBaseID]
			if kbTenant == 0 {
				kb := kbByID[scope.KnowledgeBaseID]
				if kb == nil || kb.TenantID == tenantID {
					kbTenant = tenantID
				} else if s.kbShareService != nil && userID != "" {
					hasAccess, _ := s.kbShareService.HasTenantKBPermission(ctx, scope.KnowledgeBaseID, tenantID, callerTenantRole, types.OrgRoleViewer)
					if hasAccess {
						kbTenant = kb.TenantID
					} else {
						kbTenant = tenantID
					}
				} else {
					kbTenant = tenantID
				}
				kbTenantMap[scope.KnowledgeBaseID] = kbTenant
			}
			targets = append(targets, &types.SearchTarget{
				Type:            types.SearchTargetTypeKnowledgeBase,
				KnowledgeBaseID: scope.KnowledgeBaseID,
				TenantID:        kbTenant,
				TagIDs:          append([]string(nil), scope.TagIDs...),
			})
		}
	}

	logger.Infof(ctx, "Built %d search targets: %d full KB, %d partial/tag KB, kbTenantMap=%v",
		len(targets), len(knowledgeBaseIDs), len(targets)-len(knowledgeBaseIDs), kbTenantMap)

	return targets, nil
}

// KnowledgeQAByEvent processes knowledge QA through a series of events in the pipeline

// SearchKnowledge performs knowledge base search without LLM summarization
// knowledgeBaseIDs: list of knowledge base IDs to search (supports multi-KB)
// knowledgeIDs: list of specific knowledge (file) IDs to search
func (s *sessionService) SearchKnowledge(ctx context.Context,
	knowledgeBaseIDs []string, knowledgeIDs []string, query string,
) ([]*types.SearchResult, error) {
	logger.Info(ctx, "Start knowledge base search without LLM summary")
	logger.Infof(ctx, "Knowledge base search parameters, knowledge base IDs: %v, knowledge IDs: %v, query: %s",
		knowledgeBaseIDs, knowledgeIDs, query)

	// Get tenant ID from context
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok {
		logger.Error(ctx, "Failed to get tenant ID from context")
		return nil, fmt.Errorf("tenant ID not found in context")
	}

	// Build unified search targets (computed once, used throughout pipeline)
	searchTargets, err := s.buildSearchTargets(ctx, tenantID, knowledgeBaseIDs, knowledgeIDs, nil)
	if err != nil {
		logger.Warnf(ctx, "Failed to build search targets: %v", err)
	}

	if len(searchTargets) == 0 {
		logger.Warn(ctx, "No search targets available, returning empty results")
		return []*types.SearchResult{}, nil
	}

	// Create default retrieval parameters — prefer tenant RetrievalConfig, fallback to built-in defaults
	userID := types.SessionOwnerIDFromContext(ctx)

	// Load tenant-level retrieval config (nil is safe — GetEffective* methods handle nil receiver)
	var rc *types.RetrievalConfig
	if tenant, err2 := s.tenantService.GetTenantByID(ctx, tenantID); err2 == nil {
		rc = tenant.RetrievalConfig
	}

	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			RetrievalBudget:  rc.GetEffectiveBudget(),
			Query:            query,
			UserID:           userID,
			KnowledgeBaseIDs: knowledgeBaseIDs,
			KnowledgeIDs:     knowledgeIDs,
			SearchTargets:    searchTargets,
			MaxRounds:        s.cfg.Conversation.MaxRounds,
			EmbeddingTopK:    rc.GetEffectiveEmbeddingTopK(),
			VectorThreshold:  rc.GetEffectiveVectorThreshold(),
			KeywordThreshold: rc.GetEffectiveKeywordThreshold(),
			RerankTopK:       rc.GetEffectiveRerankTopK(),
			RerankThreshold:  rc.GetEffectiveRerankThreshold(),
		},
		PipelineState: types.PipelineState{
			RewriteQuery: query,
		},
	}

	// Get default models
	models, err := s.modelService.ListModels(ctx)
	if err != nil {
		logger.Errorf(ctx, "Failed to get models: %v", err)
		return nil, err
	}

	// Use rerank model from RetrievalConfig if set, otherwise auto-select the first available
	if rc != nil && rc.RerankModelID != "" {
		chatManage.RerankModelID = rc.RerankModelID
	} else {
		for _, model := range models {
			if model == nil {
				continue
			}
			if model.Type == types.ModelTypeRerank {
				chatManage.RerankModelID = model.ID
				break
			}
		}
	}

	// Use specific event list, only including retrieval-related events, not LLM summarization
	searchEvents := []types.EventType{
		types.CHUNK_SEARCH, // Vector search
		types.CHUNK_RERANK, // Rerank search results
		types.CHUNK_MERGE,  // Merge search results
		types.FILTER_TOP_K, // Filter top K results
	}

	logger.Infof(ctx, "Trigger search event list: %v", searchEvents)

	for _, event := range searchEvents {
		logger.Infof(ctx, "Starting to trigger search event: %v", event)
		stageCtx, stageSpan := langfuse.GetManager().StartSpan(ctx, langfuse.SpanOptions{
			Name: "pipeline." + string(event),
			Metadata: map[string]interface{}{
				"event_type": string(event),
				"flow":       "search_knowledge",
			},
		})
		err := s.eventManager.Trigger(stageCtx, event, chatManage)
		var spanErr error
		if err != nil && err != chatpipeline.ErrSearchNothing {
			spanErr = err.Err
		}
		stageSpan.Finish(nil, nil, spanErr)

		if err == chatpipeline.ErrSearchNothing {
			logger.Warnf(ctx, "Event %v triggered, search result is empty", event)
			return []*types.SearchResult{}, nil
		}

		if err != nil {
			logger.Errorf(ctx, "Event triggering failed, event: %v, error type: %s, description: %s, error: %v",
				event, err.ErrorType, err.Description, err.Err)
			return nil, err.Err
		}
		logger.Infof(ctx, "Event %v triggered successfully", event)
	}

	logger.Infof(ctx, "Knowledge base search completed, found %d results", len(chatManage.MergeResult))
	return chatManage.MergeResult, nil
}

// handleFallbackResponse handles fallback response based on strategy

// handleFixedFallback handles fixed fallback response

// handleModelFallback handles model-based fallback response using streaming

// prioritizeFallbackCurrentTask gives small and locally hosted models an
// unambiguous current-turn boundary without another model call. The rendered
// fallback guidance can contain a long document catalog and conversation
// history may contain a very salient prior answer, so repeat only the current
// request before that context and finish with a short priority reminder.

// renderFallbackPrompt renders the fallback prompt template with query and image context.

// buildKBDocumentListing returns a concise listing of documents in the knowledge bases
// associated with the current pipeline. This gives the LLM visibility into KB contents
// when vector/keyword search returns empty (e.g., broad browse queries).

// consumeFallbackStream consumes the streaming response and emits events

// emitKnowledgeReferencesEvent streams retrieved chunks to the client as a
// `references` SSE event. Must run before CHAT_COMPLETION_STREAM so citations
// arrive while the connection is still open (complete closes the stream).

// emitFallbackAnswer emits fallback answer event

// resolveWebSearchProviderID returns the web search provider ID to use for a pipeline request.
// Priority: agent config > tenant default (is_default=true)
func (s *sessionService) resolveWebSearchProviderID(ctx context.Context, req *types.QARequest, tenantID uint64) string {
	// 1. Agent-level override
	if req.CustomAgent != nil && req.CustomAgent.Config.WebSearchProviderID != "" {
		return req.CustomAgent.Config.WebSearchProviderID
	}
	// 2. Tenant default
	if s.webSearchProviderRepo != nil {
		if defaultProvider, err := s.webSearchProviderRepo.GetDefault(ctx, tenantID); err == nil && defaultProvider != nil {
			return defaultProvider.ID
		}
	}
	return ""
}

// resolveWebFetchEnabled returns whether auto web fetch is enabled for this request.
func (s *sessionService) resolveWebFetchEnabled(req *types.QARequest) bool {
	if req.CustomAgent != nil {
		return req.CustomAgent.Config.WebFetchEnabled
	}
	return false
}

// resolveWebFetchTopN returns how many pages to fetch after rerank.
func (s *sessionService) resolveWebFetchTopN(req *types.QARequest) int {
	if req.CustomAgent != nil && req.CustomAgent.Config.WebFetchTopN > 0 {
		return req.CustomAgent.Config.WebFetchTopN
	}
	return 3
}

// resolveWebSearchMaxResults returns the max results for web search.
// Priority: agent config > tenant default > default (10)
func (s *sessionService) resolveWebSearchMaxResults(ctx context.Context, req *types.QARequest) int {
	if req.CustomAgent != nil && req.CustomAgent.Config.WebSearchMaxResults > 0 {
		return req.CustomAgent.Config.WebSearchMaxResults
	}
	tenantInfo, _ := types.TenantInfoFromContext(ctx)
	if tenantInfo != nil {
		return types.EffectiveWebSearchConfig(tenantInfo.WebSearchConfig).MaxResults
	}
	return types.DefaultWebSearchMaxResults
}
