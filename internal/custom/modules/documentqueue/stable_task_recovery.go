package documentqueue

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// stableTaskRepairProcessLocks are the in-process half of the terminal-task
// repair fence. PostgreSQL advisory locks provide the cross-Pod half. Stripes
// keep repair of different documents parallel while serializing contenders
// for the same stable ID (and give SQLite tests equivalent semantics).
var stableTaskRepairProcessLocks [64]sync.Mutex

type stableTaskInspector interface {
	GetTaskInfo(string, string) (*asynq.TaskInfo, error)
	DeleteTask(string, string) error
}

type stableCompletionLedger int

const (
	stableCompletionEnrichment stableCompletionLedger = iota + 1
	stableCompletionCoreFanout
	stableCompletionPostProcess
)

type stableCompletionLocator struct {
	tenantID        uint64
	knowledgeID     string
	knowledgeBaseID string
	generation      string
	itemID          string
	activeStatuses  []string
	ledger          stableCompletionLedger
}

// ResolveStableTaskConflict atomically serializes repair of a terminal Asynq
// stable-ID record across every application Pod. It returns resolved=false for
// live ownership and for terminal tasks that already have their authoritative
// PostgreSQL completion fact.
func (e *Enqueuer) ResolveStableTaskConflict(
	ctx context.Context,
	task *asynq.Task,
	queue string,
	taskID string,
	opts ...asynq.Option,
) (*asynq.TaskInfo, bool, error) {
	if e == nil || e.coordinator == nil || e.coordinator.db == nil ||
		e.coordinator.inspector == nil || e.client == nil {
		return nil, false, nil
	}
	if task == nil {
		return nil, false, errors.New("stable task recovery requires a task")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	processLock := stableTaskRepairProcessLock(queue, taskID)
	processLock.Lock()
	defer processLock.Unlock()

	var repairedInfo *asynq.TaskInfo
	resolved := false
	err := e.coordinator.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			lockID := stableTaskAdvisoryLockID(queue, taskID)
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockID).Error; err != nil {
				return fmt.Errorf("acquire stable task repair lock: %w", err)
			}
		}

		info, inspectErr := e.coordinator.inspector.GetTaskInfo(queue, taskID)
		if inspectErr != nil {
			if errors.Is(inspectErr, asynq.ErrTaskNotFound) ||
				errors.Is(inspectErr, asynq.ErrQueueNotFound) {
				return e.republishStableTask(task, opts, &repairedInfo, &resolved)
			}
			return fmt.Errorf("inspect stable task: %w", inspectErr)
		}
		if info == nil {
			return errors.New("inspect stable task returned nil task info")
		}
		if info.Type != task.Type() {
			return fmt.Errorf(
				"stable task identity collision: queue=%s id=%s retained_type=%s requested_type=%s",
				queue, taskID, info.Type, task.Type(),
			)
		}

		switch info.State {
		case asynq.TaskStatePending,
			asynq.TaskStateActive,
			asynq.TaskStateScheduled,
			asynq.TaskStateRetry,
			asynq.TaskStateAggregating:
			// A real live owner exists. The original TaskID conflict is the
			// correct idempotent acknowledgement.
			return nil
		case asynq.TaskStateCompleted:
			required, complete, err := stableTaskCompletionProof(
				ctx, tx, info.Type, info.Payload,
			)
			if err != nil {
				return err
			}
			if !required || complete {
				return nil
			}
			logger.Warnf(ctx,
				"[stable task recovery] completed Redis task has no durable completion proof; replaying queue=%s id=%s type=%s",
				queue, taskID, info.Type,
			)
		case asynq.TaskStateArchived:
			required, complete, err := stableTaskCompletionProof(
				ctx, tx, info.Type, info.Payload,
			)
			if err != nil {
				return err
			}
			if required && complete {
				return nil
			}
			if !required && !isLeaseExpiryArchive(info.LastErr) {
				// Unknown/non-generation tasks have no PostgreSQL proof and
				// retain conservative archive semantics. Generation-scoped
				// leaves are different: an archive without an immutable outcome
				// is not a terminal fact. Replaying them is safe because every
				// handler is generation-fenced and the first durable outcome
				// wins atomically. This also repairs tasks archived by Asynq
				// before its IsFailure hook is consulted at retry=max.
				return nil
			}
			logger.Warnf(ctx,
				"[stable task recovery] reviving archived task without durable completion queue=%s id=%s type=%s retried=%d/%d last_error=%q",
				queue, taskID, info.Type, info.Retried, info.MaxRetry, info.LastErr,
			)
		default:
			return fmt.Errorf(
				"stable task %s in queue %s has unsupported state %s",
				taskID, queue, info.State.String(),
			)
		}

		if err := e.coordinator.inspector.DeleteTask(queue, taskID); err != nil &&
			!errors.Is(err, asynq.ErrTaskNotFound) &&
			!errors.Is(err, asynq.ErrQueueNotFound) {
			return fmt.Errorf("delete terminal stable task: %w", err)
		}
		return e.republishStableTask(task, opts, &repairedInfo, &resolved)
	})
	if err != nil {
		return nil, false, err
	}
	return repairedInfo, resolved, nil
}

// StableTaskCompletionState implements processownership.StableTaskCompletionChecker.
// It deliberately reads PostgreSQL rather than inferring completion from an
// Asynq terminal state.
func (e *Enqueuer) StableTaskCompletionState(
	ctx context.Context,
	task *asynq.Task,
) (required bool, complete bool, err error) {
	if e == nil || e.coordinator == nil || e.coordinator.db == nil {
		return false, false, errors.New("stable task completion database is unavailable")
	}
	if task == nil {
		return false, false, errors.New("stable task completion task is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return stableTaskCompletionProof(ctx, e.coordinator.db, task.Type(), task.Payload())
}

func (e *Enqueuer) republishStableTask(
	task *asynq.Task,
	opts []asynq.Option,
	info **asynq.TaskInfo,
	resolved *bool,
) error {
	published, err := e.client.Enqueue(task, opts...)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		// A producer that does not participate in the PostgreSQL repair lock
		// won the same stable Redis ID. Ownership is still safely restored.
		*resolved = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("republish terminal stable task: %w", err)
	}
	*info = published
	*resolved = true
	return nil
}

func stableTaskAdvisoryLockID(queue, taskID string) int64 {
	digest := sha256.Sum256([]byte(queue + "\x00" + taskID))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

func stableTaskRepairProcessLock(queue, taskID string) *sync.Mutex {
	lockID := uint64(stableTaskAdvisoryLockID(queue, taskID))
	return &stableTaskRepairProcessLocks[lockID%uint64(len(stableTaskRepairProcessLocks))]
}

func isLeaseExpiryArchive(lastErr string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(lastErr)), "task lease expired")
}

// stableTaskCompletionProof returns required=true for generation-scoped leaf
// tasks. complete=true means either the immutable completion ledger exists or
// the generation is no longer current, in which case replay would be stale.
func stableTaskCompletionProof(
	ctx context.Context,
	db *gorm.DB,
	taskType string,
	payload []byte,
) (required bool, complete bool, err error) {
	locator, err := stableTaskCompletionLocatorFor(taskType, payload)
	if err != nil {
		return false, false, err
	}
	if locator == nil {
		// Unknown/non-generation task types retain the ordinary TaskID conflict
		// semantics. All stable generation task types emitted by this pipeline
		// have an explicit locator above.
		return false, false, nil
	}

	if err := db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Select("id").
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status IN ?",
			locator.tenantID,
			locator.knowledgeID,
			locator.knowledgeBaseID,
			locator.generation,
			locator.activeStatuses,
		).
		Take(&struct{ ID string }{}).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return true, true, nil
	} else if err != nil {
		return true, false, fmt.Errorf("check stable task generation: %w", err)
	}
	// A postprocess receipt is the durable publication boundary for the
	// orchestrator itself. Counted derivative leaves are recovered by their
	// PostgreSQL work items/outcome ledgers (and Wiki by its durable pending
	// operations), so pending_subtasks_count must not invalidate this receipt.
	// Replaying the orchestrator while leaves are merely queued causes repeated
	// fan-out scans and makes one parse attempt look like many failed retries.

	var count int64
	switch locator.ledger {
	case stableCompletionEnrichment:
		err = db.WithContext(ctx).
			Model(&enrichmentoutcome.Outcome{}).
			Where(
				"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND item_id = ?",
				locator.tenantID,
				locator.knowledgeID,
				locator.knowledgeBaseID,
				locator.generation,
				locator.itemID,
			).
			Count(&count).Error
	case stableCompletionCoreFanout:
		err = db.WithContext(ctx).
			Model(&types.KnowledgeFanoutCompletion{}).
			Where(
				"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND item_id = ?",
				locator.tenantID,
				locator.knowledgeID,
				locator.knowledgeBaseID,
				locator.generation,
				locator.itemID,
			).
			Count(&count).Error
	case stableCompletionPostProcess:
		err = db.WithContext(ctx).
			Model(&types.KnowledgeFanoutCompletion{}).
			Where(
				"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND item_id = ?",
				locator.tenantID,
				locator.knowledgeID,
				locator.knowledgeBaseID,
				locator.generation,
				processownership.PostProcessCompletionItem,
			).
			Count(&count).Error
	default:
		return true, false, fmt.Errorf("unknown stable completion ledger %d", locator.ledger)
	}
	if err != nil {
		return true, false, fmt.Errorf("check stable task completion proof: %w", err)
	}
	return true, count > 0, nil
}

func stableTaskCompletionLocatorFor(
	taskType string,
	payload []byte,
) (*stableCompletionLocator, error) {
	switch taskType {
	case types.TypeKnowledgePostProcess:
		var p types.KnowledgePostProcessPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("decode postprocess completion identity: %w", err)
		}
		return newStableCompletionLocator(
			p.TenantID,
			p.KnowledgeID,
			p.KnowledgeBaseID,
			p.ProcessingGeneration,
			"postprocess",
			[]string{types.ParseStatusProcessing, types.ParseStatusFinalizing},
			stableCompletionPostProcess,
		)
	case types.TypeSummaryGeneration:
		var p types.SummaryGenerationPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("decode summary completion identity: %w", err)
		}
		return enrichmentCompletionLocator(
			p.TenantID, p.KnowledgeID, p.KnowledgeBaseID,
			p.ProcessingGeneration, "summary",
		)
	case types.TypeQuestionGeneration:
		var p types.QuestionGenerationPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("decode question completion identity: %w", err)
		}
		return enrichmentCompletionLocator(
			p.TenantID, p.KnowledgeID, p.KnowledgeBaseID,
			p.ProcessingGeneration, fmt.Sprintf("question_batch[%d]", p.BatchIndex),
		)
	case types.TypeChunkExtract:
		var p types.ExtractChunkPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("decode graph completion identity: %w", err)
		}
		return enrichmentCompletionLocator(
			p.TenantID, p.KnowledgeID, p.KnowledgeBaseID,
			p.ProcessingGeneration, fmt.Sprintf("graph_chunk[%d]", p.ChunkIndex),
		)
	case types.TypeImageMultimodal:
		var p types.ImageMultimodalPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("decode multimodal completion identity: %w", err)
		}
		return coreCompletionLocator(
			p.TenantID, p.KnowledgeID, p.KnowledgeBaseID,
			p.ProcessingGeneration, fmt.Sprintf("image:%d", p.ImageIndex),
		)
	case types.TypeDataTableSummary:
		var p types.DataTableSummaryPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("decode data-table completion identity: %w", err)
		}
		return coreCompletionLocator(
			p.TenantID, p.KnowledgeID, p.KnowledgeBaseID,
			p.ProcessingGeneration, "datatable",
		)
	default:
		return nil, nil
	}
}

func enrichmentCompletionLocator(
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	generation string,
	itemID string,
) (*stableCompletionLocator, error) {
	return newStableCompletionLocator(
		tenantID, knowledgeID, knowledgeBaseID, generation, itemID,
		[]string{types.ParseStatusFinalizing}, stableCompletionEnrichment,
	)
}

func coreCompletionLocator(
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	generation string,
	itemID string,
) (*stableCompletionLocator, error) {
	return newStableCompletionLocator(
		tenantID, knowledgeID, knowledgeBaseID, generation, itemID,
		[]string{types.ParseStatusPending, types.ParseStatusProcessing},
		stableCompletionCoreFanout,
	)
}

func newStableCompletionLocator(
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	generation string,
	itemID string,
	activeStatuses []string,
	ledger stableCompletionLedger,
) (*stableCompletionLocator, error) {
	if tenantID == 0 ||
		strings.TrimSpace(knowledgeID) == "" ||
		strings.TrimSpace(knowledgeBaseID) == "" ||
		strings.TrimSpace(generation) == "" ||
		strings.TrimSpace(itemID) == "" {
		return nil, errors.New("stable task payload has incomplete completion identity")
	}
	return &stableCompletionLocator{
		tenantID:        tenantID,
		knowledgeID:     knowledgeID,
		knowledgeBaseID: knowledgeBaseID,
		generation:      generation,
		itemID:          itemID,
		activeStatuses:  activeStatuses,
		ledger:          ledger,
	}, nil
}
