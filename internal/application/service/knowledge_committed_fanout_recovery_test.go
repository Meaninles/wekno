package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type committedFanoutKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	current  *types.Knowledge
	getErr   error
	getCalls int
}

func (r *committedFanoutKnowledgeRepo) GetKnowledgeByID(
	context.Context, uint64, string,
) (*types.Knowledge, error) {
	r.getCalls++
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.current == nil {
		return nil, nil
	}
	copyKnowledge := *r.current
	copyKnowledge.ProcessingFanout = append(types.JSON(nil), r.current.ProcessingFanout...)
	return &copyKnowledge, nil
}

func (*committedFanoutKnowledgeRepo) RecordKnowledgeFanoutCompletion(
	context.Context, uint64, string, string, string, string,
) (bool, error) {
	return false, nil
}

func (*committedFanoutKnowledgeRepo) ListKnowledgeFanoutCompletions(
	context.Context, uint64, string, string, string,
) ([]string, error) {
	return nil, nil
}

func (*committedFanoutKnowledgeRepo) CountKnowledgeFanoutCompletions(
	context.Context, uint64, string, string, string,
) (int64, error) {
	return 0, nil
}

func (*committedFanoutKnowledgeRepo) KnowledgeFanoutCompletionExists(
	context.Context, uint64, string, string, string, string,
) (bool, error) {
	return false, nil
}

var _ processownership.DurableFanoutCompletionStore = (*committedFanoutKnowledgeRepo)(nil)

type committedFanoutTenantRepo struct{ interfaces.TenantRepository }

func (*committedFanoutTenantRepo) GetTenantByID(context.Context, uint64) (*types.Tenant, error) {
	return &types.Tenant{ID: 7}, nil
}

type retryingFanoutEnqueuer struct {
	mu        sync.Mutex
	attempts  int
	taskTypes []string
	failFirst error
}

func (e *retryingFanoutEnqueuer) Enqueue(
	task *asynq.Task, _ ...asynq.Option,
) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempts++
	e.taskTypes = append(e.taskTypes, task.Type())
	if e.attempts == 1 && e.failFirst != nil {
		return nil, e.failFirst
	}
	return &asynq.TaskInfo{ID: "task", Type: task.Type(), Queue: types.QueueDefault}, nil
}

func committedFanoutKnowledge(t *testing.T, knowledgeType string) *types.Knowledge {
	t.Helper()
	processedAt := time.Now()
	knowledge := &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             7,
		KnowledgeBaseID:      "kb-1",
		Type:                 knowledgeType,
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: "generation-1",
		ProcessingOwner:      "",
		ProcessedAt:          &processedAt,
	}
	plan, err := processownership.MarshalFanoutPlan(processownership.FanoutPlan{
		Version:              processownership.FanoutPlanVersion,
		TenantID:             knowledge.TenantID,
		KnowledgeID:          knowledge.ID,
		KnowledgeBaseID:      knowledge.KnowledgeBaseID,
		ProcessingGeneration: knowledge.ProcessingGeneration,
	})
	require.NoError(t, err)
	knowledge.ProcessingFanout = types.JSON(plan)
	return knowledge
}

func manualCommittedFanoutTask(t *testing.T, needCleanup bool, attempt int) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(types.ManualProcessPayload{
		TenantID:             7,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		Content:              "manual content",
		NeedCleanup:          needCleanup,
		ProcessingGeneration: "generation-1",
		ProcessingOwner:      processownership.DocumentOwner("knowledge-1", "generation-1"),
		Attempt:              attempt,
	})
	require.NoError(t, err)
	return asynq.NewTask(types.TypeManualProcess, payload)
}

func TestProcessManualUpdateReplaysCommittedFanoutWithoutRepeatingCoreWrites(t *testing.T) {
	for _, tc := range []struct {
		name        string
		needCleanup bool
		attempt     int
	}{
		{name: "create", needCleanup: false},
		{name: "edit", needCleanup: true},
		{name: "reparse", needCleanup: true, attempt: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatchErr := errors.New("redis enqueue unavailable")
			repo := &committedFanoutKnowledgeRepo{current: committedFanoutKnowledge(t, types.KnowledgeTypeManual)}
			enqueuer := &retryingFanoutEnqueuer{failFirst: dispatchErr}
			service := &knowledgeService{
				repo:       repo,
				tenantRepo: &committedFanoutTenantRepo{},
				task:       enqueuer,
			}
			task := manualCommittedFanoutTask(t, tc.needCleanup, tc.attempt)

			firstErr := service.ProcessManualUpdate(context.Background(), task)
			require.ErrorIs(t, firstErr, dispatchErr)
			require.NoError(t, service.ProcessManualUpdate(context.Background(), task))

			assert.Equal(t, 2, repo.getCalls)
			assert.Equal(t, 2, enqueuer.attempts)
			assert.Equal(t,
				[]string{types.TypeKnowledgePostProcess, types.TypeKnowledgePostProcess},
				enqueuer.taskTypes,
			)
			// repo embeds no implementations for cleanup/update/chunk writes. If
			// the retry re-entered the core path, this test would panic instead of
			// reaching the assertions above.
		})
	}
}

func TestProcessManualUpdateCommittedFanoutFailsClosedOnPlanMismatch(t *testing.T) {
	knowledge := committedFanoutKnowledge(t, types.KnowledgeTypeManual)
	var plan processownership.FanoutPlan
	require.NoError(t, json.Unmarshal(knowledge.ProcessingFanout, &plan))
	plan.ProcessingGeneration = "different-generation"
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	knowledge.ProcessingFanout = raw

	repo := &committedFanoutKnowledgeRepo{current: knowledge}
	enqueuer := &retryingFanoutEnqueuer{}
	service := &knowledgeService{repo: repo, tenantRepo: &committedFanoutTenantRepo{}, task: enqueuer}
	err = service.ProcessManualUpdate(context.Background(), manualCommittedFanoutTask(t, false, 0))
	require.ErrorContains(t, err, "identity mismatch")
	assert.Zero(t, enqueuer.attempts)
}

func finalizingRecoveryKnowledge(t *testing.T) *types.Knowledge {
	t.Helper()
	knowledge := committedFanoutKnowledge(t, "file")
	knowledge.ParseStatus = types.ParseStatusFinalizing
	knowledge.PendingSubtasksCount = 3
	knowledge.ProcessingOwner = ""
	plan, err := json.Marshal(durableEnrichmentFanout{
		Stage:                durableEnrichmentPlanStage,
		Version:              1,
		TenantID:             knowledge.TenantID,
		KnowledgeID:          knowledge.ID,
		KnowledgeBaseID:      knowledge.KnowledgeBaseID,
		ProcessingGeneration: knowledge.ProcessingGeneration,
		TextChunkCount:       1,
		SpawnSummary:         true,
	})
	require.NoError(t, err)
	knowledge.ProcessingFanout = plan
	return knowledge
}

func finalizingRecoveryDocumentTask(t *testing.T) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(types.DocumentProcessPayload{
		TenantID:             7,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "generation-1",
		ProcessingOwner: processownership.DocumentOwner(
			"knowledge-1", "generation-1",
		),
		Attempt: 1,
	})
	require.NoError(t, err)
	return asynq.NewTask(types.TypeDocumentProcess, payload)
}

func TestProcessDocumentFinalizingReplaysDurablePostProcessPlan(t *testing.T) {
	repo := &committedFanoutKnowledgeRepo{current: finalizingRecoveryKnowledge(t)}
	enqueuer := &retryingFanoutEnqueuer{}
	service := &knowledgeService{
		repo:       repo,
		tenantRepo: &committedFanoutTenantRepo{},
		task:       enqueuer,
	}

	require.NoError(t, service.ProcessDocument(
		context.Background(), finalizingRecoveryDocumentTask(t),
	))
	require.Equal(t, 1, enqueuer.attempts)
	require.Equal(t, []string{types.TypeKnowledgePostProcess}, enqueuer.taskTypes)
}

func TestProcessDocumentFinalizingWithoutDurablePlanFailsClosed(t *testing.T) {
	knowledge := finalizingRecoveryKnowledge(t)
	knowledge.ProcessingFanout = nil
	enqueuer := &retryingFanoutEnqueuer{}
	service := &knowledgeService{
		repo:       &committedFanoutKnowledgeRepo{current: knowledge},
		tenantRepo: &committedFanoutTenantRepo{},
		task:       enqueuer,
	}

	err := service.ProcessDocument(
		context.Background(), finalizingRecoveryDocumentTask(t),
	)
	require.ErrorContains(t, err, "durable enrichment fanout is missing")
	require.Zero(t, enqueuer.attempts)
}

func TestProcessManualUpdateStaleCommittedIdentityAcknowledgesWithoutReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*types.Knowledge)
	}{
		{name: "tenant", mutate: func(k *types.Knowledge) { k.TenantID = 8 }},
		{name: "knowledge", mutate: func(k *types.Knowledge) { k.ID = "knowledge-other" }},
		{name: "knowledge base", mutate: func(k *types.Knowledge) { k.KnowledgeBaseID = "kb-other" }},
		{name: "generation", mutate: func(k *types.Knowledge) { k.ProcessingGeneration = "generation-new" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			knowledge := committedFanoutKnowledge(t, types.KnowledgeTypeManual)
			tc.mutate(knowledge)
			repo := &committedFanoutKnowledgeRepo{current: knowledge}
			enqueuer := &retryingFanoutEnqueuer{}
			service := &knowledgeService{repo: repo, tenantRepo: &committedFanoutTenantRepo{}, task: enqueuer}

			require.NoError(t, service.ProcessManualUpdate(context.Background(), manualCommittedFanoutTask(t, false, 0)))
			assert.Zero(t, enqueuer.attempts)
		})
	}
}

func TestResolveSynchronousPassageProcessResultAcceptsOnlyExactCommittedCore(t *testing.T) {
	dispatchErr := errors.New("fanout dispatch failed")
	expected := committedFanoutKnowledge(t, "passage")
	current := committedFanoutKnowledge(t, "passage")
	repo := &committedFanoutKnowledgeRepo{current: current}
	service := &knowledgeService{repo: repo}

	result, err := service.resolveSynchronousPassageProcessResult(context.Background(), expected, dispatchErr)
	require.NoError(t, err)
	assert.Equal(t, current.ID, result.ID)
	assert.Equal(t, 1, repo.getCalls)
}

func TestResolveSynchronousPassageProcessResultPreservesPrecommitError(t *testing.T) {
	precommitErr := errors.New("embedding batch failed")
	expected := committedFanoutKnowledge(t, "passage")
	current := committedFanoutKnowledge(t, "passage")
	current.ParseStatus = types.ParseStatusPending
	current.ProcessingOwner = processownership.DocumentOwner(current.ID, current.ProcessingGeneration)
	current.ProcessedAt = nil
	current.ProcessingFanout = nil
	service := &knowledgeService{repo: &committedFanoutKnowledgeRepo{current: current}}

	result, err := service.resolveSynchronousPassageProcessResult(context.Background(), expected, precommitErr)
	require.ErrorIs(t, err, precommitErr)
	assert.Same(t, expected, result)
}

func TestResolveSynchronousPassageProcessResultFailsClosedOnIdentityOrPlanMismatch(t *testing.T) {
	processErr := errors.New("dispatch failed")
	expected := committedFanoutKnowledge(t, "passage")

	for _, tc := range []struct {
		name   string
		mutate func(*types.Knowledge)
	}{
		{name: "tenant", mutate: func(k *types.Knowledge) { k.TenantID = 8 }},
		{name: "knowledge", mutate: func(k *types.Knowledge) { k.ID = "knowledge-other" }},
		{name: "knowledge base", mutate: func(k *types.Knowledge) { k.KnowledgeBaseID = "kb-other" }},
		{name: "generation", mutate: func(k *types.Knowledge) { k.ProcessingGeneration = "generation-new" }},
		{name: "malformed plan", mutate: func(k *types.Knowledge) { k.ProcessingFanout = types.JSON("{") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := committedFanoutKnowledge(t, "passage")
			tc.mutate(current)
			service := &knowledgeService{repo: &committedFanoutKnowledgeRepo{current: current}}
			_, err := service.resolveSynchronousPassageProcessResult(context.Background(), expected, processErr)
			require.ErrorIs(t, err, processErr)
		})
	}
}
