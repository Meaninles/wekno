package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
)

type postProcessWikiGateKnowledgeRepoStub struct {
	interfaces.KnowledgeRepository
	knowledge            *types.Knowledge
	setFinalizingErr     error
	updateColumnsErr     error
	generationSwapCalls  int
	generationSwapValues []map[string]interface{}
	finalizeCalls        int
	finalizedItems       []string
	finalizeErr          error
}

func (s *postProcessWikiGateKnowledgeRepoStub) GetKnowledgeByID(context.Context, uint64, string) (*types.Knowledge, error) {
	return s.knowledge, nil
}

func (s *postProcessWikiGateKnowledgeRepoStub) CompareAndSwapKnowledgeProcessingGeneration(
	_ context.Context,
	_ uint64,
	_, _, _ string,
	_ []string,
	values map[string]interface{},
) (bool, error) {
	s.generationSwapCalls++
	s.generationSwapValues = append(s.generationSwapValues, values)
	if values["parse_status"] == types.ParseStatusCompleted && s.updateColumnsErr != nil {
		return false, s.updateColumnsErr
	}
	if values["parse_status"] == types.ParseStatusFinalizing && s.setFinalizingErr != nil {
		return false, s.setFinalizingErr
	}
	return true, nil
}

func (s *postProcessWikiGateKnowledgeRepoStub) FinalizeSubtaskGenerationItem(
	_ context.Context, _ uint64, _, _, _, itemID string,
) (int, bool, error) {
	s.finalizeCalls++
	s.finalizedItems = append(s.finalizedItems, itemID)
	return 0, s.finalizeErr == nil, s.finalizeErr
}

const postProcessTestGeneration = "generation-1"

type postProcessWikiGateKBServiceStub struct {
	interfaces.KnowledgeBaseService
	kb    *types.KnowledgeBase
	calls int
}

func (s *postProcessWikiGateKBServiceStub) GetKnowledgeBaseByIDOnly(context.Context, string) (*types.KnowledgeBase, error) {
	s.calls++
	return s.kb, nil
}

type postProcessWikiGateChunkServiceStub struct {
	interfaces.ChunkService
	chunks []*types.Chunk
	calls  int
}

type firstTaskFailureEnqueuer struct {
	err   error
	calls int
	tasks []*asynq.Task
}

func (e *firstTaskFailureEnqueuer) Enqueue(
	task *asynq.Task,
	_ ...asynq.Option,
) (*asynq.TaskInfo, error) {
	e.calls++
	if e.calls == 1 {
		return nil, e.err
	}
	e.tasks = append(e.tasks, task)
	return &asynq.TaskInfo{ID: "test-task", Type: task.Type(), Queue: types.QueueDefault}, nil
}

func (s *postProcessWikiGateChunkServiceStub) ListChunksByKnowledgeID(context.Context, string) ([]*types.Chunk, error) {
	s.calls++
	return s.chunks, nil
}

func TestEnrichmentPlanFinalizationSubtaskCountExcludesWiki(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		plan enrichmentPlan
		want int
	}{
		{
			name: "no enrichment",
			plan: enrichmentPlan{},
			want: 0,
		},
		{
			name: "wiki only stays outside document finalization",
			plan: enrichmentPlan{spawnWiki: true},
			want: 0,
		},
		{
			name: "summary plus wiki counts only summary",
			plan: enrichmentPlan{spawnSummary: true, spawnWiki: true},
			want: 1,
		},
		{
			name: "all document owned enrichment tasks",
			plan: enrichmentPlan{
				spawnSummary:       true,
				questionBatchCount: 2,
				spawnWiki:          true,
				graphChunkCount:    3,
			},
			want: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.plan.finalizationSubtaskCount(); got != tt.want {
				t.Fatalf("finalizationSubtaskCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestKnowledgePostProcessWikiPersistFailureStopsBeforeStateAndFanout(t *testing.T) {
	persistErr := errors.New("postgres pending queue unavailable")
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: postProcessTestGeneration,
	}}
	taskEnqueuer := &wikiQueueTaskEnqueuerStub{}
	pendingRepo := &wikiQueuePendingRepoStub{enqueueErr: persistErr}
	svc := &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		kbService: &postProcessWikiGateKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			TenantID:         42,
			WikiConfig:       &types.WikiConfig{},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		chunkService: &postProcessWikiGateChunkServiceStub{chunks: []*types.Chunk{{
			ID:        "chunk-1",
			ChunkType: types.ChunkTypeText,
			Content:   "document text",
		}}},
		taskEnqueuer: taskEnqueuer,
		pendingRepo:  pendingRepo,
	}
	payload, err := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
	})
	if err != nil {
		t.Fatalf("marshal post-process payload: %v", err)
	}

	err = svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload))
	if !errors.Is(err, persistErr) {
		t.Fatalf("Handle() error = %v, want wrapped pending persistence error", err)
	}
	if knowledgeRepo.generationSwapCalls != 1 {
		t.Fatalf("wiki persistence failure generation CAS calls = %d, want 1 durable finalizing plan",
			knowledgeRepo.generationSwapCalls)
	}
	for _, task := range taskEnqueuer.tasks {
		if task.Type() != types.TypeWikiIngest {
			t.Fatalf("wiki persistence failure fanned out task type %q", task.Type())
		}
	}
}

func TestKnowledgePostProcessNoSubtasksCompletionFailureIsRetried(t *testing.T) {
	updateErr := errors.New("knowledge update failed")
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{
		knowledge: &types.Knowledge{
			ID:                   "knowledge-1",
			TenantID:             42,
			KnowledgeBaseID:      "kb-1",
			ParseStatus:          types.ParseStatusProcessing,
			ProcessingGeneration: postProcessTestGeneration,
		},
		updateColumnsErr: updateErr,
	}
	svc := &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		kbService: &postProcessWikiGateKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			TenantID:         42,
			IndexingStrategy: types.IndexingStrategy{},
		}},
		chunkService: &postProcessWikiGateChunkServiceStub{},
		taskEnqueuer: &wikiQueueTaskEnqueuerStub{},
		pendingRepo:  &wikiQueuePendingRepoStub{},
	}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
	})

	err := svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload))
	if !errors.Is(err, updateErr) {
		t.Fatalf("Handle() error = %v, want completion update error", err)
	}
	if knowledgeRepo.generationSwapCalls != 1 {
		t.Fatalf("generation CAS calls = %d, want 1", knowledgeRepo.generationSwapCalls)
	}
}

func TestKnowledgePostProcessSetFinalizingFailureIsRetriedBeforeFanout(t *testing.T) {
	setErr := errors.New("set finalizing transaction failed")
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{
		knowledge: &types.Knowledge{
			ID:                   "knowledge-1",
			TenantID:             42,
			KnowledgeBaseID:      "kb-1",
			ParseStatus:          types.ParseStatusProcessing,
			ProcessingGeneration: postProcessTestGeneration,
		},
		setFinalizingErr: setErr,
	}
	taskEnqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		kbService: &postProcessWikiGateKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			TenantID:         42,
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: false},
		}},
		chunkService: &postProcessWikiGateChunkServiceStub{chunks: []*types.Chunk{{
			ID: "chunk-1", ChunkType: types.ChunkTypeText, Content: "text",
		}}},
		taskEnqueuer: taskEnqueuer,
		pendingRepo:  &wikiQueuePendingRepoStub{},
	}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
	})

	err := svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload))
	if !errors.Is(err, setErr) {
		t.Fatalf("Handle() error = %v, want SetFinalizing error", err)
	}
	if knowledgeRepo.generationSwapCalls != 1 {
		t.Fatalf("finalizing generation CAS calls = %d, want 1", knowledgeRepo.generationSwapCalls)
	}
	if len(taskEnqueuer.tasks) != 0 {
		t.Fatalf("SetFinalizing failure fanned out %d tasks", len(taskEnqueuer.tasks))
	}
}

func TestKnowledgePostProcessPersistedWikiRowContinuesWhenTriggerFails(t *testing.T) {
	triggerErr := errors.New("redis trigger unavailable")
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: postProcessTestGeneration,
	}}
	taskEnqueuer := &firstTaskFailureEnqueuer{err: triggerErr}
	pendingRepo := &wikiQueuePendingRepoStub{}
	svc := &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		kbService: &postProcessWikiGateKBServiceStub{kb: &types.KnowledgeBase{
			ID:               "kb-1",
			TenantID:         42,
			WikiConfig:       &types.WikiConfig{},
			IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		}},
		chunkService: &postProcessWikiGateChunkServiceStub{chunks: []*types.Chunk{{
			ID: "chunk-1", ChunkType: types.ChunkTypeText, Content: "text",
		}}},
		taskEnqueuer: taskEnqueuer,
		pendingRepo:  pendingRepo,
	}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
	})

	if err := svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload)); err != nil {
		t.Fatalf("Handle() error = %v, want durable Wiki row to tolerate trigger failure", err)
	}
	if len(pendingRepo.enqueued) != 1 {
		t.Fatalf("persisted Wiki rows = %d, want 1", len(pendingRepo.enqueued))
	}
	if knowledgeRepo.generationSwapCalls != 1 {
		t.Fatalf("finalizing generation CAS calls = %d, want 1", knowledgeRepo.generationSwapCalls)
	}
	if knowledgeRepo.finalizeCalls != 0 {
		t.Fatalf("durably enqueued summary slot releases = %d, want 0", knowledgeRepo.finalizeCalls)
	}
	if len(taskEnqueuer.tasks) != 1 || taskEnqueuer.tasks[0].Type() != types.TypeSummaryGeneration {
		t.Fatalf("post-Wiki tasks = %d, want one summary", len(taskEnqueuer.tasks))
	}
}

func TestKnowledgePostProcessNilKnowledgeBaseReturnsExplicitError(t *testing.T) {
	svc := &KnowledgePostProcessService{
		knowledgeRepo: &postProcessWikiGateKnowledgeRepoStub{knowledge: &types.Knowledge{
			ID:                   "knowledge-1",
			TenantID:             42,
			KnowledgeBaseID:      "kb-1",
			ParseStatus:          types.ParseStatusProcessing,
			ProcessingGeneration: postProcessTestGeneration,
		}},
		kbService:    &postProcessWikiGateKBServiceStub{},
		chunkService: &postProcessWikiGateChunkServiceStub{},
	}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
	})

	err := svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload))
	if err == nil || !strings.Contains(err.Error(), "returned nil without error") {
		t.Fatalf("Handle() error = %v, want explicit nil-KB invariant error", err)
	}
}

func TestKnowledgePostProcessMissingGenerationFailsClosed(t *testing.T) {
	svc := &KnowledgePostProcessService{knowledgeRepo: &postProcessWikiGateKnowledgeRepoStub{}}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID: 42, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1",
	})
	err := svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload))
	if err == nil || !strings.Contains(err.Error(), "processing generation identity is required") {
		t.Fatalf("Handle() error = %v, want missing generation rejection", err)
	}
}

func TestKnowledgePostProcessStaleGenerationDoesNotReadKBChunksOrMutate(t *testing.T) {
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: "new-generation",
	}}
	kbService := &postProcessWikiGateKBServiceStub{}
	chunkService := &postProcessWikiGateChunkServiceStub{}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		kbService:     kbService,
		chunkService:  chunkService,
		taskEnqueuer:  enqueuer,
	}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "old-generation",
	})
	if err := svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload)); err != nil {
		t.Fatalf("Handle() stale generation error = %v", err)
	}
	if kbService.calls != 0 || chunkService.calls != 0 || knowledgeRepo.generationSwapCalls != 0 || len(enqueuer.tasks) != 0 {
		t.Fatalf("stale generation side effects: kb=%d chunks=%d cas=%d tasks=%d",
			kbService.calls, chunkService.calls, knowledgeRepo.generationSwapCalls, len(enqueuer.tasks))
	}
}

func TestKnowledgePostProcessFinalizingReplayTaskIDConflictOwnsSlot(t *testing.T) {
	plan := durableEnrichmentFanout{
		Stage:                durableEnrichmentPlanStage,
		Version:              1,
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
		TextChunkCount:       1,
		SpawnSummary:         true,
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusFinalizing,
		ProcessingGeneration: postProcessTestGeneration,
		ProcessingFanout:     types.JSON(planBytes),
		PendingSubtasksCount: 1,
	}}
	enqueuer := &wikiQueueTaskEnqueuerStub{err: asynq.ErrTaskIDConflict}
	svc := &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		taskEnqueuer:  enqueuer,
	}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
	})
	if err := svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload)); err != nil {
		t.Fatalf("Handle() replay error = %v", err)
	}
	if knowledgeRepo.generationSwapCalls != 0 {
		t.Fatalf("replay generation CAS calls = %d, want 0", knowledgeRepo.generationSwapCalls)
	}
	if knowledgeRepo.finalizeCalls != 0 {
		t.Fatalf("TaskID conflict released %d slot(s), want 0", knowledgeRepo.finalizeCalls)
	}
	if len(enqueuer.tasks) != 1 || enqueuer.tasks[0].Type() != types.TypeSummaryGeneration {
		t.Fatalf("replay tasks = %d, want one summary", len(enqueuer.tasks))
	}
	var gotTaskID string
	for _, opt := range enqueuer.opts[0] {
		if opt.Type() == asynq.TaskIDOpt {
			gotTaskID, _ = opt.Value().(string)
		}
	}
	wantTaskID := processownership.SummaryTaskID("knowledge-1", postProcessTestGeneration)
	if gotTaskID != wantTaskID {
		t.Fatalf("summary TaskID = %q, want %q", gotTaskID, wantTaskID)
	}
}

func TestKnowledgePostProcessReconciliationFailureReturnsForRetry(t *testing.T) {
	t.Setenv("NEO4J_ENABLE", "false")
	plan := durableEnrichmentFanout{
		Stage:                durableEnrichmentPlanStage,
		Version:              1,
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
		TextChunkCount:       1,
		GraphTasks: []durableGraphTask{{
			ChunkID: "chunk-1", ChunkIndex: 0, ModelID: "graph-model",
		}},
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	finalizeErr := errors.New("ledger unavailable")
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{
		knowledge: &types.Knowledge{
			ID:                   "knowledge-1",
			TenantID:             42,
			KnowledgeBaseID:      "kb-1",
			ParseStatus:          types.ParseStatusFinalizing,
			ProcessingGeneration: postProcessTestGeneration,
			ProcessingFanout:     types.JSON(planBytes),
			PendingSubtasksCount: 1,
		},
		finalizeErr: finalizeErr,
	}
	// Graph extraction is a deterministic no-op when Neo4j is disabled, so its
	// stable slot has no owner and exercises durable reconciliation.
	svc := &KnowledgePostProcessService{knowledgeRepo: knowledgeRepo}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
	})
	err = svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload))
	if !errors.Is(err, finalizeErr) {
		t.Fatalf("Handle() error = %v, want reconciliation DB error", err)
	}
	if knowledgeRepo.finalizeCalls != 1 || len(knowledgeRepo.finalizedItems) != 1 ||
		knowledgeRepo.finalizedItems[0] != "graph_chunk[0]" {
		t.Fatalf("finalized items = %v calls=%d, want [graph_chunk[0]]/1",
			knowledgeRepo.finalizedItems, knowledgeRepo.finalizeCalls)
	}
}

func TestKnowledgePostProcessEnqueueFailureReturnsForStableReplay(t *testing.T) {
	plan := durableEnrichmentFanout{
		Stage:                durableEnrichmentPlanStage,
		Version:              1,
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
		TextChunkCount:       1,
		SpawnSummary:         true,
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	repo := &postProcessWikiGateKnowledgeRepoStub{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusFinalizing,
		ProcessingGeneration: postProcessTestGeneration,
		ProcessingFanout:     types.JSON(planBytes),
		PendingSubtasksCount: 1,
	}}
	enqueueErr := errors.New("queue temporarily unavailable")
	enqueuer := &wikiQueueTaskEnqueuerStub{err: enqueueErr}
	svc := &KnowledgePostProcessService{knowledgeRepo: repo, taskEnqueuer: enqueuer}
	payload, _ := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
	})
	err = svc.Handle(context.Background(), asynq.NewTask(types.TypeKnowledgePostProcess, payload))
	if !errors.Is(err, enqueueErr) {
		t.Fatalf("Handle() error = %v, want queue failure for Asynq retry", err)
	}
	if repo.finalizeCalls != 0 {
		t.Fatalf("transient enqueue failure released %d durable slot(s), want 0", repo.finalizeCalls)
	}
}
