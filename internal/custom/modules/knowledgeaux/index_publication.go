package knowledgeaux

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const IndexRetirementTask = "knowledge:index_retirement"

func (r *Registry) DrainIndexRetirements(ctx context.Context, remove func(context.Context, uint64, IndexRetirement) error) error {
	return DrainIndexRetirements(ctx, r.db, remove)
}

// CleanupRetiredGeneration keeps uploaded originals and the currently visible
// generation. Every derived file is deleted through its existing owned binding.
func (r *Registry) CleanupRetiredGeneration(ctx context.Context, tenantID uint64, receipt IndexRetirement) error {
	var live types.Knowledge
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, receipt.KnowledgeID).First(&live).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && live.PublishedGeneration == receipt.Generation {
		return errors.New("cannot retire the visible generation")
	}
	objects, err := r.list(ctx, tenantID, receipt.KnowledgeBaseID, []string{receipt.KnowledgeID})
	if err != nil {
		return err
	}
	var targets []deleteTarget
	for _, registered := range objects {
		object := registered.object
		if object.ProcessingGeneration != receipt.Generation || isPersistentSourceKind(object.Kind) {
			continue
		}
		targets = append(targets, deleteTarget{path: object.Path, fallbackProvider: object.FallbackProvider, registered: true, binding: object.Binding, quarantined: object.Quarantined, deleteComplete: registered.deleteComplete})
	}
	return r.deleteTargets(ctx, tenantID, receipt.KnowledgeBaseID, receipt.KnowledgeID, targets, false, false)
}

// IndexRetirement is the durable receipt for resources hidden by an atomic
// generation switch. Cleanup addresses stable chunk IDs, never a document ID.
type IndexRetirement struct {
	KnowledgeID      string   `json:"knowledge_id"`
	KnowledgeBaseID  string   `json:"knowledge_base_id"`
	Generation       string   `json:"generation"`
	EmbeddingModelID string   `json:"embedding_model_id"`
	VectorStoreID    *string  `json:"vector_store_id"`
	KnowledgeType    string   `json:"knowledge_type"`
	ChunkIDs         []string `json:"chunk_ids"`
}

// PublishChunksTx runs inside the exact owner/generation core-commit transaction.
// Existing readers keep the previous enabled rows until this transaction commits.
func PublishChunksTx(tx *gorm.DB, before, next *types.Knowledge) error {
	if before == nil || next == nil || before.ID != next.ID || before.TenantID != next.TenantID || next.ProcessingGeneration == "" {
		return errors.New("index publication requires an exact document generation")
	}
	var old []*types.Chunk
	if err := tx.Select("id", "processing_generation").Where("tenant_id = ? AND knowledge_id = ? AND processing_generation <> ? AND is_enabled = ?", next.TenantID, next.ID, next.ProcessingGeneration, true).Order("id").Find(&old).Error; err != nil {
		return err
	}
	if before.PublishedGeneration != "" && before.PublishedGeneration != next.ProcessingGeneration {
		var kb types.KnowledgeBase
		if err := tx.Where("tenant_id = ? AND id = ?", next.TenantID, next.KnowledgeBaseID).Take(&kb).Error; err != nil {
			return err
		}
		// A generation can own page images or split files without text chunks.
		// Its cleanup receipt must not depend on a non-empty vector index.
		for start := 0; start < max(1, len(old)); start += 500 {
			ids := make([]string, 0, 500)
			for _, c := range old[start:min(start+500, len(old))] {
				ids = append(ids, c.ID)
			}
			payload, err := json.Marshal(IndexRetirement{KnowledgeID: next.ID, KnowledgeBaseID: next.KnowledgeBaseID, Generation: before.PublishedGeneration, EmbeddingModelID: before.PublishedEmbeddingModelID, VectorStoreID: kb.VectorStoreID, KnowledgeType: before.Type, ChunkIDs: ids})
			if err != nil {
				return err
			}
			if err := tx.Create(&types.TaskPendingOp{TenantID: next.TenantID, TaskType: IndexRetirementTask, Scope: "knowledge", ScopeID: next.ID, Op: "retire", Payload: payload, EnqueuedAt: time.Now()}).Error; err != nil {
				return err
			}
		}
	}
	if err := tx.Model(&types.Chunk{}).Where("tenant_id = ? AND knowledge_id = ? AND processing_generation <> ?", next.TenantID, next.ID, next.ProcessingGeneration).Update("is_enabled", false).Error; err != nil {
		return err
	}
	if err := tx.Model(&types.Chunk{}).Where("tenant_id = ? AND knowledge_id = ? AND processing_generation = ?", next.TenantID, next.ID, next.ProcessingGeneration).Update("is_enabled", true).Error; err != nil {
		return err
	}
	return tx.Model(&types.Knowledge{}).Where("tenant_id = ? AND id = ?", next.TenantID, next.ID).Updates(map[string]interface{}{
		"published_generation": next.ProcessingGeneration, "published_embedding_model_id": next.EmbeddingModelID,
	}).Error
}

// DraftChunkIDs excludes the currently published generation even if a stale
// worker calls cleanup after an ambiguous successful core commit.
func (r *Registry) DraftChunkIDs(ctx context.Context, k *types.Knowledge) ([]string, error) {
	var ids []string
	err := r.db.WithContext(ctx).Model(&types.Chunk{}).Where("tenant_id = ? AND knowledge_id = ? AND processing_generation = ? AND processing_generation <> (SELECT published_generation FROM knowledges WHERE tenant_id = ? AND id = ?)", k.TenantID, k.ID, k.ProcessingGeneration, k.TenantID, k.ID).Pluck("id", &ids).Error
	return ids, err
}

func (r *Registry) DeleteDraftChunks(ctx context.Context, k *types.Knowledge, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Unscoped().Where("tenant_id = ? AND knowledge_id = ? AND processing_generation = ? AND id IN ? AND processing_generation <> (SELECT published_generation FROM knowledges WHERE tenant_id = ? AND id = ?)", k.TenantID, k.ID, k.ProcessingGeneration, ids, k.TenantID, k.ID).Delete(&types.Chunk{}).Error
}

// DrainIndexRetirements needs no new scheduler: the elected housekeeping
// runner supplies a bounded context. Provider deletion is idempotent; the
// receipt remains until both vector and relational deletion have succeeded.
func DrainIndexRetirements(ctx context.Context, db *gorm.DB, remove func(context.Context, uint64, IndexRetirement) error) error {
	var rows []types.TaskPendingOp
	if err := db.WithContext(ctx).Where("task_type = ?", IndexRetirementTask).Order("enqueued_at,id").Limit(32).Find(&rows).Error; err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		var payload IndexRetirement
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			failures = append(failures, err)
			continue
		}
		var enabled int64
		if err := db.WithContext(ctx).Model(&types.Chunk{}).Where("tenant_id = ? AND knowledge_id = ? AND id IN ? AND is_enabled = ?", row.TenantID, payload.KnowledgeID, payload.ChunkIDs, true).Count(&enabled).Error; err != nil {
			failures = append(failures, err)
			continue
		}
		if enabled > 0 {
			failures = append(failures, errors.New("retirement receipt overlaps enabled chunks"))
			continue
		}
		if err := remove(ctx, row.TenantID, payload); err != nil {
			failures = append(failures, err)
			continue
		}
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Unscoped().Where("tenant_id = ? AND knowledge_id = ? AND id IN ? AND is_enabled = ?", row.TenantID, payload.KnowledgeID, payload.ChunkIDs, false).Delete(&types.Chunk{}).Error; err != nil {
				return err
			}
			return tx.Where("id = ? AND task_type = ?", row.ID, IndexRetirementTask).Delete(&types.TaskPendingOp{}).Error
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
