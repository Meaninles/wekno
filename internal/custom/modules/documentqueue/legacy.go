package documentqueue

import (
	"context"
	"errors"

	"github.com/hibiken/asynq"
)

// ForwardLegacyRoot moves a root document/manual delivery found on one of the
// pre-document queues into the durable document workflow queue. The legacy
// delivery may be retried or duplicated, so registration and publication both
// converge on the workflow generation's stable identity.
//
// A leased or terminal workflow means another QueueDocument delivery already
// owns, or has completed, the work. In either case the legacy copy is safe to
// acknowledge without executing its delegate on the background worker pool.
func (c *Coordinator) ForwardLegacyRoot(ctx context.Context, task *asynq.Task) error {
	if task == nil {
		return errors.New("document queue: legacy root task is nil")
	}
	workflow, _, err := c.RegisterWorkflow(ctx, task.Type(), task.Payload())
	if err != nil {
		return err
	}
	_, err = c.Dispatch(ctx, workflow)
	if errors.Is(err, asynq.ErrTaskIDConflict) ||
		errors.Is(err, ErrStaleDelivery) ||
		errors.Is(err, ErrAlreadyLeased) {
		return nil
	}
	return err
}
