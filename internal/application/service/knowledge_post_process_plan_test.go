package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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
	generationOutcomes   enrichmentoutcome.Aggregate
	completionErr        error
	completionItems      map[string]struct{}
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

func (s *postProcessWikiGateKnowledgeRepoStub) FinalizeSubtaskGenerationItemOutcome(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, knowledgeBaseID, generation, itemID, _, _ string,
) (int, bool, error) {
	return s.FinalizeSubtaskGenerationItem(
		ctx, tenantID, knowledgeID, knowledgeBaseID, generation, itemID,
	)
}

func (s *postProcessWikiGateKnowledgeRepoStub) RecordGenerationOutcome(
	context.Context, uint64, string, string, string, string, string, string,
) (bool, error) {
	return true, nil
}

func (s *postProcessWikiGateKnowledgeRepoStub) GetGenerationOutcomeAggregate(
	context.Context, uint64, string, string, string,
) (enrichmentoutcome.Aggregate, error) {
	return s.generationOutcomes, nil
}

func (s *postProcessWikiGateKnowledgeRepoStub) RecordKnowledgeFanoutCompletion(
	_ context.Context, _ uint64, _, _, _, itemID string,
) (bool, error) {
	if s.completionErr != nil {
		return false, s.completionErr
	}
	if s.completionItems == nil {
		s.completionItems = make(map[string]struct{})
	}
	if _, exists := s.completionItems[itemID]; exists {
		return false, nil
	}
	s.completionItems[itemID] = struct{}{}
	return true, nil
}

func (s *postProcessWikiGateKnowledgeRepoStub) ListKnowledgeFanoutCompletions(
	context.Context, uint64, string, string, string,
) ([]string, error) {
	items := make([]string, 0, len(s.completionItems))
	for item := range s.completionItems {
		items = append(items, item)
	}
	return items, nil
}

func (s *postProcessWikiGateKnowledgeRepoStub) CountKnowledgeFanoutCompletions(
	context.Context, uint64, string, string, string,
) (int64, error) {
	return int64(len(s.completionItems)), nil
}

func (s *postProcessWikiGateKnowledgeRepoStub) KnowledgeFanoutCompletionExists(
	_ context.Context, _ uint64, _, _, _, itemID string,
) (bool, error) {
	_, exists := s.completionItems[itemID]
	return exists, nil
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

func TestOrdinaryGenerationEnrichmentPlanIncludesMultimodalChildren(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(
		"file:ordinary-generation-enrichment?mode=memory&cache=shared",
	), &gorm.Config{})
	require.NoError(t, err)

	manager := documentsplit.NewManagerWithConfig(
		db, &wikiQueueTaskEnqueuerStub{}, documentsplit.Config{},
	)
	require.NoError(t, manager.Migrate(context.Background()))

	const (
		tenantID   = uint64(42)
		knowledge  = "knowledge-image"
		kbID       = "kb-image"
		generation = "generation-image"
	)
	require.NoError(t, db.Create([]*types.Chunk{
		{
			ID: "text", TenantID: tenantID, KnowledgeID: knowledge,
			KnowledgeBaseID: kbID, ProcessingGeneration: generation,
			ChunkType: types.ChunkTypeText, ChunkIndex: 0, Content: "![image](local://image.png)",
		},
		{
			ID: "ocr", TenantID: tenantID, KnowledgeID: knowledge,
			KnowledgeBaseID: kbID, ProcessingGeneration: generation,
			ChunkType: types.ChunkTypeImageOCR, ChunkIndex: 0, ParentChunkID: "text",
			Content: "approval owner digital management department",
		},
		{
			ID: "caption", TenantID: tenantID, KnowledgeID: knowledge,
			KnowledgeBaseID: kbID, ProcessingGeneration: generation,
			ChunkType: types.ChunkTypeImageCaption, ChunkIndex: 0, ParentChunkID: "text",
			Content: "an approval notice with a deadline and security warning",
		},
	}).Error)

	svc := &KnowledgePostProcessService{splitManager: manager}
	payload := types.KnowledgePostProcessPayload{
		TenantID: tenantID, KnowledgeID: knowledge, KnowledgeBaseID: kbID,
		ProcessingGeneration: generation,
	}
	plan, generationBacked, err := svc.buildPagedSplitEnrichmentPlan(
		context.Background(),
		payload,
		&types.KnowledgeBase{
			ID: kbID, TenantID: tenantID, SummaryModelID: "graph-model",
			IndexingStrategy: types.IndexingStrategy{
				VectorEnabled: true, GraphEnabled: true, WikiEnabled: true,
			},
		},
		types.EffectiveProcessConfig{
			GraphEnabled: true,
			QuestionGenerationConfig: types.QuestionGenerationConfig{
				Enabled: true, QuestionCount: 1,
			},
		},
		0,
	)
	require.NoError(t, err)
	require.True(t, generationBacked)
	require.Equal(t, 3, plan.TextChunkCount)
	require.Equal(t, 1, plan.QuestionChunkCount)
	require.Equal(t, 3, plan.GraphChunkCount)
	require.Equal(t, 1, plan.GraphBatchCount)
	require.True(t, plan.SpawnSummary)
	require.True(t, plan.SpawnWiki)

	selected, total, err := loadGenerationChunkStrata(
		context.Background(), manager, tenantID, knowledge, generation,
		[]types.ChunkType{
			types.ChunkTypeText,
			types.ChunkTypeImageOCR,
			types.ChunkTypeImageCaption,
		},
		int64(plan.GraphChunkCount),
	)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)
	require.ElementsMatch(t, []string{"text", "ocr", "caption"}, []string{
		selected[0].ID, selected[1].ID, selected[2].ID,
	})
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

func TestKnowledgePostProcessNoSubtasksPreservesCoreFanoutFailure(t *testing.T) {
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{
		knowledge: &types.Knowledge{
			ID:                   "knowledge-1",
			TenantID:             42,
			KnowledgeBaseID:      "kb-1",
			ParseStatus:          types.ParseStatusProcessing,
			ProcessingGeneration: postProcessTestGeneration,
		},
		generationOutcomes: enrichmentoutcome.Aggregate{Total: 1, Failed: 1},
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

	if err := svc.Handle(
		context.Background(),
		asynq.NewTask(types.TypeKnowledgePostProcess, payload),
	); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(knowledgeRepo.generationSwapValues) != 1 {
		t.Fatalf("generation CAS values = %d, want 1", len(knowledgeRepo.generationSwapValues))
	}
	if got := knowledgeRepo.generationSwapValues[0]["enrichment_status"]; got != types.EnrichmentStatusFailed {
		t.Fatalf("enrichment status = %v, want failed", got)
	}
	if _, ok := knowledgeRepo.completionItems[processownership.PostProcessCompletionItem]; !ok {
		t.Fatal("successful post-process did not persist its generation receipt")
	}
}

func TestKnowledgePostProcessCompletionReceiptFailureIsRetried(t *testing.T) {
	receiptErr := errors.New("completion ledger unavailable")
	knowledgeRepo := &postProcessWikiGateKnowledgeRepoStub{
		knowledge: &types.Knowledge{
			ID:                   "knowledge-1",
			TenantID:             42,
			KnowledgeBaseID:      "kb-1",
			ParseStatus:          types.ParseStatusProcessing,
			ProcessingGeneration: postProcessTestGeneration,
		},
		completionErr: receiptErr,
	}
	svc := &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		kbService: &postProcessWikiGateKBServiceStub{kb: &types.KnowledgeBase{
			ID: "kb-1", TenantID: 42, IndexingStrategy: types.IndexingStrategy{},
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
	require.ErrorIs(t, err, receiptErr)
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

func TestKnowledgePostProcessVersion3ReplaysPagedQuestionsWithoutSummary(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	require.NoError(t, db.Create(&types.Chunk{
		ID:                   "chunk-1",
		SeqID:                1,
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		Content:              "durable question source",
		ChunkIndex:           0,
		ChunkType:            types.ChunkTypeText,
		ProcessingGeneration: postProcessTestGeneration,
	}).Error)

	plan := durableEnrichmentFanout{
		Stage:                durableEnrichmentPlanStage,
		Version:              3,
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
		TextChunkCount:       1,
		SpawnSummary:         false,
		QuestionCount:        1,
		QuestionChunkCount:   1,
		QuestionBatchCount:   1,
	}
	planBytes, err := json.Marshal(plan)
	require.NoError(t, err)
	repo := &postProcessWikiGateKnowledgeRepoStub{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusFinalizing,
		ProcessingGeneration: postProcessTestGeneration,
		ProcessingFanout:     types.JSON(planBytes),
		PendingSubtasksCount: 1,
	}}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &KnowledgePostProcessService{
		knowledgeRepo: repo,
		taskEnqueuer:  enqueuer,
		splitManager: documentsplit.NewManagerWithConfig(
			db, nil, documentsplit.Config{},
		),
	}
	payload, err := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: postProcessTestGeneration,
		Attempt:              1,
	})
	require.NoError(t, err)

	require.NoError(t, svc.Handle(
		context.Background(),
		asynq.NewTask(types.TypeKnowledgePostProcess, payload),
	))
	require.Len(t, enqueuer.tasks, 1)
	require.Equal(t, types.TypeQuestionGeneration, enqueuer.tasks[0].Type())
	var question types.QuestionGenerationPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &question))
	require.Equal(t, 0, question.BatchIndex)
	require.Equal(t, []string{"chunk-1"}, question.ChunkIDs)
	require.Zero(t, repo.finalizeCalls)
	_, receiptRecorded := repo.completionItems[processownership.PostProcessCompletionItem]
	require.True(t, receiptRecorded)
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
