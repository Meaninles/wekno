package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// upsertGenerationChunks makes deterministic generated-chunk IDs useful under
// handler retries. A duplicate insert is re-read and converted into an update;
// identity fields are verified before updating so an impossible UUID collision
// cannot overwrite another tenant/knowledge's chunk.
func upsertGenerationChunks(
	ctx context.Context,
	chunkService interfaces.ChunkService,
	chunks []*types.Chunk,
) error {
	if chunkService == nil {
		return errors.New("generation chunk upsert: chunk service is unavailable")
	}
	updates := make([]*types.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk == nil || chunk.ID == "" || chunk.TenantID == 0 || chunk.KnowledgeID == "" ||
			chunk.KnowledgeBaseID == "" {
			return errors.New("generation chunk upsert: complete chunk identity is required")
		}
		existing, getErr := chunkService.GetChunkByID(ctx, chunk.ID)
		if getErr == nil && existing != nil {
			if err := validateGeneratedChunkIdentity(existing, chunk); err != nil {
				return err
			}
			chunk.SeqID = existing.SeqID
			chunk.CreatedAt = existing.CreatedAt
			updates = append(updates, chunk)
			continue
		}
		if createErr := chunkService.CreateChunks(ctx, []*types.Chunk{chunk}); createErr != nil {
			// The read/insert window can race another retry. Re-read the stable
			// ID and update it when that competing insert won.
			existing, retryReadErr := chunkService.GetChunkByID(ctx, chunk.ID)
			if retryReadErr != nil || existing == nil {
				return errors.Join(getErr, createErr, retryReadErr)
			}
			if err := validateGeneratedChunkIdentity(existing, chunk); err != nil {
				return err
			}
			chunk.SeqID = existing.SeqID
			chunk.CreatedAt = existing.CreatedAt
			updates = append(updates, chunk)
		}
	}
	if len(updates) > 0 {
		if err := chunkService.UpdateChunks(ctx, updates); err != nil {
			return fmt.Errorf("update deterministic generation chunks: %w", err)
		}
	}
	return nil
}

func validateGeneratedChunkIdentity(existing, replacement *types.Chunk) error {
	if existing.TenantID != replacement.TenantID ||
		existing.KnowledgeID != replacement.KnowledgeID ||
		existing.KnowledgeBaseID != replacement.KnowledgeBaseID ||
		existing.ChunkType != replacement.ChunkType {
		return fmt.Errorf("generation chunk %s identity collision", replacement.ID)
	}
	return nil
}
