package terminalrepair

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
)

type repairRepoFake struct {
	interfaces.KnowledgeRepository
	mu              sync.Mutex
	knowledge       *types.Knowledge
	items           []string
	completions     map[string]struct{}
	outcomes        map[string]string
	fail            error
	documentRepairs []documentRepairCall
}

type documentRepairCall struct {
	tenantID                     uint64
	knowledgeID, knowledgeBaseID string
	generation, owner            string
	values                       map[string]interface{}
}

func (r *repairRepoFake) GetKnowledgeByID(context.Context, uint64, string) (*types.Knowledge, error) {
	if r.knowledge == nil {
		return nil, errors.New("not found")
	}
	copy := *r.knowledge
	return &copy, nil
}

func (r *repairRepoFake) FailDocumentProcessingGeneration(
	_ context.Context,
	tenantID uint64,
	knowledgeID, knowledgeBaseID, generation, owner string,
	values map[string]interface{},
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return false, r.fail
	}
	r.documentRepairs = append(r.documentRepairs, documentRepairCall{
		tenantID: tenantID, knowledgeID: knowledgeID, knowledgeBaseID: knowledgeBaseID,
		generation: generation, owner: owner, values: values,
	})
	return true, nil
}

func (r *repairRepoFake) FinalizeSubtaskGenerationItemOutcome(
	_ context.Context, _ uint64, _, _, _, item, _, _ string,
) (int, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return 0, false, r.fail
	}
	r.items = append(r.items, item)
	return 0, true, nil
}

func (r *repairRepoFake) RecordKnowledgeFanoutCompletion(
	_ context.Context, _ uint64, _, _, _, item string,
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return false, r.fail
	}
	if r.completions == nil {
		r.completions = make(map[string]struct{})
	}
	_, exists := r.completions[item]
	r.completions[item] = struct{}{}
	return !exists, nil
}

func (r *repairRepoFake) ListKnowledgeFanoutCompletions(
	context.Context, uint64, string, string, string,
) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]string, 0, len(r.completions))
	for item := range r.completions {
		items = append(items, item)
	}
	return items, nil
}

func (r *repairRepoFake) CountKnowledgeFanoutCompletions(
	context.Context, uint64, string, string, string,
) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int64(len(r.completions)), nil
}

func (r *repairRepoFake) KnowledgeFanoutCompletionExists(
	_ context.Context, _ uint64, _, _, _, item string,
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.completions[item]
	return ok, nil
}

func (r *repairRepoFake) RecordGenerationOutcome(
	_ context.Context,
	_ uint64,
	_, _, _ string,
	item string,
	status string,
	_ string,
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return false, r.fail
	}
	if r.outcomes == nil {
		r.outcomes = make(map[string]string)
	}
	if _, exists := r.outcomes[item]; exists {
		return false, nil
	}
	r.outcomes[item] = status
	return true, nil
}

func (r *repairRepoFake) GetGenerationOutcomeAggregate(
	context.Context, uint64, string, string, string,
) (enrichmentoutcome.Aggregate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var aggregate enrichmentoutcome.Aggregate
	for _, status := range r.outcomes {
		aggregate.Total++
		switch status {
		case enrichmentoutcome.StatusFailed:
			aggregate.Failed++
		case enrichmentoutcome.StatusDegraded:
			aggregate.Degraded++
		case enrichmentoutcome.StatusCompleted:
			aggregate.Completed++
		}
	}
	return aggregate, nil
}

type repairEnqueuerFake struct {
	mu      sync.Mutex
	byID    map[string]*asynq.Task
	ordered []*asynq.Task
}

type moveRepairerFake struct {
	mu       sync.Mutex
	calls    int
	taskType string
	taskErr  string
	err      error
}

func (r *moveRepairerFake) RepairKnowledgeMoveDeadLetter(
	_ context.Context,
	task *asynq.Task,
	taskErr error,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.taskType = task.Type()
	if taskErr != nil {
		r.taskErr = taskErr.Error()
	}
	return r.err
}

func (e *repairEnqueuerFake) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var taskID string
	for _, opt := range opts {
		if opt.Type() == asynq.TaskIDOpt {
			taskID, _ = opt.Value().(string)
		}
	}
	if taskID != "" {
		if _, exists := e.byID[taskID]; exists {
			return nil, asynq.ErrTaskIDConflict
		}
		if e.byID == nil {
			e.byID = make(map[string]*asynq.Task)
		}
		e.byID[taskID] = task
	}
	e.ordered = append(e.ordered, task)
	return &asynq.TaskInfo{ID: taskID, Type: task.Type()}, nil
}

func taskWithPayload(t *testing.T, taskType string, payload any) *asynq.Task {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return asynq.NewTask(taskType, raw)
}

func TestRepairDerivesStableEnrichmentItems(t *testing.T) {
	tests := []struct {
		name     string
		taskType string
		payload  any
		wantItem string
	}{
		{
			name:     "summary",
			taskType: types.TypeSummaryGeneration,
			payload:  types.SummaryGenerationPayload{TenantID: 1, KnowledgeID: "k", KnowledgeBaseID: "kb", ProcessingGeneration: "g"},
			wantItem: "summary",
		},
		{
			name:     "question batch",
			taskType: types.TypeQuestionGeneration,
			payload:  types.QuestionGenerationPayload{TenantID: 1, KnowledgeID: "k", KnowledgeBaseID: "kb", ProcessingGeneration: "g", ChunkIDs: []string{"c"}, BatchIndex: 7},
			wantItem: "question_batch[7]",
		},
		{
			name:     "graph chunk",
			taskType: types.TypeChunkExtract,
			payload:  types.ExtractChunkPayload{TenantID: 1, KnowledgeID: "k", KnowledgeBaseID: "kb", ProcessingGeneration: "g", ChunkID: "c", ChunkIndex: 9},
			wantItem: "graph_chunk[9]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &repairRepoFake{}
			svc := New(repo, nil, nil)
			if err := svc.Repair(context.Background(), taskWithPayload(t, tt.taskType, tt.payload), errors.New("permanent")); err != nil {
				t.Fatalf("Repair() error = %v", err)
			}
			if len(repo.items) != 1 || repo.items[0] != tt.wantItem {
				t.Fatalf("finalized items = %#v, want [%q]", repo.items, tt.wantItem)
			}
		})
	}
}

func TestRepairCompletesImageAndDataTableFanInThenEnqueuesPostProcess(t *testing.T) {
	plan := processownership.FanoutPlan{
		Version:              processownership.FanoutPlanVersion,
		TenantID:             1,
		KnowledgeID:          "k",
		KnowledgeBaseID:      "kb",
		ProcessingGeneration: "g",
		Images:               []processownership.ImageFanout{{ChunkID: "c", ImageURL: "local://image", Index: 0}},
		DataTable:            &processownership.DataTableFanout{SummaryModel: "summary", EmbeddingModel: "embedding"},
	}
	rawPlan, err := processownership.MarshalFanoutPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	repo := &repairRepoFake{knowledge: &types.Knowledge{
		ID:                   "k",
		TenantID:             1,
		KnowledgeBaseID:      "kb",
		ProcessingGeneration: "g",
		ProcessingFanout:     rawPlan,
		ParseStatus:          types.ParseStatusProcessing,
	}}
	enqueuer := &repairEnqueuerFake{}
	svc := New(repo, enqueuer, nil)

	image := types.ImageMultimodalPayload{TenantID: 1, KnowledgeID: "k", KnowledgeBaseID: "kb", ProcessingGeneration: "g", ChunkID: "c", ImageIndex: 0}
	if err := svc.Repair(context.Background(), taskWithPayload(t, types.TypeImageMultimodal, image), errors.New("vlm failed")); err != nil {
		t.Fatalf("image Repair() error = %v", err)
	}
	if len(enqueuer.ordered) != 0 {
		t.Fatalf("postprocess enqueued before all fanout completed: %d", len(enqueuer.ordered))
	}
	table := types.DataTableSummaryPayload{TenantID: 1, KnowledgeID: "k", KnowledgeBaseID: "kb", ProcessingGeneration: "g"}
	if err := svc.Repair(context.Background(), taskWithPayload(t, types.TypeDataTableSummary, table), errors.New("table failed")); err != nil {
		t.Fatalf("data-table Repair() error = %v", err)
	}
	if len(enqueuer.ordered) != 1 || enqueuer.ordered[0].Type() != types.TypeKnowledgePostProcess {
		t.Fatalf("enqueued tasks = %#v, want one postprocess task", enqueuer.ordered)
	}
	if repo.outcomes["multimodal.image[0]"] != enrichmentoutcome.StatusFailed {
		t.Fatalf("image terminal outcome = %q, want failed", repo.outcomes["multimodal.image[0]"])
	}
	if repo.outcomes["datatable.summary"] != enrichmentoutcome.StatusFailed {
		t.Fatalf("data-table terminal outcome = %q, want failed", repo.outcomes["datatable.summary"])
	}
}

func TestEnqueueTerminalRepairUsesStableTaskID(t *testing.T) {
	enqueuer := &repairEnqueuerFake{}
	original := taskWithPayload(t, types.TypeSummaryGeneration, types.SummaryGenerationPayload{
		TenantID: 1, KnowledgeID: "k", KnowledgeBaseID: "kb", ProcessingGeneration: "g",
	})
	if err := Enqueue(enqueuer, original, errors.New("db down")); err != nil {
		t.Fatalf("first Enqueue() error = %v", err)
	}
	if err := Enqueue(enqueuer, original, errors.New("db still down")); err != nil {
		t.Fatalf("duplicate Enqueue() error = %v", err)
	}
	if len(enqueuer.byID) != 1 {
		t.Fatalf("stable repair tasks = %d, want 1", len(enqueuer.byID))
	}
	for _, task := range enqueuer.byID {
		if task.Type() != types.TypeKnowledgeTerminalRepair {
			t.Fatalf("repair task type = %q", task.Type())
		}
	}
}

func TestKnowledgeMoveRepairBypassesSingleDocumentIdentityAndSurvivesRepairTask(t *testing.T) {
	move := &moveRepairerFake{}
	svc := New(&repairRepoFake{}, nil, nil)
	svc.SetKnowledgeMoveRepairer(move)
	original := taskWithPayload(t, types.TypeKnowledgeMove, types.KnowledgeMovePayload{
		TenantID: 1, KnowledgeIDs: []string{"k1", "k2"},
		SourceKBID: "source", TargetKBID: "target", Mode: "reparse",
	})
	repairPayload := types.KnowledgeTerminalRepairPayload{
		OriginalTaskType: original.Type(),
		OriginalPayload:  append(json.RawMessage(nil), original.Payload()...),
		LastError:        "database unavailable",
	}

	if err := svc.Handle(context.Background(), taskWithPayload(t, types.TypeKnowledgeTerminalRepair, repairPayload)); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	move.mu.Lock()
	defer move.mu.Unlock()
	if move.calls != 1 || move.taskType != types.TypeKnowledgeMove || move.taskErr != "database unavailable" {
		t.Fatalf("move repair calls=%d type=%q err=%q", move.calls, move.taskType, move.taskErr)
	}
}

func TestKnowledgeListReparseRepairFailsOnlyExactChildGeneration(t *testing.T) {
	generation, owner := processownership.BatchReparseIdentity(7, "batch-1", "knowledge-1")
	payload := types.KnowledgeListReparsePayload{
		TenantID: 7, KnowledgeIDs: []string{"knowledge-1"}, BatchID: "batch-1",
		ProcessingGeneration: generation, ProcessingOwner: owner,
	}
	repo := &repairRepoFake{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ParseStatus: types.ParseStatusPending, ProcessingGeneration: generation, ProcessingOwner: owner,
	}}
	svc := New(repo, nil, nil)
	if err := svc.Repair(
		context.Background(),
		taskWithPayload(t, types.TypeKnowledgeListReparse, payload),
		errors.New("cleanup database unavailable"),
	); err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if len(repo.documentRepairs) != 1 {
		t.Fatalf("document repairs = %d, want 1", len(repo.documentRepairs))
	}
	call := repo.documentRepairs[0]
	if call.generation != generation || call.owner != owner || call.knowledgeBaseID != "kb-1" {
		t.Fatalf("repair identity = %#v", call)
	}
	if got := call.values["parse_status"]; got != types.ParseStatusFailed {
		t.Fatalf("parse_status = %#v, want failed", got)
	}

	repo.documentRepairs = nil
	repo.knowledge.ProcessingGeneration = "newer-generation"
	repo.knowledge.ProcessingOwner = processownership.DocumentOwner("knowledge-1", "newer-generation")
	if err := svc.Repair(
		context.Background(),
		taskWithPayload(t, types.TypeKnowledgeListReparse, payload),
		errors.New("late exhausted child"),
	); err != nil {
		t.Fatalf("stale Repair() error = %v", err)
	}
	if len(repo.documentRepairs) != 0 {
		t.Fatalf("stale child repaired newer generation: %#v", repo.documentRepairs)
	}
}

func TestKnowledgeListReparseParentRepairIsNoop(t *testing.T) {
	repo := &repairRepoFake{}
	svc := New(repo, nil, nil)
	payload := types.KnowledgeListReparsePayload{
		TenantID: 7, KnowledgeIDs: []string{"knowledge-1", "knowledge-2"}, BatchID: "batch-1",
	}
	if err := svc.Repair(
		context.Background(),
		taskWithPayload(t, types.TypeKnowledgeListReparse, payload),
		errors.New("parent dispatch exhausted"),
	); err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if len(repo.documentRepairs) != 0 {
		t.Fatalf("parent must not repair a document row: %#v", repo.documentRepairs)
	}
}
