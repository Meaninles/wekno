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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// persistentTransferOwner is intentionally narrower than types.Knowledge. It
// is the authoritative owner identity that a persistent source-file ledger
// must follow across a knowledge-base move.
type persistentTransferOwner struct {
	ID                   string
	TenantID             uint64
	KnowledgeBaseID      string
	ProcessingGeneration string
	FilePath             string
	ParseStatus          string
	DeletedAt            gorm.DeletedAt
}

// TransferPersistentOwnershipTx moves the source-file ownership proof in the
// same transaction that publishes a document's new KB identity. The caller
// must already hold both parent KB locks in deterministic order. Missing
// legacy ownership is tolerated so the startup backfill can adopt it, while a
// present but ambiguous/quarantined proof fails the move closed.
func TransferPersistentOwnershipTx(
	tx *gorm.DB,
	tenantID uint64,
	knowledgeID string,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	processingGeneration string,
	filePath string,
) (int, error) {
	return transferPersistentOwnershipTx(
		tx,
		tenantID,
		knowledgeID,
		sourceKnowledgeBaseID,
		targetKnowledgeBaseID,
		processingGeneration,
		filePath,
		false,
	)
}

func transferPersistentOwnershipTx(
	tx *gorm.DB,
	tenantID uint64,
	knowledgeID string,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	processingGeneration string,
	filePath string,
	allowMovedSourceQuarantine bool,
) (int, error) {
	knowledgeID = strings.TrimSpace(knowledgeID)
	sourceKnowledgeBaseID = strings.TrimSpace(sourceKnowledgeBaseID)
	targetKnowledgeBaseID = strings.TrimSpace(targetKnowledgeBaseID)
	processingGeneration = strings.TrimSpace(processingGeneration)
	filePath = strings.TrimSpace(filePath)
	if tx == nil || tenantID == 0 || knowledgeID == "" || sourceKnowledgeBaseID == "" ||
		targetKnowledgeBaseID == "" || processingGeneration == "" || filePath == "" {
		return 0, ErrInvalidObject
	}
	if sourceKnowledgeBaseID == targetKnowledgeBaseID {
		return 0, nil
	}

	ownerQuery := tx.Unscoped().Table("knowledges").
		Select("id", "tenant_id", "knowledge_base_id", "processing_generation", "file_path", "parse_status", "deleted_at").
		Where("tenant_id = ? AND id = ?", tenantID, knowledgeID)
	if tx.Dialector.Name() != "sqlite" {
		ownerQuery = ownerQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var owner persistentTransferOwner
	if err := ownerQuery.Take(&owner).Error; err != nil {
		return 0, fmt.Errorf("transfer persistent auxiliary owner: %w", err)
	}
	if owner.DeletedAt.Valid || owner.KnowledgeBaseID != targetKnowledgeBaseID ||
		owner.ProcessingGeneration != processingGeneration || strings.TrimSpace(owner.FilePath) != filePath {
		return 0, ErrKnowledgeFence
	}
	switch owner.ParseStatus {
	case types.ParseStatusDeleting, types.ParseStatusCancelling, types.ParseStatusCancelled:
		return 0, ErrKnowledgeFence
	}

	prefix := objectKeyPrefix(knowledgeID)
	ledgerQuery := tx.Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id IN ? AND op = ? AND substr(dedup_key, 1, length(?)) = ?",
		tenantID,
		TaskType,
		types.TaskScopeKnowledgeBase,
		[]string{sourceKnowledgeBaseID, targetKnowledgeBaseID},
		operationOwned,
		prefix,
		prefix,
	).Order("id ASC")
	if tx.Dialector.Name() != "sqlite" {
		ledgerQuery = ledgerQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var rows []*types.TaskPendingOp
	if err := ledgerQuery.Find(&rows).Error; err != nil {
		return 0, fmt.Errorf("transfer persistent auxiliary ownership: lock ledger: %w", err)
	}

	var matches []*types.TaskPendingOp
	var canonical Object
	for _, row := range rows {
		object, err := decodeObject(row.Payload)
		if err != nil || object.TenantID != tenantID || object.KnowledgeBaseID != row.ScopeID ||
			object.KnowledgeID != knowledgeID || objectKey(object.KnowledgeID, object.Path) != row.DedupKey {
			return 0, fmt.Errorf("transfer persistent auxiliary ownership: corrupt ledger row %d: %w", row.ID, errors.Join(err, ErrInvalidObject))
		}
		if !isPersistentSourceKind(object.Kind) || object.Path != filePath {
			continue
		}
		if object.Binding == nil {
			return 0, fmt.Errorf("transfer persistent auxiliary ownership row %d: %w", row.ID, ErrBindingMissing)
		}
		if object.Quarantined && (!allowMovedSourceQuarantine ||
			object.QuarantineReason != quarantineReasonSharedPhysical) {
			return 0, fmt.Errorf("transfer persistent auxiliary ownership row %d: %w", row.ID, ErrBindingQuarantined)
		}
		if len(matches) == 0 {
			canonical = object
		} else if canonical.Kind != object.Kind || canonical.Path != object.Path ||
			!sameBinding(canonical.Binding, object.Binding) {
			return 0, fmt.Errorf("transfer persistent auxiliary ownership: conflicting proofs for %s: %w", knowledgeID, ErrBindingMismatch)
		}
		matches = append(matches, row)
	}
	if len(matches) == 0 {
		return 0, nil
	}

	// Keep the oldest durable proof. Delete exact duplicates before changing
	// its scope so the partial unique index cannot reject an otherwise safe,
	// idempotent repair left by an interrupted legacy move.
	keeper := matches[0]
	if len(matches) > 1 {
		duplicateIDs := make([]int64, 0, len(matches)-1)
		for _, row := range matches[1:] {
			duplicateIDs = append(duplicateIDs, row.ID)
		}
		if err := tx.Where("id IN ?", duplicateIDs).Delete(&types.TaskPendingOp{}).Error; err != nil {
			return 0, fmt.Errorf("transfer persistent auxiliary ownership: merge duplicate ledgers: %w", err)
		}
	}

	canonical.KnowledgeBaseID = targetKnowledgeBaseID
	canonical.ProcessingGeneration = processingGeneration
	canonical.Quarantined = false
	canonical.QuarantineReason = ""
	payload, err := json.Marshal(canonical)
	if err != nil {
		return 0, fmt.Errorf("transfer persistent auxiliary ownership: encode target proof: %w", err)
	}
	result := tx.Model(&types.TaskPendingOp{}).
		Where("id = ? AND tenant_id = ? AND task_type = ? AND scope = ? AND op = ? AND dedup_key = ?",
			keeper.ID, tenantID, TaskType, types.TaskScopeKnowledgeBase, operationOwned, keeper.DedupKey).
		Updates(map[string]interface{}{
			"scope_id":    targetKnowledgeBaseID,
			"payload":     payload,
			"fail_count":  0,
			"enqueued_at": time.Now().UTC(),
			"claimed_at":  nil,
		})
	if result.Error != nil {
		return 0, fmt.Errorf("transfer persistent auxiliary ownership: publish target proof: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return 0, ErrReservationLost
	}
	return len(matches), nil
}

// repairMovedPersistentOwnership is the startup compatibility/recovery lane
// for a move that committed before the atomic transfer above was installed or
// while an older process was draining. Both endpoint KBs must still be active;
// tombstoned/missing owners remain under the ordinary deletion recovery path.
func (r *Registry) repairMovedPersistentOwnership(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	processingGeneration string,
	filePath string,
) (int, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("knowledge auxiliary object registry is unavailable")
	}
	var transferred int
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := kbwritefence.LockActiveSharedSet(
			tx, tenantID, sourceKnowledgeBaseID, targetKnowledgeBaseID,
		); err != nil {
			return err
		}
		var err error
		transferred, err = transferPersistentOwnershipTx(
			tx,
			tenantID,
			knowledgeID,
			sourceKnowledgeBaseID,
			targetKnowledgeBaseID,
			processingGeneration,
			filePath,
			true,
		)
		return err
	})
	return transferred, err
}

// reconcileMovedPersistentOwnership runs before the binding backfill groups
// physical paths. Without this pass, a perfectly legal KB move appears as two
// logical owners for one raw path: the current knowledge row in the target KB
// and its still-source-scoped durable ledger. Repairing that exact identity
// first prevents a false shared-object quarantine while preserving the strict
// quarantine for genuinely different knowledge owners.
func (r *Registry) reconcileMovedPersistentOwnership(ctx context.Context) error {
	return r.walkAuxiliaryLedgerRows(ctx, func(row *types.TaskPendingOp) error {
		if row == nil {
			return nil
		}
		object, err := decodeObject(row.Payload)
		if err != nil || !isPersistentSourceKind(object.Kind) ||
			object.TenantID != row.TenantID || object.KnowledgeBaseID != row.ScopeID ||
			objectKey(object.KnowledgeID, object.Path) != row.DedupKey {
			// The normal backfill/recovery validation owns malformed ledgers. This
			// narrow pre-pass must never reinterpret them as a legal move.
			return nil
		}

		var owner persistentTransferOwner
		err = r.db.WithContext(ctx).Unscoped().Table("knowledges").
			Select("id", "tenant_id", "knowledge_base_id", "processing_generation", "file_path", "parse_status", "deleted_at").
			Where("tenant_id = ? AND id = ?", object.TenantID, object.KnowledgeID).
			Take(&owner).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if owner.DeletedAt.Valid || owner.KnowledgeBaseID == object.KnowledgeBaseID ||
			strings.TrimSpace(owner.KnowledgeBaseID) == "" ||
			strings.TrimSpace(owner.ProcessingGeneration) == "" ||
			strings.TrimSpace(owner.FilePath) != object.Path {
			return nil
		}
		switch owner.ParseStatus {
		case types.ParseStatusDeleting, types.ParseStatusCancelling, types.ParseStatusCancelled:
			return nil
		}

		_, err = r.repairMovedPersistentOwnership(
			ctx,
			object.TenantID,
			object.KnowledgeID,
			object.KnowledgeBaseID,
			owner.KnowledgeBaseID,
			owner.ProcessingGeneration,
			owner.FilePath,
		)
		if errors.Is(err, kbwritefence.ErrKnowledgeBaseUnavailable) ||
			errors.Is(err, ErrKnowledgeFence) {
			// A concurrent/tombstoned lifecycle is not a move proof. Leave the
			// row untouched for ordinary deletion recovery.
			return nil
		}
		return err
	})
}
