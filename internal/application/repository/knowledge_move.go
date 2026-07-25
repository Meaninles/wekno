package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// WithActiveKnowledgeMoveScope keeps both move endpoints active while the
// caller mutates external vector state, chunks, and the authoritative
// knowledge row. PostgreSQL holds parent SHARE locks for the complete callback;
// Lite mode uses kbwritefence's process-wide delete gate.
func (r *knowledgeRepository) WithActiveKnowledgeMoveScope(
	ctx context.Context,
	tenantID uint64,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	work func() error,
) error {
	return kbwritefence.WithActiveSharedSet(
		ctx,
		r.db,
		tenantID,
		[]string{sourceKnowledgeBaseID, targetKnowledgeBaseID},
		work,
	)
}

// PrepareKnowledgeMoveReparseRecovery replaces any lifecycle identity left by
// the completed source parse with a fresh recovery generation before cleanup.
// The exact persisted move marker is part of the fence, so concurrent parent
// retries cannot both publish different recovery owners.
func (r *knowledgeRepository) PrepareKnowledgeMoveReparseRecovery(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	expectedGeneration string,
	expectedOwner string,
	expectedMarker string,
	newGeneration string,
	newOwner string,
	newMarker string,
	updatedAt time.Time,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" ||
		expectedMarker == "" || newGeneration == "" || newOwner == "" ||
		newMarker == "" || updatedAt.IsZero() {
		return false, errors.New("prepare knowledge move recovery reparse: complete identity is required")
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND COALESCE(processing_generation, '') = ? AND COALESCE(processing_owner, '') = ? AND error_message = ? AND deleted_at IS NULL",
			tenantID,
			knowledgeID,
			knowledgeBaseID,
			types.ParseStatusProcessing,
			expectedGeneration,
			expectedOwner,
			expectedMarker,
		).
		Updates(map[string]interface{}{
			"processing_generation":  newGeneration,
			"processing_owner":       newOwner,
			"processing_workflow_id": "",
			"processing_fanout":      nil,
			"pending_subtasks_count": 0,
			"enrichment_status":      types.EnrichmentStatusNone,
			"wiki_status":            types.WikiStatusNone,
			"wiki_error_message":     "",
			"error_message":          newMarker,
			"updated_at":             updatedAt,
		})
	return result.RowsAffected == 1, result.Error
}

// FinalizeReuseVectorKnowledgeMove atomically publishes the target knowledge
// identity and moves every durable fan-out completion fact into the same KB
// scope. Fan-out writers lock the knowledge row before inserting, so the
// knowledge-first ordering below also fences a completion that arrives while
// the external vector/chunk move is in progress.
func (r *knowledgeRepository) FinalizeReuseVectorKnowledgeMove(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	expectedGeneration string,
	expectedOwner string,
	wikiMarker string,
	updatedAt time.Time,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || sourceKnowledgeBaseID == "" ||
		targetKnowledgeBaseID == "" || sourceKnowledgeBaseID == targetKnowledgeBaseID ||
		expectedGeneration == "" || wikiMarker == "" || updatedAt.IsZero() {
		return false, errors.New("finalize reuse-vector knowledge move: complete distinct identity is required")
	}

	var moved bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := kbwritefence.LockActiveSharedSet(
			tx, tenantID, sourceKnowledgeBaseID, targetKnowledgeBaseID,
		); err != nil {
			return fmt.Errorf("finalize reuse-vector knowledge move: lock parent knowledge bases: %w", err)
		}
		result := tx.Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND processing_generation = ? AND COALESCE(processing_owner, '') = ? AND deleted_at IS NULL",
				tenantID,
				knowledgeID,
				sourceKnowledgeBaseID,
				types.ParseStatusProcessing,
				expectedGeneration,
				expectedOwner,
			).
			Updates(map[string]interface{}{
				"knowledge_base_id":      targetKnowledgeBaseID,
				"parse_status":           types.ParseStatusCompleted,
				"error_message":          wikiMarker,
				"pending_subtasks_count": 0,
				"processing_owner":       "",
				"processing_fanout":      nil,
				"updated_at":             updatedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		var owner struct {
			FilePath string
		}
		if err := tx.Table("knowledges").Select("file_path").
			Where("tenant_id = ? AND id = ? AND knowledge_base_id = ? AND deleted_at IS NULL",
				tenantID, knowledgeID, targetKnowledgeBaseID).
			Take(&owner).Error; err != nil {
			return fmt.Errorf("finalize reuse-vector knowledge move: load target source path: %w", err)
		}
		if _, err := knowledgeaux.TransferPersistentOwnershipTx(
			tx,
			tenantID,
			knowledgeID,
			sourceKnowledgeBaseID,
			targetKnowledgeBaseID,
			expectedGeneration,
			owner.FilePath,
		); err != nil {
			return fmt.Errorf("finalize reuse-vector knowledge move: transfer source ownership: %w", err)
		}

		// Move all generations, not just the current one. This repairs any older
		// completion facts in the same document scope and lets the next reparse's
		// target-scoped retention cleanup bound the ledger correctly.
		if err := tx.Model(&types.KnowledgeFanoutCompletion{}).
			Where(
				"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ?",
				tenantID, knowledgeID, sourceKnowledgeBaseID,
			).
			Update("knowledge_base_id", targetKnowledgeBaseID).Error; err != nil {
			return err
		}
		if err := tx.Model(&enrichmentoutcome.Outcome{}).
			Where(
				"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ?",
				tenantID, knowledgeID, sourceKnowledgeBaseID,
			).
			Update("knowledge_base_id", targetKnowledgeBaseID).Error; err != nil {
			return err
		}
		moved = true
		return nil
	})
	return moved, err
}

// FinalizeReparseKnowledgeMove atomically switches a claimed document into
// target Pending and removes completion facts from the destroyed parse. The
// new processing generation cannot have published a completion before this
// transition, and generation-scoped writers take the same knowledge-row lock,
// so deleting the complete per-document ledger is safe and race-free.
func (r *knowledgeRepository) FinalizeReparseKnowledgeMove(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	expectedGeneration string,
	expectedOwner string,
	targetEmbeddingModelID string,
	moveMarker string,
	processingWorkflowID string,
	bindPreparedWorkflowTx func(*gorm.DB, func(*gorm.DB) error) error,
	updatedAt time.Time,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || sourceKnowledgeBaseID == "" ||
		targetKnowledgeBaseID == "" || expectedGeneration == "" || expectedOwner == "" ||
		moveMarker == "" || updatedAt.IsZero() {
		return false, errors.New("finalize reparse knowledge move: complete identity is required")
	}
	if sourceKnowledgeBaseID == targetKnowledgeBaseID && processingWorkflowID == "" {
		return false, errors.New("finalize reparse knowledge move: source recovery workflow binding is required")
	}
	if sourceKnowledgeBaseID == targetKnowledgeBaseID && bindPreparedWorkflowTx == nil {
		return false, errors.New("finalize reparse knowledge move: source recovery workflow transaction is required")
	}
	if sourceKnowledgeBaseID != targetKnowledgeBaseID && processingWorkflowID != "" {
		return false, errors.New("finalize reparse knowledge move: cross-KB workflow must bind with Wiki settlement")
	}
	if sourceKnowledgeBaseID != targetKnowledgeBaseID && bindPreparedWorkflowTx != nil {
		return false, errors.New("finalize reparse knowledge move: cross-KB workflow transaction must bind with Wiki settlement")
	}

	var moved bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := kbwritefence.LockActiveSharedSet(
			tx, tenantID, sourceKnowledgeBaseID, targetKnowledgeBaseID,
		); err != nil {
			return fmt.Errorf("finalize reparse knowledge move: lock parent knowledge bases: %w", err)
		}
		finalize := func(tx *gorm.DB) error {
			result := tx.Model(&types.Knowledge{}).
				Where(
					"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND processing_generation = ? AND processing_owner = ? AND deleted_at IS NULL",
					tenantID,
					knowledgeID,
					sourceKnowledgeBaseID,
					types.ParseStatusProcessing,
					expectedGeneration,
					expectedOwner,
				).
				Updates(map[string]interface{}{
					"knowledge_base_id":      targetKnowledgeBaseID,
					"embedding_model_id":     targetEmbeddingModelID,
					"parse_status":           types.ParseStatusPending,
					"error_message":          moveMarker,
					"enable_status":          "disabled",
					"description":            "",
					"processed_at":           nil,
					"pending_subtasks_count": 0,
					"enrichment_status":      types.EnrichmentStatusNone,
					"wiki_status":            types.WikiStatusNone,
					"wiki_error_message":     "",
					"processing_fanout":      nil,
					"processing_workflow_id": processingWorkflowID,
					"storage_size":           0,
					"updated_at":             updatedAt,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return nil
			}
			var owner struct {
				FilePath string
			}
			if err := tx.Table("knowledges").Select("file_path").
				Where("tenant_id = ? AND id = ? AND knowledge_base_id = ? AND deleted_at IS NULL",
					tenantID, knowledgeID, targetKnowledgeBaseID).
				Take(&owner).Error; err != nil {
				return fmt.Errorf("finalize reparse knowledge move: load target source path: %w", err)
			}
			if _, err := knowledgeaux.TransferPersistentOwnershipTx(
				tx,
				tenantID,
				knowledgeID,
				sourceKnowledgeBaseID,
				targetKnowledgeBaseID,
				expectedGeneration,
				owner.FilePath,
			); err != nil {
				return fmt.Errorf("finalize reparse knowledge move: transfer source ownership: %w", err)
			}

			if err := tx.Where("tenant_id = ? AND knowledge_id = ?", tenantID, knowledgeID).
				Delete(&types.KnowledgeFanoutCompletion{}).Error; err != nil {
				return err
			}
			if err := tx.Where("tenant_id = ? AND knowledge_id = ?", tenantID, knowledgeID).
				Delete(&enrichmentoutcome.Outcome{}).Error; err != nil {
				return err
			}
			moved = true
			return nil
		}
		if bindPreparedWorkflowTx != nil {
			// The queue coordinator locks the workflow before finalize is allowed
			// to lock/update knowledge, then validates this exact binding.
			return bindPreparedWorkflowTx(tx, finalize)
		}
		return finalize(tx)
	})
	if err != nil {
		moved = false
	}
	return moved, err
}

// FailKnowledgeMoveGeneration terminally releases only a synchronous move
// recovery phase. The exact marker is part of the fence so a delayed parent
// dead-letter cannot fail a child document parse that has already advanced to
// an enqueue/queued marker or a newer generation.
func (r *knowledgeRepository) FailKnowledgeMoveGeneration(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
	expectedOwner string,
	expectedMarker string,
	errorMessage string,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || expectedKnowledgeBaseID == "" ||
		expectedGeneration == "" || expectedMarker == "" || errorMessage == "" {
		return false, errors.New("fail knowledge move generation: complete identity is required")
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND processing_generation = ? AND COALESCE(processing_owner, '') = ? AND error_message = ? AND deleted_at IS NULL",
			tenantID,
			knowledgeID,
			expectedKnowledgeBaseID,
			types.ParseStatusProcessing,
			expectedGeneration,
			expectedOwner,
			expectedMarker,
		).
		Updates(map[string]interface{}{
			"parse_status":           types.ParseStatusFailed,
			"error_message":          errorMessage,
			"pending_subtasks_count": 0,
			"enrichment_status":      types.EnrichmentStatusNone,
			"wiki_status":            types.WikiStatusNone,
			"wiki_error_message":     "",
			"processing_owner":       "",
			"processing_fanout":      nil,
			"updated_at":             time.Now(),
		})
	return result.RowsAffected == 1, result.Error
}
