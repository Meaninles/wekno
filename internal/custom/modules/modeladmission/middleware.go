package modeladmission

import (
	"context"

	"github.com/hibiken/asynq"
)

// AsynqMiddleware classifies all durable worker traffic as background work.
// Interactive HTTP chat remains unmarked and can use the reserved capacity.
func AsynqMiddleware() asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			ctx = WithProviderExecutionTracking(WithBackground(ctx))
			return next.ProcessTask(ctx, task)
		})
	}
}

func BackgroundTaskHandler(
	handler func(context.Context, *asynq.Task) error,
) func(context.Context, *asynq.Task) error {
	return func(ctx context.Context, task *asynq.Task) error {
		ctx = WithProviderExecutionTracking(WithBackground(ctx))
		return handler(ctx, task)
	}
}
