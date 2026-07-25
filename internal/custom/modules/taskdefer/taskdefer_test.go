package taskdefer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/types"
)

type deferTestEnqueuer struct {
	mu       sync.Mutex
	tasks    []*asynq.Task
	options  [][]asynq.Option
	err      error
	required bool
	complete bool
	checkErr error
}

func (e *deferTestEnqueuer) Enqueue(
	task *asynq.Task,
	opts ...asynq.Option,
) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks = append(e.tasks, task)
	e.options = append(e.options, append([]asynq.Option(nil), opts...))
	if e.err != nil {
		return nil, e.err
	}
	return &asynq.TaskInfo{ID: "accepted"}, nil
}

func (e *deferTestEnqueuer) StableTaskCompletionState(
	context.Context,
	*asynq.Task,
) (bool, bool, error) {
	return e.required, e.complete, e.checkErr
}

func summaryTask(t *testing.T) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(types.SummaryGenerationPayload{
		TenantID:             1,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "generation-1",
	})
	require.NoError(t, err)
	return asynq.NewTaskWithHeaders(
		types.TypeSummaryGeneration,
		payload,
		map[string]string{"trace": "trace-1"},
	)
}

func optionValue(opts []asynq.Option, optionType asynq.OptionType) any {
	for _, option := range opts {
		if option.Type() == optionType {
			return option.Value()
		}
	}
	return nil
}

func TestScheduleResumePersistsExactOriginalAndControlOptions(t *testing.T) {
	enqueuer := &deferTestEnqueuer{}
	original := summaryTask(t)
	require.NoError(t, scheduleResume(
		enqueuer,
		original,
		types.QueueLow,
		"summary:knowledge-1:generation-1",
		3,
		2*time.Second,
	))
	require.Len(t, enqueuer.tasks, 1)
	require.Equal(t, TypeResume, enqueuer.tasks[0].Type())
	require.Equal(t, types.QueueCritical, optionValue(enqueuer.options[0], asynq.QueueOpt))
	require.Equal(t, 2*time.Second, optionValue(enqueuer.options[0], asynq.ProcessInOpt))
	require.Equal(t, resumeMaxRetry, optionValue(enqueuer.options[0], asynq.MaxRetryOpt))
	require.NotNil(t, optionValue(enqueuer.options[0], asynq.UniqueOpt))

	var payload resumePayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &payload))
	require.Equal(t, original.Type(), payload.TaskType)
	require.Equal(t, original.Payload(), payload.TaskPayload)
	require.Equal(t, original.Headers(), payload.Headers)
	require.Equal(t, types.QueueLow, payload.Queue)
	require.Equal(t, "summary:knowledge-1:generation-1", payload.TaskID)
	require.Equal(t, 3, payload.MaxRetry)
}

func TestScheduleResumeTreatsUniqueOwnerAsDurableAcceptance(t *testing.T) {
	enqueuer := &deferTestEnqueuer{err: asynq.ErrDuplicateTask}
	require.NoError(t, scheduleResume(
		enqueuer, summaryTask(t), types.QueueLow, "stable-id", 3, time.Second,
	))

	queueErr := errors.New("redis unavailable")
	enqueuer.err = queueErr
	require.ErrorIs(t, scheduleResume(
		enqueuer, summaryTask(t), types.QueueLow, "stable-id", 3, time.Second,
	), queueErr)
}

func TestResumeHandlerWaitsForLiveOriginalAndStopsAfterDurableCompletion(t *testing.T) {
	original := summaryTask(t)
	payloadBytes, err := json.Marshal(resumePayload{
		Version:     resumePayloadVersion,
		TaskType:    original.Type(),
		TaskPayload: original.Payload(),
		Headers:     original.Headers(),
		Queue:       types.QueueLow,
		TaskID:      "stable-id",
		MaxRetry:    3,
	})
	require.NoError(t, err)
	wake := asynq.NewTask(TypeResume, payloadBytes)

	enqueuer := &deferTestEnqueuer{
		err:      asynq.ErrTaskIDConflict,
		required: true,
	}
	err = NewHandler(enqueuer).Handle(context.Background(), wake)
	require.ErrorIs(t, err, ErrOriginalTaskLive)

	enqueuer.complete = true
	require.NoError(t, NewHandler(enqueuer).Handle(context.Background(), wake))

	enqueuer.complete = false
	enqueuer.err = nil
	require.NoError(t, NewHandler(enqueuer).Handle(context.Background(), wake))
	require.Equal(t, original.Type(), enqueuer.tasks[len(enqueuer.tasks)-1].Type())
}

func TestResumeHandlerFailsClosedWithoutCompletionProof(t *testing.T) {
	original := summaryTask(t)
	payloadBytes, err := json.Marshal(resumePayload{
		Version:     resumePayloadVersion,
		TaskType:    original.Type(),
		TaskPayload: original.Payload(),
		Queue:       types.QueueLow,
		TaskID:      "stable-id",
		MaxRetry:    3,
	})
	require.NoError(t, err)
	enqueuer := &deferTestEnqueuer{err: asynq.ErrTaskIDConflict}
	require.ErrorIs(t,
		NewHandler(enqueuer).Handle(
			context.Background(),
			asynq.NewTask(TypeResume, payloadBytes),
		),
		ErrOriginalTaskLive,
	)
}

func TestOnlyGenerationModelLeavesAreEligible(t *testing.T) {
	for _, taskType := range []string{
		types.TypeChunkExtract,
		types.TypeQuestionGeneration,
		types.TypeSummaryGeneration,
		types.TypeImageMultimodal,
		types.TypeDataTableSummary,
	} {
		require.True(t, replayableModelLeaf(taskType), taskType)
	}
	for _, taskType := range []string{
		types.TypeDocumentProcess,
		types.TypeKnowledgePostProcess,
		types.TypeWikiIngest,
		TypeResume,
	} {
		require.False(t, replayableModelLeaf(taskType), taskType)
	}
}

func TestProviderAndCancelDelaysAreRecognized(t *testing.T) {
	provider := &modeladmission.ProviderUnavailableError{
		Kind:       modeladmission.KindChat,
		RetryAfter: 7 * time.Second,
		Cause:      errors.New("connection reset"),
	}
	require.True(t, modeladmission.IsModelWorkDeferred(provider))
	delay, ok := RetryDelay(ErrOriginalTaskLive)
	require.True(t, ok)
	require.Equal(t, resumeConflictDelay, delay)
	_, ok = RetryDelay(context.Canceled)
	require.False(t, ok)
}
