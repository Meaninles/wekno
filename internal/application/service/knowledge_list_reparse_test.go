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
)

type partialReparseChildEnqueuer struct {
	mu          sync.Mutex
	persisted   map[string]string
	failOnceFor string
	failed      bool
	conflicts   int
}

func (e *partialReparseChildEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var taskID string
	for _, opt := range opts {
		if opt.Type() == asynq.TaskIDOpt {
			taskID, _ = opt.Value().(string)
		}
	}
	var payload types.KnowledgeListReparsePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return nil, err
	}
	if len(payload.KnowledgeIDs) != 1 {
		return nil, errors.New("child must contain exactly one knowledge ID")
	}
	id := payload.KnowledgeIDs[0]
	wantGeneration, wantOwner := processownership.BatchReparseIdentity(payload.TenantID, payload.BatchID, id)
	if payload.ProcessingGeneration != wantGeneration || payload.ProcessingOwner != wantOwner {
		return nil, errors.New("child must contain its deterministic generation identity")
	}
	if payload.ExpectedSnapshot == nil || payload.ExpectedSnapshots != nil {
		return nil, errors.New("child must carry only its one durable expected snapshot")
	}
	if id == e.failOnceFor && !e.failed {
		e.failed = true
		return nil, errors.New("transient enqueue failure")
	}
	if _, exists := e.persisted[taskID]; exists {
		e.conflicts++
		return nil, asynq.ErrTaskIDConflict
	}
	e.persisted[taskID] = id
	return &asynq.TaskInfo{ID: taskID, Type: task.Type()}, nil
}

type batchReparseClaimOutcome struct {
	commit bool
	err    error
}

type batchReparseSnapshotRepoFake struct {
	interfaces.KnowledgeRepository
	mu       sync.Mutex
	rows     map[string]*types.Knowledge
	outcomes []batchReparseClaimOutcome
}

func (r *batchReparseSnapshotRepoFake) GetKnowledgeByID(
	_ context.Context, tenantID uint64, id string,
) (*types.Knowledge, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.rows[id]
	if row == nil || row.TenantID != tenantID {
		return nil, errors.New("knowledge not found")
	}
	copy := *row
	return &copy, nil
}

func (r *batchReparseSnapshotRepoFake) CompareAndSwapBatchReparseSnapshot(
	_ context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	expectedUpdatedAt time.Time,
	values map[string]interface{},
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.rows[id]
	if row == nil || row.TenantID != tenantID || row.KnowledgeBaseID != expectedKnowledgeBaseID ||
		row.ParseStatus != expectedParseStatus || row.ProcessingGeneration != expectedGeneration ||
		row.ProcessingOwner != expectedOwner || !row.UpdatedAt.Equal(expectedUpdatedAt) {
		return false, nil
	}
	outcome := batchReparseClaimOutcome{commit: true}
	if len(r.outcomes) > 0 {
		outcome = r.outcomes[0]
		r.outcomes = r.outcomes[1:]
	}
	if outcome.commit {
		applyBatchReparseClaimValues(row, values)
	}
	if outcome.err != nil {
		return false, outcome.err
	}
	return outcome.commit, nil
}

func applyBatchReparseClaimValues(row *types.Knowledge, values map[string]interface{}) {
	if value, ok := values["parse_status"].(string); ok {
		row.ParseStatus = value
	}
	if value, ok := values["processing_generation"].(string); ok {
		row.ProcessingGeneration = value
	}
	if value, ok := values["processing_owner"].(string); ok {
		row.ProcessingOwner = value
	}
	if value, ok := values["processing_workflow_id"].(string); ok {
		row.ProcessingWorkflowID = value
	}
	if value, ok := values["error_message"].(string); ok {
		row.ErrorMessage = value
	}
	if value, ok := values["updated_at"].(time.Time); ok {
		row.UpdatedAt = value
	}
	if _, ok := values["processed_at"]; ok {
		row.ProcessedAt = nil
	}
}

func newBatchReparseSnapshotFixture(t *testing.T) (*batchReparseSnapshotRepoFake, types.KnowledgeListReparsePayload) {
	t.Helper()
	rows := make(map[string]*types.Knowledge)
	snapshots := make(map[string]types.KnowledgeReparseExpectedSnapshot)
	ids := []string{"knowledge-a", "knowledge-b", "knowledge-c"}
	for i, id := range ids {
		row := &types.Knowledge{
			ID: id, TenantID: 42, KnowledgeBaseID: "kb-1",
			ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "old-" + id,
			UpdatedAt: time.Date(2026, 7, 22, 1, 2, 3, i*1000, time.UTC),
		}
		rows[id] = row
		snapshot, err := processownership.CaptureBatchReparseSnapshot(row)
		if err != nil {
			t.Fatal(err)
		}
		snapshots[id] = snapshot
	}
	return &batchReparseSnapshotRepoFake{rows: rows}, types.KnowledgeListReparsePayload{
		TenantID: 42, KnowledgeIDs: ids, BatchID: "batch-1", ExpectedSnapshots: snapshots,
	}
}

func TestDispatchKnowledgeListReparseChildrenPartialRetryIsStable(t *testing.T) {
	repo, payload := newBatchReparseSnapshotFixture(t)
	enqueuer := &partialReparseChildEnqueuer{
		persisted:   make(map[string]string),
		failOnceFor: "knowledge-b",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	if err := dispatchKnowledgeListReparseChildren(context.Background(), repo, enqueuer, payload, raw); err == nil {
		t.Fatal("first fan-out must report the partial enqueue failure")
	}
	if got := len(enqueuer.persisted); got != 2 {
		t.Fatalf("persisted children after partial failure = %d, want 2", got)
	}
	if err := dispatchKnowledgeListReparseChildren(context.Background(), repo, enqueuer, payload, raw); err != nil {
		t.Fatalf("retry fan-out error = %v", err)
	}
	if got := len(enqueuer.persisted); got != 3 {
		t.Fatalf("persisted children after retry = %d, want 3", got)
	}
	if enqueuer.conflicts != 2 {
		t.Fatalf("stable child TaskID conflicts on parent retry = %d, want 2", enqueuer.conflicts)
	}
	seen := make(map[string]int)
	for _, id := range enqueuer.persisted {
		seen[id]++
	}
	for _, id := range payload.KnowledgeIDs {
		if seen[id] != 1 {
			t.Fatalf("knowledge %s has %d durable child tasks, want exactly 1", id, seen[id])
		}
	}
}

func TestDispatchKnowledgeListReparseChildrenPartialRetryDoesNotRefreshSnapshot(t *testing.T) {
	repo, payload := newBatchReparseSnapshotFixture(t)
	enqueuer := &partialReparseChildEnqueuer{
		persisted: make(map[string]string), failOnceFor: "knowledge-b",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchKnowledgeListReparseChildren(
		context.Background(), repo, enqueuer, payload, raw,
	); err == nil {
		t.Fatal("first fan-out must report the partial enqueue failure")
	}
	repo.mu.Lock()
	repo.rows["knowledge-b"].ProcessingGeneration = "newer-generation"
	repo.rows["knowledge-b"].ProcessingOwner = processownership.DocumentOwner("knowledge-b", "newer-generation")
	repo.rows["knowledge-b"].UpdatedAt = repo.rows["knowledge-b"].UpdatedAt.Add(time.Second)
	repo.mu.Unlock()
	if err := dispatchKnowledgeListReparseChildren(
		context.Background(), repo, enqueuer, payload, raw,
	); err != nil {
		t.Fatalf("parent retry should ACK the stale unpersisted item: %v", err)
	}
	if got := len(enqueuer.persisted); got != 2 {
		t.Fatalf("old parent adopted a newer generation: persisted=%d, want original 2", got)
	}
}

func TestBatchReparseMarkerAttemptIsGenerationFenced(t *testing.T) {
	marker := batchReparseMarker(batchReparsePreparing, "generation-a", 7)
	if attempt, ok := batchReparseMarkerAttempt(marker, batchReparsePreparing, "generation-a"); !ok || attempt != 7 {
		t.Fatalf("marker attempt = %d,%v, want 7,true", attempt, ok)
	}
	if _, ok := batchReparseMarkerAttempt(marker, batchReparsePreparing, "generation-b"); ok {
		t.Fatal("a newer generation parsed an older generation's preparation marker")
	}
	if _, ok := batchReparseMarkerAttempt(
		batchReparseMarker(batchReparseReady, "generation-a", 7),
		batchReparsePreparing,
		"generation-a",
	); ok {
		t.Fatal("ready marker was mistaken for a preparation marker")
	}
}

func TestBatchReparseClaimErrorWithoutCommitRetriesSameExpectedSnapshot(t *testing.T) {
	repo, payload := newBatchReparseSnapshotFixture(t)
	id := payload.KnowledgeIDs[0]
	expected := payload.ExpectedSnapshots[id]
	generation, owner := processownership.BatchReparseIdentity(payload.TenantID, payload.BatchID, id)
	repo.outcomes = []batchReparseClaimOutcome{
		{err: errors.New("transient database error")},
		{commit: true},
	}
	values := batchReparseClaimTestValues(generation, owner)

	current, _ := repo.GetKnowledgeByID(context.Background(), payload.TenantID, id)
	resolution, _, err := resolveBatchReparseChild(current, generation, owner, &expected)
	if err != nil || resolution != batchReparseClaimExpected {
		t.Fatalf("initial resolution = %v, %v", resolution, err)
	}
	if _, err := claimBatchReparseExpectedSnapshot(context.Background(), repo, expected, values); err == nil {
		t.Fatal("first claim must return the injected transient error")
	}

	current, _ = repo.GetKnowledgeByID(context.Background(), payload.TenantID, id)
	resolution, _, err = resolveBatchReparseChild(current, generation, owner, &expected)
	if err != nil || resolution != batchReparseClaimExpected {
		t.Fatalf("retry resolution after no-commit error = %v, %v; want claim", resolution, err)
	}
	swapped, err := claimBatchReparseExpectedSnapshot(context.Background(), repo, expected, values)
	if err != nil || !swapped {
		t.Fatalf("retry claim = %v, %v; want true,nil", swapped, err)
	}
	current, _ = repo.GetKnowledgeByID(context.Background(), payload.TenantID, id)
	resolution, _, err = resolveBatchReparseChild(current, generation, owner, &expected)
	if err != nil || resolution != batchReparseResumePreparation {
		t.Fatalf("post-claim resolution = %v, %v; want resume preparation", resolution, err)
	}
}

func TestBatchReparseUncertainCommitResumesOwnGeneration(t *testing.T) {
	repo, payload := newBatchReparseSnapshotFixture(t)
	id := payload.KnowledgeIDs[0]
	repo.rows[id].ProcessingWorkflowID = "workflow-from-previous-generation"
	expected := payload.ExpectedSnapshots[id]
	generation, owner := processownership.BatchReparseIdentity(payload.TenantID, payload.BatchID, id)
	repo.outcomes = []batchReparseClaimOutcome{{commit: true, err: errors.New("commit response lost")}}
	if _, err := claimBatchReparseExpectedSnapshot(
		context.Background(), repo, expected, batchReparseClaimTestValues(generation, owner),
	); err == nil {
		t.Fatal("uncertain commit must surface its response error")
	}
	current, _ := repo.GetKnowledgeByID(context.Background(), payload.TenantID, id)
	if current.ProcessingWorkflowID != "" {
		t.Fatalf("new generation retained previous workflow binding %q", current.ProcessingWorkflowID)
	}
	resolution, _, err := resolveBatchReparseChild(current, generation, owner, &expected)
	if err != nil || resolution != batchReparseResumePreparation {
		t.Fatalf("uncertain-commit retry resolution = %v, %v; want resume preparation", resolution, err)
	}
}

func TestBatchReparseOldChildSkipsNewGenerationAndUpdatedAtABA(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*types.Knowledge)
	}{
		{
			name: "newer generation",
			mutate: func(row *types.Knowledge) {
				row.ProcessingGeneration = "newer-generation"
				row.ProcessingOwner = processownership.DocumentOwner(row.ID, row.ProcessingGeneration)
				row.UpdatedAt = row.UpdatedAt.Add(time.Second)
			},
		},
		{
			name: "status ABA with newer timestamp",
			mutate: func(row *types.Knowledge) {
				row.UpdatedAt = row.UpdatedAt.Add(time.Microsecond)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, payload := newBatchReparseSnapshotFixture(t)
			id := payload.KnowledgeIDs[0]
			expected := payload.ExpectedSnapshots[id]
			generation, owner := processownership.BatchReparseIdentity(payload.TenantID, payload.BatchID, id)
			repo.mu.Lock()
			test.mutate(repo.rows[id])
			repo.mu.Unlock()
			current, _ := repo.GetKnowledgeByID(context.Background(), payload.TenantID, id)
			resolution, _, err := resolveBatchReparseChild(current, generation, owner, &expected)
			if err != nil || resolution != batchReparseStale {
				t.Fatalf("resolution = %v, %v; want stale,nil", resolution, err)
			}
		})
	}
}

func batchReparseClaimTestValues(generation, owner string) map[string]interface{} {
	return map[string]interface{}{
		"parse_status":           types.ParseStatusProcessing,
		"processed_at":           nil,
		"processing_generation":  generation,
		"processing_owner":       owner,
		"processing_workflow_id": "",
		"error_message":          batchReparseMarker(batchReparsePreparing, generation, 0),
		"updated_at":             time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC),
	}
}
