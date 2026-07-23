package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
)

type dataTableKnowledgeServiceStub struct {
	interfaces.KnowledgeService
	knowledge   *types.Knowledge
	getCalls    int
	updateCalls int
}

func (s *dataTableKnowledgeServiceStub) GetKnowledgeByID(context.Context, string) (*types.Knowledge, error) {
	s.getCalls++
	return s.knowledge, nil
}

func (s *dataTableKnowledgeServiceStub) UpdateKnowledge(context.Context, *types.Knowledge) error {
	s.updateCalls++
	return nil
}

func TestDataTableSummaryStaleGenerationRejectsBeforeFanInOrModels(t *testing.T) {
	knowledgeService := &dataTableKnowledgeServiceStub{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: "new-generation",
	}}
	svc := &DataTableSummaryService{knowledgeService: knowledgeService}
	payload, _ := json.Marshal(types.DataTableSummaryPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "old-generation",
		SummaryModel:         "summary-model",
		EmbeddingModel:       "embedding-model",
	})
	if err := svc.Handle(context.Background(), asynq.NewTask(types.TypeDataTableSummary, payload)); err != nil {
		t.Fatalf("Handle() stale generation error = %v", err)
	}
	if knowledgeService.getCalls != 1 {
		t.Fatalf("knowledge reads = %d, want 1", knowledgeService.getCalls)
	}
}

func TestDataTableCleanupDoesNotOverwriteDocumentLifecycle(t *testing.T) {
	knowledgeService := &dataTableKnowledgeServiceStub{}
	svc := &DataTableSummaryService{knowledgeService: knowledgeService}
	svc.cleanupOnFailure(context.Background(), &extractionResources{
		knowledge: &types.Knowledge{ID: "knowledge-1", ParseStatus: types.ParseStatusCompleted},
	}, nil, context.Canceled)
	if knowledgeService.updateCalls != 0 {
		t.Fatalf("cleanup lifecycle updates = %d, want 0", knowledgeService.updateCalls)
	}
}

func TestStableDataTableChunkIDIsGenerationScoped(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 42, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1",
	}
	first := stableDataTableChunkID(knowledge, "summary")
	if again := stableDataTableChunkID(knowledge, "summary"); again != first {
		t.Fatalf("same table artifact ID changed: %s != %s", again, first)
	}
	if columns := stableDataTableChunkID(knowledge, "columns"); columns == first {
		t.Fatal("table summary and columns artifact IDs collided")
	}
	knowledge.ProcessingGeneration = "generation-2"
	if nextGeneration := stableDataTableChunkID(knowledge, "summary"); nextGeneration == first {
		t.Fatal("different generations reused a table artifact ID")
	}
}

func TestDataTableTerminalFanInSurvivesCancelledWorkerContext(t *testing.T) {
	payload := types.DataTableSummaryPayload{
		TenantID:             42,
		KnowledgeID:          "knowledge-cancelled-table",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "generation-1",
		SummaryModel:         "summary-model",
		EmbeddingModel:       "embedding-model",
	}
	plan := processownership.FanoutPlan{
		Version:              processownership.FanoutPlanVersion,
		TenantID:             payload.TenantID,
		KnowledgeID:          payload.KnowledgeID,
		KnowledgeBaseID:      payload.KnowledgeBaseID,
		ProcessingGeneration: payload.ProcessingGeneration,
		DataTable: &processownership.DataTableFanout{
			SummaryModel: payload.SummaryModel, EmbeddingModel: payload.EmbeddingModel,
		},
	}
	store := &imageGenerationKnowledgeRepoStub{rejectCanceled: true}
	enqueuer := &wikiQueueTaskEnqueuerStub{}
	svc := &DataTableSummaryService{taskEnqueuer: enqueuer}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.completeDataTableFanIn(ctx, payload, plan, store); err != nil {
		t.Fatalf("terminal data-table fan-in on cancelled worker context: %v", err)
	}
	if _, completed := store.completed[processownership.DataTableFanoutItem()]; !completed {
		t.Fatal("cancelled worker context lost durable data-table completion")
	}
	if len(enqueuer.tasks) != 1 || enqueuer.tasks[0].Type() != types.TypeKnowledgePostProcess {
		t.Fatalf("postprocess tasks = %d, want 1", len(enqueuer.tasks))
	}
}

func TestBuildSampleDataDescriptionAcceptsStringRows(t *testing.T) {
	svc := &DataTableSummaryService{}
	description := svc.buildSampleDataDescription(&types.ToolResult{
		Data: map[string]interface{}{
			"rows": []map[string]string{
				{"资产编号": "A-001", "部门": "财务部"},
				{"资产编号": "A-002", "部门": "运营部"},
			},
		},
	}, 10)
	if !strings.Contains(description, `"资产编号":"A-001"`) ||
		!strings.Contains(description, `"部门":"运营部"`) {
		t.Fatalf("string-valued rows were omitted: %q", description)
	}
}
