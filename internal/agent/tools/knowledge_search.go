package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/chatretrieval"
	"sync"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var knowledgeSearchTool = BaseTool{
	name: ToolKnowledgeSearch,
	description: `Primary hybrid knowledge-base search for retrieving evidence by semantic meaning and lexical relevance.

This tool combines vector and keyword retrieval, then reranks the candidates. It is the default first retrieval route for ordinary knowledge questions, including procedures, definitions, named objects, exact identifiers, and FAQ-style questions.

## Invocation boundary
Call this tool only when the current request needs knowledge-base evidence. Do
not call it for conversation-state updates, source quoting, or reformatting that
can be completed solely from user-authored dialogue. Honor an explicit
no-retrieval boundary. Conversely, a knowledge question may still require this
tool when it merely quotes or discusses a prohibited action; decide from the
semantic task, not isolated negative words.

## Required Input Behavior
"queries" must contain **1–5 concise, well-formed natural-language evidence needs**. Preserve exact names, identifiers, versions, error codes, or quoted phrases when they are material to the request; do not replace them with broader concepts. Avoid unprocessed long messages, redundant paraphrases, or disconnected keyword lists.

One focused call is normally enough. Use literal chunk grep or a document read only when this hybrid result is truncated, catalog-only, ambiguous, or lacks a specifically requested exact detail. Do not repeat equivalent searches after sufficient claim-bearing evidence is available.

## Parameters
- queries (required): 1–5 concise natural-language evidence questions.
- knowledge_base_ids (optional): limit the search scope.

When multiple knowledge bases are available and the user names one, resolve
that name from the system-provided knowledge-base list and pass only its ID in
knowledge_base_ids. A document title does not prove knowledge-base membership.

## Output
Returns hybrid-retrieved chunks ranked by relevance and reranked when applicable.`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "queries": {
      "type": "array",
      "description": "REQUIRED: 1-5 semantic questions/topics (e.g., ['What is RAG?', 'RAG benefits'])",
      "items": {
        "type": "string"
      },
      "minItems": 1,
      "maxItems": 5
    },
    "knowledge_base_ids": {
      "type": "array",
      "description": "Optional: KB IDs to search",
      "items": {
        "type": "string"
      },
      "minItems": 0,
      "maxItems": 10
    }
  },
  "required": ["queries"]
}`),
}

// KnowledgeSearchInput defines the input parameters for knowledge search tool
type KnowledgeSearchInput struct {
	Queries          []string `json:"queries"`
	KnowledgeBaseIDs []string `json:"knowledge_base_ids,omitempty"`
}

// searchResultWithMeta wraps search result with metadata about which query matched it
type searchResultWithMeta struct {
	*types.SearchResult
	SourceQuery       string
	QueryType         string // "vector" or "keyword"
	KnowledgeBaseID   string // ID of the knowledge base this result came from
	KnowledgeBaseType string // Type of the knowledge base (document, faq, etc.)
}

// KnowledgeSearchTool searches the current authorized resource scope.
type KnowledgeSearchTool struct {
	BaseTool
	knowledgeBaseService interfaces.KnowledgeBaseService
	knowledgeService     interfaces.KnowledgeService
	chunkService         interfaces.ChunkService
	searchTargets        types.SearchTargets // Pre-computed unified search targets
	rerankModel          rerank.Reranker
	agentConfig          *types.AgentConfig
	config               *config.Config // Global config for fallback values

}

// NewKnowledgeSearchTool creates a new knowledge search tool
func NewKnowledgeSearchTool(
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	searchTargets types.SearchTargets,
	rerankModel rerank.Reranker,
	agentConfig *types.AgentConfig,
	cfg *config.Config,
) *KnowledgeSearchTool {
	return &KnowledgeSearchTool{
		BaseTool:             knowledgeSearchTool,
		knowledgeBaseService: knowledgeBaseService,
		knowledgeService:     knowledgeService,
		chunkService:         chunkService,
		searchTargets:        searchTargets,
		rerankModel:          rerankModel,
		agentConfig:          agentConfig,
		config:               cfg,
	}
}

// Execute executes the knowledge search tool
func (t *KnowledgeSearchTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Execute started")

	// Parse args from json.RawMessage
	var input KnowledgeSearchInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][KnowledgeSearch] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	// Log input arguments
	argsJSON, _ := json.MarshalIndent(input, "", "  ")
	logger.Debugf(ctx, "[Tool][KnowledgeSearch] Input args:\n%s", string(argsJSON))

	// Determine which KBs to search - user can optionally filter to specific KBs
	var userSpecifiedKBs []string
	if len(input.KnowledgeBaseIDs) > 0 {
		userSpecifiedKBs = input.KnowledgeBaseIDs
		logger.Infof(ctx, "[Tool][KnowledgeSearch] User specified %d knowledge bases: %v", len(userSpecifiedKBs), userSpecifiedKBs)
	}

	// Use pre-computed search targets, optionally filtered by user-specified KBs
	searchTargets := t.searchTargets
	if len(userSpecifiedKBs) > 0 {
		// Filter search targets to only include user-specified KBs
		userKBSet := make(map[string]bool)
		for _, kbID := range userSpecifiedKBs {
			userKBSet[kbID] = true
		}
		var filteredTargets types.SearchTargets
		for _, target := range t.searchTargets {
			if userKBSet[target.KnowledgeBaseID] {
				filteredTargets = append(filteredTargets, target)
			}
		}
		searchTargets = filteredTargets
	}

	// Validate search targets
	if len(searchTargets) == 0 {
		logger.Errorf(ctx, "[Tool][KnowledgeSearch] No search targets available")
		return &types.ToolResult{
			Success: false,
			Error:   "no knowledge bases specified and no search targets configured",
		}, fmt.Errorf("no search targets available")
	}

	kbIDs := searchTargets.GetAllKnowledgeBaseIDs()
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Using %d search targets across %d KBs", len(searchTargets), len(kbIDs))

	// Parse query parameter
	queries := input.Queries

	// Validate: query must be provided
	if len(queries) == 0 {
		logger.Errorf(ctx, "[Tool][KnowledgeSearch] No queries provided")
		return &types.ToolResult{
			Success: false,
			Error:   "queries parameter is required",
		}, fmt.Errorf("no queries provided")
	}

	logger.Infof(ctx, "[Tool][KnowledgeSearch] Queries: %v", queries)

	// Search parameters: fall back to global config, then to hardcoded defaults.
	// We used to read tenant.ConversationConfig here as the first source of
	// truth, but that field was removed when the chat pipeline moved to
	// CustomAgent — tenant-level KV settings now live on the agent itself.
	var topK, rerankTopK int
	var vectorThreshold, keywordThreshold, minScore float64

	// Agent-level retrieval strategy is the source of truth for configured
	// agents. Fall back to global config only for legacy/default paths.
	if t.agentConfig != nil {
		topK = t.agentConfig.EmbeddingTopK
		rerankTopK = t.agentConfig.RerankTopK
		vectorThreshold = t.agentConfig.VectorThreshold
		keywordThreshold = t.agentConfig.KeywordThreshold
		minScore = t.agentConfig.RerankThreshold
	}

	// Fallback to global config if not set. Thresholds are special: 0 is a
	// valid explicit agent-level value meaning "no threshold", so only fall
	// back for thresholds when no agent runtime config was provided.
	if topK == 0 && t.config != nil {
		topK = t.config.Conversation.EmbeddingTopK
	}
	if rerankTopK == 0 && t.config != nil {
		rerankTopK = t.config.Conversation.RerankTopK
	}
	if t.agentConfig == nil && vectorThreshold == 0 && t.config != nil {
		vectorThreshold = t.config.Conversation.VectorThreshold
	}
	if t.agentConfig == nil && keywordThreshold == 0 && t.config != nil {
		keywordThreshold = t.config.Conversation.KeywordThreshold
	}

	// Final fallback to hardcoded defaults if config is not available
	if topK == 0 {
		topK = 5
	}
	if rerankTopK == 0 {
		rerankTopK = topK
	}
	if t.agentConfig == nil && vectorThreshold == 0 {
		vectorThreshold = 0.6
	}
	if t.agentConfig == nil && keywordThreshold == 0 {
		keywordThreshold = 0.5
	}
	if t.agentConfig == nil && minScore == 0 {
		minScore = 0.3
	}

	logger.Infof(
		ctx,
		"[Tool][KnowledgeSearch] Search params: top_k=%d, rerank_top_k=%d, vector_threshold=%.2f, keyword_threshold=%.2f, min_score=%.2f",
		topK,
		rerankTopK,
		vectorThreshold,
		keywordThreshold,
		minScore,
	)

	// Execute concurrent search using pre-computed search targets
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Starting concurrent search with %d search targets",
		len(searchTargets))
	kbTypeMap, kbNameMap := t.getKnowledgeBaseInfo(ctx, kbIDs)

	allResults, searchedKBs, searchErr := t.concurrentSearchByTargets(ctx, queries, searchTargets,
		topK, vectorThreshold, keywordThreshold, kbTypeMap)
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Concurrent search completed: %d raw results", len(allResults))
	if searchErr != nil && len(allResults) == 0 {
		failed := &types.ToolResult{Success: false, Error: searchErr.Error()}
		chatretrieval.Receipt(failed, "document_chunks", kbIDs, searchedKBs, queries, 0, searchErr)
		return failed, searchErr
	}

	// Note: HybridSearch now uses RRF (Reciprocal Rank Fusion) which produces normalized scores
	// RRF scores are in range [0, ~0.033] (max when rank=1 on both sides: 2/(60+1))
	// Threshold filtering is already done inside HybridSearch before RRF, so we skip it here

	baseResults := make([]*types.SearchResult, 0, len(allResults))
	byID := map[string]*searchResultWithMeta{}
	for _, result := range allResults {
		baseResults = append(baseResults, result.SearchResult)
		byID[chatretrieval.Identity(result.SearchResult)] = result
	}
	var scorer chatretrieval.Scorer
	if t.rerankModel != nil {
		scorer = func(ctx context.Context, query string, passages []string) (map[int]float64, error) {
			ranked, err := t.rerankModel.Rerank(ctx, query, passages)
			scores := map[int]float64{}
			for _, row := range ranked {
				scores[row.Index] = row.RelevanceScore
			}
			return scores, err
		}
	}
	budget := t.retrievalBudget(topK, rerankTopK)
	budget.EvidenceTokens = 0 // Resolve exact source fragments before applying the context budget.
	ranked, rankErr := chatretrieval.Rank(ctx, queries, baseResults, scorer, minScore, budget)
	if rankErr != nil {
		return &types.ToolResult{Success: false, Error: rankErr.Error()}, rankErr
	}
	deduplicatedResults := make([]*searchResultWithMeta, 0, len(ranked))
	for _, result := range ranked {
		meta := *byID[chatretrieval.Identity(result)]
		meta.SearchResult = result
		deduplicatedResults = append(deduplicatedResults, &meta)
	}

	// Log all ranked results (including lower ranks for rerank debugging)
	if len(deduplicatedResults) > 0 {
		total := len(deduplicatedResults)
		for i, r := range deduplicatedResults {
			logger.Infof(ctx, "[Tool][KnowledgeSearch][Rank %d/%d] score=%.3f, type=%s, kb=%s, chunk_id=%s",
				i+1, total, r.Score, r.QueryType, r.KnowledgeID, r.ID)
		}
	}

	// Enrich image info for search results (lazy-loaded from child image chunks)
	if t.chunkService != nil && len(deduplicatedResults) > 0 {
		byTenant := make(map[uint64][]*types.SearchResult)
		for _, r := range deduplicatedResults {
			tid := t.searchTargets.GetTenantIDForKB(r.KnowledgeBaseID)
			if tid == 0 {
				continue
			}
			byTenant[tid] = append(byTenant[tid], r.SearchResult)
		}
		for tid, batch := range byTenant {
			searchutil.EnrichSearchResultsImageInfo(ctx, t.chunkService.GetRepository(), tid, batch)
		}
	}

	// Build output
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Formatting output with %d final results", len(deduplicatedResults))
	result, err := t.formatOutput(ctx, deduplicatedResults, kbIDs, queries, kbNameMap)
	if err != nil {
		logger.Errorf(ctx, "[Tool][KnowledgeSearch] Failed to format output: %v", err)
		return result, err
	}
	result.Data["search_targets"] = searchTargets
	chatretrieval.Receipt(result, "document_chunks", kbIDs, searchedKBs, queries, len(allResults), searchErr)

	logger.Infof(ctx, "[Tool][KnowledgeSearch] Output: %s", result.Output)
	return result, nil
}

// getKnowledgeBaseInfo fetches model-visible type/name provenance for each KB.
// Both values come from the same already-required lookup, so adding the name
// does not add a retrieval or model round trip.
func (t *KnowledgeSearchTool) getKnowledgeBaseInfo(ctx context.Context, kbIDs []string) (map[string]string, map[string]string) {
	kbTypeMap := make(map[string]string, len(kbIDs))
	kbNameMap := make(map[string]string, len(kbIDs))

	for _, kbID := range kbIDs {
		if kbID == "" {
			continue
		}
		if _, exists := kbTypeMap[kbID]; exists {
			continue
		}

		kb, err := t.knowledgeBaseService.GetKnowledgeBaseByID(ctx, kbID)
		if err != nil {
			logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to fetch knowledge base %s info: %v", kbID, err)
			continue
		}

		kbTypeMap[kbID] = kb.Type
		kbNameMap[kbID] = kb.Name
	}

	return kbTypeMap, kbNameMap
}

// concurrentSearchByTargets executes hybrid search using pre-computed search targets.
// Targets sharing the same underlying embedding model (identified by model name + endpoint)
// are grouped so the query embedding is computed once per (model, query) pair, and all
// full-KB targets in a group are combined into a single retrieval call.
func (t *KnowledgeSearchTool) concurrentSearchByTargets(
	ctx context.Context,
	queries []string,
	searchTargets types.SearchTargets,
	topK int,
	vectorThreshold, keywordThreshold float64,
	kbTypeMap map[string]string,
) ([]*searchResultWithMeta, []string, error) {
	// Batch-fetch KB records for embedding model grouping
	kbIDs := searchTargets.GetAllKnowledgeBaseIDs()
	var kbList []*types.KnowledgeBase
	if kbs, err := t.knowledgeBaseService.GetKnowledgeBasesByIDsOnly(ctx, kbIDs); err == nil {
		kbList = kbs
	}

	// Filter out non-searchable KBs (wiki-only / graph-only). knowledge_search
	// can only serve KBs with vector or keyword indexing; feeding a wiki-only
	// KB into HybridSearch causes spurious "model ID cannot be empty" errors
	// because such KBs have no EmbeddingModelID configured. Such scopes
	// should be queried via wiki_search / graph tools instead.
	//
	// KBs that we couldn't fetch from the repo (not in kbList) are kept so
	// the downstream HybridSearch path can still surface the real error.
	searchableKBs := make(map[string]bool, len(kbList))
	knownKBs := make(map[string]bool, len(kbList))
	for _, kb := range kbList {
		if kb == nil {
			continue
		}
		knownKBs[kb.ID] = true
		if kb.IsVectorEnabled() || kb.IsKeywordEnabled() {
			searchableKBs[kb.ID] = true
		}
	}
	filteredTargets := make(types.SearchTargets, 0, len(searchTargets))
	for _, st := range searchTargets {
		if searchableKBs[st.KnowledgeBaseID] {
			filteredTargets = append(filteredTargets, st)
			continue
		}
		if knownKBs[st.KnowledgeBaseID] {
			logger.Infof(ctx, "[Tool][KnowledgeSearch] Skipping non-searchable KB %s (no vector/keyword index, likely wiki/graph-only)", st.KnowledgeBaseID)
			continue
		}
		// KB record unavailable; keep so downstream can surface real errors.
		filteredTargets = append(filteredTargets, st)
	}
	if len(filteredTargets) == 0 {
		logger.Infof(ctx, "[Tool][KnowledgeSearch] No searchable KBs in scope (all wiki/graph-only); skipping retrieval")
		return nil, nil, nil
	}
	searchTargets = filteredTargets

	// Resolve actual model identities (name + endpoint) for cross-tenant grouping
	modelKeyMap := t.knowledgeBaseService.ResolveEmbeddingModelKeys(ctx, kbList)

	groups := make(map[string][]*types.SearchTarget)
	for _, st := range searchTargets {
		key := modelKeyMap[st.KnowledgeBaseID]
		groups[key] = append(groups[key], st)
	}

	searchSlots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allResults := make([]*searchResultWithMeta, 0)
	var failures []error

	for _, query := range queries {
		q := query
		for modelKey, targets := range groups {
			wg.Add(1)
			go func(q string, modelKey string, targets []*types.SearchTarget) {
				defer wg.Done()

				// Compute embedding once for this (model, query) pair
				var queryEmbedding []float32
				if modelKey != "" {
					emb, err := t.knowledgeBaseService.GetQueryEmbedding(ctx, targets[0].KnowledgeBaseID, q)
					if err != nil {
						logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to pre-compute embedding for model %s: %v", modelKey, err)
					} else {
						queryEmbedding = emb
					}
				}

				// Separate full-KB targets (combinable) from specific-knowledge targets
				var fullKBIDs []string
				var knowledgeTargets []*types.SearchTarget
				for _, st := range targets {
					if st.Type == types.SearchTargetTypeKnowledgeBase && len(st.TagIDs) == 0 {
						fullKBIDs = append(fullKBIDs, st.KnowledgeBaseID)
					} else {
						knowledgeTargets = append(knowledgeTargets, st)
					}
				}

				var innerWg sync.WaitGroup

				// Combined retrieval for all full-KB targets in this group
				if len(fullKBIDs) > 0 {
					innerWg.Add(1)
					go func() {
						defer innerWg.Done()
						select {
						case searchSlots <- struct{}{}:
						case <-ctx.Done():
							mu.Lock()
							failures = append(failures, ctx.Err())
							mu.Unlock()
							return
						}
						defer func() { <-searchSlots }()
						searchParams := types.SearchParams{
							QueryText:        q,
							QueryEmbedding:   queryEmbedding,
							KnowledgeBaseIDs: fullKBIDs,
							MatchCount:       topK,
							CandidateCount:   t.retrievalBudget(topK, 0).Candidates,
							VectorThreshold:  vectorThreshold,
							KeywordThreshold: keywordThreshold,
						}
						kbResults, err := t.knowledgeBaseService.HybridSearch(ctx, fullKBIDs[0], searchParams)
						if err != nil {
							logger.Warnf(ctx, "[Tool][KnowledgeSearch] Combined search failed for KBs %v: %v", fullKBIDs, err)
							mu.Lock()
							failures = append(failures, fmt.Errorf("knowledge bases %v: %w", fullKBIDs, err))
							mu.Unlock()
							return
						}
						mu.Lock()
						for _, r := range kbResults {
							r.MatchedQueries = []string{q}
							allResults = append(allResults, &searchResultWithMeta{
								SearchResult:      r,
								SourceQuery:       q,
								QueryType:         "hybrid",
								KnowledgeBaseID:   r.KnowledgeBaseID,
								KnowledgeBaseType: kbTypeMap[r.KnowledgeBaseID],
							})
						}
						mu.Unlock()
					}()
				}

				// Individual retrieval for specific-knowledge targets
				for _, target := range knowledgeTargets {
					st := target
					innerWg.Add(1)
					go func() {
						defer innerWg.Done()
						select {
						case searchSlots <- struct{}{}:
						case <-ctx.Done():
							mu.Lock()
							failures = append(failures, ctx.Err())
							mu.Unlock()
							return
						}
						defer func() { <-searchSlots }()
						searchParams := types.SearchParams{
							QueryText:        q,
							QueryEmbedding:   queryEmbedding,
							MatchCount:       topK,
							CandidateCount:   t.retrievalBudget(topK, 0).Candidates,
							VectorThreshold:  vectorThreshold,
							KeywordThreshold: keywordThreshold,
							KnowledgeIDs:     st.KnowledgeIDs,
							TagIDs:           st.TagIDs,
						}
						kbResults, err := t.knowledgeBaseService.HybridSearch(ctx, st.KnowledgeBaseID, searchParams)
						if err != nil {
							logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to search KB %s: %v", st.KnowledgeBaseID, err)
							mu.Lock()
							failures = append(failures, fmt.Errorf("knowledge base %s: %w", st.KnowledgeBaseID, err))
							mu.Unlock()
							return
						}
						mu.Lock()
						for _, r := range kbResults {
							r.MatchedQueries = []string{q}
							allResults = append(allResults, &searchResultWithMeta{
								SearchResult:      r,
								SourceQuery:       q,
								QueryType:         "hybrid",
								KnowledgeBaseID:   r.KnowledgeBaseID,
								KnowledgeBaseType: kbTypeMap[r.KnowledgeBaseID],
							})
						}
						mu.Unlock()
					}()
				}

				innerWg.Wait()
			}(q, modelKey, targets)
		}
	}
	wg.Wait()
	return allResults, searchTargets.GetAllKnowledgeBaseIDs(), errors.Join(failures...)
}

// rerankResults applies reranking to all search results (including FAQ entries)
// using the rerank model or LLM fallback, then filters by threshold and applies
// one relevance scale.

func (t *KnowledgeSearchTool) rerankThreshold() float64 {
	// An agent-level zero is an explicit "no absolute cutoff" setting. Do not
	// silently replace it with a global/default threshold: doing so makes the
	// same configured agent lose recall depending on unrelated tenant settings.
	if t.agentConfig != nil {
		return t.agentConfig.RerankThreshold
	}
	if t.config != nil && t.config.Conversation != nil && t.config.Conversation.RerankThreshold > 0 {
		return t.config.Conversation.RerankThreshold
	}
	return 0.3
}

// formatOutput resolves exact fragments before budgeting and rendering. Full
// source/score metadata remains available to the UI and persisted trace.
func (t *KnowledgeSearchTool) formatOutput(ctx context.Context, results []*searchResultWithMeta, kbsToSearch, queries []string, kbNames map[string]string) (*types.ToolResult, error) {
	inputs := make([]*types.SearchResult, 0, len(results))
	for _, r := range results {
		if r != nil && r.SearchResult != nil {
			inputs = append(inputs, r.SearchResult)
		}
	}
	var refs []*types.SearchResult
	if len(inputs) > 0 {
		tenantID, _ := types.TenantIDFromContext(ctx)
		var err error
		refs, err = sourcerefs.ResolveRetrievalEvidence(ctx, t.chunkService.GetRepository(), tenantID, inputs)
		if err != nil {
			return nil, fmt.Errorf("resolve exact knowledge-search evidence: %w", err)
		}
	}
	budget := t.retrievalBudget(0, 0)
	refs = chatretrieval.Select(refs, len(refs), budget.EvidenceTokens)
	output, rows := chatretrieval.FormatEvidence(refs, kbNames)
	counts := map[string]int{}
	for _, ref := range refs {
		counts[ref.KnowledgeBaseID]++
	}
	data := map[string]any{"knowledge_base_ids": kbsToSearch, "queries": queries, "results": rows,
		"count": len(rows), "kb_counts": counts, "display_type": "search_results"}
	return &types.ToolResult{Success: true, Output: output, Data: data, SourceReferences: sourcerefs.CitableReferences(refs)}, nil
}

func (t *KnowledgeSearchTool) retrievalBudget(recall, final int) chatretrieval.Budget {
	if t.agentConfig == nil {
		return chatretrieval.ResolveBudget(recall, final)
	}
	return chatretrieval.ResolveBudget(recall, final, t.agentConfig.RetrievalBudget)
}
