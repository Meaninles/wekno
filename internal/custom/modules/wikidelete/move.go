package wikidelete

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MoveRequest describes the Wiki queue transition that follows a successful
// cross-knowledge-base document move. The source retract and target ingest
// rows are supplied by the service so their payloads can contain a complete
// page/chunk snapshot and the caller's language.
//
// The coordinator is deliberately invoked after the document CAS. It locks
// the authoritative knowledge row and refuses to mutate either queue while the
// document still belongs to SourceKnowledgeBaseID, which makes a compensated
// or failed move a strict no-op.
type MoveRequest struct {
	TenantID              uint64
	KnowledgeID           string
	SourceKnowledgeBaseID string
	TargetKnowledgeBaseID string
	TargetWikiEnabled     bool
	ExpectedMarker        string
	// A reparse move is not business-ready until the exact durable document
	// workflow is attached in this same transaction.  Keeping the immutable
	// workflow ID here (rather than publishing Redis work from the caller)
	// closes the crash window between Wiki settlement and parser hand-off.
	TargetProcessingWorkflowID   string
	ExpectedProcessingGeneration string
	ExpectedProcessingOwner      string
	// BindTargetWorkflowTx must delegate to the document queue's prepared
	// workflow transition primitive. The primitive locks/validates workflow
	// before invoking the supplied business transition and validates the exact
	// Pending knowledge binding afterward.
	BindTargetWorkflowTx   func(*gorm.DB, func(*gorm.DB) error) error
	SourceRetractPendingOp *types.TaskPendingOp
	TargetIngestPendingOp  *types.TaskPendingOp
}

// MoveResult tells the caller which durable queues need a best-effort wake-up
// and preserves the endpoint tombstone states observed under the coordinator's
// locks. A false TargetIngestPersisted is expected while a reparse move is
// pending; normal document post-processing will persist its target ingest
// after the new parse generation reaches Completed.
type MoveResult struct {
	SourceRetractPersisted     bool
	TargetIngestPersisted      bool
	TargetWorkflowBound        bool
	SourceKnowledgeBaseDeleted bool
	TargetKnowledgeBaseDeleted bool
	AlreadySettled             bool
}

type movedKnowledgeIdentity struct {
	ID                   string
	TenantID             uint64
	KnowledgeBaseID      string
	ParseStatus          string
	ProcessingGeneration string
	ProcessingOwner      string
	ProcessingWorkflowID string
	ProcessedAt          *time.Time
	ErrorMessage         string
}

type pendingPayloadIdentity struct {
	Op                   string `json:"op"`
	KnowledgeID          string `json:"knowledge_id"`
	ProcessingGeneration string `json:"processing_generation,omitempty"`
}

var errMoveAlreadySettled = errors.New("wiki move already settled")

// IsMovePending checks the authoritative generation marker before callers
// quarantine source pages. The marker is cleared only by PrepareMove in the
// same transaction that persists the durable queue transition.
func (c *Coordinator) IsMovePending(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, sourceKBID, expectedMarker string,
) (bool, error) {
	if c == nil || c.db == nil || tenantID == 0 || knowledgeID == "" ||
		sourceKBID == "" || expectedMarker == "" {
		return false, fmt.Errorf("%w: complete move marker identity is required", ErrInvalidRequest)
	}
	var row movedKnowledgeIdentity
	err := c.db.WithContext(ctx).Table("knowledges").
		Select("id", "tenant_id", "knowledge_base_id", "parse_status", "error_message").
		Where("id = ?", knowledgeID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("inspect moved knowledge marker: %w", err)
	}
	if row.TenantID != tenantID {
		return false, fmt.Errorf("%w: moved knowledge tenant mismatch", ErrKnowledgeIdentity)
	}
	if row.ErrorMessage != expectedMarker {
		return false, nil
	}
	if row.KnowledgeBaseID == sourceKBID {
		return false, fmt.Errorf("%w: knowledge %q still belongs to source KB %q", ErrKnowledgeIdentity, knowledgeID, sourceKBID)
	}
	return true, nil
}

// PrepareMove atomically replaces obsolete source/target ingest work with one
// exact source retract and, when the authoritative target row is Completed and
// Wiki-enabled, one fresh target ingest. Retract uses the partial unique index
// from migration 000071; target ingest is delete+insert under the knowledge row
// lock so retries remain bounded to one current row.
//
// A row that has since moved on to a third KB still authorizes cleanup of the
// original source, but it does not enqueue work for the now-stale target. A
// missing row or a different/cleared marker is already settled and performs no
// queue mutation. A row still in the source rejects the whole transaction.
func (c *Coordinator) PrepareMove(ctx context.Context, request MoveRequest) (MoveResult, error) {
	var result MoveResult
	if c == nil || c.db == nil {
		return result, fmt.Errorf("%w: nil database", ErrInvalidRequest)
	}
	if err := validateMoveRequest(request); err != nil {
		return result, err
	}

	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		sourceDeleted, targetDeleted, err := lockKnowledgeBasesForWikiMove(
			tx,
			request.TenantID,
			request.SourceKnowledgeBaseID,
			request.TargetKnowledgeBaseID,
		)
		if err != nil {
			return fmt.Errorf("%w: lock Wiki move parent knowledge bases: %w", ErrKnowledgeIdentity, err)
		}
		result.SourceKnowledgeBaseDeleted = sourceDeleted
		result.TargetKnowledgeBaseDeleted = targetDeleted

		settle := func(tx *gorm.DB) error {
			query := tx.Table("knowledges").
				Select("id", "tenant_id", "knowledge_base_id", "parse_status", "processing_generation", "processing_owner", "processing_workflow_id", "processed_at", "error_message").
				Where("id = ?", request.KnowledgeID)
			if tx.Dialector.Name() != "sqlite" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE"})
			}

			var row movedKnowledgeIdentity
			lookup := query.Take(&row)
			if lookup.Error != nil && !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("lock moved knowledge row: %w", lookup.Error)
			}
			if errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
				return errMoveAlreadySettled
			}
			if row.TenantID != request.TenantID {
				return fmt.Errorf(
					"%w: moved knowledge %q belongs to tenant=%d, request has tenant=%d",
					ErrKnowledgeIdentity, row.ID, row.TenantID, request.TenantID,
				)
			}
			if row.ErrorMessage != request.ExpectedMarker {
				// A workflow transition may already hold the workflow lock. Returning
				// a sentinel (instead of committing a no-op) rolls that whole binding
				// attempt back when the move marker changed concurrently.
				return errMoveAlreadySettled
			}
			if row.KnowledgeBaseID == request.SourceKnowledgeBaseID {
				return fmt.Errorf(
					"%w: knowledge %q still belongs to source KB %q",
					ErrKnowledgeIdentity, row.ID, row.KnowledgeBaseID,
				)
			}

			// Deleting obsolete ingest rows is safe for both active and tombstoned
			// endpoints and prevents a stale pre-move operation from surviving this
			// exact settlement. Creation below is strictly active-endpoint-only.
			if err := deleteMoveIngest(
				tx, request.TenantID, request.SourceKnowledgeBaseID, request.KnowledgeID,
			); err != nil {
				return fmt.Errorf("delete obsolete source Wiki ingest: %w", err)
			}
			if err := deleteMoveIngest(
				tx, request.TenantID, request.TargetKnowledgeBaseID, request.KnowledgeID,
			); err != nil {
				return fmt.Errorf("delete obsolete target Wiki ingest: %w", err)
			}

			if sourceDeleted {
				// KB-delete recovery no longer scans a tombstoned scope. Consume any
				// exact stale retract left by an older delivery so clearing the move
				// marker cannot strand an unwakeable queue row.
				if err := deleteMoveRetract(
					tx, request.TenantID, request.SourceKnowledgeBaseID, request.KnowledgeID,
				); err != nil {
					return fmt.Errorf("delete obsolete source Wiki retract: %w", err)
				}
			} else {
				if err := ensureExactRetract(tx, Request{
					TenantID:        request.TenantID,
					KnowledgeID:     request.KnowledgeID,
					KnowledgeBaseID: request.SourceKnowledgeBaseID,
					PendingOp:       request.SourceRetractPendingOp,
				}); err != nil {
					return err
				}
				result.SourceRetractPersisted = true
			}

			targetIsCurrentAndReady := row.KnowledgeBaseID == request.TargetKnowledgeBaseID &&
				row.ParseStatus == types.ParseStatusCompleted && row.ProcessedAt != nil
			if !targetDeleted && request.TargetWikiEnabled && targetIsCurrentAndReady {
				var identity pendingPayloadIdentity
				if err := json.Unmarshal(request.TargetIngestPendingOp.Payload, &identity); err != nil {
					return fmt.Errorf("decode target Wiki ingest identity: %w", err)
				}
				if identity.ProcessingGeneration == "" || identity.ProcessingGeneration != row.ProcessingGeneration {
					return fmt.Errorf(
						"%w: target Wiki ingest generation %q does not match authoritative %q",
						ErrKnowledgeIdentity, identity.ProcessingGeneration, row.ProcessingGeneration,
					)
				}
				op := *request.TargetIngestPendingOp
				op.Payload = append([]byte(nil), request.TargetIngestPendingOp.Payload...)
				if op.EnqueuedAt.IsZero() {
					op.EnqueuedAt = time.Now().UTC()
				}
				if err := tx.Create(&op).Error; err != nil {
					return fmt.Errorf("insert target Wiki ingest for knowledge %q: %w", request.KnowledgeID, err)
				}
				result.TargetIngestPersisted = true
			}

			targetIsCurrentAndPending := row.KnowledgeBaseID == request.TargetKnowledgeBaseID &&
				row.ParseStatus == types.ParseStatusPending
			updates := map[string]interface{}{
				"error_message": "",
				"updated_at":    time.Now().UTC(),
			}
			if !targetDeleted && targetIsCurrentAndPending {
				if request.TargetProcessingWorkflowID == "" ||
					request.ExpectedProcessingGeneration == "" || request.ExpectedProcessingOwner == "" {
					return fmt.Errorf(
						"%w: pending target reparse requires an exact durable workflow binding",
						ErrInvalidRequest,
					)
				}
				if row.ProcessingGeneration != request.ExpectedProcessingGeneration ||
					row.ProcessingOwner != request.ExpectedProcessingOwner ||
					(row.ProcessingWorkflowID != "" && row.ProcessingWorkflowID != request.TargetProcessingWorkflowID) {
					return fmt.Errorf(
						"%w: target reparse generation changed before Wiki settlement",
						ErrKnowledgeIdentity,
					)
				}
				updates["processing_workflow_id"] = request.TargetProcessingWorkflowID
				result.TargetWorkflowBound = true
			} else if request.TargetProcessingWorkflowID != "" && !targetDeleted {
				return fmt.Errorf(
					"%w: durable target workflow cannot bind to knowledge status %q in KB %q",
					ErrKnowledgeIdentity, row.ParseStatus, row.KnowledgeBaseID,
				)
			}

			clear := tx.Table("knowledges").
				Where("id = ? AND tenant_id = ? AND error_message = ?",
					request.KnowledgeID, request.TenantID, request.ExpectedMarker).
				Updates(updates)
			if clear.Error != nil {
				return fmt.Errorf("clear moved knowledge Wiki marker: %w", clear.Error)
			}
			if clear.RowsAffected != 1 {
				return fmt.Errorf("%w: moved knowledge Wiki marker changed during settlement", ErrKnowledgeIdentity)
			}
			return nil
		}

		if request.TargetProcessingWorkflowID != "" && !targetDeleted {
			if request.BindTargetWorkflowTx == nil {
				return fmt.Errorf("%w: target workflow transaction is unavailable", ErrInvalidRequest)
			}
			// Parent KB locks precede all child writes. Inside the document queue
			// primitive the order is workflow -> knowledge, matching Abort and
			// activation and preventing a split business/workflow commit.
			return request.BindTargetWorkflowTx(tx, settle)
		}
		return settle(tx)
	})
	if err != nil {
		if errors.Is(err, errMoveAlreadySettled) {
			result.AlreadySettled = true
			return result, nil
		}
		return MoveResult{}, err
	}
	return result, nil
}

func validateMoveRequest(request MoveRequest) error {
	if request.TenantID == 0 || request.KnowledgeID == "" ||
		request.SourceKnowledgeBaseID == "" || request.TargetKnowledgeBaseID == "" ||
		request.SourceKnowledgeBaseID == request.TargetKnowledgeBaseID || request.ExpectedMarker == "" {
		return fmt.Errorf("%w: complete, distinct move identity is required", ErrInvalidRequest)
	}
	workflowFields := 0
	for _, value := range []string{
		request.TargetProcessingWorkflowID,
		request.ExpectedProcessingGeneration,
		request.ExpectedProcessingOwner,
	} {
		if value != "" {
			workflowFields++
		}
	}
	if workflowFields != 0 && workflowFields != 3 {
		return fmt.Errorf("%w: target workflow binding identity is incomplete", ErrInvalidRequest)
	}
	if workflowFields == 3 && request.BindTargetWorkflowTx == nil {
		return fmt.Errorf("%w: target workflow transactional binder is required", ErrInvalidRequest)
	}
	if err := validateMovePendingOp(
		request.SourceRetractPendingOp,
		request.TenantID,
		request.SourceKnowledgeBaseID,
		request.KnowledgeID,
		wikiOpRetract,
	); err != nil {
		return fmt.Errorf("%w: invalid source retract: %v", ErrInvalidRequest, err)
	}
	if err := validateMovePendingOp(
		request.TargetIngestPendingOp,
		request.TenantID,
		request.TargetKnowledgeBaseID,
		request.KnowledgeID,
		wikiOpIngest,
	); err != nil {
		return fmt.Errorf("%w: invalid target ingest: %v", ErrInvalidRequest, err)
	}
	return nil
}

func validateMovePendingOp(
	op *types.TaskPendingOp,
	tenantID uint64,
	kbID, knowledgeID, operation string,
) error {
	if op == nil || op.ID != 0 || op.TenantID != tenantID ||
		op.TaskType != types.TypeWikiIngest || op.Scope != types.TaskScopeKnowledgeBase ||
		op.ScopeID != kbID || op.Op != operation {
		return fmt.Errorf("queue identity does not match move")
	}
	if len(op.Payload) == 0 || !json.Valid(op.Payload) {
		return fmt.Errorf("payload must be valid JSON")
	}
	var identity pendingPayloadIdentity
	if err := json.Unmarshal(op.Payload, &identity); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if identity.Op != operation || identity.KnowledgeID != knowledgeID {
		return fmt.Errorf(
			"payload identity op=%q knowledge=%q does not match move",
			identity.Op, identity.KnowledgeID,
		)
	}
	expectedDedupKey := knowledgeID
	if operation == wikiOpIngest {
		var err error
		expectedDedupKey, err = wikiqueue.IngestDedupKey(knowledgeID, identity.ProcessingGeneration)
		if err != nil {
			return fmt.Errorf("invalid ingest generation: %w", err)
		}
	}
	if op.DedupKey != expectedDedupKey {
		return fmt.Errorf("queue dedup identity does not match move")
	}
	return nil
}

func deleteMoveIngest(tx *gorm.DB, tenantID uint64, kbID, knowledgeID string) error {
	dedupPrefix, err := wikiqueue.IngestDedupPrefix(knowledgeID)
	if err != nil {
		return err
	}
	return tx.Where(
		`tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key LIKE ? ESCAPE '\'`,
		tenantID,
		types.TypeWikiIngest,
		types.TaskScopeKnowledgeBase,
		kbID,
		wikiOpIngest,
		dedupPrefix+"%",
	).Delete(&types.TaskPendingOp{}).Error
}

func deleteMoveRetract(tx *gorm.DB, tenantID uint64, kbID, knowledgeID string) error {
	return tx.Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
		tenantID,
		types.TypeWikiIngest,
		types.TaskScopeKnowledgeBase,
		kbID,
		wikiOpRetract,
		knowledgeID,
	).Delete(&types.TaskPendingOp{}).Error
}
