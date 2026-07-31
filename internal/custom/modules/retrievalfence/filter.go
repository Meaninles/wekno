// Package retrievalfence applies the final authorization and generation fence
// after any vector-store implementation returns candidates. This protects
// every backend, including stores whose metadata schema cannot express all
// lifecycle predicates in the native query.
package retrievalfence

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

type Scope struct {
	TenantID        uint64
	KnowledgeBaseID string
}

type ChunkLoader func(context.Context, []string) ([]*types.Chunk, error)
type KnowledgeLoader func(context.Context, uint64, []string) ([]*types.Knowledge, error)

func Filter(
	ctx context.Context,
	results []*types.RetrieveResult,
	scopes []Scope,
	loadChunks ChunkLoader,
	loadKnowledge KnowledgeLoader,
) ([]*types.RetrieveResult, error) {
	allowedKBs := make(map[string]uint64, len(scopes))
	for _, scope := range scopes {
		if scope.TenantID > 0 && scope.KnowledgeBaseID != "" {
			allowedKBs[scope.KnowledgeBaseID] = scope.TenantID
		}
	}
	if len(allowedKBs) == 0 {
		return nil, fmt.Errorf("retrieval fence: no authorized knowledge base scope")
	}

	chunkIDs := make([]string, 0)
	seenChunk := make(map[string]struct{})
	for _, result := range results {
		if result == nil {
			continue
		}
		for _, item := range result.Results {
			if item != nil && item.ChunkID != "" {
				if _, exists := seenChunk[item.ChunkID]; !exists {
					seenChunk[item.ChunkID] = struct{}{}
					chunkIDs = append(chunkIDs, item.ChunkID)
				}
			}
		}
	}
	if len(chunkIDs) == 0 {
		return results, nil
	}
	chunks, err := loadChunks(ctx, chunkIDs)
	if err != nil {
		return nil, fmt.Errorf("retrieval fence load chunks: %w", err)
	}
	chunkByID := make(map[string]*types.Chunk, len(chunks))
	knowledgeByTenant := make(map[uint64]map[string]struct{})
	for _, chunk := range chunks {
		if chunk == nil || chunk.ID == "" {
			continue
		}
		expectedTenant, authorized := allowedKBs[chunk.KnowledgeBaseID]
		if !authorized || expectedTenant != chunk.TenantID || !chunk.IsEnabled ||
			chunk.DeletedAt.Valid {
			continue
		}
		chunkByID[chunk.ID] = chunk
		if knowledgeByTenant[chunk.TenantID] == nil {
			knowledgeByTenant[chunk.TenantID] = make(map[string]struct{})
		}
		knowledgeByTenant[chunk.TenantID][chunk.KnowledgeID] = struct{}{}
	}

	knowledgeByID := make(map[string]*types.Knowledge)
	for tenantID, idsSet := range knowledgeByTenant {
		ids := make([]string, 0, len(idsSet))
		for id := range idsSet {
			ids = append(ids, id)
		}
		rows, loadErr := loadKnowledge(ctx, tenantID, ids)
		if loadErr != nil {
			return nil, fmt.Errorf("retrieval fence load knowledge tenant %d: %w", tenantID, loadErr)
		}
		for _, knowledge := range rows {
			if knowledge != nil {
				knowledgeByID[knowledge.ID] = knowledge
			}
		}
	}

	filtered := make([]*types.RetrieveResult, 0, len(results))
	for _, result := range results {
		if result == nil {
			continue
		}
		copyResult := *result
		copyResult.Results = make([]*types.IndexWithScore, 0, len(result.Results))
		for _, item := range result.Results {
			if item == nil {
				continue
			}
			chunk := chunkByID[item.ChunkID]
			knowledge := knowledgeByID[item.KnowledgeID]
			if chunk == nil || knowledge == nil {
				continue
			}
			expectedTenant, authorized := allowedKBs[item.KnowledgeBaseID]
			if !authorized ||
				chunk.TenantID != expectedTenant ||
				chunk.KnowledgeBaseID != item.KnowledgeBaseID ||
				chunk.KnowledgeID != item.KnowledgeID ||
				knowledge.TenantID != expectedTenant ||
				knowledge.KnowledgeBaseID != item.KnowledgeBaseID ||
				knowledge.EnableStatus != "enabled" ||
				knowledge.DeletedAt.Valid ||
				knowledge.CoreStatus != types.CoreStatusReady {
				continue
			}
			if knowledge.Type != types.KnowledgeTypeFAQ &&
				(knowledge.ProcessingGeneration == "" ||
					chunk.ProcessingGeneration == "" ||
					chunk.ProcessingGeneration != knowledge.ProcessingGeneration) {
				continue
			}
			copyResult.Results = append(copyResult.Results, item)
		}
		filtered = append(filtered, &copyResult)
	}
	return filtered, nil
}
