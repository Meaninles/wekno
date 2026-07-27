package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

type cancelRaceKnowledgeRepository struct {
	interfaces.KnowledgeRepository
	mu        sync.Mutex
	knowledge types.Knowledge
	claimed   chan struct{}
	claimOnce sync.Once
}

func (r *cancelRaceKnowledgeRepository) GetKnowledgeByID(
	_ context.Context,
	tenantID uint64,
	knowledgeID string,
) (*types.Knowledge, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.knowledge.TenantID != tenantID || r.knowledge.ID != knowledgeID {
		return nil, nil
	}
	copy := r.knowledge
	return &copy, nil
}

func (r *cancelRaceKnowledgeRepository) CompareAndSwapKnowledgeProcessingGeneration(
	_ context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	generation string,
	expectedStatuses []string,
	values map[string]interface{},
) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.knowledge.TenantID != tenantID || r.knowledge.ID != knowledgeID ||
		r.knowledge.KnowledgeBaseID != knowledgeBaseID ||
		r.knowledge.ProcessingGeneration != generation ||
		!cancelRaceContainsString(expectedStatuses, r.knowledge.ParseStatus) {
		return false, nil
	}
	if status, ok := values["parse_status"].(string); ok {
		r.knowledge.ParseStatus = status
	}
	if message, ok := values["error_message"].(string); ok {
		r.knowledge.ErrorMessage = message
	}
	if owner, ok := values["processing_owner"].(string); ok {
		r.knowledge.ProcessingOwner = owner
	}
	if _, ok := values["processing_fanout"]; ok {
		r.knowledge.ProcessingFanout = nil
	}
	if count, ok := values["pending_subtasks_count"].(int); ok {
		r.knowledge.PendingSubtasksCount = count
	}
	if status, ok := values["summary_status"].(string); ok {
		r.knowledge.SummaryStatus = status
	}
	if status, ok := values["enrichment_status"].(string); ok {
		r.knowledge.EnrichmentStatus = status
	}
	if status, ok := values["wiki_status"].(string); ok {
		r.knowledge.WikiStatus = status
	}
	if message, ok := values["wiki_error_message"].(string); ok {
		r.knowledge.WikiErrorMessage = message
	}
	if r.knowledge.ParseStatus == types.ParseStatusCancelling {
		r.claimOnce.Do(func() { close(r.claimed) })
	}
	return true, nil
}

func (r *cancelRaceKnowledgeRepository) snapshot() types.Knowledge {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.knowledge
}

func cancelRaceContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type cancelRaceTaskInspector struct {
	interfaces.TaskInspector
	mu     sync.Mutex
	active bool
}

func (i *cancelRaceTaskInspector) CancelTasksForKnowledge(
	context.Context,
	string,
) (int, int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.active {
		return 0, 1, nil
	}
	return 0, 0, nil
}

func (i *cancelRaceTaskInspector) DocumentLifecycleTaskKnowledgeIDs(
	_ context.Context,
	targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	live := make(map[string]bool)
	if i.active && len(targets) > 0 {
		live[targets[0].KnowledgeID] = true
	}
	return live, nil
}

func (i *cancelRaceTaskInspector) DeletionTaskKnowledgeIDs(
	ctx context.Context,
	targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	return i.DocumentLifecycleTaskKnowledgeIDs(ctx, targets)
}

func (i *cancelRaceTaskInspector) finishLateWrite() {
	i.mu.Lock()
	i.active = false
	i.mu.Unlock()
}

func TestCancelQuiescenceBlocksImmediateReparseUntilLateWriterExits(t *testing.T) {
	repo := &cancelRaceKnowledgeRepository{
		knowledge: types.Knowledge{
			ID:                   "knowledge-cancel-race",
			TenantID:             7,
			KnowledgeBaseID:      "kb-1",
			ParseStatus:          types.ParseStatusProcessing,
			ProcessingGeneration: "generation-1",
			ProcessingOwner:      "document:knowledge-cancel-race:generation-1",
			SummaryStatus:        types.SummaryStatusProcessing,
			EnrichmentStatus:     types.EnrichmentStatusPending,
			WikiStatus:           types.WikiStatusPending,
			WikiErrorMessage:     "old pending error",
		},
		claimed: make(chan struct{}),
	}
	inspector := &cancelRaceTaskInspector{active: true}
	service := &knowledgeService{repo: repo, taskInspector: inspector}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	cancelDone := make(chan error, 1)
	go func() {
		_, err := service.CancelKnowledgeParse(ctx, repo.knowledge.ID)
		cancelDone <- err
	}()

	select {
	case <-repo.claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel never published durable cancelling intent")
	}

	claimed := repo.snapshot()
	require.Equal(t, types.ParseStatusCancelling, claimed.ParseStatus)
	require.NotEmpty(t, claimed.ProcessingOwner, "owner must remain until active writer exits")

	// A user clicking reparse immediately after cancel cannot allocate a new
	// generation while the old worker is still between heartbeat and artifact
	// publication.
	_, reparseErr := service.ReparseKnowledge(ctx, claimed.ID, nil)
	require.Error(t, reparseErr)
	require.Equal(t, "generation-1", repo.snapshot().ProcessingGeneration)

	select {
	case err := <-cancelDone:
		t.Fatalf("cancel crossed quiescence while the late writer was active: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	// The simulated late CreateChunks/BatchIndex call has now returned. Only
	// after two empty queue snapshots may cancellation consume the owner.
	inspector.finishLateWrite()
	select {
	case err := <-cancelDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not finish after lifecycle worker quiesced")
	}

	finished := repo.snapshot()
	require.Equal(t, types.ParseStatusCancelled, finished.ParseStatus)
	require.Empty(t, finished.ProcessingOwner)
	require.Equal(t, "generation-1", finished.ProcessingGeneration)
	require.Equal(t, types.SummaryStatusNone, finished.SummaryStatus)
	require.Equal(t, types.EnrichmentStatusNone, finished.EnrichmentStatus)
	require.Equal(t, types.WikiStatusNone, finished.WikiStatus)
	require.Empty(t, finished.WikiErrorMessage)
}

type disappearingTaskInspector struct {
	interfaces.TaskInspector
	mu              sync.Mutex
	livenessCalls   int
	cancellationIDs []string
}

func (i *disappearingTaskInspector) CancelTasksForKnowledge(
	_ context.Context,
	knowledgeID string,
) (int, int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cancellationIDs = append(i.cancellationIDs, knowledgeID)
	return 0, 0, nil
}

func (i *disappearingTaskInspector) DocumentLifecycleTaskKnowledgeIDs(
	context.Context,
	[]interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.livenessCalls++
	if i.livenessCalls == 1 {
		return nil, errors.Join(
			errors.New("read snapshotted task"),
			asynq.ErrTaskNotFound,
		)
	}
	return map[string]bool{}, nil
}

func (i *disappearingTaskInspector) DeletionTaskKnowledgeIDs(
	ctx context.Context,
	targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	return i.DocumentLifecycleTaskKnowledgeIDs(ctx, targets)
}

func TestCancelQuiescenceRetriesSnapshottedTaskDisappearanceFailClosed(t *testing.T) {
	inspector := &disappearingTaskInspector{}
	knowledge := &types.Knowledge{
		ID:              "knowledge-disappearing-task",
		KnowledgeBaseID: "kb-1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, quiesceKnowledgeDeletionWithInspector(
		ctx,
		inspector,
		[]*types.Knowledge{knowledge},
	))

	inspector.mu.Lock()
	defer inspector.mu.Unlock()
	require.Equal(t, 3, inspector.livenessCalls,
		"one unknown snapshot plus two empty snapshots are required")
	require.Len(t, inspector.cancellationIDs, 2,
		"unknown attribution must repeat cancellation for every target")
	for _, id := range inspector.cancellationIDs {
		require.Equal(t, knowledge.ID, id)
	}
}
