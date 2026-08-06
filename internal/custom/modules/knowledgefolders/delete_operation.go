package knowledgefolders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const folderDeleteTaskBatchSize = 200

// RequestDeleteFolder atomically hides a complete subtree and snapshots the
// exact document identities that the native deletion pipeline must clean.
// No document or child folder is ever moved to the parent.
func (s *Service) RequestDeleteFolder(
	ctx context.Context,
	kbID, folderID string,
) (*FolderDeleteOperation, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	op := &FolderDeleteOperation{
		ID:              uuid.NewString(),
		TenantID:        tenantID,
		KnowledgeBaseID: strings.TrimSpace(kbID),
		RequestedBy:     userIDFromContext(ctx),
		Status:          FolderDeleteOperationPending,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		root, err := s.findFolder(tx, tenantID, kbID, folderID, true)
		if err != nil {
			return err
		}
		op.RootFolderID = root.ID
		op.RootFolderName = root.Name
		op.ParentFolderID = root.ParentID

		descendants := tx.Model(&FolderClosure{}).
			Select("descendant_id").
			Where(
				"tenant_id = ? AND knowledge_base_id = ? AND ancestor_id = ?",
				tenantID, kbID, root.ID,
			)
		folderQuery := tx.Model(&Folder{}).
			Where("tenant_id = ? AND knowledge_base_id = ? AND id IN (?)", tenantID, kbID, descendants).
			Order("id ASC")
		if tx.Dialector.Name() != "sqlite" {
			folderQuery = folderQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var folders []Folder
		if err := folderQuery.Find(&folders).Error; err != nil {
			return err
		}
		if len(folders) == 0 {
			return ErrFolderNotFound
		}
		folderIDs := make([]string, 0, len(folders))
		for _, folder := range folders {
			if folder.DeleteStatus == FolderDeleteStatusDeleting {
				return ErrFolderNotFound
			}
			folderIDs = append(folderIDs, folder.ID)
		}

		var knowledgeIDs []string
		documentQuery := tx.Model(&types.Knowledge{}).
			Select("id").
			Where(
				"tenant_id = ? AND knowledge_base_id = ? AND folder_id IN ?",
				tenantID, kbID, folderIDs,
			).
			Order("id ASC")
		if tx.Dialector.Name() != "sqlite" {
			documentQuery = documentQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := documentQuery.Pluck("id", &knowledgeIDs).Error; err != nil {
			return err
		}
		op.TotalDocumentCount = int64(len(knowledgeIDs))
		if err := tx.Create(op).Error; err != nil {
			return err
		}
		if err := tx.Model(&Folder{}).
			Where("tenant_id = ? AND knowledge_base_id = ? AND id IN ?", tenantID, kbID, folderIDs).
			Updates(map[string]any{
				"delete_status":       FolderDeleteStatusDeleting,
				"delete_operation_id": op.ID,
				"updated_at":          now,
			}).Error; err != nil {
			return err
		}
		// The subtree is hidden immediately, so the visible child count must
		// change in the same transaction rather than waiting for final cleanup.
		if root.ParentID != "" {
			if err := updateDirectChildCount(tx, root.ParentID, -1); err != nil {
				return err
			}
		}
		if len(knowledgeIDs) == 0 {
			return nil
		}
		items := make([]FolderDeleteOperationItem, 0, len(knowledgeIDs))
		for _, knowledgeID := range knowledgeIDs {
			items = append(items, FolderDeleteOperationItem{
				OperationID: op.ID,
				KnowledgeID: knowledgeID,
				CreatedAt:   now,
			})
		}
		if err := tx.CreateInBatches(items, folderDeleteTaskBatchSize).Error; err != nil {
			return err
		}
		// Visibility changes synchronously. The native worker will atomically
		// establish its Wiki retract intent before destructive cleanup and is
		// idempotent when the row is already in this state.
		if err := tx.Model(&types.Knowledge{}).
			Where("tenant_id = ? AND knowledge_base_id = ? AND id IN ?", tenantID, kbID, knowledgeIDs).
			Updates(map[string]any{
				"parse_status": types.ParseStatusDeleting,
				"updated_at":   now,
			}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if op.TotalDocumentCount == 0 {
		if err := s.tryFinalizeDeleteOperation(ctx, op.ID); err != nil {
			return nil, err
		}
		return s.getDeleteOperation(ctx, tenantID, kbID, op.ID)
	}
	// Dispatch failure does not lose the durable request. The operation status
	// endpoint retries dispatch and startup recovery can safely do the same.
	_ = s.dispatchDeleteOperation(ctx, op)
	return s.getDeleteOperation(ctx, tenantID, kbID, op.ID)
}

// DeleteFolder remains as a service-level compatibility wrapper. The old mode
// is intentionally ignored: recursive deletion is now the only delete meaning.
func (s *Service) DeleteFolder(ctx context.Context, kbID, folderID, _ string) error {
	_, err := s.RequestDeleteFolder(ctx, kbID, folderID)
	return err
}

func (s *Service) dispatchDeleteOperation(ctx context.Context, op *FolderDeleteOperation) error {
	if op == nil || op.Status == FolderDeleteOperationCompleted {
		return nil
	}
	if s.taskEnqueuer == nil {
		err := errors.New("knowledge folder delete queue is unavailable")
		s.recordDeleteOperationError(ctx, op.ID, err)
		return err
	}
	var ids []string
	if err := s.db.WithContext(ctx).Model(&FolderDeleteOperationItem{}).
		Where("operation_id = ?", op.ID).
		Order("knowledge_id ASC").
		Pluck("knowledge_id", &ids).Error; err != nil {
		s.recordDeleteOperationError(ctx, op.ID, err)
		return err
	}
	for start := 0; start < len(ids); start += folderDeleteTaskBatchSize {
		end := start + folderDeleteTaskBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		payload, err := json.Marshal(types.KnowledgeListDeletePayload{
			TenantID:                op.TenantID,
			KnowledgeIDs:            ids[start:end],
			ExpectedKnowledgeBaseID: op.KnowledgeBaseID,
		})
		if err != nil {
			s.recordDeleteOperationError(ctx, op.ID, err)
			return err
		}
		batchIndex := start / folderDeleteTaskBatchSize
		_, err = s.taskEnqueuer.Enqueue(
			asynq.NewTask(types.TypeKnowledgeListDelete, payload),
			asynq.Queue("low"),
			asynq.TaskID(fmt.Sprintf("folder-delete:%s:%d", op.ID, batchIndex)),
			asynq.MaxRetry(3),
			asynq.Timeout(6*time.Hour),
		)
		if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) && !errors.Is(err, asynq.ErrDuplicateTask) {
			s.recordDeleteOperationError(ctx, op.ID, err)
			return err
		}
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Model(&FolderDeleteOperation{}).
		Where("id = ? AND status <> ?", op.ID, FolderDeleteOperationCompleted).
		Updates(map[string]any{
			"status":        FolderDeleteOperationRunning,
			"last_error":    "",
			"dispatched_at": now,
			"updated_at":    now,
		}).Error
}

func (s *Service) recordDeleteOperationError(ctx context.Context, operationID string, operationErr error) {
	if operationErr == nil {
		return
	}
	_ = s.db.WithContext(ctx).Model(&FolderDeleteOperation{}).
		Where("id = ? AND status <> ?", operationID, FolderDeleteOperationCompleted).
		Updates(map[string]any{
			"status":     FolderDeleteOperationPending,
			"last_error": operationErr.Error(),
			"updated_at": time.Now().UTC(),
		}).Error
}

func (s *Service) GetDeleteOperation(
	ctx context.Context,
	kbID, operationID string,
) (*FolderDeleteOperation, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	op, err := s.getDeleteOperation(ctx, tenantID, kbID, operationID)
	if err != nil {
		return nil, err
	}
	if op.Status != FolderDeleteOperationCompleted && op.DispatchedAt == nil {
		_ = s.dispatchDeleteOperation(ctx, op)
	}
	if err := s.tryFinalizeDeleteOperation(ctx, op.ID); err != nil {
		return nil, err
	}
	return s.getDeleteOperation(ctx, tenantID, kbID, operationID)
}

func (s *Service) getDeleteOperation(
	ctx context.Context,
	tenantID uint64,
	kbID, operationID string,
) (*FolderDeleteOperation, error) {
	var op FolderDeleteOperation
	if err := s.db.WithContext(ctx).Where(
		"tenant_id = ? AND knowledge_base_id = ? AND id = ?",
		tenantID, strings.TrimSpace(kbID), strings.TrimSpace(operationID),
	).First(&op).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrFolderNotFound
		}
		return nil, err
	}
	return &op, nil
}

// OnKnowledgeDeleteCompleted is registered as the native document-delete
// completion hook. It never changes document cleanup; it only finalizes folder
// operations whose complete snapshot has now disappeared.
func (s *Service) OnKnowledgeDeleteCompleted(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeIDs []string,
) error {
	if tenantID == 0 || len(knowledgeIDs) == 0 {
		return nil
	}
	var operationIDs []string
	if err := s.db.WithContext(ctx).Model(&FolderDeleteOperationItem{}).
		Distinct("custom_knowledge_folder_delete_items.operation_id").
		Joins("JOIN custom_knowledge_folder_delete_operations AS op ON op.id = custom_knowledge_folder_delete_items.operation_id").
		Where("op.tenant_id = ? AND op.knowledge_base_id = ? AND op.status <> ?", tenantID, knowledgeBaseID, FolderDeleteOperationCompleted).
		Where("custom_knowledge_folder_delete_items.knowledge_id IN ?", knowledgeIDs).
		Pluck("custom_knowledge_folder_delete_items.operation_id", &operationIDs).Error; err != nil {
		return err
	}
	for _, operationID := range operationIDs {
		if err := s.tryFinalizeDeleteOperation(ctx, operationID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) tryFinalizeDeleteOperation(ctx context.Context, operationID string) error {
	var active int64
	if err := s.db.WithContext(ctx).Unscoped().Table("knowledges AS knowledge").
		Joins("JOIN custom_knowledge_folder_delete_items AS item ON item.knowledge_id = knowledge.id").
		Where("item.operation_id = ? AND knowledge.deleted_at IS NULL", operationID).
		Count(&active).Error; err != nil {
		return err
	}
	var op FolderDeleteOperation
	if err := s.db.WithContext(ctx).Where("id = ?", operationID).First(&op).Error; err != nil {
		return err
	}
	deleted := op.TotalDocumentCount - active
	if deleted < 0 {
		deleted = 0
	}
	if active > 0 {
		return s.db.WithContext(ctx).Model(&FolderDeleteOperation{}).
			Where("id = ?", operationID).
			Updates(map[string]any{
				"deleted_document_count": deleted,
				"updated_at":             time.Now().UTC(),
			}).Error
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked FolderDeleteOperation
		query := tx.Where("id = ?", operationID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&locked).Error; err != nil {
			return err
		}
		if locked.Status == FolderDeleteOperationCompleted {
			return nil
		}
		var folderIDs []string
		if err := tx.Model(&Folder{}).
			Where("delete_operation_id = ?", operationID).
			Pluck("id", &folderIDs).Error; err != nil {
			return err
		}
		if len(folderIDs) > 0 {
			if err := tx.Where("ancestor_id IN ? OR descendant_id IN ?", folderIDs, folderIDs).
				Delete(&FolderClosure{}).Error; err != nil {
				return err
			}
			if err := tx.Where("folder_id IN ?", folderIDs).Delete(&FolderStats{}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", folderIDs).Delete(&Folder{}).Error; err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		return tx.Model(&FolderDeleteOperation{}).Where("id = ?", operationID).
			Updates(map[string]any{
				"status":                 FolderDeleteOperationCompleted,
				"deleted_document_count": locked.TotalDocumentCount,
				"last_error":             "",
				"completed_at":           now,
				"updated_at":             now,
			}).Error
	})
}
