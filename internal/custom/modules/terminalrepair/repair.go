// Package terminalrepair provides a generation-fenced recovery lane for task
// terminalization. An exhausted business task gets one immediate repair
// attempt; if the database is unavailable, a dedicated Asynq task with a
// stable ID and a high retry budget persists the repair independently of the
// already-exhausted original task.
package terminalrepair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
)

const (
	maxRetry = 25
	timeout  = 2 * time.Minute
)

type documentGenerationRepository interface {
	FailDocumentProcessingGeneration(
		context.Context, uint64, string, string, string, string, map[string]interface{},
	) (bool, error)
}

type postProcessGenerationRepository interface {
	CompletePostProcessDeadLetterGeneration(
		context.Context, uint64, string, string, string,
	) (bool, error)
}

type enrichmentGenerationRepository interface {
	FinalizeSubtaskGenerationItem(
		context.Context, uint64, string, string, string, string,
	) (int, bool, error)
}

// knowledgeMoveRepairer is implemented by the application knowledge service.
// Knowledge-move payloads describe a batch rather than one document generation,
// so they must bypass decodeIdentity and let the move service inspect each
// document's persisted, generation-fenced recovery marker.
type knowledgeMoveRepairer interface {
	RepairKnowledgeMoveDeadLetter(context.Context, *asynq.Task, error) error
}

// AttemptFinalizer is the small portion of the span tracker used after a
// terminal lifecycle repair. Defining the contract here avoids importing the
// application service package (and therefore avoids an import cycle).
type AttemptFinalizer interface {
	FinalizeAttempt(
		ctx context.Context,
		knowledgeID string,
		attempt int,
		status string,
		output types.JSONMap,
		errorCode, errorMessage string,
	)
}

// Service repairs the exact lifecycle/fanout identity carried by an exhausted
// task. Every repository operation is generation fenced and idempotent.
type Service struct {
	repo        interfaces.KnowledgeRepository
	enqueuer    interfaces.TaskEnqueuer
	tracker     AttemptFinalizer
	document    documentGenerationRepository
	postProcess postProcessGenerationRepository
	enrichment  enrichmentGenerationRepository
	fanout      processownership.DurableFanoutCompletionStore
	move        knowledgeMoveRepairer
}

func New(
	repo interfaces.KnowledgeRepository,
	enqueuer interfaces.TaskEnqueuer,
	tracker AttemptFinalizer,
) *Service {
	s := &Service{repo: repo, enqueuer: enqueuer, tracker: tracker}
	s.document, _ = repo.(documentGenerationRepository)
	s.postProcess, _ = repo.(postProcessGenerationRepository)
	s.enrichment, _ = repo.(enrichmentGenerationRepository)
	s.fanout, _ = repo.(processownership.DurableFanoutCompletionStore)
	return s
}

// SetKnowledgeMoveRepairer installs the narrow move-recovery capability without
// importing application/service (which would create an import cycle). The
// parameter is intentionally dynamic because interfaces.KnowledgeService does
// not expose this internal dead-letter hook.
func (s *Service) SetKnowledgeMoveRepairer(candidate interface{}) {
	if s == nil {
		return
	}
	s.move, _ = candidate.(knowledgeMoveRepairer)
}

type identity struct {
	types.TracingContext
	TenantID             uint64   `json:"tenant_id,omitempty"`
	KnowledgeID          string   `json:"knowledge_id,omitempty"`
	KnowledgeBaseID      string   `json:"knowledge_base_id,omitempty"`
	ProcessingGeneration string   `json:"processing_generation,omitempty"`
	ProcessingOwner      string   `json:"processing_owner,omitempty"`
	Attempt              int      `json:"attempt,omitempty"`
	BatchIndex           int      `json:"batch_index,omitempty"`
	ChunkID              string   `json:"chunk_id,omitempty"`
	ChunkIDs             []string `json:"chunk_ids,omitempty"`
	ChunkIndex           int      `json:"chunk_index,omitempty"`
	ImageIndex           int      `json:"image_index,omitempty"`
	Language             string   `json:"language,omitempty"`
}

func decodeIdentity(task *asynq.Task) (identity, error) {
	var id identity
	if task == nil {
		return id, errors.New("terminal repair: nil original task")
	}
	if err := json.Unmarshal(task.Payload(), &id); err != nil {
		return id, fmt.Errorf("terminal repair: decode %s payload: %w", task.Type(), err)
	}
	if id.TenantID == 0 || strings.TrimSpace(id.KnowledgeID) == "" ||
		strings.TrimSpace(id.KnowledgeBaseID) == "" ||
		strings.TrimSpace(id.ProcessingGeneration) == "" {
		return id, fmt.Errorf("terminal repair: incomplete fenced identity for %s", task.Type())
	}
	return id, nil
}

// Repair immediately applies the task-type-specific terminal transition.
// A nil error includes stale-generation no-ops: another lifecycle owner has
// already won, so no repair is required.
func (s *Service) Repair(ctx context.Context, task *asynq.Task, taskErr error) error {
	if s == nil || task == nil {
		return nil
	}
	if task.Type() == types.TypeKnowledgeTerminalRepair {
		return nil
	}
	if !RepairableTaskType(task.Type()) {
		return nil
	}
	errText := "unknown task error"
	if taskErr != nil {
		errText = taskErr.Error()
	}
	if len(errText) > 8192 {
		errText = errText[:8192]
	}
	if task.Type() == types.TypeKnowledgeMove {
		if s.move == nil {
			return errors.New("terminal repair: knowledge move repairer is unavailable")
		}
		return s.move.RepairKnowledgeMoveDeadLetter(ctx, task, taskErr)
	}
	if task.Type() == types.TypeKnowledgeListReparse {
		return s.repairKnowledgeListReparse(ctx, task, errText)
	}
	id, err := decodeIdentity(task)
	if err != nil {
		return err
	}

	switch task.Type() {
	case types.TypeDocumentProcess, types.TypeManualProcess:
		return s.repairDocument(ctx, task.Type(), id, errText)
	case types.TypeKnowledgePostProcess:
		return s.repairPostProcess(ctx, id, errText)
	case types.TypeSummaryGeneration:
		return s.repairEnrichmentSlot(ctx, id, "summary")
	case types.TypeQuestionGeneration:
		item := "question_legacy"
		if len(id.ChunkIDs) > 0 || id.ChunkID != "" {
			item = fmt.Sprintf("question_batch[%d]", id.BatchIndex)
		}
		return s.repairEnrichmentSlot(ctx, id, item)
	case types.TypeChunkExtract:
		return s.repairEnrichmentSlot(ctx, id, fmt.Sprintf("graph_chunk[%d]", id.ChunkIndex))
	case types.TypeImageMultimodal:
		return s.repairCoreFanout(ctx, id, processownership.ImageFanoutItem(id.ImageIndex))
	case types.TypeDataTableSummary:
		return s.repairCoreFanout(ctx, id, processownership.DataTableFanoutItem())
	default:
		return nil
	}
}

// RepairableTaskType reports whether an exhausted task carries enough stable
// identity for the terminal-repair worker. Lite mode uses this before
// scheduling a repair because it does not run the Asynq dead-letter middleware.
func RepairableTaskType(taskType string) bool {
	switch taskType {
	case types.TypeDocumentProcess,
		types.TypeManualProcess,
		types.TypeKnowledgeListReparse,
		types.TypeKnowledgeMove,
		types.TypeKnowledgePostProcess,
		types.TypeSummaryGeneration,
		types.TypeQuestionGeneration,
		types.TypeChunkExtract,
		types.TypeImageMultimodal,
		types.TypeDataTableSummary:
		return true
	default:
		return false
	}
}

func (s *Service) repairKnowledgeListReparse(ctx context.Context, task *asynq.Task, errText string) error {
	var payload types.KnowledgeListReparsePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("terminal repair: decode batch reparse payload: %w", err)
	}
	// A multi-item parent owns no document row, and legacy in-flight children
	// carried no generation. Both are safe no-ops; all newly emitted children
	// contain exactly one complete stable identity.
	if len(payload.KnowledgeIDs) != 1 || payload.ProcessingGeneration == "" || payload.ProcessingOwner == "" {
		return nil
	}
	knowledgeID := strings.TrimSpace(payload.KnowledgeIDs[0])
	if payload.TenantID == 0 || knowledgeID == "" {
		return errors.New("terminal repair: incomplete batch reparse identity")
	}
	expectedGeneration, expectedOwner := processownership.BatchReparseIdentity(
		payload.TenantID, payload.BatchID, knowledgeID,
	)
	if expectedGeneration == "" || expectedGeneration != payload.ProcessingGeneration ||
		expectedOwner != payload.ProcessingOwner {
		return errors.New("terminal repair: invalid batch reparse generation owner")
	}
	if s.repo == nil || s.document == nil {
		return errors.New("terminal repair: document generation repository is unavailable")
	}
	knowledge, err := s.repo.GetKnowledgeByID(ctx, payload.TenantID, knowledgeID)
	if err != nil {
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("terminal repair: load batch reparse knowledge: %w", err)
	}
	if knowledge == nil || knowledge.TenantID != payload.TenantID ||
		knowledge.ProcessingGeneration != payload.ProcessingGeneration ||
		knowledge.ProcessingOwner != payload.ProcessingOwner {
		return nil
	}
	message := "task " + task.Type() + " exhausted retries before downstream processing: " + errText
	_, err = s.document.FailDocumentProcessingGeneration(
		ctx,
		payload.TenantID,
		knowledgeID,
		knowledge.KnowledgeBaseID,
		payload.ProcessingGeneration,
		payload.ProcessingOwner,
		map[string]interface{}{
			"parse_status":           types.ParseStatusFailed,
			"error_message":          message,
			"pending_subtasks_count": 0,
			"processing_owner":       "",
			"processing_fanout":      nil,
		},
	)
	if err != nil {
		return fmt.Errorf("terminal repair batch reparse generation: %w", err)
	}
	return nil
}

func (s *Service) repairDocument(ctx context.Context, taskType string, id identity, errText string) error {
	if s.document == nil {
		return errors.New("terminal repair: document generation repository is unavailable")
	}
	owner := strings.TrimSpace(id.ProcessingOwner)
	if owner == "" {
		return fmt.Errorf("terminal repair: missing processing owner for %s", taskType)
	}
	message := "task " + taskType + " exhausted retries: " + errText
	changed, err := s.document.FailDocumentProcessingGeneration(
		ctx, id.TenantID, id.KnowledgeID, id.KnowledgeBaseID,
		id.ProcessingGeneration, owner,
		map[string]interface{}{
			"parse_status":           types.ParseStatusFailed,
			"error_message":          message,
			"pending_subtasks_count": 0,
			"processing_owner":       "",
			"processing_fanout":      nil,
		},
	)
	if err != nil {
		return fmt.Errorf("terminal repair document generation: %w", err)
	}
	if changed && s.tracker != nil && id.Attempt > 0 {
		s.tracker.FinalizeAttempt(ctx, id.KnowledgeID, id.Attempt,
			types.SpanStatusFailed, nil, "TASK_TIMEOUT", message)
	}
	return nil
}

func (s *Service) repairPostProcess(ctx context.Context, id identity, errText string) error {
	if s.postProcess == nil {
		return errors.New("terminal repair: postprocess generation repository is unavailable")
	}
	changed, err := s.postProcess.CompletePostProcessDeadLetterGeneration(
		ctx, id.TenantID, id.KnowledgeID, id.KnowledgeBaseID, id.ProcessingGeneration,
	)
	if err != nil {
		return fmt.Errorf("terminal repair postprocess generation: %w", err)
	}
	if changed && s.tracker != nil && id.Attempt > 0 {
		s.tracker.FinalizeAttempt(ctx, id.KnowledgeID, id.Attempt,
			types.SpanStatusDone, types.JSONMap{
				"enrichment_degraded": true,
				"postprocess_error":   errText,
			}, "", "")
	}
	return nil
}

func (s *Service) repairEnrichmentSlot(ctx context.Context, id identity, item string) error {
	if s.enrichment == nil {
		return errors.New("terminal repair: enrichment generation repository is unavailable")
	}
	if _, _, err := s.enrichment.FinalizeSubtaskGenerationItem(
		ctx, id.TenantID, id.KnowledgeID, id.KnowledgeBaseID,
		id.ProcessingGeneration, item,
	); err != nil {
		return fmt.Errorf("terminal repair enrichment item %s: %w", item, err)
	}
	return nil
}

func (s *Service) repairCoreFanout(ctx context.Context, id identity, item string) error {
	if s.repo == nil || s.fanout == nil {
		return errors.New("terminal repair: durable core fanout repository is unavailable")
	}
	knowledge, err := s.repo.GetKnowledgeByID(ctx, id.TenantID, id.KnowledgeID)
	if err != nil {
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("terminal repair: load fanout generation: %w", err)
	}
	if knowledge == nil || knowledge.KnowledgeBaseID != id.KnowledgeBaseID ||
		knowledge.ProcessingGeneration != id.ProcessingGeneration ||
		(knowledge.ParseStatus != types.ParseStatusPending && knowledge.ParseStatus != types.ParseStatusProcessing) {
		return nil
	}
	plan, err := processownership.ParseFanoutPlan(knowledge.ProcessingFanout)
	if err != nil {
		return fmt.Errorf("terminal repair: parse durable fanout plan: %w", err)
	}
	remaining, _, err := processownership.CompleteDurableFanoutItem(ctx, s.fanout, nil, plan, item)
	if err != nil {
		return fmt.Errorf("terminal repair core fanout item %s: %w", item, err)
	}
	if remaining > 0 {
		return nil
	}
	if s.enqueuer == nil {
		return errors.New("terminal repair: postprocess enqueuer is unavailable")
	}
	if err := processownership.EnqueuePostProcess(s.enqueuer, types.KnowledgePostProcessPayload{
		TracingContext:       id.TracingContext,
		TenantID:             id.TenantID,
		KnowledgeID:          id.KnowledgeID,
		KnowledgeBaseID:      id.KnowledgeBaseID,
		ProcessingGeneration: id.ProcessingGeneration,
		Language:             id.Language,
		Attempt:              id.Attempt,
	}); err != nil {
		return fmt.Errorf("terminal repair: enqueue postprocess after fan-in: %w", err)
	}
	return nil
}

// Handle executes a previously persisted terminal-repair task.
func (s *Service) Handle(ctx context.Context, task *asynq.Task) error {
	var payload types.KnowledgeTerminalRepairPayload
	if task == nil {
		return errors.New("terminal repair: nil repair task")
	}
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("terminal repair: decode repair payload: %w", err)
	}
	if strings.TrimSpace(payload.OriginalTaskType) == "" || len(payload.OriginalPayload) == 0 {
		return errors.New("terminal repair: original task type and payload are required")
	}
	original := asynq.NewTask(payload.OriginalTaskType, payload.OriginalPayload)
	return s.Repair(ctx, original, errors.New(payload.LastError))
}

// Enqueue persists a dedicated repair task. The stable ID is derived from the
// immutable original type+payload; concurrent callbacks and callback retries
// therefore collapse to one repair job, and a TaskID conflict means the
// repair is already durable.
func Enqueue(enqueuer interfaces.TaskEnqueuer, original *asynq.Task, taskErr error) error {
	if enqueuer == nil {
		return errors.New("terminal repair: task enqueuer is unavailable")
	}
	if original == nil {
		return errors.New("terminal repair: nil original task")
	}
	errText := "unknown task error"
	if taskErr != nil {
		errText = taskErr.Error()
	}
	payloadBytes, err := json.Marshal(types.KnowledgeTerminalRepairPayload{
		OriginalTaskType: original.Type(),
		OriginalPayload:  append(json.RawMessage(nil), original.Payload()...),
		LastError:        errText,
	})
	if err != nil {
		return fmt.Errorf("terminal repair: encode payload: %w", err)
	}
	digest := sha256.Sum256(append(append([]byte(original.Type()), 0), original.Payload()...))
	taskID := "terminal-repair:" + hex.EncodeToString(digest[:16])
	repairTask := asynq.NewTask(types.TypeKnowledgeTerminalRepair, payloadBytes)
	_, err = enqueuer.Enqueue(
		repairTask,
		asynq.Queue(types.QueueCritical),
		asynq.MaxRetry(maxRetry),
		asynq.Timeout(timeout),
		asynq.Retention(processownership.GenerationTaskRetention),
		asynq.TaskID(taskID),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		logger.Infof(context.Background(), "terminal repair task already persisted: %s", taskID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("terminal repair: enqueue %s: %w", taskID, err)
	}
	return nil
}
