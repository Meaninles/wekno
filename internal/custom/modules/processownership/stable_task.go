package processownership

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hibiken/asynq"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// StableTaskConflictResolver is an optional producer capability used when a
// generation-scoped task ID already exists. A normal pending/active/retry
// conflict still means another worker owns the task. Implementations may,
// however, remove a terminal Redis record whose durable database completion
// proof is missing so the exact same task ID can be published again.
//
// The task is passed to let the resolver derive the generation-scoped
// completion identity from the immutable payload. This is deliberately kept
// outside the generic TaskEnqueuer interface: Lite/synchronous enqueuers do not
// retain terminal Redis task records and therefore need no revival support.
type StableTaskConflictResolver interface {
	ResolveStableTaskConflict(
		context.Context,
		*asynq.Task,
		string,
		string,
		...asynq.Option,
	) (*asynq.TaskInfo, bool, error)
}

// StableTaskCompletionChecker exposes the PostgreSQL completion proof used by
// stable-task recovery. Delayed infrastructure-resume workers use it to
// distinguish "the original task is still live" from "the original task has
// already committed its generation-scoped result" after both cases surface as
// an Asynq TaskID conflict.
type StableTaskCompletionChecker interface {
	StableTaskCompletionState(
		context.Context,
		*asynq.Task,
	) (required bool, complete bool, err error)
}

// StableTaskCompletionState asks the concrete producer for the authoritative
// generation completion state. A producer without a durable checker returns
// required=false; callers must then fail closed instead of guessing from a
// transient Redis state.
func StableTaskCompletionState(
	ctx context.Context,
	enqueuer interfaces.TaskEnqueuer,
	task *asynq.Task,
) (required bool, complete bool, err error) {
	if enqueuer == nil {
		return false, false, errors.New("stable task completion checker is unavailable")
	}
	checker, ok := enqueuer.(StableTaskCompletionChecker)
	if !ok || checker == nil {
		return false, false, nil
	}
	return checker.StableTaskCompletionState(ctx, task)
}

// EnqueueStableTask publishes a generation-scoped task and, on TaskID
// conflict, asks the concrete producer whether the retained record is a safe
// terminal record to revive. The second enqueue is intentionally allowed to
// return ErrTaskIDConflict: when several application replicas race to repair
// the same archived task, exactly one wins the stable ID and every other
// replica observes that winner.
func EnqueueStableTask(
	ctx context.Context,
	enqueuer interfaces.TaskEnqueuer,
	task *asynq.Task,
	queue string,
	taskID string,
	opts ...asynq.Option,
) (*asynq.TaskInfo, error) {
	if enqueuer == nil {
		return nil, errors.New("stable task enqueuer is unavailable")
	}
	if task == nil {
		return nil, errors.New("stable task is nil")
	}
	queue = strings.TrimSpace(queue)
	taskID = strings.TrimSpace(taskID)
	if queue == "" || taskID == "" {
		return nil, errors.New("stable task queue and task ID are required")
	}

	enqueueOptions := make([]asynq.Option, 0, len(opts)+2)
	enqueueOptions = append(enqueueOptions, opts...)
	// Append the ownership options last so a caller cannot accidentally
	// override the queue or stable identity through opts.
	enqueueOptions = append(enqueueOptions, asynq.Queue(queue), asynq.TaskID(taskID))

	info, err := enqueuer.Enqueue(task, enqueueOptions...)
	if !errors.Is(err, asynq.ErrTaskIDConflict) {
		return info, err
	}
	resolver, ok := enqueuer.(StableTaskConflictResolver)
	if !ok || resolver == nil {
		return nil, err
	}
	resolvedInfo, resolved, resolveErr := resolver.ResolveStableTaskConflict(
		ctx, task, queue, taskID, enqueueOptions...,
	)
	if resolveErr != nil {
		return nil, fmt.Errorf("resolve stable task conflict queue=%s id=%s: %w", queue, taskID, resolveErr)
	}
	if !resolved {
		return nil, err
	}
	return resolvedInfo, nil
}
