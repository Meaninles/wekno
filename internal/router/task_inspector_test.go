package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const taskInspectorPendingOpsDDL = `
CREATE TABLE task_pending_ops (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id   INTEGER NOT NULL,
    task_type   VARCHAR(64) NOT NULL,
    scope       VARCHAR(32) NOT NULL,
    scope_id    VARCHAR(64) NOT NULL,
    op          VARCHAR(32) NOT NULL,
    dedup_key   VARCHAR(128) NOT NULL DEFAULT '',
    payload     TEXT NOT NULL DEFAULT '{}',
    fail_count  INTEGER NOT NULL DEFAULT 0,
    enqueued_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    claimed_at  DATETIME
);
`

// TestTaskInspector_DurableWikiPendingIntegration intentionally uses the real
// GORM repository and the production inspector implementation. It protects
// the cross-layer contract housekeeping depends on: wiki triggers themselves
// are KB-scoped, while the per-document identity is the PostgreSQL row's
// dedup_key, and only an ingest op for the exact KB/document is live work.
func TestTaskInspector_DurableWikiPendingIntegration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(taskInspectorPendingOpsDDL).Error)
	require.NoError(t, db.Exec(`CREATE TABLE knowledge_bases (
		id VARCHAR(64) PRIMARY KEY, tenant_id INTEGER NOT NULL, deleted_at DATETIME
	)`).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO knowledge_bases (id, tenant_id) VALUES (?, ?)", "kb-A", 1,
	).Error)

	pendingRepo := repository.NewTaskPendingOpsRepository(db)
	inspector := NewNoopTaskInspector(pendingRepo, NewSyncTaskExecutor())
	ctx := context.Background()

	ingest := &types.TaskPendingOp{
		TenantID: 1,
		TaskType: types.TypeWikiIngest,
		Scope:    types.TaskScopeKnowledgeBase,
		ScopeID:  "kb-A",
		Op:       "ingest",
		DedupKey: "kid-A:generation-A",
	}
	require.NoError(t, pendingRepo.Enqueue(ctx, ingest))
	require.NoError(t, pendingRepo.Enqueue(ctx, &types.TaskPendingOp{
		TenantID: 1,
		TaskType: types.TypeWikiIngest,
		Scope:    types.TaskScopeKnowledgeBase,
		ScopeID:  "kb-A",
		Op:       "retract",
		DedupKey: "kid-retract-only",
	}))

	queued, err := inspector.HasQueuedTasksForKnowledge(ctx, "kb-A", "kid-A")
	require.NoError(t, err)
	assert.True(t, queued)

	queued, err = inspector.HasQueuedTasksForKnowledge(ctx, "kb-B", "kid-A")
	require.NoError(t, err)
	assert.False(t, queued, "same knowledge key under another KB must not match")

	queued, err = inspector.HasQueuedTasksForKnowledge(ctx, "kb-A", "kid-retract-only")
	require.NoError(t, err)
	assert.False(t, queued, "retraction is cleanup, not live document enrichment")

	batch, err := inspector.QueuedKnowledgeIDs(ctx, []interfaces.KnowledgeTaskTarget{
		{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-A"},
		{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-retract-only"},
		{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-missing"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"kid-A": true}, batch,
		"the broad batch probe must report only exact live ingest ops")

	lifecycleOwners, err := inspector.DocumentLifecycleTaskKnowledgeIDs(ctx, []interfaces.KnowledgeTaskTarget{
		{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-A"},
	})
	require.NoError(t, err)
	assert.Empty(t, lifecycleOwners,
		"durable Wiki work is live work, but never a document finalization counter owner")

	require.NoError(t, pendingRepo.DeleteByIDs(ctx, []int64{ingest.ID}))
	queued, err = inspector.HasQueuedTasksForKnowledge(ctx, "kb-A", "kid-A")
	require.NoError(t, err)
	assert.False(t, queued, "consumed pending row must stop protecting the document")
}

func TestTaskInspector_LiteWikiKnowledgeBaseCancelAndQuiescence(t *testing.T) {
	executor := NewSyncTaskExecutor()
	ctxA, cancelA := context.WithCancel(context.Background())
	_, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	executor.tasks["wiki-a"] = &syncTaskRecord{
		taskType: types.TypeWikiIngest,
		payload:  []byte(`{"tenant_id":7,"knowledge_base_id":"kb-a"}`),
		cancel:   cancelA,
		active:   true,
	}
	executor.tasks["wiki-b"] = &syncTaskRecord{
		taskType: types.TypeWikiIngest,
		payload:  []byte(`{"tenant_id":7,"knowledge_base_id":"kb-b"}`),
		cancel:   cancelB,
	}
	inspector := &asynqTaskInspector{syncTasks: executor}

	live, err := inspector.HasWikiTasksForKnowledgeBase(context.Background(), "kb-a")
	require.NoError(t, err)
	require.True(t, live)
	deleted, cancelled, err := inspector.CancelWikiTasksForKnowledgeBase(context.Background(), "kb-a")
	require.NoError(t, err)
	require.Zero(t, deleted)
	require.Equal(t, 1, cancelled)
	select {
	case <-ctxA.Done():
	case <-time.After(time.Second):
		t.Fatal("active Lite Wiki task did not receive cancellation")
	}

	// Cancellation is not quiescence: the task remains visible until its
	// handler exits and the executor removes the live record.
	live, err = inspector.HasWikiTasksForKnowledgeBase(context.Background(), "kb-a")
	require.NoError(t, err)
	require.True(t, live)
	executor.mu.Lock()
	delete(executor.tasks, "wiki-a")
	executor.mu.Unlock()
	live, err = inspector.HasWikiTasksForKnowledgeBase(context.Background(), "kb-a")
	require.NoError(t, err)
	require.False(t, live)
	live, err = inspector.HasWikiTasksForKnowledgeBase(context.Background(), "kb-b")
	require.NoError(t, err)
	require.True(t, live, "another KB's Wiki task must remain untouched")

	executor.tasks["malformed"] = &syncTaskRecord{
		taskType: types.TypeWikiIngest, payload: []byte(`{"tenant_id":7}`), cancel: func() {},
	}
	_, err = inspector.HasWikiTasksForKnowledgeBase(context.Background(), "kb-a")
	require.ErrorContains(t, err, "knowledge_base_id")
}

func TestTaskInspector_SummaryProbeFiltersTaskTypes(t *testing.T) {
	payload, err := json.Marshal(knowledgeIDsProbe{KnowledgeID: "kid-summary"})
	require.NoError(t, err)
	summaryOnly := map[string]struct{}{types.TypeSummaryGeneration: {}}

	knowledgeIDs, ok := taskKnowledgeIDsForTypes(
		types.TypeSummaryGeneration, payload, summaryOnly,
	)
	assert.True(t, ok)
	assert.Equal(t, []string{"kid-summary"}, knowledgeIDs)

	_, ok = taskKnowledgeIDsForTypes(types.TypeQuestionGeneration, payload, summaryOnly)
	assert.False(t, ok,
		"summary housekeeping must not treat an unrelated enrichment task as summary liveness")

	_, ok = taskKnowledgeIDsForTypes(types.TypeSummaryGeneration, []byte("not-json"), summaryOnly)
	assert.False(t, ok,
		"an unparseable payload cannot be attributed to a summary candidate")
}

func TestTaskInspector_StrictSingleAndMultiKnowledgeIdentity(t *testing.T) {
	t.Parallel()

	marshal := func(value any) []byte {
		t.Helper()
		payload, err := json.Marshal(value)
		require.NoError(t, err)
		return payload
	}
	tests := []struct {
		name        string
		taskType    string
		payload     []byte
		wantIDs     []string
		wantTracked bool
		wantErr     bool
	}{
		{
			name:        "data table summary owns one document",
			taskType:    types.TypeDataTableSummary,
			payload:     marshal(knowledgeIDsProbe{KnowledgeID: "kid-table"}),
			wantIDs:     []string{"kid-table"},
			wantTracked: true,
		},
		{
			name:        "FAQ import owns its materialized knowledge",
			taskType:    types.TypeFAQImport,
			payload:     marshal(knowledgeIDsProbe{KnowledgeID: "kid-faq"}),
			wantIDs:     []string{"kid-faq"},
			wantTracked: true,
		},
		{
			name:        "FAQ dry run owns no document",
			taskType:    types.TypeFAQImport,
			payload:     marshal(knowledgeIDsProbe{DryRun: true}),
			wantTracked: false,
		},
		{
			name:        "move owns all unique documents",
			taskType:    types.TypeKnowledgeMove,
			payload:     marshal(knowledgeIDsProbe{KnowledgeIDs: []string{"kid-a", "kid-b", "kid-a"}}),
			wantIDs:     []string{"kid-a", "kid-b"},
			wantTracked: true,
		},
		{
			name:        "list reparse owns all documents",
			taskType:    types.TypeKnowledgeListReparse,
			payload:     marshal(knowledgeIDsProbe{KnowledgeIDs: []string{"kid-a", "kid-b"}}),
			wantIDs:     []string{"kid-a", "kid-b"},
			wantTracked: true,
		},
		{
			name:        "non dry FAQ without identity is unknown",
			taskType:    types.TypeFAQImport,
			payload:     marshal(knowledgeIDsProbe{}),
			wantTracked: true,
			wantErr:     true,
		},
		{
			name:        "multi identity rejects empty member",
			taskType:    types.TypeKnowledgeMove,
			payload:     marshal(knowledgeIDsProbe{KnowledgeIDs: []string{"kid-a", ""}}),
			wantTracked: true,
			wantErr:     true,
		},
		{
			name:        "multi task rejects single identity field",
			taskType:    types.TypeKnowledgeListReparse,
			payload:     marshal(knowledgeIDsProbe{KnowledgeID: "kid-a"}),
			wantTracked: true,
			wantErr:     true,
		},
		{
			name:        "owned malformed payload is unknown",
			taskType:    types.TypeDataTableSummary,
			payload:     []byte("not-json"),
			wantTracked: true,
			wantErr:     true,
		},
		{
			name:        "unowned type is ignored without decoding",
			taskType:    types.TypeWikiIngest,
			payload:     []byte("not-json"),
			wantTracked: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ids, tracked, err := taskKnowledgeIDsForTypesStrict(
				test.taskType, test.payload, taskTypesForDocumentLifecycle,
			)
			assert.Equal(t, test.wantTracked, tracked)
			assert.Equal(t, test.wantIDs, ids)
			if test.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestTaskInspector_BatchTaskIsVisibleButNotIndividuallyCancelable(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(knowledgeIDsProbe{KnowledgeIDs: []string{"kid-a", "kid-b"}})
	require.NoError(t, err)

	ids, tracked := taskKnowledgeIDs(types.TypeKnowledgeMove, payload)
	assert.True(t, tracked)
	assert.Equal(t, []string{"kid-a", "kid-b"}, ids)
	assert.False(t, matchesKnowledge(types.TypeKnowledgeMove, payload, "kid-a"))
	assert.False(t, matchesKnowledge(types.TypeKnowledgeMove, payload, "kid-b"))
	assert.False(t, matchesKnowledge(types.TypeKnowledgeMove, payload, "kid-c"))
}

func TestTaskInspector_MissingDurableRepositoryReturnsUnknown(t *testing.T) {
	inspector := NewNoopTaskInspector(nil, NewSyncTaskExecutor())
	queued, err := inspector.HasQueuedTasksForKnowledge(
		context.Background(), "kb-A", "kid-A")
	assert.False(t, queued)
	assert.Error(t, err,
		"missing PG queue visibility must be surfaced, never collapsed into no live work")
}

func TestTaskInspector_MissingIdentityReturnsUnknown(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(taskInspectorPendingOpsDDL).Error)
	inspector := NewNoopTaskInspector(repository.NewTaskPendingOpsRepository(db), NewSyncTaskExecutor())

	queued, err := inspector.HasQueuedTasksForKnowledge(
		context.Background(), "", "kid-A")
	assert.False(t, queued)
	assert.Error(t, err,
		"an incomplete identity cannot safely prove that no durable task exists")
}

func TestTaskInspector_ConflictingBatchIdentityReturnsUnknown(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(taskInspectorPendingOpsDDL).Error)
	inspector := NewNoopTaskInspector(repository.NewTaskPendingOpsRepository(db), NewSyncTaskExecutor())

	queued, err := inspector.QueuedKnowledgeIDs(context.Background(), []interfaces.KnowledgeTaskTarget{
		{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-A"},
		{KnowledgeBaseID: "kb-B", KnowledgeID: "kid-A"},
	})
	assert.Nil(t, queued)
	assert.Error(t, err,
		"a conflicting compound identity must remain unknown instead of probing the wrong KB")
}

type fakeLiveTaskSnapshotter struct {
	idsByQueue map[string][]string
	errByQueue map[string]error
	calls      []string
}

func (f *fakeLiveTaskSnapshotter) LiveTaskIDs(
	_ context.Context,
	queue string,
) ([]string, error) {
	f.calls = append(f.calls, queue)
	if err := f.errByQueue[queue]; err != nil {
		return nil, err
	}
	return f.idsByQueue[queue], nil
}

type fakeAsynqTaskInfoReader struct {
	infoByID map[string]*asynq.TaskInfo
	errByID  map[string]error
	calls    []string
}

func (f *fakeAsynqTaskInfoReader) GetTaskInfo(queue, id string) (*asynq.TaskInfo, error) {
	f.calls = append(f.calls, queue+"/"+id)
	if err := f.errByID[id]; err != nil {
		return nil, err
	}
	info, ok := f.infoByID[id]
	if !ok {
		return nil, asynq.ErrTaskNotFound
	}
	return info, nil
}

func taskInfoForKnowledge(t *testing.T, id, taskType, knowledgeID string) *asynq.TaskInfo {
	t.Helper()
	payload, err := json.Marshal(knowledgeIDsProbe{KnowledgeID: knowledgeID})
	require.NoError(t, err)
	return &asynq.TaskInfo{ID: id, Type: taskType, Payload: payload}
}

func TestTaskInspector_AtomicSnapshotFindsLiveTask(t *testing.T) {
	snapshots := &fakeLiveTaskSnapshotter{idsByQueue: map[string][]string{
		types.QueueDefault: {"task-1", "task-1"},
	}}
	reader := &fakeAsynqTaskInfoReader{infoByID: map[string]*asynq.TaskInfo{
		"task-1": taskInfoForKnowledge(
			t, "task-1", types.TypeKnowledgePostProcess, "kid-live",
		),
	}}
	inspector := &asynqTaskInspector{snapshotter: snapshots, taskInfo: reader}

	queued, err := inspector.DocumentLifecycleTaskKnowledgeIDs(
		context.Background(),
		[]interfaces.KnowledgeTaskTarget{{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-live"}},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"kid-live": true}, queued)
	assert.Equal(t, []string{types.QueueDefault}, snapshots.calls,
		"a queue's four live states must be represented by one snapshot call")
	assert.Equal(t, []string{types.QueueDefault + "/task-1"}, reader.calls,
		"duplicate/corrupt state membership must not trigger inconsistent rereads")
}

func TestTaskInspector_AtomicSnapshotFindsEveryBatchOwner(t *testing.T) {
	payload, err := json.Marshal(knowledgeIDsProbe{KnowledgeIDs: []string{"kid-a", "kid-b"}})
	require.NoError(t, err)
	snapshots := &fakeLiveTaskSnapshotter{idsByQueue: map[string][]string{
		types.QueueLow: {"batch-reparse"},
	}}
	reader := &fakeAsynqTaskInfoReader{infoByID: map[string]*asynq.TaskInfo{
		"batch-reparse": {
			ID:      "batch-reparse",
			Type:    types.TypeKnowledgeListReparse,
			Payload: payload,
		},
	}}
	inspector := &asynqTaskInspector{snapshotter: snapshots, taskInfo: reader}

	queued, err := inspector.DocumentLifecycleTaskKnowledgeIDs(
		context.Background(),
		[]interfaces.KnowledgeTaskTarget{
			{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-a"},
			{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-b"},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"kid-a": true, "kid-b": true}, queued)
}

func TestLiteTaskInspectorKeepsCancelledActiveWorkerVisible(t *testing.T) {
	executor := NewSyncTaskExecutor()
	started := make(chan struct{})
	release := make(chan struct{})
	executor.RegisterHandler(types.TypeDataTableSummary, func(ctx context.Context, _ *asynq.Task) error {
		close(started)
		<-release // deliberately ignore cancellation until the simulated write returns
		return ctx.Err()
	})
	payload, err := json.Marshal(knowledgeIDsProbe{KnowledgeID: "kid-table"})
	require.NoError(t, err)
	_, err = executor.Enqueue(
		asynq.NewTask(types.TypeDataTableSummary, payload),
		asynq.MaxRetry(0),
	)
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Lite task did not start")
	}

	inspector := NewNoopTaskInspector(nil, executor)
	targets := []interfaces.KnowledgeTaskTarget{{
		KnowledgeBaseID: "kb-A",
		KnowledgeID:     "kid-table",
	}}
	live, err := inspector.DocumentLifecycleTaskKnowledgeIDs(context.Background(), targets)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"kid-table": true}, live)

	deleted, cancelled, err := inspector.CancelTasksForKnowledge(context.Background(), "kid-table")
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.Equal(t, 1, cancelled)
	live, err = inspector.DocumentLifecycleTaskKnowledgeIDs(context.Background(), targets)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"kid-table": true}, live,
		"cancellation is not quiescence until the active handler returns")

	close(release)
	require.Eventually(t, func() bool {
		live, probeErr := inspector.DocumentLifecycleTaskKnowledgeIDs(context.Background(), targets)
		return probeErr == nil && len(live) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestLiteCancellationOfOneBatchOwnerPreservesOtherDocuments(t *testing.T) {
	executor := NewSyncTaskExecutor()
	ran := make(chan struct{})
	executor.RegisterHandler(types.TypeKnowledgeMove, func(context.Context, *asynq.Task) error {
		close(ran)
		return nil
	})
	payload, err := json.Marshal(knowledgeIDsProbe{KnowledgeIDs: []string{"kid-a", "kid-b"}})
	require.NoError(t, err)
	_, err = executor.Enqueue(
		asynq.NewTask(types.TypeKnowledgeMove, payload),
		asynq.ProcessIn(100*time.Millisecond),
		asynq.MaxRetry(0),
	)
	require.NoError(t, err)

	inspector := NewNoopTaskInspector(nil, executor)
	targets := []interfaces.KnowledgeTaskTarget{
		{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-a"},
		{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-b"},
	}
	live, err := inspector.DocumentLifecycleTaskKnowledgeIDs(context.Background(), targets)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"kid-a": true, "kid-b": true}, live)

	deleted, cancelled, err := inspector.CancelTasksForKnowledge(context.Background(), "kid-a")
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.Zero(t, cancelled)
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("batch controller was lost while deleting one member")
	}
	require.Eventually(t, func() bool {
		live, probeErr := inspector.DocumentLifecycleTaskKnowledgeIDs(context.Background(), targets)
		return probeErr == nil && len(live) == 0
	}, time.Second, 10*time.Millisecond,
		"the preserved controller should finish normally for its surviving documents")
}

func TestTaskInspector_AtomicSnapshotFailureIsUnknown(t *testing.T) {
	snapshotErr := errors.New("redis unavailable")
	snapshots := &fakeLiveTaskSnapshotter{errByQueue: map[string]error{
		types.QueueDefault: snapshotErr,
	}}
	inspector := &asynqTaskInspector{
		snapshotter: snapshots,
		taskInfo:    &fakeAsynqTaskInfoReader{},
	}

	queued, err := inspector.DocumentLifecycleTaskKnowledgeIDs(
		context.Background(),
		[]interfaces.KnowledgeTaskTarget{{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-A"}},
	)
	assert.Nil(t, queued)
	assert.ErrorIs(t, err, snapshotErr,
		"snapshot errors must preserve candidates instead of proving an empty queue")
}

func TestTaskInspector_SnapshottedTaskDisappearanceIsUnknown(t *testing.T) {
	snapshots := &fakeLiveTaskSnapshotter{idsByQueue: map[string][]string{
		types.QueueDefault: {"vanished-task"},
	}}
	reader := &fakeAsynqTaskInfoReader{errByID: map[string]error{
		"vanished-task": asynq.ErrTaskNotFound,
	}}
	inspector := &asynqTaskInspector{snapshotter: snapshots, taskInfo: reader}

	queued, err := inspector.DocumentLifecycleTaskKnowledgeIDs(
		context.Background(),
		[]interfaces.KnowledgeTaskTarget{{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-A"}},
	)
	assert.Nil(t, queued)
	assert.ErrorIs(t, err, asynq.ErrTaskNotFound,
		"a task live at snapshot time cannot silently disappear before payload attribution")
}

func TestTaskInspector_SnapshottedMalformedOwnedPayloadIsUnknown(t *testing.T) {
	snapshots := &fakeLiveTaskSnapshotter{idsByQueue: map[string][]string{
		types.QueueDefault: {"bad-task"},
	}}
	reader := &fakeAsynqTaskInfoReader{infoByID: map[string]*asynq.TaskInfo{
		"bad-task": {
			ID:      "bad-task",
			Type:    types.TypeSummaryGeneration,
			Payload: []byte("not-json"),
		},
	}}
	inspector := &asynqTaskInspector{snapshotter: snapshots, taskInfo: reader}

	queued, err := inspector.SummaryTaskKnowledgeIDs(
		context.Background(),
		[]interfaces.KnowledgeTaskTarget{{KnowledgeBaseID: "kb-A", KnowledgeID: "kid-A"}},
	)
	assert.Nil(t, queued)
	assert.Error(t, err,
		"an owned task with unreadable identity must fail closed")
}

func TestAsynqLiveStateKeysShareQueueHashTag(t *testing.T) {
	keys := asynqLiveStateKeys(types.QueueDocumentHeavy)
	assert.Equal(t, []string{
		"asynq:{document_heavy}:pending",
		"asynq:{document_heavy}:active",
		"asynq:{document_heavy}:scheduled",
		"asynq:{document_heavy}:retry",
	}, keys)
	for _, key := range keys {
		assert.Contains(t, key, "{"+types.QueueDocumentHeavy+"}",
			"all Lua KEYS must share one Redis Cluster hash slot")
	}
}

func TestParseLiveTaskIDSnapshot(t *testing.T) {
	ids, err := parseLiveTaskIDSnapshot([]interface{}{"pending", []byte("active")})
	require.NoError(t, err)
	assert.Equal(t, []string{"pending", "active"}, ids)

	_, err = parseLiveTaskIDSnapshot([]interface{}{"good", int64(1)})
	assert.Error(t, err, "unexpected Redis response types must fail closed")

	_, err = parseLiveTaskIDSnapshot("not-an-array")
	assert.Error(t, err, "unexpected Redis response shapes must fail closed")
}

func TestRedisLiveTaskSnapshotterIntegration(t *testing.T) {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		t.Skip("REDIS_ADDR is not configured")
	}
	db, _ := strconv.Atoi(os.Getenv("REDIS_DB"))
	client := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Username: os.Getenv("REDIS_USERNAME"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       db,
	})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err())

	queue := fmt.Sprintf("task_inspector_test_%d", time.Now().UnixNano())
	keys := asynqLiveStateKeys(queue)
	t.Cleanup(func() { _ = client.Del(context.Background(), keys...).Err() })
	require.NoError(t, client.RPush(ctx, keys[0], "pending-1").Err())
	require.NoError(t, client.RPush(ctx, keys[1], "active-1").Err())
	require.NoError(t, client.ZAdd(ctx, keys[2], redis.Z{Score: 1, Member: "scheduled-1"}).Err())
	require.NoError(t, client.ZAdd(ctx, keys[3], redis.Z{Score: 1, Member: "retry-1"}).Err())

	snapshotter := &redisLiveTaskSnapshotter{client: client}
	ids, err := snapshotter.LiveTaskIDs(ctx, queue)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"pending-1", "active-1", "scheduled-1", "retry-1",
	}, ids, "all four live states must be captured by one Lua invocation")

	_, err = liveTaskSnapshotScript.Run(ctx, client, keys, 3).Result()
	assert.Error(t, err,
		"an oversized atomic snapshot must fail closed before any unbounded range read")
}
