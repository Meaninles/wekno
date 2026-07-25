package processownership

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"
)

type stableTaskTestEnqueuer struct {
	enqueueCalls int
	resolveCalls int
	resolveInfo  *asynq.TaskInfo
	resolved     bool
	resolveErr   error
}

func (e *stableTaskTestEnqueuer) Enqueue(
	*asynq.Task,
	...asynq.Option,
) (*asynq.TaskInfo, error) {
	e.enqueueCalls++
	return nil, asynq.ErrTaskIDConflict
}

func (e *stableTaskTestEnqueuer) ResolveStableTaskConflict(
	context.Context,
	*asynq.Task,
	string,
	string,
	...asynq.Option,
) (*asynq.TaskInfo, bool, error) {
	e.resolveCalls++
	return e.resolveInfo, e.resolved, e.resolveErr
}

func TestEnqueueStableTaskReturnsRepairWinner(t *testing.T) {
	want := &asynq.TaskInfo{ID: "stable-1", Queue: "graph"}
	enqueuer := &stableTaskTestEnqueuer{
		resolveInfo: want,
		resolved:    true,
	}
	info, err := EnqueueStableTask(
		context.Background(),
		enqueuer,
		asynq.NewTask("chunk:extract", nil),
		"graph",
		"stable-1",
	)
	if err != nil {
		t.Fatalf("EnqueueStableTask() error = %v", err)
	}
	if info != want {
		t.Fatalf("EnqueueStableTask() info = %#v, want repair result %#v", info, want)
	}
	if enqueuer.enqueueCalls != 1 || enqueuer.resolveCalls != 1 {
		t.Fatalf("calls = enqueue:%d resolve:%d, want 1/1",
			enqueuer.enqueueCalls, enqueuer.resolveCalls)
	}
}

func TestEnqueueStableTaskPreservesLiveOwnerConflict(t *testing.T) {
	enqueuer := &stableTaskTestEnqueuer{}
	_, err := EnqueueStableTask(
		context.Background(),
		enqueuer,
		asynq.NewTask("summary:generation", nil),
		"low",
		"stable-live",
	)
	if !errors.Is(err, asynq.ErrTaskIDConflict) {
		t.Fatalf("EnqueueStableTask() error = %v, want TaskID conflict", err)
	}
	if enqueuer.enqueueCalls != 1 || enqueuer.resolveCalls != 1 {
		t.Fatalf("calls = enqueue:%d resolve:%d, want 1/1",
			enqueuer.enqueueCalls, enqueuer.resolveCalls)
	}
}

func TestEnqueueStableTaskSurfacesResolverFailure(t *testing.T) {
	resolveErr := errors.New("repair lock unavailable")
	enqueuer := &stableTaskTestEnqueuer{resolveErr: resolveErr}
	_, err := EnqueueStableTask(
		context.Background(),
		enqueuer,
		asynq.NewTask("question:generation", nil),
		"question",
		"stable-error",
	)
	if !errors.Is(err, resolveErr) {
		t.Fatalf("EnqueueStableTask() error = %v, want %v", err, resolveErr)
	}
}

func TestEnqueueStableTaskWithoutResolverKeepsConflict(t *testing.T) {
	enqueuer := &fanoutTestEnqueuer{failAt: 1, err: asynq.ErrTaskIDConflict}
	_, err := EnqueueStableTask(
		context.Background(),
		enqueuer,
		asynq.NewTask("summary:generation", nil),
		"low",
		"stable-no-resolver",
	)
	if !errors.Is(err, asynq.ErrTaskIDConflict) {
		t.Fatalf("EnqueueStableTask() error = %v, want TaskID conflict", err)
	}
}
