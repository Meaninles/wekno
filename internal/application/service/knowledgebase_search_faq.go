package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"slices"
)

// applyFAQPostProcessing handles FAQ-specific post-processing: iterative retrieval
// when not enough unique chunks are found, or negative question filtering otherwise.
// For non-FAQ knowledge bases, returns the input unchanged.
//
// The iterative retrieval path fans out across the supplied storeGroups so
// multi-store FAQ searches grow TopK uniformly across every bound vector
// store. A typed AppError raised inside that path (e.g.
// ErrVectorStoreUnavailable from a per-group timeout) is propagated to the
// caller so the user receives a faithful failure response rather than a
// silently truncated chunk list.
func (s *knowledgeBaseService) applyFAQPostProcessing(
	ctx context.Context,
	kbs []*types.KnowledgeBase,
	chunks []*types.IndexWithScore,
	vectorResults []*types.IndexWithScore,
	groups []*storeGroup,
	params types.SearchParams,
	matchCount int,
) ([]*types.IndexWithScore, error) {
	faqScope := faqTenantScope(kbs)
	if len(faqScope) == 0 {
		return chunks, nil
	}

	// Check if we need iterative retrieval for FAQ with separate indexing.
	// Only use iterative retrieval if we don't have enough unique chunks
	// after first deduplication.
	needsIterativeRetrieval := allKnowledgeBasesFAQ(kbs) &&
		len(chunks) < params.MatchCount && len(vectorResults) >= matchCount
	if needsIterativeRetrieval {
		logger.Info(ctx, "Not enough unique chunks, using iterative retrieval for FAQ")
		return s.iterativeRetrieveWithDeduplication(
			ctx,
			groups,
			kbs,
			params.MatchCount,
			params.QueryText,
		)
	}

	// Filter by negative questions if not using iterative retrieval.
	result, err := s.filterByNegativeQuestions(ctx, chunks, params.QueryText, kbs)
	if err != nil {
		return nil, err
	}
	logger.Infof(ctx, "Result count after negative question filtering: %d", len(result))
	return result, nil
}

// iterativeRetrieveWithDeduplication performs iterative retrieval until enough unique chunks are found.
// This is used for FAQ knowledge bases with separate indexing mode.
// Negative question filtering is applied after each iteration with chunk data caching.
//
// Each iteration only updates group.TopK; the underlying BaseParams stays
// immutable so the fan-out goroutines inside retrieveFromStores never
// observe a mid-mutation slice. Engines and grouping are computed once
// upstream and reused across iterations.
//
// Returns (results, error). A typed AppError raised inside
// retrieveFromStores (e.g. per-group timeout, or vector-store binding
// invalid) is propagated to the caller so the user sees an honest failure
// instead of a silently truncated result set. Non-AppError failures
// continue to break the loop with a warning and return whatever partial
// uniqueChunks have been accumulated — preserving the existing behavior
// for transient retrieve errors.
func (s *knowledgeBaseService) iterativeRetrieveWithDeduplication(ctx context.Context,
	groups []*storeGroup,
	kbs []*types.KnowledgeBase,
	matchCount int,
	queryText string,
) ([]*types.IndexWithScore, error) {
	maxIterations := 5
	// Start with a larger TopK since we're called when first retrieval wasn't enough
	// The first retrieval already used matchCount*3, so start from there
	currentTopK := matchCount * 3
	uniqueChunks := make(map[string]*types.IndexWithScore)
	// Cache chunk data to avoid repeated DB queries across iterations
	chunkDataCache := make(map[string]*types.Chunk)
	// Track chunks that have been filtered out by negative questions
	filteredOutChunks := make(map[string]struct{})

	queryTextLower := strings.ToLower(strings.TrimSpace(queryText))
	for i := 0; i < maxIterations; i++ {
		// Bump only the per-group TopK. BaseParams is immutable and read
		// concurrently inside retrieveFromStores; paramsWithTopK rebuilds
		// a fresh slice per call so no goroutine ever sees a half-mutated
		// value.
		for _, grp := range groups {
			grp.TopK = currentTopK
		}

		retrieveResults, err := s.retrieveFromStores(
			ctx, groups, retriever.EngineAwareNormalizer{})
		if err != nil {
			// Typed AppErrors must surface to HybridSearch so the user
			// sees the failure rather than a silently truncated chunk
			// list. Non-AppError failures (e.g. transient infra hiccups)
			// preserve the existing "warn and break" behavior so the
			// caller still gets partial results from the iterations that
			// succeeded.
			if _, ok := apperrors.IsAppError(err); ok {
				logger.WarnWithFields(ctx, logger.Fields{
					"iteration": i + 1,
				}, "Iterative retrieval surfaced typed failure")
				return nil, err
			}
			logger.Warnf(ctx, "Iterative retrieval failed at iteration %d: %v", i+1, err)
			break
		}

		// Collect results
		iterationResults := []*types.IndexWithScore{}
		for _, retrieveResult := range retrieveResults {
			iterationResults = append(iterationResults, retrieveResult.Results...)
		}

		if len(iterationResults) == 0 {
			logger.Infof(ctx, "No results found at iteration %d", i+1)
			break
		}

		totalRetrieved := len(iterationResults)

		// Collect new chunk IDs that need to be fetched from DB
		newResults := make([]*types.IndexWithScore, 0)
		for _, result := range iterationResults {
			if _, cached := chunkDataCache[result.ChunkID]; !cached {
				if _, filtered := filteredOutChunks[result.ChunkID]; !filtered {
					newResults = append(newResults, result)
				}
			}
		}

		// Batch fetch only new chunks
		if len(newResults) > 0 {
			newChunks, err := s.loadFAQChunksForResults(ctx, newResults, kbs)
			if err != nil {
				return nil, fmt.Errorf("load FAQ evidence at iteration %d: %w", i+1, err)
			}
			for id, chunk := range newChunks {
				chunkDataCache[id] = chunk
			}
			for _, result := range newResults {
				if _, ok := newChunks[result.ChunkID]; !ok {
					filteredOutChunks[result.ChunkID] = struct{}{}
			}
		}

		// Deduplicate, merge, and filter in one pass
		for _, result := range iterationResults {
			// Skip if already filtered out
			if _, filtered := filteredOutChunks[result.ChunkID]; filtered {
				continue
			}

			// Check negative questions using cached data
			chunkData, ok := chunkDataCache[result.ChunkID]
			if !ok || chunkData.ChunkType != types.ChunkTypeFAQ {
				filteredOutChunks[result.ChunkID] = struct{}{}
				delete(uniqueChunks, result.ChunkID)
				continue
			}
			meta, metaErr := chunkData.FAQMetadata()
			if metaErr != nil || meta == nil {
				filteredOutChunks[result.ChunkID] = struct{}{}
				delete(uniqueChunks, result.ChunkID)
				continue
			}
			if s.matchesNegativeQuestions(queryTextLower, meta.NegativeQuestions) {
				filteredOutChunks[result.ChunkID] = struct{}{}
				delete(uniqueChunks, result.ChunkID)
				continue
			}

			// Keep highest score for each chunk
			if existing, ok := uniqueChunks[result.ChunkID]; !ok || result.Score > existing.Score {
				uniqueChunks[result.ChunkID] = result
			}
		}

		logger.Infof(
			ctx,
			"After iteration %d: retrieved %d results, found %d valid unique chunks (target: %d)",
			i+1,
			totalRetrieved,
			len(uniqueChunks),
			matchCount,
		)

		// Early stop: Check if we have enough unique chunks after deduplication and filtering
		if len(uniqueChunks) >= matchCount {
			logger.Infof(ctx, "Found enough unique chunks after %d iterations", i+1)
			break
		}

		// Early stop: If we got fewer results than TopK, there are no more results to retrieve
		if totalRetrieved < currentTopK {
			logger.Infof(ctx, "No more results available (got %d < %d), stopping iteration", totalRetrieved, currentTopK)
			break
		}

		// Increase TopK for next iteration
		currentTopK *= 2
	}

	// Convert map to slice and sort by score
	result := make([]*types.IndexWithScore, 0, len(uniqueChunks))
	for _, chunk := range uniqueChunks {
		result = append(result, chunk)
	}

	slices.SortFunc(result, sortByScoreDesc)

	logger.Infof(ctx, "Iterative retrieval completed: %d unique chunks found after filtering", len(result))
	return result, nil
}

// filterByNegativeQuestions filters out chunks that match negative questions for FAQ knowledge bases.
func (s *knowledgeBaseService) filterByNegativeQuestions(ctx context.Context,
	chunks []*types.IndexWithScore,
	queryText string,
	kbs []*types.KnowledgeBase,
) ([]*types.IndexWithScore, error) {
	if len(chunks) == 0 {
		return chunks, nil
	}

	queryTextLower := strings.ToLower(strings.TrimSpace(queryText))
	if queryTextLower == "" {
		return chunks, nil
	}

	chunkMap, err := s.loadFAQChunksForResults(ctx, chunks, kbs)
	if err != nil {
		return nil, fmt.Errorf("load FAQ evidence for negative-question filtering: %w", err)
	}
	faqScope := faqTenantScope(kbs)

	// Filter out chunks that match negative questions
	filteredChunks := make([]*types.IndexWithScore, 0, len(chunks))
	for _, chunk := range chunks {
		if _, isFAQ := faqTenantForResult(chunk, faqScope); !isFAQ {
			filteredChunks = append(filteredChunks, chunk)
			continue
		}
		chunkData, ok := chunkMap[chunk.ChunkID]
		if !ok {
			logger.Warnf(ctx, "Dropping FAQ hit %s because its evidence row is unavailable", chunk.ChunkID)
			continue
		}

		// Only filter FAQ type chunks
		if chunkData.ChunkType != types.ChunkTypeFAQ {
			filteredChunks = append(filteredChunks, chunk)
			continue
		}

		// Get FAQ metadata and check negative questions
		meta, err := chunkData.FAQMetadata()
		if err != nil || meta == nil {
			logger.Warnf(ctx, "Dropping FAQ hit %s because metadata is invalid: %v", chunk.ChunkID, err)
			continue
		}

		// Check if query matches any negative question
		if s.matchesNegativeQuestions(queryTextLower, meta.NegativeQuestions) {
			logger.Debugf(ctx, "Filtered FAQ chunk %s due to negative question match", chunk.ChunkID)
			continue
		}

		// Keep the chunk
		filteredChunks = append(filteredChunks, chunk)
	}

	return filteredChunks, nil
}

func faqTenantScope(kbs []*types.KnowledgeBase) map[string]uint64 {
	scope := make(map[string]uint64)
	for _, kb := range kbs {
		if kb != nil && kb.Type == types.KnowledgeBaseTypeFAQ {
			scope[kb.ID] = kb.TenantID
		}
	}
	return scope
}

func allKnowledgeBasesFAQ(kbs []*types.KnowledgeBase) bool {
	if len(kbs) == 0 {
		return false
	}
	for _, kb := range kbs {
		if kb == nil || kb.Type != types.KnowledgeBaseTypeFAQ {
			return false
		}
	}
	return true
}

func faqTenantForResult(result *types.IndexWithScore, scope map[string]uint64) (uint64, bool) {
	if result == nil {
		return 0, false
	}
	if tenantID, ok := scope[result.KnowledgeBaseID]; ok {
		return tenantID, true
	}
	if result.KnowledgeBaseID == "" && len(scope) == 1 {
		for _, tenantID := range scope {
			return tenantID, true
		}
	}
	return 0, false
}

func (s *knowledgeBaseService) loadFAQChunksForResults(
	ctx context.Context,
	results []*types.IndexWithScore,
	kbs []*types.KnowledgeBase,
) (map[string]*types.Chunk, error) {
	scope := faqTenantScope(kbs)
	idsByTenant := make(map[uint64][]string)
	seen := make(map[string]bool)
	for _, result := range results {
		tenantID, ok := faqTenantForResult(result, scope)
		if !ok || result == nil || result.ChunkID == "" || seen[result.ChunkID] {
			continue
		}
		seen[result.ChunkID] = true
		idsByTenant[tenantID] = append(idsByTenant[tenantID], result.ChunkID)
	}
	tenants := make([]uint64, 0, len(idsByTenant))
	for tenantID := range idsByTenant {
		tenants = append(tenants, tenantID)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i] < tenants[j] })
	out := make(map[string]*types.Chunk, len(seen))
	for _, tenantID := range tenants {
		ids := idsByTenant[tenantID]
		sort.Strings(ids)
		rows, err := s.chunkRepo.ListChunksByID(ctx, tenantID, ids)
		if err != nil {
			return nil, err
		}
		for _, chunk := range rows {
			if chunk != nil {
				out[chunk.ID] = chunk
			}
		}
	}
	return out, nil
}

// matchesNegativeQuestions checks if the query text matches any negative questions.
// Returns true if the query matches any negative question, false otherwise.
func (s *knowledgeBaseService) matchesNegativeQuestions(queryTextLower string, negativeQuestions []string) bool {
	if len(negativeQuestions) == 0 {
		return false
	}

	for _, negativeQ := range negativeQuestions {
		negativeQLower := strings.ToLower(strings.TrimSpace(negativeQ))
		if negativeQLower == "" {
			continue
		}
		// Check if query text is exactly the same as the negative question
		if queryTextLower == negativeQLower {
			return true
		}
	}
	return false
}
