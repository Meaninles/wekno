package knowledgeaux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrSourceMigrationConflict = errors.New("knowledge source migration lost its ownership fence")

// AdoptMigratedSource atomically creates the durable source-file ownership
// ledger and switches knowledges.file_path. The provider copy must already be
// verified before this method is called. The old provider object is deliberately
// left untouched so an operator can roll the row back to expectedPath.
//
// The KB -> knowledge lock order is shared with deletion and other knowledge
// writers. expectedGeneration and expectedPath form an optimistic fence against
// a reparse, move, delete, or a concurrent migration that won while the remote
// object was being staged.
func (r *Registry) AdoptMigratedSource(
	ctx context.Context,
	expectedPath string,
	expectedGeneration string,
	object Object,
	service interfaces.FileService,
) (Object, error) {
	expectedPath = strings.TrimSpace(expectedPath)
	expectedGeneration = strings.TrimSpace(expectedGeneration)
	if r == nil || r.db == nil || expectedPath == "" || object.Kind != KindSourceFile {
		return Object{}, ErrInvalidObject
	}
	object, err := r.prepareBoundObject(ctx, object, []interfaces.FileService{service})
	if err != nil {
		return Object{}, err
	}
	if object.ProcessingGeneration != expectedGeneration {
		return Object{}, fmt.Errorf("%w: caller generation changed", ErrSourceMigrationConflict)
	}

	err = kbwritefence.WithActive(
		ctx,
		r.db,
		object.TenantID,
		object.KnowledgeBaseID,
		func(tx *gorm.DB) error {
			query := tx.Table("knowledges").
				Select(
					"id",
					"tenant_id",
					"knowledge_base_id",
					"file_path",
					"processing_generation",
					"parse_status",
				).
				Where(
					"tenant_id = ? AND knowledge_base_id = ? AND id = ? AND deleted_at IS NULL",
					object.TenantID,
					object.KnowledgeBaseID,
					object.KnowledgeID,
				)
			if tx.Dialector.Name() != "sqlite" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var owner struct {
				ID                   string
				TenantID             uint64
				KnowledgeBaseID      string
				FilePath             string
				ProcessingGeneration string
				ParseStatus          string
			}
			if err := query.Take(&owner).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrSourceMigrationConflict
				}
				return fmt.Errorf("migrate source ownership: lock knowledge: %w", err)
			}
			if strings.TrimSpace(owner.FilePath) != expectedPath ||
				strings.TrimSpace(owner.ProcessingGeneration) != expectedGeneration ||
				owner.ParseStatus != types.ParseStatusCompleted {
				return ErrSourceMigrationConflict
			}

			if owner.ProcessingGeneration == "" {
				object.ProcessingGeneration = uuid.NewString()
			} else {
				object.ProcessingGeneration = owner.ProcessingGeneration
			}
			object, err = normalizeObject(object)
			if err != nil {
				return err
			}

			prefix := objectKeyPrefix(object.KnowledgeID)
			ledgerQuery := tx.Where(
				"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? "+
					"AND substr(dedup_key, 1, length(?)) = ?",
				object.TenantID,
				TaskType,
				types.TaskScopeKnowledgeBase,
				object.KnowledgeBaseID,
				operationOwned,
				prefix,
				prefix,
			)
			if tx.Dialector.Name() != "sqlite" {
				ledgerQuery = ledgerQuery.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var existing []*types.TaskPendingOp
			if err := ledgerQuery.Find(&existing).Error; err != nil {
				return fmt.Errorf("migrate source ownership: lock ledger: %w", err)
			}

			key := objectKey(object.KnowledgeID, object.Path)
			var destinationRows []*types.TaskPendingOp
			for _, row := range existing {
				persisted, err := decodeObject(row.Payload)
				if err != nil {
					return fmt.Errorf("migrate source ownership: corrupt ledger row %d: %w", row.ID, err)
				}
				if !isPersistentSourceKind(persisted.Kind) {
					continue
				}
				if row.DedupKey != key || persisted.Path != object.Path {
					return fmt.Errorf(
						"%w: another source ledger already owns %q",
						ErrSourceMigrationConflict,
						persisted.Path,
					)
				}
				if persisted.Quarantined {
					return ErrBindingQuarantined
				}
				if !sameObject(persisted, object) {
					return fmt.Errorf(
						"%w: destination source ledger differs",
						ErrSourceMigrationConflict,
					)
				}
				destinationRows = append(destinationRows, row)
			}

			payload, err := json.Marshal(object)
			if err != nil {
				return fmt.Errorf("migrate source ownership: encode ledger: %w", err)
			}
			if len(destinationRows) == 0 {
				if err := tx.Create(&types.TaskPendingOp{
					TenantID:   object.TenantID,
					TaskType:   TaskType,
					Scope:      types.TaskScopeKnowledgeBase,
					ScopeID:    object.KnowledgeBaseID,
					Op:         operationOwned,
					DedupKey:   key,
					Payload:    payload,
					EnqueuedAt: time.Now().UTC(),
				}).Error; err != nil {
					return fmt.Errorf("migrate source ownership: create ledger: %w", err)
				}
			} else {
				ids := make([]int64, 0, len(destinationRows))
				for _, row := range destinationRows {
					ids = append(ids, row.ID)
				}
				if err := tx.Model(&types.TaskPendingOp{}).
					Where("id IN ?", ids).
					Updates(map[string]interface{}{
						"payload":     payload,
						"fail_count":  0,
						"enqueued_at": time.Now().UTC(),
						"claimed_at":  nil,
					}).Error; err != nil {
					return fmt.Errorf("migrate source ownership: refresh ledger: %w", err)
				}
			}

			result := tx.Exec(
				`UPDATE knowledges
				 SET file_path = ?, processing_generation = ?
				 WHERE tenant_id = ? AND knowledge_base_id = ? AND id = ?
				   AND deleted_at IS NULL AND parse_status = ?
				   AND file_path = ? AND processing_generation = ?`,
				object.Path,
				object.ProcessingGeneration,
				object.TenantID,
				object.KnowledgeBaseID,
				object.KnowledgeID,
				types.ParseStatusCompleted,
				expectedPath,
				expectedGeneration,
			)
			if result.Error != nil {
				return fmt.Errorf("migrate source ownership: switch source path: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return ErrSourceMigrationConflict
			}
			return nil
		},
	)
	if err != nil {
		return Object{}, err
	}
	return object, nil
}
