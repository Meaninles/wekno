package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// documentWorkflowLifecycle is the deliberately narrow producer-side surface
// exposed by the durable document queue. Keeping it local avoids widening the
// generic TaskEnqueuer contract for task types which do not need an outbox.
type documentWorkflowLifecycle interface {
	PrepareDocumentWorkflow(
		context.Context, *asynq.Task, ...asynq.Option,
	) (*documentqueue.Workflow, bool, error)
	AbortDocumentWorkflow(context.Context, documentqueue.WorkflowBinding, string) error
	ResumeDocumentWorkflow(context.Context, documentqueue.WorkflowBinding) (*asynq.TaskInfo, error)
}

type documentWorkflowTransactionalBinder interface {
	BindDocumentWorkflowTx(*gorm.DB, documentqueue.WorkflowBinding) error
}

type documentWorkflowTransactionalTransitionBinder interface {
	BindDocumentWorkflowTransitionTx(
		*gorm.DB,
		documentqueue.WorkflowBinding,
		func(*gorm.DB) error,
	) error
}

type documentReparseWorkflowCommitter interface {
	CommitPreparedReparse(
		context.Context,
		documentqueue.WorkflowBinding,
		documentqueue.ReparsePendingTransition,
	) error
}

type documentWorkflowCancellationCommitter interface {
	CommitDocumentWorkflowCancellation(
		context.Context,
		documentqueue.CancellationBinding,
		time.Time,
	) error
}

// preparedDocumentWorkflow is the handle for an exact immutable task/options
// plan already persisted by the queue before a generation became ready.
type preparedDocumentWorkflow struct {
	binding documentqueue.WorkflowBinding
}

func (s *knowledgeService) prepareDocumentWorkflow(
	ctx context.Context,
	task *asynq.Task,
	opts ...asynq.Option,
) (*preparedDocumentWorkflow, error) {
	if s == nil || s.task == nil {
		return nil, errors.New("prepare document workflow: task enqueuer is unavailable")
	}
	lifecycle, ok := s.task.(documentWorkflowLifecycle)
	if !ok || lifecycle == nil {
		return nil, errors.New("prepare document workflow: durable document queue is unavailable")
	}
	workflow, _, err := lifecycle.PrepareDocumentWorkflow(ctx, task, opts...)
	if err != nil {
		return nil, fmt.Errorf("prepare document workflow: %w", err)
	}
	binding, err := documentqueue.BindingForWorkflow(workflow)
	if err != nil {
		return nil, fmt.Errorf("prepare document workflow binding: %w", err)
	}
	return &preparedDocumentWorkflow{binding: binding}, nil
}

// attachKnowledge validates the exact generation identity before placing the
// workflow ID on the row that the caller is about to commit. For creates, the
// repository commits this field and InitialTagIDs in the same transaction.
func (p *preparedDocumentWorkflow) attachKnowledge(knowledge *types.Knowledge) error {
	if p == nil || knowledge == nil {
		return errors.New("attach document workflow: dependencies are unavailable")
	}
	if p.binding.TenantID != knowledge.TenantID ||
		p.binding.KnowledgeBaseID != strings.TrimSpace(knowledge.KnowledgeBaseID) ||
		p.binding.KnowledgeID != strings.TrimSpace(knowledge.ID) ||
		p.binding.ProcessingGeneration != strings.TrimSpace(knowledge.ProcessingGeneration) ||
		p.binding.ProcessingOwner != strings.TrimSpace(knowledge.ProcessingOwner) {
		return errors.New("attach document workflow: knowledge generation identity mismatch")
	}
	knowledge.ProcessingWorkflowID = p.binding.WorkflowID
	return nil
}

func documentWorkflowBindingForKnowledge(
	knowledge *types.Knowledge,
) (documentqueue.WorkflowBinding, error) {
	if knowledge == nil {
		return documentqueue.WorkflowBinding{}, errors.New("document workflow binding: knowledge is nil")
	}
	binding := documentqueue.WorkflowBinding{
		WorkflowID:           strings.TrimSpace(knowledge.ProcessingWorkflowID),
		TenantID:             knowledge.TenantID,
		KnowledgeBaseID:      strings.TrimSpace(knowledge.KnowledgeBaseID),
		KnowledgeID:          strings.TrimSpace(knowledge.ID),
		ProcessingGeneration: strings.TrimSpace(knowledge.ProcessingGeneration),
		ProcessingOwner:      strings.TrimSpace(knowledge.ProcessingOwner),
	}
	if binding.WorkflowID == "" || binding.KnowledgeBaseID == "" || binding.KnowledgeID == "" ||
		binding.ProcessingGeneration == "" || binding.ProcessingOwner == "" {
		return documentqueue.WorkflowBinding{}, errors.New("document workflow binding is incomplete")
	}
	return binding, nil
}

func documentCancellationBindingForKnowledge(
	knowledge *types.Knowledge,
) (documentqueue.CancellationBinding, error) {
	if knowledge == nil {
		return documentqueue.CancellationBinding{}, errors.New("document cancellation binding: knowledge is nil")
	}
	binding := documentqueue.CancellationBinding{
		WorkflowID:           strings.TrimSpace(knowledge.ProcessingWorkflowID),
		TenantID:             knowledge.TenantID,
		KnowledgeBaseID:      strings.TrimSpace(knowledge.KnowledgeBaseID),
		KnowledgeID:          strings.TrimSpace(knowledge.ID),
		ProcessingGeneration: strings.TrimSpace(knowledge.ProcessingGeneration),
	}
	if binding.WorkflowID == "" || binding.KnowledgeBaseID == "" ||
		binding.KnowledgeID == "" || binding.ProcessingGeneration == "" {
		return documentqueue.CancellationBinding{}, errors.New("document cancellation binding is incomplete")
	}
	return binding, nil
}

// resumeBoundDocumentWorkflow never rebuilds a task from mutable KB state or
// injects new tracing fields. It resumes the already-persisted immutable plan.
func (s *knowledgeService) resumeBoundDocumentWorkflow(
	ctx context.Context,
	knowledge *types.Knowledge,
) error {
	if s == nil || s.task == nil {
		return errors.New("resume document workflow: task enqueuer is unavailable")
	}
	lifecycle, ok := s.task.(documentWorkflowLifecycle)
	if !ok || lifecycle == nil {
		return errors.New("resume document workflow: durable document queue is unavailable")
	}
	binding, err := documentWorkflowBindingForKnowledge(knowledge)
	if err != nil {
		return err
	}
	if _, err := lifecycle.ResumeDocumentWorkflow(ctx, binding); err != nil {
		return fmt.Errorf("resume bound document workflow: %w", err)
	}
	return nil
}

// bindPreparedDocumentWorkflowTx lets lifecycle coordinators such as move
// make their final Pending transition and workflow binding in one transaction.
func (s *knowledgeService) bindPreparedDocumentWorkflowTx(
	tx *gorm.DB,
	prepared *preparedDocumentWorkflow,
) error {
	if s == nil || s.task == nil || tx == nil || prepared == nil {
		return errors.New("bind prepared document workflow: dependencies are unavailable")
	}
	binder, ok := s.task.(documentWorkflowTransactionalBinder)
	if !ok || binder == nil {
		return errors.New("bind prepared document workflow: transactional binder is unavailable")
	}
	return binder.BindDocumentWorkflowTx(tx, prepared.binding)
}

// bindPreparedDocumentWorkflowTransitionTx holds the prepared workflow row
// lock across the caller's business transition and validates the resulting
// exact Pending knowledge binding before the transaction can commit.
func (s *knowledgeService) bindPreparedDocumentWorkflowTransitionTx(
	tx *gorm.DB,
	prepared *preparedDocumentWorkflow,
	transition func(*gorm.DB) error,
) error {
	if s == nil || s.task == nil || tx == nil || prepared == nil || transition == nil {
		return errors.New("bind prepared document workflow transition: dependencies are unavailable")
	}
	binder, ok := s.task.(documentWorkflowTransactionalTransitionBinder)
	if !ok || binder == nil {
		return errors.New("bind prepared document workflow transition: transactional binder is unavailable")
	}
	return binder.BindDocumentWorkflowTransitionTx(tx, prepared.binding, transition)
}

// commitPreparedReparseWorkflow is the only producer-side transition from a
// claimed reparse generation to Pending. The custom queue coordinator owns the
// database transaction so the Pending state and processing_workflow_id can
// never become visible independently.
func (s *knowledgeService) commitPreparedReparseWorkflow(
	ctx context.Context,
	prepared *preparedDocumentWorkflow,
	knowledge *types.Knowledge,
	embeddingModelID string,
	errorMessage string,
	updatedAt time.Time,
) error {
	if s == nil || s.task == nil || prepared == nil || knowledge == nil {
		return errors.New("commit prepared reparse workflow: dependencies are unavailable")
	}
	committer, ok := s.task.(documentReparseWorkflowCommitter)
	if !ok || committer == nil {
		return errors.New("commit prepared reparse workflow: atomic committer is unavailable")
	}
	if err := prepared.attachKnowledge(knowledge); err != nil {
		return err
	}
	if err := committer.CommitPreparedReparse(
		ctx,
		prepared.binding,
		documentqueue.ReparsePendingTransition{
			EmbeddingModelID: embeddingModelID,
			ErrorMessage:     errorMessage,
			UpdatedAt:        updatedAt,
		},
	); err != nil {
		return fmt.Errorf("commit prepared reparse workflow: %w", err)
	}
	return nil
}

// commitDocumentWorkflowCancellation publishes the terminal business state
// together with the exact durable queue row. The boolean is false only for
// non-document/legacy test enqueuers which do not expose the durable
// coordinator; production document workflows always take the atomic path.
func (s *knowledgeService) commitDocumentWorkflowCancellation(
	ctx context.Context,
	knowledge *types.Knowledge,
	updatedAt time.Time,
) (bool, error) {
	if s == nil || s.task == nil || knowledge == nil ||
		strings.TrimSpace(knowledge.ProcessingWorkflowID) == "" {
		return false, nil
	}
	committer, ok := s.task.(documentWorkflowCancellationCommitter)
	if !ok || committer == nil {
		return false, nil
	}
	binding, err := documentCancellationBindingForKnowledge(knowledge)
	if err != nil {
		return true, err
	}
	if err := committer.CommitDocumentWorkflowCancellation(
		ctx, binding, updatedAt,
	); err != nil {
		return true, fmt.Errorf("commit document workflow cancellation: %w", err)
	}
	return true, nil
}

// abortUnboundDocumentWorkflow is used only when the business transaction did
// not commit. The coordinator independently verifies that no knowledge row is
// bound before cancelling, which also makes ambiguous commit outcomes safe.
func (s *knowledgeService) abortUnboundDocumentWorkflow(
	ctx context.Context,
	prepared *preparedDocumentWorkflow,
	reason string,
) {
	if s == nil || prepared == nil || s.task == nil {
		return
	}
	lifecycle, ok := s.task.(documentWorkflowLifecycle)
	if !ok || lifecycle == nil {
		return
	}
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := lifecycle.AbortDocumentWorkflow(abortCtx, prepared.binding, reason); err != nil {
		logger.Warnf(abortCtx,
			"Failed to abort unbound document workflow %s for knowledge %s: %v",
			prepared.binding.WorkflowID, prepared.binding.KnowledgeID, err,
		)
	}
}

// handleUncommittedReparseWorkflow preserves the one immutable preparation
// owned by a deterministic batch child. That child retries the same generation
// and therefore must be able to bind the same Preparing row later. Standalone
// reparse calls may discard an unbound preparation because their next request
// allocates a fresh generation. AbortDocumentWorkflow independently refuses an
// ambiguously committed binding.
func (s *knowledgeService) handleUncommittedReparseWorkflow(
	ctx context.Context,
	prepared *preparedDocumentWorkflow,
	stableChild bool,
) {
	if stableChild {
		return
	}
	s.abortUnboundDocumentWorkflow(ctx, prepared, "reparse Pending transaction failed")
}

// dispatchPreparedDocumentWorkflow activates and publishes the exact plan.
// Once the business binding has committed, an error here must not roll the
// knowledge back or mark it failed: the durable recovery loop owns activation
// and Redis publication retries from that point onward.
func (s *knowledgeService) dispatchPreparedDocumentWorkflow(
	ctx context.Context,
	prepared *preparedDocumentWorkflow,
) {
	if s == nil || prepared == nil || s.task == nil {
		return
	}
	lifecycle, ok := s.task.(documentWorkflowLifecycle)
	if !ok || lifecycle == nil {
		logger.Warnf(ctx,
			"Document workflow activation deferred: durable queue unavailable workflow=%s knowledge=%s",
			prepared.binding.WorkflowID, prepared.binding.KnowledgeID,
		)
		return
	}
	if _, err := lifecycle.ResumeDocumentWorkflow(ctx, prepared.binding); err != nil {
		logger.Warnf(ctx,
			"Document workflow activation deferred to recovery: workflow=%s knowledge=%s error=%v",
			prepared.binding.WorkflowID, prepared.binding.KnowledgeID, err,
		)
	}
}
