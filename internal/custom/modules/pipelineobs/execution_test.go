package pipelineobs

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

func TestExecutionContextRoundTrip(t *testing.T) {
	ctx := WithExecution(context.Background(), "worker-a", "boot-a", "chunk:extract")
	execution, ok := ExecutionFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "worker-a", execution.InstanceID)
	require.Equal(t, "boot-a", execution.BootID)
	require.Equal(t, "chunk:extract", execution.TaskType)
}

func TestDocumentTaskStageUsesBoundedLabels(t *testing.T) {
	require.Equal(t, "document", DocumentTaskStage("document:process"))
	require.Equal(t, "split_part", DocumentTaskStage("document:split_part"))
	require.Equal(t, "summary", DocumentTaskStage("summary:generation"))
	require.Equal(t, "questions", DocumentTaskStage("question:generation"))
	require.Equal(t, "graph", DocumentTaskStage("chunk:extract"))
	require.Equal(t, "wiki", DocumentTaskStage("wiki:ingest"))
	require.Equal(t, "other", DocumentTaskStage("caller-controlled-value"))
}

func TestAsynqExecutionMiddlewarePropagatesIdentityAndError(t *testing.T) {
	wantErr := errors.New("retry me")
	handler := AsynqExecutionMiddleware("worker-b", "boot-b")(
		asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			execution, ok := ExecutionFromContext(ctx)
			require.True(t, ok)
			require.Equal(t, "worker-b", execution.InstanceID)
			require.Equal(t, "boot-b", execution.BootID)
			require.Equal(t, "wiki:ingest", execution.TaskType)
			return wantErr
		}),
	)
	err := handler.ProcessTask(context.Background(), asynq.NewTask("wiki:ingest", nil))
	require.ErrorIs(t, err, wantErr)
}
