package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
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

// KnowledgeSearchTool searches knowledge bases with flexible query modes.
// seenChunks lets repeated calls in the same session surface previously-
// returned chunks in a compact form (mirroring wiki_search's de-duping UX)
// so the LLM doesn't burn tokens re-reading identical content.
type KnowledgeSearchTool struct {
	BaseTool
	knowledgeBaseService interfaces.KnowledgeBaseService
	knowledgeService     interfaces.KnowledgeService
	chunkService         interfaces.ChunkService
	searchTargets        types.SearchTargets // Pre-computed unified search targets
	rerankModel          rerank.Reranker
	chatModel            chat.Chat // Optional chat model for LLM-based reranking
	agentConfig          *types.AgentConfig
	config               *config.Config // Global config for fallback values

	seenMu     sync.Mutex
	seenChunks map[string]bool
}

// NewKnowledgeSearchTool creates a new knowledge search tool
func NewKnowledgeSearchTool(
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	searchTargets types.SearchTargets,
	rerankModel rerank.Reranker,
	chatModel chat.Chat,
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
		chatModel:            chatModel,
		agentConfig:          agentConfig,
		config:               cfg,
		seenChunks:           make(map[string]bool),
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

	allResults, searchErr := t.concurrentSearchByTargets(ctx, queries, searchTargets,
		topK, vectorThreshold, keywordThreshold, kbTypeMap)
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Concurrent search completed: %d raw results", len(allResults))
	if searchErr != nil && len(allResults) == 0 {
		return &types.ToolResult{Success: false, Error: "Knowledge retrieval failed: " + searchErr.Error()}, searchErr
	}

	// Note: HybridSearch now uses RRF (Reciprocal Rank Fusion) which produces normalized scores
	// RRF scores are in range [0, ~0.033] (max when rank=1 on both sides: 2/(60+1))
	// Threshold filtering is already done inside HybridSearch before RRF, so we skip it here

	// Deduplicate before reranking to reduce processing overhead
	deduplicatedBeforeRerank := t.deduplicateResults(allResults)

	// Apply ReRank if model is configured
	// Prefer rerankModel; fall back to chatModel (LLM-based reranking) if unavailable
	// Use first query for reranking (or combine all queries if needed)
	rerankQuery := ""
	if len(queries) > 0 {
		rerankQuery = queries[0]
		if len(queries) > 1 {
			// Combine multiple queries for reranking
			rerankQuery = strings.Join(queries, " ")
		}
	}

	// Variable to hold results through reranking and MMR stages
	var filteredResults []*searchResultWithMeta

	if (t.rerankModel != nil || t.chatModel != nil) && len(deduplicatedBeforeRerank) > 0 && rerankQuery != "" {
		logger.Infof(ctx, "[Tool][KnowledgeSearch] Applying rerank, input: %d results, threshold: %.2f, queries: %v",
			len(deduplicatedBeforeRerank), t.rerankThreshold(), queries)
		rerankedResults, err := t.rerankResults(ctx, rerankQuery, deduplicatedBeforeRerank)
		if err != nil {
			logger.Errorf(ctx, "[Tool][KnowledgeSearch] Rerank failed: %v", err)
			return &types.ToolResult{
				Success: false,
				Error:   fmt.Sprintf("rerank failed: %v", err),
			}, err
		} else {
			filteredResults = rerankedResults
			logger.Infof(ctx, "[Tool][KnowledgeSearch] Rerank completed successfully: %d results",
				len(filteredResults))
		}
	} else {
		// No reranking model available, use deduplicated results
		filteredResults = deduplicatedBeforeRerank
	}

	// One ranking policy: retrieval order without a reranker, model relevance
	// with a reranker. Identity dedup already ran before reranking.
	deduplicatedResults := filteredResults
	sort.SliceStable(deduplicatedResults, func(i, j int) bool {
		if deduplicatedResults[i].Score != deduplicatedResults[j].Score {
			return deduplicatedResults[i].Score > deduplicatedResults[j].Score
		}
		return searchResultIdentity(deduplicatedResults[i]) < searchResultIdentity(deduplicatedResults[j])
	})
	if rerankTopK > 0 && len(deduplicatedResults) > rerankTopK {
		deduplicatedResults = deduplicatedResults[:rerankTopK]
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
	if result.Data != nil {
		result.Data["retrieval_status"] = "complete"
		if searchErr != nil {
			result.Data["retrieval_status"] = "partial"
			result.Data["retrieval_error"] = searchErr.Error()
			result.Output = "Retrieval is incomplete: " + searchErr.Error() + ". Available evidence follows; unavailable sources cannot be treated as having no answer.\n\n" + result.Output
		}
	}
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
) ([]*searchResultWithMeta, error) {
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
		return nil, nil
	}
	searchTargets = filteredTargets

	// Resolve actual model identities (name + endpoint) for cross-tenant grouping
	modelKeyMap := t.knowledgeBaseService.ResolveEmbeddingModelKeys(ctx, kbList)

	groups := make(map[string][]*types.SearchTarget)
	for _, st := range searchTargets {
		key := modelKeyMap[st.KnowledgeBaseID]
		groups[key] = append(groups[key], st)
	}

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
						searchParams := types.SearchParams{
							QueryText:        q,
							QueryEmbedding:   queryEmbedding,
							KnowledgeBaseIDs: fullKBIDs,
							MatchCount:       topK,
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
						searchParams := types.SearchParams{
							QueryText:        q,
							QueryEmbedding:   queryEmbedding,
							MatchCount:       topK,
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
	return allResults, errors.Join(failures...)
}

// rerankResults applies reranking to all search results (including FAQ entries)
// using the rerank model or LLM fallback, then filters by threshold and applies
// one relevance scale.
func (t *KnowledgeSearchTool) rerankResults(
	ctx context.Context,
	query string,
	results []*searchResultWithMeta,
) ([]*searchResultWithMeta, error) {
	if len(results) == 0 {
		return results, nil
	}

	var (
		reranked []*searchResultWithMeta
		err      error
	)

	if t.rerankModel != nil {
		reranked, err = t.rerankWithModel(ctx, query, results)
	} else if t.chatModel != nil {
		reranked, err = t.rerankWithLLM(ctx, query, results)
	} else {
		return results, nil
	}

	if err != nil {
		return nil, err
	}

	logger.Debugf(ctx, "[Tool][KnowledgeSearch] Rerank produced %d results after threshold filter", len(reranked))
	return reranked, nil
}

func (t *KnowledgeSearchTool) getFAQMetadata(
	ctx context.Context,
	chunkID string,
	cache map[string]*types.FAQChunkMetadata,
) (*types.FAQChunkMetadata, error) {
	if chunkID == "" || t.chunkService == nil {
		return nil, nil
	}

	if meta, ok := cache[chunkID]; ok {
		return meta, nil
	}

	chunk, err := t.chunkService.GetChunkByID(ctx, chunkID)
	if err != nil {
		cache[chunkID] = nil
		return nil, err
	}
	if chunk == nil {
		cache[chunkID] = nil
		return nil, nil
	}

	meta, err := chunk.FAQMetadata()
	if err != nil {
		cache[chunkID] = nil
		return nil, err
	}
	cache[chunkID] = meta
	return meta, nil
}

// rerankWithLLM uses LLM prompt to score and rerank search results
// Uses batch processing to handle large result sets efficiently
func (t *KnowledgeSearchTool) rerankWithLLM(
	ctx context.Context,
	query string,
	results []*searchResultWithMeta,
) ([]*searchResultWithMeta, error) {
	logger.Infof(ctx, "[Tool][KnowledgeSearch] Using LLM for reranking %d results", len(results))

	if len(results) == 0 {
		return results, nil
	}

	// Batch size: process 15 results at a time to balance quality and token usage
	// This prevents token overflow and improves processing efficiency
	const batchSize = 15
	const maxContentLength = 800 // Maximum characters per passage to avoid excessive tokens

	// Process in batches
	allScores := make([]float64, len(results))

	for batchStart := 0; batchStart < len(results); batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > len(results) {
			batchEnd = len(results)
		}

		batch := results[batchStart:batchEnd]
		logger.Debugf(ctx, "[Tool][KnowledgeSearch] Processing rerank batch %d-%d of %d results",
			batchStart+1, batchEnd, len(results))

		// Build prompt with query and batch passages
		var passagesBuilder strings.Builder
		for i, result := range batch {
			// Get enriched passage (content + image info)
			enrichedContent := t.getEnrichedPassage(ctx, result.SearchResult)
			// Truncate content if too long to save tokens
			content := enrichedContent
			if len([]rune(content)) > maxContentLength {
				runes := []rune(content)
				content = string(runes[:maxContentLength]) + "..."
			}
			// Use clear separators to distinguish each passage
			if i > 0 {
				passagesBuilder.WriteString("\n")
			}
			passagesBuilder.WriteString("─────────────────────────────────────────────────────────────\n")
			passagesBuilder.WriteString(fmt.Sprintf("Passage %d:\n", i+1))
			passagesBuilder.WriteString("─────────────────────────────────────────────────────────────\n")
			passagesBuilder.WriteString(content + "\n")
		}

		// Optimized prompt focused on retrieval matching and reranking
		prompt := fmt.Sprintf(
			`You are a search result reranking expert. Your task is to evaluate how well each retrieved passage matches the user's search query and information need.

User Query: %s

Your task: Rerank these search results by evaluating their retrieval relevance - how well each passage answers or relates to the query.

Scoring Criteria (0.0 to 1.0):
- 1.0 (0.9-1.0): Directly answers the query, contains key information needed, highly relevant
- 0.8 (0.7-0.8): Strongly related, provides substantial relevant information
- 0.6 (0.5-0.6): Moderately related, contains some relevant information but may be incomplete
- 0.4 (0.3-0.4): Weakly related, minimal relevance to the query
- 0.2 (0.1-0.2): Barely related, mostly irrelevant
- 0.0 (0.0): Completely irrelevant, no relation to the query

Evaluation Factors:
1. Query-Answer Match: Does the passage directly address what the user is asking?
2. Information Completeness: Does it provide sufficient information to answer the query?
3. Semantic Relevance: Does the content semantically relate to the query intent?
4. Key Term Coverage: Does it cover important terms/concepts from the query?
5. Information Accuracy: Is the information accurate and trustworthy?

Retrieved Passages:
%s

IMPORTANT: Return exactly %d scores, one per line, in this exact format:
Passage 1: X.XX
Passage 2: X.XX
Passage 3: X.XX
...
Passage %d: X.XX

Output only the scores, no explanations or additional text.`,
			query,
			passagesBuilder.String(),
			len(batch),
			len(batch),
		)

		messages := []chat.Message{
			{
				Role:    "system",
				Content: "You are a professional search result reranking expert specializing in information retrieval. You evaluate how well retrieved passages match user queries in search scenarios. Focus on retrieval relevance: whether the passage answers the query, provides needed information, and matches the user's information need. Always respond with scores only, no explanations.",
			},
			{
				Role:    "user",
				Content: prompt,
			},
		}

		// Calculate appropriate max tokens based on batch size
		// Each score line is ~15 tokens, add buffer for safety
		maxTokens := len(batch)*20 + 100

		response, err := t.chatModel.Chat(ctx, messages, &chat.ChatOptions{
			Temperature: 0.1, // Low temperature for consistent scoring
			MaxTokens:   maxTokens,
		})
		if err != nil {
			return nil, fmt.Errorf("LLM rerank batch %d-%d failed: %w", batchStart+1, batchEnd, err)
		}

		logger.Infof(ctx, "[Tool][KnowledgeSearch] LLM rerank batch %d-%d response: %s",
			batchStart+1, batchEnd, response.Content)

		// Parse scores from response
		batchScores, err := t.parseScoresFromResponse(response.Content, len(batch))
		if err != nil {
			return nil, fmt.Errorf(
				"parse LLM rerank batch %d-%d: %w",
				batchStart+1,
				batchEnd,
				err,
			)
		}

		// Store scores for this batch
		for i, score := range batchScores {
			if batchStart+i < len(allScores) {
				allScores[batchStart+i] = score
			}
		}
	}

	// Create rerank rank results and apply the same threshold + composite path as the model.
	rankResults := make([]rerank.RankResult, 0, len(results))
	for i, score := range allScores {
		if i >= len(results) {
			break
		}
		rankResults = append(rankResults, rerank.RankResult{
			Index:          i,
			RelevanceScore: score,
		})
	}
	sort.Slice(rankResults, func(i, j int) bool {
		return rankResults[i].RelevanceScore > rankResults[j].RelevanceScore
	})

	ranked := t.applyModelRerankScores(results, rankResults, t.rerankThreshold())
	logger.Infof(ctx, "[Tool][KnowledgeSearch] LLM reranked %d/%d results above threshold %.2f",
		len(ranked), len(results), t.rerankThreshold())
	return ranked, nil
}

// parseScoresFromResponse parses scores from LLM response text
func (t *KnowledgeSearchTool) parseScoresFromResponse(responseText string, expectedCount int) ([]float64, error) {
	lines := strings.Split(strings.TrimSpace(responseText), "\n")
	scores := make([]float64, 0, expectedCount)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Try to extract score from various formats:
		// "Passage 1: 0.85"
		// "1: 0.85"
		// "0.85"
		// etc.
		parts := strings.Split(line, ":")
		var scoreStr string
		if len(parts) >= 2 {
			scoreStr = strings.TrimSpace(parts[len(parts)-1])
		} else {
			scoreStr = strings.TrimSpace(line)
		}

		// Remove any non-numeric characters except decimal point
		scoreStr = strings.TrimFunc(scoreStr, func(r rune) bool {
			return (r < '0' || r > '9') && r != '.'
		})

		if scoreStr == "" {
			continue
		}

		score, err := strconv.ParseFloat(scoreStr, 64)
		if err != nil {
			continue // Skip invalid scores
		}

		// Clamp score to [0.0, 1.0]
		if score < 0.0 {
			score = 0.0
		}
		if score > 1.0 {
			score = 1.0
		}

		scores = append(scores, score)
	}

	if len(scores) == 0 {
		return nil, fmt.Errorf("no valid scores found in response")
	}

	if len(scores) != expectedCount {
		return nil, fmt.Errorf("expected %d scores, got %d", expectedCount, len(scores))
	}

	return scores, nil
}

// rerankWithModel uses the rerank model for reranking.
func (t *KnowledgeSearchTool) rerankWithModel(
	ctx context.Context,
	query string,
	results []*searchResultWithMeta,
) ([]*searchResultWithMeta, error) {
	passages := make([]string, len(results))
	for i, result := range results {
		passages[i] = t.getEnrichedPassage(ctx, result.SearchResult)
	}

	rerankResp, err := t.rerankModel.Rerank(ctx, query, passages)
	if err != nil {
		return nil, fmt.Errorf("rerank call failed: %w", err)
	}

	ranked := t.applyModelRerankScores(results, rerankResp, t.rerankThreshold())
	logger.Infof(
		ctx,
		"[Tool][KnowledgeSearch] Reranked %d/%d results above threshold %.2f",
		len(ranked),
		len(results),
		t.rerankThreshold(),
	)
	return ranked, nil
}

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

// filterKnowledgeSearchScores applies the configured relevance cutoff exactly.
func filterKnowledgeSearchScores(
	rankResults []rerank.RankResult,
	candidateCount int,
	threshold float64,
) []rerank.RankResult {
	if candidateCount <= 0 || len(rankResults) == 0 {
		return nil
	}
	ordered := append([]rerank.RankResult(nil), rankResults...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].RelevanceScore > ordered[j].RelevanceScore
	})

	retained := make([]rerank.RankResult, 0, len(ordered))
	seen := make(map[int]struct{}, candidateCount)
	for _, result := range ordered {
		if result.Index < 0 || result.Index >= candidateCount {
			continue
		}
		if _, exists := seen[result.Index]; exists {
			continue
		}
		seen[result.Index] = struct{}{}
		if result.RelevanceScore < threshold {
			continue
		}
		retained = append(retained, result)
	}
	return retained
}

func (t *KnowledgeSearchTool) applyModelRerankScores(
	originals []*searchResultWithMeta,
	rankResults []rerank.RankResult,
	threshold float64,
) []*searchResultWithMeta {
	filtered := filterKnowledgeSearchScores(rankResults, len(originals), threshold)
	out := make([]*searchResultWithMeta, 0, len(filtered))
	for _, rr := range filtered {
		if rr.Index < 0 || rr.Index >= len(originals) {
			continue
		}
		newResult := *originals[rr.Index]
		copyOfSource := *newResult.SearchResult
		newResult.SearchResult = &copyOfSource
		newResult.Score = rr.RelevanceScore
		if t.faqPriorityEnabled() && newResult.KnowledgeBaseType == types.KnowledgeBaseTypeFAQ {
			newResult.Score *= t.faqScoreBoost()
		}
		out = append(out, &newResult)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}

// deduplicateResults combines only identical evidence identities. Distinct
// children and documents retain their own provenance, regardless of content.
func (t *KnowledgeSearchTool) deduplicateResults(results []*searchResultWithMeta) []*searchResultWithMeta {
	byID := make(map[string]*searchResultWithMeta)
	for _, r := range results {
		if r == nil || r.SearchResult == nil {
			continue
		}
		key := searchResultIdentity(r)
		if old, ok := byID[key]; !ok || r.Score > old.Score || (r.Score == old.Score && r.SourceQuery < old.SourceQuery) {
			byID[key] = r
		}
	}
	out := make([]*searchResultWithMeta, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return searchResultIdentity(out[i]) < searchResultIdentity(out[j])
	})
	return out
}
func searchResultIdentity(r *searchResultWithMeta) string {
	if r.ID != "" {
		return r.KnowledgeBaseID + ":" + r.KnowledgeID + ":" + r.ID
	}
	return fmt.Sprintf("%s:%s:%d:%d:%d:%s", r.KnowledgeBaseID, r.KnowledgeID, r.ChunkIndex, r.StartAt, r.EndAt, r.Content)
}

// formatOutput formats the search results for display
func (t *KnowledgeSearchTool) formatOutput(
	ctx context.Context,
	results []*searchResultWithMeta,
	kbsToSearch []string,
	queries []string,
	kbNameMap map[string]string,
) (*types.ToolResult, error) {
	if len(results) == 0 {
		data := map[string]interface{}{
			"knowledge_base_ids": kbsToSearch,
			"results":            []interface{}{},
			"count":              0,
		}
		if len(queries) > 0 {
			data["queries"] = queries
		}
		output := fmt.Sprintf("No relevant content found in %d knowledge base(s).\n\n", len(kbsToSearch))
		output += "=== ⚠️ CRITICAL - Next Steps ===\n"
		output += "- ❌ DO NOT use training data or general knowledge to answer\n"
		output += "- ✅ If web_search is enabled: You MUST use web_search to find information\n"
		output += "- ✅ If web_search is disabled: State 'I couldn't find relevant information in the knowledge base'\n"
		output += "- NEVER fabricate or infer answers - ONLY use retrieved content\n"

		return &types.ToolResult{
			Success: true,
			Output:  output,
			Data:    data,
		}, nil
	}

	exactInputs := make([]*types.SearchResult, 0, len(results))
	for _, result := range results {
		if result != nil && result.SearchResult != nil {
			exactInputs = append(exactInputs, result.SearchResult)
		}
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	exactReferences, err := sourcerefs.ResolveQuickAnswerEvidence(
		ctx, t.chunkService.GetRepository(), tenantID, exactInputs,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve exact knowledge-search evidence: %w", err)
	}

	// Count results by KB
	kbCounts := make(map[string]int)
	for _, r := range results {
		kbCounts[r.KnowledgeBaseID]++
	}

	// Format individual results as XML. Tag names are kept in sync with
	// wiki_search (`<search_results>`, per-entry element, `<query>`) so that
	// agents and downstream consumers see a single consistent shape across
	// all retrieval tools.
	var ob strings.Builder
	ob.WriteString(fmt.Sprintf("<search_results count=\"%d\">\n", len(results)))
	ob.WriteString(
		"<evidence_sufficiency_instruction>" + retrievalEvidenceSufficiencyInstruction + " " +
			"chunk_index is a logical chunk ordinal, never a page, sheet row, source line, JSON item, image frame, or audio time. " +
			"Never convert chunk_index into offset; knowledge_id paging uses the returned next_offset. " +
			"For source citations use source_locator and record keys in the content only.</evidence_sufficiency_instruction>\n",
	)
	for _, q := range queries {
		ob.WriteString(fmt.Sprintf("<query>%s</query>\n", xmlEscape(q)))
	}

	formattedResults := make([]map[string]interface{}, 0, len(results))

	faqMetadataCache := make(map[string]*types.FAQChunkMetadata)

	knowledgeChunkMap := make(map[string]map[int]bool)
	knowledgeTotalMap := make(map[string]int64)
	knowledgeTitleMap := make(map[string]string)

	for i, result := range results {
		knowledgeBaseName := kbNameMap[result.KnowledgeBaseID]
		var faqMeta *types.FAQChunkMetadata
		if result.KnowledgeBaseType == types.KnowledgeBaseTypeFAQ {
			meta, err := t.getFAQMetadata(ctx, result.ID, faqMetadataCache)
			if err != nil {
				logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to load FAQ metadata for chunk %s: %v", result.ID, err)
			} else {
				faqMeta = meta
			}
		}

		if knowledgeChunkMap[result.KnowledgeID] == nil {
			knowledgeChunkMap[result.KnowledgeID] = make(map[int]bool)
		}
		knowledgeChunkMap[result.KnowledgeID][result.ChunkIndex] = true
		knowledgeTitleMap[result.KnowledgeID] = result.KnowledgeTitle

		// Cache total chunk count per knowledge
		if _, exists := knowledgeTotalMap[result.KnowledgeID]; !exists {
			effectiveTenantID := t.searchTargets.GetTenantIDForKB(result.KnowledgeBaseID)
			if effectiveTenantID == 0 {
				logger.Warnf(ctx, "[Tool][KnowledgeSearch] KB %s not found in searchTargets, skipping chunk count", result.KnowledgeBaseID)
				knowledgeTotalMap[result.KnowledgeID] = 0
			} else {
				// Use the same chunk-type filter as list_knowledge_chunks so the
				// total reported here matches what list_knowledge_chunks can page
				// over. Mismatched filters previously let LLMs compute offsets
				// against an inflated/deflated total and page past the end.
				_, total, err := t.chunkService.GetRepository().ListPagedChunksByKnowledgeID(ctx,
					effectiveTenantID, result.KnowledgeID,
					&types.Pagination{Page: 1, PageSize: 1},
					[]types.ChunkType{types.ChunkTypeText, types.ChunkTypeFAQ}, "", "", "", "", "",
				)
				if err != nil {
					logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to get total chunks for knowledge %s: %v", result.KnowledgeID, err)
					knowledgeTotalMap[result.KnowledgeID] = 0
				} else {
					knowledgeTotalMap[result.KnowledgeID] = total
				}
			}
		}

		t.seenMu.Lock()
		seen := t.seenChunks[result.ID]
		t.seenChunks[result.ID] = true
		t.seenMu.Unlock()

		isFAQ := faqMeta != nil
		directFAQRecommended := isFAQ && t.faqDirectAnswerRecommended(result.Score)
		if seen {
			// Compact rendering for chunks we already returned in a previous
			// knowledge_search call during this session. The model has the
			// content in context already, so re-emitting it only burns tokens.
			if isFAQ {
				ob.WriteString(fmt.Sprintf(
					"<faq rank=\"%d\" faq_id=\"%s\" index=\"%d\" knowledge_base_id=\"%s\" knowledge_base_name=\"%s\" knowledge_title=\"%s\" score=\"%.3f\" source_query=\"%s\" already_seen=\"true\">\n",
					i+1,
					xmlEscape(result.ID),
					result.ChunkIndex,
					xmlEscape(result.KnowledgeBaseID),
					xmlEscape(knowledgeBaseName),
					xmlEscape(result.KnowledgeTitle),
					result.Score,
					xmlEscape(result.SourceQuery),
				))
			} else {
				ob.WriteString(fmt.Sprintf(
					"<chunk rank=\"%d\" chunk_id=\"%s\" chunk_index=\"%d\" knowledge_id=\"%s\" knowledge_base_id=\"%s\" knowledge_base_name=\"%s\" knowledge_title=\"%s\" score=\"%.3f\" source_query=\"%s\" already_seen=\"true\">\n",
					i+1,
					xmlEscape(result.ID),
					result.ChunkIndex,
					xmlEscape(result.KnowledgeID),
					xmlEscape(result.KnowledgeBaseID),
					xmlEscape(knowledgeBaseName),
					xmlEscape(result.KnowledgeTitle),
					result.Score,
					xmlEscape(result.SourceQuery),
				))
			}
			writeModelSourceLocator(&ob, result.SourceLocator)
			ob.WriteString("<note>(content omitted, already returned in a previous knowledge_search call this session)</note>\n")
			if directFAQRecommended {
				ob.WriteString(fmt.Sprintf(
					"<direct_answer_recommended threshold=\"%.3f\">true</direct_answer_recommended>\n",
					t.faqDirectAnswerThreshold(),
				))
			}
			if isFAQ {
				ob.WriteString("</faq>\n")
			} else {
				ob.WriteString("</chunk>\n")
			}
		} else {
			if isFAQ {
				ob.WriteString(fmt.Sprintf(
					"<faq rank=\"%d\" faq_id=\"%s\" index=\"%d\" knowledge_base_id=\"%s\" knowledge_base_name=\"%s\" knowledge_title=\"%s\" score=\"%.3f\" source_query=\"%s\">\n",
					i+1,
					xmlEscape(result.ID),
					result.ChunkIndex,
					xmlEscape(result.KnowledgeBaseID),
					xmlEscape(knowledgeBaseName),
					xmlEscape(result.KnowledgeTitle),
					result.Score,
					xmlEscape(result.SourceQuery),
				))
			} else {
				ob.WriteString(fmt.Sprintf(
					"<chunk rank=\"%d\" chunk_id=\"%s\" chunk_index=\"%d\" knowledge_id=\"%s\" knowledge_base_id=\"%s\" knowledge_base_name=\"%s\" knowledge_title=\"%s\" score=\"%.3f\" source_query=\"%s\">\n",
					i+1,
					xmlEscape(result.ID),
					result.ChunkIndex,
					xmlEscape(result.KnowledgeID),
					xmlEscape(result.KnowledgeBaseID),
					xmlEscape(knowledgeBaseName),
					xmlEscape(result.KnowledgeTitle),
					result.Score,
					xmlEscape(result.SourceQuery),
				))
			}
			writeModelSourceLocator(&ob, result.SourceLocator)
			if directFAQRecommended {
				ob.WriteString(fmt.Sprintf(
					"<direct_answer_recommended threshold=\"%.3f\">true</direct_answer_recommended>\n",
					t.faqDirectAnswerThreshold(),
				))
			}
			snippet := ""
			if faqMeta != nil {
				snippet = faqMatchSnippetFromQueries(faqMeta, queries)
			}
			if snippet == "" {
				snippet = extractSnippetForQueries(result.Content, queries)
			}
			if snippet != "" {
				ob.WriteString(fmt.Sprintf("<match_snippet>%s</match_snippet>\n", xmlEscape(snippet)))
			}
			exactModelContent := renderKnowledgeSearchExactEvidence(result, exactReferences)
			if strings.TrimSpace(exactModelContent) == "" && strings.TrimSpace(result.Content) != "" {
				return nil, fmt.Errorf("render exact knowledge-search evidence for result %s", result.ID)
			}
			ob.WriteString(fmt.Sprintf("<content>%s</content>\n", exactModelContent))

			if result.ImageInfo != "" {
				var imageInfos []types.ImageInfo
				if err := json.Unmarshal([]byte(result.ImageInfo), &imageInfos); err == nil && len(imageInfos) > 0 {
					for _, img := range imageInfos {
						ob.WriteString(fmt.Sprintf("<image url=\"%s\">\n", xmlEscape(img.URL)))
						if img.Caption != "" {
							ob.WriteString(fmt.Sprintf("<image_caption>%s</image_caption>\n", xmlEscape(img.Caption)))
						}
						if img.OCRText != "" {
							ob.WriteString(fmt.Sprintf("<image_ocr>%s</image_ocr>\n", xmlEscape(img.OCRText)))
						}
						ob.WriteString("</image>\n")
					}
				}
			}

			if isFAQ {
				writeFAQFieldsXML(&ob, faqMeta)
				ob.WriteString("</faq>\n")
			} else {
				ob.WriteString("</chunk>\n")
			}
		}

		formattedResults = append(formattedResults, map[string]interface{}{
			"result_index":        i + 1,
			"content":             result.Content,
			"knowledge_id":        result.KnowledgeID,
			"knowledge_title":     result.KnowledgeTitle,
			"knowledge_base_id":   result.KnowledgeBaseID,
			"knowledge_base_name": knowledgeBaseName,
			"match_type":          result.MatchType,
			"source_query":        result.SourceQuery,
			"query_type":          result.QueryType,
			"knowledge_base_type": result.KnowledgeBaseType,
			"score":               result.Score,
		})

		last := formattedResults[len(formattedResults)-1]
		if len(result.SourceLocator) > 0 && json.Valid(result.SourceLocator) {
			last["source_locator"] = json.RawMessage(append([]byte(nil), result.SourceLocator...))
		}

		if result.ImageInfo != "" {
			var imageInfos []types.ImageInfo
			if err := json.Unmarshal([]byte(result.ImageInfo), &imageInfos); err == nil && len(imageInfos) > 0 {
				imageList := make([]map[string]string, 0, len(imageInfos))
				for _, img := range imageInfos {
					imgData := make(map[string]string)
					if img.URL != "" {
						imgData["url"] = img.URL
					}
					if img.Caption != "" {
						imgData["caption"] = img.Caption
					}
					if img.OCRText != "" {
						imgData["ocr_text"] = img.OCRText
					}
					if len(imgData) > 0 {
						imageList = append(imageList, imgData)
					}
				}
				if len(imageList) > 0 {
					last["images"] = imageList
				}
			}
		}

		if faqMeta != nil {
			last["faq_id"] = result.ID
			last["index"] = result.ChunkIndex
			last["faq_direct_answer_recommended"] = directFAQRecommended
			last["faq_direct_answer_threshold"] = t.faqDirectAnswerThreshold()
			if faqMeta.StandardQuestion != "" {
				last["faq_standard_question"] = faqMeta.StandardQuestion
			}
			appendSimilarQuestionsToChunkData(last, faqMeta.SimilarQuestions)
			if len(faqMeta.Answers) > 0 {
				last["faq_answers"] = faqMeta.Answers
			}
		} else {
			last["chunk_id"] = result.ID
			last["chunk_index"] = result.ChunkIndex
		}
	}

	// Retrieval statistics
	ob.WriteString("<retrieval_statistics>\n")
	for knowledgeID, retrievedChunks := range knowledgeChunkMap {
		totalChunks := knowledgeTotalMap[knowledgeID]
		retrievedCount := len(retrievedChunks)
		title := knowledgeTitleMap[knowledgeID]
		if totalChunks > 0 {
			remaining := totalChunks - int64(retrievedCount)
			percentage := float64(retrievedCount) / float64(totalChunks) * 100
			ob.WriteString(fmt.Sprintf("<document_stat knowledge_id=\"%s\" title=\"%s\" total_chunks=\"%d\" retrieved=\"%d\" remaining=\"%d\" coverage=\"%.1f%%\" />\n",
				xmlEscape(knowledgeID), xmlEscape(title), totalChunks, retrievedCount, remaining, percentage))
		}
	}
	ob.WriteString("</retrieval_statistics>\n")
	ob.WriteString("</search_results>")

	output := ob.String()

	data := map[string]interface{}{
		"knowledge_base_ids": kbsToSearch,
		"results":            formattedResults,
		"count":              len(formattedResults),
		"kb_counts":          kbCounts,
		"display_type":       "search_results",
	}

	if len(queries) > 0 {
		data["queries"] = queries
	}

	return &types.ToolResult{
		Success:          true,
		Output:           output,
		Data:             data,
		SourceReferences: sourcerefs.CitableReferences(exactReferences),
	}, nil
}

func renderKnowledgeSearchExactEvidence(
	result *searchResultWithMeta,
	exactReferences []*types.SearchResult,
) string {
	if result == nil || result.SearchResult == nil {
		return ""
	}
	allowedIDs := make(map[string]struct{}, len(result.SubChunkID)+1)
	allowedIDs[result.ID] = struct{}{}
	for _, id := range result.SubChunkID {
		allowedIDs[id] = struct{}{}
	}
	var builder strings.Builder
	for _, ref := range exactReferences {
		if ref == nil || ref.KnowledgeID != result.KnowledgeID {
			continue
		}
		matches := false
		switch {
		case result.ParentChunkID != "" && result.ChunkType == string(types.ChunkTypeText) && len(result.SubChunkID) > 0:
			matches = ref.ParentChunkID == result.ParentChunkID
		case result.ParentChunkID != "" && result.ChunkType == string(types.ChunkTypeSummary):
			// Summary chunks are retrieval aids. Exact evidence resolution maps
			// them to the claim-bearing parent text, whose ID is ParentChunkID.
			matches = ref.ID == result.ParentChunkID
		case result.ParentChunkID != "" &&
			(result.ChunkType == string(types.ChunkTypeImageOCR) || result.ChunkType == string(types.ChunkTypeImageCaption)):
			matches = ref.ID == result.ParentChunkID
		default:
			_, matches = allowedIDs[ref.ID]
		}
		if !matches || strings.TrimSpace(ref.EvidenceContent) == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		if sourcerefs.IsSupportedCitationReference(ref) {
			fmt.Fprintf(&builder, "[EXACT_FRAGMENT chunk_id=\"%s\"]\n", ref.ID)
			builder.WriteString(strings.TrimSpace(ref.EvidenceContent))
			builder.WriteString("\n[/EXACT_FRAGMENT]")
		} else {
			builder.WriteString("[ANALYSIS_CONTEXT]\n")
			builder.WriteString(strings.TrimSpace(ref.EvidenceContent))
			builder.WriteString("\n[/ANALYSIS_CONTEXT]")
		}
	}
	return builder.String()
}

func writeModelSourceLocator(builder *strings.Builder, locator types.JSON) {
	if builder == nil {
		return
	}
	if value := sourcerefs.ModelSourceLocator(locator); value != "" {
		fmt.Fprintf(builder, "<source_locator>%s</source_locator>\n", xmlEscape(value))
	}
}

// chunkRange represents a continuous range of chunk indices
type chunkRange struct {
	start int
	end   int
}

// getEnrichedPassage builds the same evidence-rich rerank passage shape used by
// quick-answer search. SearchResult metadata is already returned by the hybrid
// search, so this improves semantic ranking without another database, model, or
// retrieval call. In particular, a matched FAQ wording or a generated document
// question must not be discarded before reranking.
func (t *KnowledgeSearchTool) getEnrichedPassage(ctx context.Context, result *types.SearchResult) string {
	if result == nil {
		return ""
	}
	combinedText := result.Content
	enrichments := make([]string, 0)
	appendEnrichment := func(label, value string) {
		value = strings.TrimSpace(value)
		if value == "" || strings.Contains(combinedText, value) {
			return
		}
		for _, existing := range enrichments {
			if strings.Contains(existing, value) {
				return
			}
		}
		enrichments = append(enrichments, label+value)
	}

	appendEnrichment("Matched text: ", result.MatchedContent)

	// 解析ImageInfo
	if result.ImageInfo != "" {
		var imageInfos []types.ImageInfo
		if err := json.Unmarshal([]byte(result.ImageInfo), &imageInfos); err != nil {
			logger.Warnf(ctx, "[Tool][KnowledgeSearch] Failed to parse image info: %v", err)
		} else {
			for _, img := range imageInfos {
				appendEnrichment("Image Caption: ", img.Caption)
				appendEnrichment("Image Text: ", img.OCRText)
			}
		}
	}

	if len(result.ChunkMetadata) > 0 {
		var documentMeta types.DocumentChunkMetadata
		if err := json.Unmarshal(result.ChunkMetadata, &documentMeta); err == nil {
			for _, question := range documentMeta.GetQuestionStrings() {
				appendEnrichment("Related question: ", question)
			}
		}
		var faqMeta types.FAQChunkMetadata
		if err := json.Unmarshal(result.ChunkMetadata, &faqMeta); err == nil {
			appendEnrichment("FAQ question: ", faqMeta.StandardQuestion)
			for _, question := range faqMeta.SimilarQuestions {
				appendEnrichment("Equivalent question: ", question)
			}
			for _, answer := range faqMeta.Answers {
				appendEnrichment("FAQ answer: ", answer)
			}
		}
	}

	if len(enrichments) > 0 {
		if combinedText != "" {
			combinedText += "\n\n"
		}
		combinedText += strings.Join(enrichments, "\n")
	}

	logger.Debugf(ctx, "[Tool][KnowledgeSearch] Enriched passage: content_len=%d, enrichments=%d",
		len(result.Content), len(enrichments))

	return combinedText
}

func (t *KnowledgeSearchTool) faqPriorityEnabled() bool {
	return t.agentConfig != nil && t.agentConfig.FAQPriorityEnabled
}

func (t *KnowledgeSearchTool) faqScoreBoost() float64 {
	if t.agentConfig != nil && t.agentConfig.FAQScoreBoost > 0 {
		return t.agentConfig.FAQScoreBoost
	}
	return 1.0
}

func (t *KnowledgeSearchTool) faqDirectAnswerThreshold() float64 {
	if t.agentConfig != nil && t.agentConfig.FAQDirectAnswerThreshold > 0 {
		return t.agentConfig.FAQDirectAnswerThreshold
	}
	return 0.9
}

func (t *KnowledgeSearchTool) faqDirectAnswerRecommended(score float64) bool {
	return t.faqPriorityEnabled() && score >= t.faqDirectAnswerThreshold()
}

// extractSnippetForQueries tries to produce a short contextual snippet around
// the first occurrence of any token extracted from the provided queries.
// When no token matches (common for fully paraphrased semantic queries) it
// falls back to the leading 160 runes of content so callers always get
// something to scan. The snippet is single-lined and bounded in length to
// keep the rendered XML compact.
func extractSnippetForQueries(content string, queries []string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}

	tokens := searchQueryTokens(queries)

	lowered := strings.ToLower(content)
	earliest := -1
	earliestEnd := -1
	for _, tok := range tokens {
		idx := strings.Index(lowered, tok)
		if idx < 0 {
			continue
		}
		end := idx + len(tok)
		if earliest < 0 || idx < earliest {
			earliest = idx
			earliestEnd = end
		}
	}

	if earliest < 0 {
		runes := []rune(content)
		if len(runes) > snippetContextRunes*2 {
			return strings.TrimSpace(string(runes[:snippetContextRunes*2])) + " ..."
		}
		return content
	}

	matchStr := content[earliest:earliestEnd]
	before := content[:earliest]
	after := content[earliestEnd:]

	beforeRunes := []rune(before)
	if len(beforeRunes) > snippetContextRunes {
		beforeRunes = beforeRunes[len(beforeRunes)-snippetContextRunes:]
	}
	afterRunes := []rune(after)
	if len(afterRunes) > snippetContextRunes {
		afterRunes = afterRunes[:snippetContextRunes]
	}

	snippet := string(beforeRunes) + matchStr + string(afterRunes)
	snippet = strings.ReplaceAll(snippet, "\n", " ")
	for strings.Contains(snippet, "  ") {
		snippet = strings.ReplaceAll(snippet, "  ", " ")
	}
	return "... " + strings.TrimSpace(snippet) + " ..."
}
