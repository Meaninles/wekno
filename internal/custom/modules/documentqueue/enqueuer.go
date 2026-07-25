package documentqueue

import (
	"context"
	"errors"
	"time"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// Enqueuer decorates the raw Asynq client. Root document tasks first cross the
// PostgreSQL outbox; every other task remains an ordinary Asynq task. This is
// the only producer-side interception point, so upload, reparse, move and
// recovery flows all share identical durability semantics.
type Enqueuer struct {
	client      *asynq.Client
	coordinator *Coordinator
}

func NewEnqueuer(client *asynq.Client, coordinator *Coordinator) interfaces.TaskEnqueuer {
	return &Enqueuer{client: client, coordinator: coordinator}
}

// PrepareDocumentWorkflow exposes the durable preparation boundary to service
// code through a narrow type assertion on the injected TaskEnqueuer. The
// returned workflow ID must be committed on the exact knowledge generation
// before Enqueue is called.
func (e *Enqueuer) PrepareDocumentWorkflow(
	ctx context.Context, task *asynq.Task, opts ...asynq.Option,
) (*Workflow, bool, error) {
	if e == nil || e.coordinator == nil {
		return nil, false, errors.New("document queue coordinator is unavailable")
	}
	if task == nil || (task.Type() != types.TypeDocumentProcess && task.Type() != types.TypeManualProcess) {
		return nil, false, errors.New("document queue preparation requires a root document task")
	}
	return e.coordinator.PrepareWorkflowWithOptions(ctx, task.Type(), task.Payload(), opts)
}

func (e *Enqueuer) AbortDocumentWorkflow(
	ctx context.Context, binding WorkflowBinding, reason string,
) error {
	if e == nil || e.coordinator == nil {
		return errors.New("document queue coordinator is unavailable")
	}
	return e.coordinator.AbortPreparedWorkflow(ctx, binding, reason)
}

func (e *Enqueuer) BindDocumentWorkflow(ctx context.Context, binding WorkflowBinding) error {
	if e == nil || e.coordinator == nil {
		return errors.New("document queue coordinator is unavailable")
	}
	return e.coordinator.BindPreparedWorkflow(ctx, binding)
}

func (e *Enqueuer) BindDocumentWorkflowTx(tx *gorm.DB, binding WorkflowBinding) error {
	if e == nil || e.coordinator == nil {
		return errors.New("document queue coordinator is unavailable")
	}
	return e.coordinator.BindPreparedWorkflowTx(tx, binding)
}

func (e *Enqueuer) BindDocumentWorkflowTransitionTx(
	tx *gorm.DB,
	binding WorkflowBinding,
	transition func(*gorm.DB) error,
) error {
	if e == nil || e.coordinator == nil {
		return errors.New("document queue coordinator is unavailable")
	}
	return e.coordinator.BindPreparedWorkflowTransitionTx(tx, binding, transition)
}

func (e *Enqueuer) CommitPreparedReparse(
	ctx context.Context,
	binding WorkflowBinding,
	transition ReparsePendingTransition,
) error {
	if e == nil || e.coordinator == nil {
		return errors.New("document queue coordinator is unavailable")
	}
	return e.coordinator.CommitPreparedReparse(ctx, binding, transition)
}

func (e *Enqueuer) CommitDocumentWorkflowCancellation(
	ctx context.Context,
	binding CancellationBinding,
	updatedAt time.Time,
) error {
	if e == nil || e.coordinator == nil {
		return errors.New("document queue coordinator is unavailable")
	}
	return e.coordinator.CommitWorkflowCancellation(ctx, binding, updatedAt)
}

func (e *Enqueuer) ActivateDocumentWorkflow(
	ctx context.Context, binding WorkflowBinding,
) (*Workflow, bool, error) {
	if e == nil || e.coordinator == nil {
		return nil, false, errors.New("document queue coordinator is unavailable")
	}
	return e.coordinator.ActivatePreparedWorkflow(ctx, binding)
}

func (e *Enqueuer) LoadDocumentWorkflow(
	ctx context.Context, binding WorkflowBinding,
) (*Workflow, error) {
	if e == nil || e.coordinator == nil {
		return nil, errors.New("document queue coordinator is unavailable")
	}
	return e.coordinator.LoadWorkflow(ctx, binding)
}

// ResumeDocumentWorkflow continues from the persisted immutable plan. It does
// not rebuild caller options or payload, so API retries and crash recovery keep
// the original tracing and producer TaskID in the plan hash.
func (e *Enqueuer) ResumeDocumentWorkflow(
	ctx context.Context, binding WorkflowBinding,
) (*asynq.TaskInfo, error) {
	if e == nil || e.coordinator == nil {
		return nil, errors.New("document queue coordinator is unavailable")
	}
	workflow, err := e.coordinator.LoadWorkflow(ctx, binding)
	if err != nil {
		return nil, err
	}
	workflow, _, err = e.coordinator.ActivatePreparedWorkflow(ctx, binding)
	if err != nil {
		return nil, err
	}
	accepted := synthesizedTaskInfo(workflow)
	switch workflow.State {
	case StateLeased, StateWaitingExternal, StateCompleted, StateFailed, StateCancelled, StateSuperseded:
		return accepted, nil
	case StateQueued:
		// Activation is the durable acceptance boundary. Publish only the
		// scheduler's current fair head; successful claims form an admission
		// chain and fill all fleet slots without mirroring a thousand-document
		// backlog into Redis.
		info, dispatchErr := e.coordinator.dispatchNextQueued(ctx)
		if dispatchErr == nil {
			if info != nil && info.ID == workflowTaskID(workflow.ID, workflow.DispatchEpoch) {
				return info, nil
			}
			return accepted, nil
		}
		if errors.Is(dispatchErr, asynq.ErrTaskIDConflict) {
			return accepted, nil
		}
		// The PostgreSQL row remains the acceptance boundary. Recovery retries
		// publication without asking the caller to reconstruct the exact plan.
		logger.Warnf(ctx,
			"[document queue] resumed delivery deferred workflow=%s knowledge=%s: %v",
			workflow.ID, workflow.KnowledgeID, dispatchErr,
		)
		return accepted, nil
	default:
		return nil, ErrStaleDelivery
	}
}

func (e *Enqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	if e == nil || e.client == nil {
		return nil, errors.New("task enqueuer is unavailable")
	}
	if task == nil || (task.Type() != types.TypeDocumentProcess && task.Type() != types.TypeManualProcess) {
		return e.client.Enqueue(task, opts...)
	}
	if e.coordinator == nil {
		return nil, errors.New("document queue coordinator is unavailable")
	}
	workflow, _, err := e.coordinator.PrepareWorkflowWithOptions(
		context.Background(), task.Type(), task.Payload(), opts,
	)
	if err != nil {
		return nil, err
	}
	binding, err := BindingForWorkflow(workflow)
	if err != nil {
		return nil, err
	}
	return e.ResumeDocumentWorkflow(context.Background(), binding)
}
