package wikiqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const pendingOpsTestDDL = `
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
    claimed_at  DATETIME,
    map_ready_at DATETIME,
    next_attempt_at DATETIME,
    map_resource_pool_id VARCHAR(64) NOT NULL DEFAULT '',
    map_dispatch_epoch INTEGER NOT NULL DEFAULT 0,
    map_dispatch_task_id VARCHAR(190) NOT NULL DEFAULT '',
    map_dispatch_lease_until DATETIME
);`

const recoveryKnowledgeBasesTestDDL = `
CREATE TABLE knowledge_bases (
    id         VARCHAR(64) PRIMARY KEY,
    tenant_id  INTEGER NOT NULL,
    deleted_at DATETIME
);`

var testDBSequence atomic.Uint64

func newRecoveryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:wikiqueue-recovery-%d?mode=memory&cache=shared", testDBSequence.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.Exec(pendingOpsTestDDL).Error)
	require.NoError(t, db.Exec(recoveryKnowledgeBasesTestDDL).Error)
	return db
}

func insertPendingOp(t *testing.T, db *gorm.DB, tenantID uint64, taskType, scope, scopeID, op string) {
	t.Helper()
	if taskType == types.TypeWikiIngest && scope == types.TaskScopeKnowledgeBase {
		require.NoError(t, db.Exec(
			"INSERT OR IGNORE INTO knowledge_bases (id, tenant_id) VALUES (?, ?)", scopeID, tenantID,
		).Error)
	}
	require.NoError(t, db.Exec(
		`INSERT INTO task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		 VALUES (?, ?, ?, ?, ?, ?, '{}')`,
		tenantID, taskType, scope, scopeID, op, scopeID+"-doc",
	).Error)
}

type enqueueCall struct {
	task *asynq.Task
	opts []asynq.Option
}

type recordingEnqueuer struct {
	mu      sync.Mutex
	calls   []enqueueCall
	errFunc func(call int, task *asynq.Task) error
	called  chan struct{}
}

type activeKeyCheckerStub struct {
	mu     sync.Mutex
	values map[string]int64
	err    error
	keys   []string
}

type resourceCleanerStub struct {
	name    string
	cleanup types.CleanupFunc
}

func (s *resourceCleanerStub) Register(cleanup types.CleanupFunc) {
	s.cleanup = cleanup
}

func (s *resourceCleanerStub) RegisterWithName(name string, cleanup types.CleanupFunc) {
	s.name = name
	s.cleanup = cleanup
}

func (s *resourceCleanerStub) Cleanup(_ context.Context) []error {
	if s.cleanup == nil {
		return nil
	}
	if err := s.cleanup(); err != nil {
		return []error{err}
	}
	return nil
}

func (s *activeKeyCheckerStub) Exists(ctx context.Context, keys ...string) *redis.IntCmd {
	s.mu.Lock()
	s.keys = append(s.keys, keys...)
	value := int64(0)
	for _, key := range keys {
		value += s.values[key]
	}
	err := s.err
	s.mu.Unlock()

	cmd := redis.NewIntCmd(ctx, "exists", keys)
	cmd.SetVal(value)
	cmd.SetErr(err)
	return cmd
}

func (s *activeKeyCheckerStub) snapshotKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.keys)
}

func (e *recordingEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	call := len(e.calls)
	e.calls = append(e.calls, enqueueCall{task: task, opts: slices.Clone(opts)})
	errFunc := e.errFunc
	called := e.called
	e.mu.Unlock()

	if called != nil {
		select {
		case called <- struct{}{}:
		default:
		}
	}
	if errFunc != nil {
		if err := errFunc(call, task); err != nil {
			return nil, err
		}
	}
	return &asynq.TaskInfo{ID: fmt.Sprintf("task-%d", call+1), Type: task.Type(), Queue: types.QueueLow}, nil
}

func (e *recordingEnqueuer) snapshot() []enqueueCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

func optionValue(opts []asynq.Option, optionType asynq.OptionType) (any, bool) {
	for _, opt := range opts {
		if opt.Type() == optionType {
			return opt.Value(), true
		}
	}
	return nil, false
}

func splitRecoveryCalls(calls []enqueueCall) (commits, maps []enqueueCall) {
	for _, call := range calls {
		var payload struct {
			TaskMode string `json:"task_mode"`
		}
		_ = json.Unmarshal(call.task.Payload(), &payload)
		if payload.TaskMode == "map" {
			maps = append(maps, call)
		} else {
			commits = append(commits, call)
		}
	}
	return commits, maps
}

func TestRecoverNowPublishesOneStableTriggerPerPendingKnowledgeBase(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-a", "ingest")
	insertPendingOp(t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-a", "retract")
	insertPendingOp(t, db, 8, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-b", "ingest")
	insertPendingOp(t, db, 9, "summary:generation", types.TaskScopeKnowledgeBase, "kb-c", "summary")
	insertPendingOp(t, db, 10, types.TypeWikiIngest, "knowledge", "knowledge-1", "ingest")

	enqueuer := &recordingEnqueuer{}
	recovery := NewRecovery(db, enqueuer, nil)
	require.NoError(t, recovery.RecoverNow(context.Background()))

	calls := enqueuer.snapshot()
	require.Len(t, calls, 4)
	commits, maps := splitRecoveryCalls(calls)
	require.Len(t, commits, 2)
	require.Len(t, maps, 2)
	assert.Equal(t, types.TypeWikiIngest, commits[0].task.Type())
	assert.JSONEq(t, `{"tenant_id":7,"knowledge_base_id":"kb-a"}`, string(commits[0].task.Payload()))
	assert.Equal(t, `{"tenant_id":7,"knowledge_base_id":"kb-a"}`, string(commits[0].task.Payload()), "payload bytes must be stable for asynq.Unique")
	assert.JSONEq(t, `{"tenant_id":8,"knowledge_base_id":"kb-b"}`, string(commits[1].task.Payload()))
	assert.JSONEq(t,
		`{"tenant_id":7,"knowledge_base_id":"kb-a","task_mode":"map","map_dedup_key":"kb-a-doc"}`,
		string(maps[0].task.Payload()))
	assert.JSONEq(t,
		`{"tenant_id":8,"knowledge_base_id":"kb-b","task_mode":"map","map_dedup_key":"kb-b-doc"}`,
		string(maps[1].task.Payload()))

	wantOptions := map[asynq.OptionType]any{
		asynq.QueueOpt:     types.QueueWikiControl,
		asynq.MaxRetryOpt:  defaultMaxRetry,
		asynq.TimeoutOpt:   defaultTaskTimeout,
		asynq.ProcessInOpt: defaultProcessDelay,
		asynq.UniqueOpt:    defaultUniqueTTL,
	}
	for optionType, want := range wantOptions {
		got, ok := optionValue(commits[0].opts, optionType)
		require.Truef(t, ok, "missing asynq option %v", optionType)
		assert.Equal(t, want, got)
	}

	var rowCount int64
	require.NoError(t, db.Table("task_pending_ops").Count(&rowCount).Error)
	assert.EqualValues(t, 5, rowCount, "recovery must never acknowledge or delete durable work")
}

func TestRecoverNowDoesNotPublishForTombstonedKnowledgeBase(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-deleted", "ingest")
	require.NoError(t, db.Exec(
		"UPDATE knowledge_bases SET deleted_at = ? WHERE id = ? AND tenant_id = ?",
		time.Now().UTC(), "kb-deleted", 7,
	).Error)

	enqueuer := &recordingEnqueuer{}
	recovery := NewRecovery(db, enqueuer, nil)
	require.NoError(t, recovery.RecoverNow(context.Background()))
	assert.Empty(t, enqueuer.snapshot())
}

func TestRecoverNowTreatsUniqueDuplicateAsHealthy(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 42, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1", "ingest")
	enqueuer := &recordingEnqueuer{errFunc: func(_ int, _ *asynq.Task) error {
		return asynq.ErrDuplicateTask
	}}

	err := NewRecovery(db, enqueuer, nil).RecoverNow(context.Background())
	require.NoError(t, err)
	assert.Len(t, enqueuer.snapshot(), 2)
}

func TestRecoverNowSkipsKnowledgeBaseWithActiveWorker(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 42, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-active", "ingest")
	insertPendingOp(t, db, 42, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-idle", "ingest")
	enqueuer := &recordingEnqueuer{}
	activeKeys := &activeKeyCheckerStub{values: map[string]int64{
		wikiActiveKeyPrefix + "kb-active": 1,
	}}
	recovery := NewRecovery(db, enqueuer, nil)
	recovery.activeKeys = activeKeys

	require.NoError(t, recovery.RecoverNow(context.Background()))
	calls := enqueuer.snapshot()
	require.Len(t, calls, 3)
	commits, maps := splitRecoveryCalls(calls)
	require.Len(t, commits, 1)
	require.Len(t, maps, 2, "Map is document-local and must continue while another commit owns the KB")
	assert.JSONEq(t, `{"tenant_id":42,"knowledge_base_id":"kb-idle"}`, string(commits[0].task.Payload()))
	assert.Equal(t, []string{
		wikiActiveKeyPrefix + "kb-active",
		wikiActiveKeyPrefix + "kb-idle",
	}, activeKeys.snapshotKeys())

	var rowCount int64
	require.NoError(t, db.Table("task_pending_ops").Count(&rowCount).Error)
	assert.EqualValues(t, 2, rowCount)
}

func TestRecoverNowRedisLookupFailureStillAttemptsTrigger(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 42, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-uncertain", "ingest")
	redisErr := errors.New("redis exists timeout")
	enqueuer := &recordingEnqueuer{}
	recovery := NewRecovery(db, enqueuer, nil)
	recovery.activeKeys = &activeKeyCheckerStub{err: redisErr}

	err := recovery.RecoverNow(context.Background())
	require.ErrorIs(t, err, redisErr)
	assert.Contains(t, err.Error(), "check active worker")
	assert.Len(t, enqueuer.snapshot(), 2, "Redis uncertainty must not suppress either durable lane")
}

func TestRecoverNowContinuesAfterRedisFailureAndRetriesNextRound(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 1, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-a", "ingest")
	insertPendingOp(t, db, 1, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-b", "ingest")
	redisErr := errors.New("redis unavailable")
	enqueuer := &recordingEnqueuer{errFunc: func(call int, _ *asynq.Task) error {
		if call == 0 {
			return redisErr
		}
		return nil
	}}
	recovery := NewRecovery(db, enqueuer, nil)

	err := recovery.RecoverNow(context.Background())
	require.ErrorIs(t, err, redisErr)
	assert.Contains(t, err.Error(), `knowledge_base="kb-a"`)
	assert.Len(t, enqueuer.snapshot(), 4, "one failed KB must not prevent later KBs or Maps from being triggered")

	require.NoError(t, recovery.RecoverNow(context.Background()))
	assert.Len(t, enqueuer.snapshot(), 8, "both durable lanes must be retried on the next scan")

	var rowCount int64
	require.NoError(t, db.Table("task_pending_ops").Count(&rowCount).Error)
	assert.EqualValues(t, 2, rowCount)
}

func TestRecoverNowStopsEnqueueingPromptlyAfterCancellation(t *testing.T) {
	db := newRecoveryTestDB(t)
	for i := 0; i < 50; i++ {
		insertPendingOp(t, db, 1, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			fmt.Sprintf("kb-cancel-%02d", i), "ingest")
	}

	ctx, cancel := context.WithCancel(context.Background())
	enqueuer := &recordingEnqueuer{}
	enqueuer.errFunc = func(call int, _ *asynq.Task) error {
		if call == 0 {
			cancel()
		}
		return nil
	}

	err := NewRecovery(db, enqueuer, nil).RecoverNow(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, enqueuer.snapshot(), 1,
		"shutdown cancellation must prevent the remaining durable scopes from being enqueued")
}

func TestRecoverNowReturnsPostgresQueryFailure(t *testing.T) {
	db := newRecoveryTestDB(t)
	require.NoError(t, db.Exec("DROP TABLE task_pending_ops").Error)

	err := NewRecovery(db, &recordingEnqueuer{}, nil).RecoverNow(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list pending scopes")
}

func TestStartIsImmediateIdempotentAndRestartable(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 12, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-start", "ingest")
	enqueuer := &recordingEnqueuer{called: make(chan struct{}, 4)}
	config := DefaultConfig()
	config.ScanInterval = time.Hour
	recovery := NewRecoveryWithConfig(db, enqueuer, nil, config)

	recovery.Start(context.Background())
	recovery.Start(context.Background())
	deadline := time.Now().Add(time.Second)
	for len(enqueuer.snapshot()) < 2 && time.Now().Before(deadline) {
		select {
		case <-enqueuer.called:
		case <-time.After(10 * time.Millisecond):
		}
	}
	require.Len(t, enqueuer.snapshot(), 2, "startup scan did not publish both durable lanes")
	time.Sleep(25 * time.Millisecond)
	assert.Len(t, enqueuer.snapshot(), 2, "second Start must not create another loop")

	recovery.Stop()
	recovery.Stop()

	recovery.Start(context.Background())
	deadline = time.Now().Add(time.Second)
	for len(enqueuer.snapshot()) < 4 && time.Now().Before(deadline) {
		select {
		case <-enqueuer.called:
		case <-time.After(10 * time.Millisecond):
		}
	}
	recovery.Stop()
	assert.Len(t, enqueuer.snapshot(), 4)
}

func TestLoopSurvivesPanicAndRetriesOnNextTick(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 13, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-panic", "ingest")
	enqueuer := &recordingEnqueuer{called: make(chan struct{}, 8)}
	enqueuer.errFunc = func(call int, _ *asynq.Task) error {
		if call == 0 {
			panic("synthetic enqueue panic")
		}
		return nil
	}
	config := DefaultConfig()
	config.ScanInterval = 10 * time.Millisecond
	config.ScanTimeout = time.Second
	recovery := NewRecoveryWithConfig(db, enqueuer, nil, config)
	recovery.Start(context.Background())
	defer recovery.Stop()

	deadline := time.After(time.Second)
	for len(enqueuer.snapshot()) < 2 {
		select {
		case <-enqueuer.called:
		case <-deadline:
			t.Fatalf("loop did not recover after panic; calls=%d", len(enqueuer.snapshot()))
		}
	}
}

func TestStartRecoveryRegistersGracefulShutdown(t *testing.T) {
	db := newRecoveryTestDB(t)
	insertPendingOp(t, db, 14, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-lifecycle", "ingest")
	enqueuer := &recordingEnqueuer{called: make(chan struct{}, 2)}
	recovery := NewRecovery(db, enqueuer, nil)
	cleaner := &resourceCleanerStub{}

	StartRecovery(recovery, cleaner)
	select {
	case <-enqueuer.called:
	case <-time.After(time.Second):
		t.Fatal("lifecycle registration did not start immediate scan")
	}
	assert.Equal(t, "WikiQueueRecovery", cleaner.name)
	require.NotNil(t, cleaner.cleanup)
	assert.Empty(t, cleaner.Cleanup(context.Background()))
	assert.Empty(t, cleaner.Cleanup(context.Background()), "registered shutdown must be idempotent")
}

func TestRecoverNowRejectsMissingDependencies(t *testing.T) {
	db := newRecoveryTestDB(t)
	assert.EqualError(t, NewRecovery(nil, &recordingEnqueuer{}, nil).RecoverNow(context.Background()), "wiki queue recovery: database is nil")
	assert.EqualError(t, NewRecovery(db, nil, nil).RecoverNow(context.Background()), "wiki queue recovery: task enqueuer is nil")
	assert.True(t, strings.Contains((&Recovery{}).RecoverNow(context.Background()).Error(), "database is nil"))

	var payload triggerPayload
	require.NoError(t, json.Unmarshal([]byte(`{"tenant_id":1,"knowledge_base_id":"kb"}`), &payload))
	assert.Equal(t, triggerPayload{TenantID: 1, KnowledgeBaseID: "kb"}, payload)
}

func TestMapDispatchReservationBoundsPoolAndPublishesFencedWake(t *testing.T) {
	db := newRecoveryTestDB(t)
	for index := 1; index <= 3; index++ {
		insertPendingOp(
			t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			"kb-a", "ingest",
		)
		require.NoError(t, db.Exec(
			`UPDATE task_pending_ops
			 SET dedup_key = ?, payload = ?
			 WHERE id = ?`,
			fmt.Sprintf("knowledge-%d:g-1", index),
			fmt.Sprintf(`{"knowledge_id":"knowledge-%d","processing_generation":"g-1"}`, index),
			index,
		).Error)
	}

	var candidates []pendingMapRow
	require.NoError(t, db.Table("task_pending_ops").
		Select("id, tenant_id, scope_id, dedup_key, payload, map_resource_pool_id, map_dispatch_epoch, map_dispatch_lease_until").
		Order("id").Scan(&candidates).Error)
	require.Len(t, candidates, 3)

	enqueuer := &recordingEnqueuer{}
	recovery := &Recovery{db: db, enqueuer: enqueuer, config: DefaultConfig()}
	first, err := recovery.reserveMapDispatch(
		context.Background(), candidates[0], "pool-a", 2, 2, time.Minute,
	)
	require.NoError(t, err)
	second, err := recovery.reserveMapDispatch(
		context.Background(), candidates[1], "pool-a", 2, 2, time.Minute,
	)
	require.NoError(t, err)
	_, err = recovery.reserveMapDispatch(
		context.Background(), candidates[2], "pool-a", 2, 2, time.Minute,
	)
	require.ErrorIs(t, err, errMapDispatchWindowFull)

	require.EqualValues(t, 1, first.MapDispatchEpoch)
	require.EqualValues(t, 1, second.MapDispatchEpoch)
	require.NoError(t, recovery.enqueueReservedMap(first))
	calls := enqueuer.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, types.TypeWikiIngest, calls[0].task.Type())
	assert.JSONEq(t,
		`{"tenant_id":7,"knowledge_base_id":"kb-a","task_mode":"map","map_dedup_key":"knowledge-1:g-1","map_dispatch_epoch":1,"knowledge_id":"knowledge-1","processing_generation":"g-1"}`,
		string(calls[0].task.Payload()),
	)
	wantOptions := map[asynq.OptionType]any{
		asynq.QueueOpt:     types.QueueWikiMap,
		asynq.TaskIDOpt:    "wiki-map:1:1",
		asynq.MaxRetryOpt:  0,
		asynq.TimeoutOpt:   defaultTaskTimeout,
		asynq.ProcessInOpt: defaultProcessDelay,
	}
	for optionType, want := range wantOptions {
		got, ok := optionValue(calls[0].opts, optionType)
		require.Truef(t, ok, "missing asynq option %v", optionType)
		assert.Equal(t, want, got)
	}
	_, hasUnique := optionValue(calls[0].opts, asynq.UniqueOpt)
	assert.False(t, hasUnique, "the database epoch, not a Redis uniqueness TTL, fences Map wake-ups")

	require.NoError(t, db.Table("task_pending_ops").Where("id = ?", first.ID).
		Update("map_dispatch_lease_until", time.Now().UTC().Add(-time.Second)).Error)
	third, err := recovery.reserveMapDispatch(
		context.Background(), candidates[2], "pool-a", 2, 2, time.Minute,
	)
	require.NoError(t, err, "an expired reservation must immediately free one pool slot")
	require.EqualValues(t, 1, third.MapDispatchEpoch)

	require.NoError(t, recovery.releaseMapDispatchReservation(
		context.Background(), second.ID, second.MapDispatchEpoch,
	))
	firstAgain, err := recovery.reserveMapDispatch(
		context.Background(), candidates[0], "pool-a", 2, 2, time.Minute,
	)
	require.NoError(t, err, "a failed publication must release its reservation")
	require.EqualValues(t, 2, firstAgain.MapDispatchEpoch)
}

func TestFairMapCandidatesBalanceProjectedWorkAcrossKnowledgeBases(t *testing.T) {
	db := newRecoveryTestDB(t)
	now := time.Now().UTC()
	for index := 0; index < 4; index++ {
		insertPendingOp(
			t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			"kb-older", "ingest",
		)
		insertPendingOp(
			t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			"kb-newer", "ingest",
		)
	}
	require.NoError(t, db.Table("task_pending_ops").Where("scope_id = ?", "kb-older").
		Updates(map[string]any{"enqueued_at": now.Add(-2 * time.Hour)}).Error)
	require.NoError(t, db.Table("task_pending_ops").Where("scope_id = ?", "kb-newer").
		Updates(map[string]any{"enqueued_at": now.Add(-time.Hour)}).Error)

	// Two older-KB rows are already running. The next four projected slots must
	// first catch the newer KB up to the same in-flight level, then alternate.
	require.NoError(t, db.Exec(`
		UPDATE task_pending_ops
		   SET map_dispatch_lease_until = ?
		 WHERE id IN (
		       SELECT id FROM task_pending_ops
		        WHERE scope_id = ? ORDER BY id LIMIT 2
		 )`, now.Add(time.Minute), "kb-older").Error)
	recovery := &Recovery{db: db, config: DefaultConfig()}
	rows, err := recovery.listFairMapCandidates(context.Background(), now, 4)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	assert.Equal(t, []string{"kb-newer", "kb-newer", "kb-older", "kb-newer"}, []string{
		rows[0].ScopeID, rows[1].ScopeID, rows[2].ScopeID, rows[3].ScopeID,
	})

	// With no active reservations, the older scope wins each deterministic
	// tie, but both scopes still receive one row per round.
	require.NoError(t, db.Table("task_pending_ops").Where("1 = 1").
		Update("map_dispatch_lease_until", now.Add(-time.Second)).Error)
	rows, err = recovery.listFairMapCandidates(context.Background(), now, 4)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	assert.Equal(t, []string{"kb-older", "kb-newer", "kb-older", "kb-newer"}, []string{
		rows[0].ScopeID, rows[1].ScopeID, rows[2].ScopeID, rows[3].ScopeID,
	})
}

func TestMapDispatchProtectsDerivativeShareAndBorrowsWhenDerivativeIsIdle(t *testing.T) {
	db := newRecoveryTestDB(t)
	for index := 1; index <= 2; index++ {
		insertPendingOp(
			t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			"kb-a", "ingest",
		)
		require.NoError(t, db.Table("task_pending_ops").Where("id = ?", index).
			Updates(map[string]any{
				"dedup_key":            fmt.Sprintf("knowledge-%d:g-1", index),
				"map_resource_pool_id": "pool-a",
			}).Error)
	}
	require.NoError(t, db.Exec(`
		CREATE TABLE custom_derivative_work_items (
			id TEXT PRIMARY KEY,
			resource_pool_id TEXT NOT NULL,
			work_kind TEXT NOT NULL,
			state TEXT NOT NULL,
			next_attempt_at DATETIME NOT NULL,
			dispatch_lease_until DATETIME
		)
	`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO custom_derivative_work_items
			(id, resource_pool_id, work_kind, state, next_attempt_at)
		VALUES (?, ?, ?, ?, ?)
	`, "derivative-1", "pool-a", "summary", "queued", time.Now().UTC().Add(-time.Second)).Error)

	var candidates []pendingMapRow
	require.NoError(t, db.Table("task_pending_ops").
		Select("id, tenant_id, scope_id, dedup_key, payload, map_resource_pool_id, map_dispatch_epoch, map_dispatch_lease_until").
		Order("id").Scan(&candidates).Error)
	require.Len(t, candidates, 2)
	recovery := &Recovery{db: db, enqueuer: &recordingEnqueuer{}, config: DefaultConfig()}
	_, err := recovery.reserveMapDispatch(
		context.Background(), candidates[0], "pool-a", 3, 1, time.Minute,
	)
	require.NoError(t, err)
	_, err = recovery.reserveMapDispatch(
		context.Background(), candidates[1], "pool-a", 3, 1, time.Minute,
	)
	require.ErrorIs(t, err, errMapDispatchWindowFull)

	require.NoError(t, db.Exec("DELETE FROM custom_derivative_work_items").Error)
	_, err = recovery.reserveMapDispatch(
		context.Background(), candidates[1], "pool-a", 3, 1, time.Minute,
	)
	require.NoError(t, err, "Wiki lane should borrow the idle derivative share")
}

func TestMapDispatchLeavesAutomaticallyDerivedCommitWindowWhenReadyWorkExists(t *testing.T) {
	db := newRecoveryTestDB(t)
	for index := 1; index <= 6; index++ {
		insertPendingOp(
			t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			"kb-a", "ingest",
		)
		require.NoError(t, db.Table("task_pending_ops").Where("id = ?", index).
			Updates(map[string]any{
				"dedup_key":            fmt.Sprintf("knowledge-%d:g-1", index),
				"map_resource_pool_id": "pool-a",
			}).Error)
	}
	readyAt := time.Now().UTC()
	require.NoError(t, db.Table("task_pending_ops").Where("id = ?", 6).
		Update("map_ready_at", readyAt).Error)

	var candidates []pendingMapRow
	require.NoError(t, db.Table("task_pending_ops").
		Select("id, tenant_id, scope_id, dedup_key, payload, map_resource_pool_id, map_dispatch_epoch, map_dispatch_lease_until").
		Where("map_ready_at IS NULL").Order("id").Scan(&candidates).Error)
	require.Len(t, candidates, 5)
	recovery := &Recovery{db: db, enqueuer: &recordingEnqueuer{}, config: DefaultConfig()}

	// A six-slot global window automatically derives Map:Commit=4:2. Map may
	// borrow all six only while there is no commit-ready durable demand.
	for index := 0; index < 4; index++ {
		_, err := recovery.reserveMapDispatch(
			context.Background(), candidates[index], "pool-a", 6, 6, time.Minute,
		)
		require.NoError(t, err)
	}
	_, err := recovery.reserveMapDispatch(
		context.Background(), candidates[4], "pool-a", 6, 6, time.Minute,
	)
	require.ErrorIs(t, err, errMapDispatchWindowFull)
}

func TestMapDispatchSubdividesContendedWikiShareForCommit(t *testing.T) {
	db := newRecoveryTestDB(t)
	for index := 1; index <= 4; index++ {
		insertPendingOp(
			t, db, 7, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			"kb-a", "ingest",
		)
		require.NoError(t, db.Table("task_pending_ops").Where("id = ?", index).
			Updates(map[string]any{
				"dedup_key":            fmt.Sprintf("knowledge-%d:g-1", index),
				"map_resource_pool_id": "pool-a",
			}).Error)
	}
	readyAt := time.Now().UTC()
	require.NoError(t, db.Table("task_pending_ops").Where("id = ?", 4).
		Update("map_ready_at", readyAt).Error)
	require.NoError(t, db.Exec(`
		CREATE TABLE custom_derivative_work_items (
			id TEXT PRIMARY KEY,
			resource_pool_id TEXT NOT NULL,
			work_kind TEXT NOT NULL,
			state TEXT NOT NULL,
			next_attempt_at DATETIME NOT NULL,
			dispatch_lease_until DATETIME
		)
	`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO custom_derivative_work_items
			(id, resource_pool_id, work_kind, state, next_attempt_at)
		VALUES (?, ?, ?, ?, ?)
	`, "derivative-1", "pool-a", "summary", "queued", time.Now().UTC().Add(-time.Second)).Error)

	var candidates []pendingMapRow
	require.NoError(t, db.Table("task_pending_ops").
		Select("id, tenant_id, scope_id, dedup_key, payload, map_resource_pool_id, map_dispatch_epoch, map_dispatch_lease_until").
		Where("map_ready_at IS NULL").Order("id").Scan(&candidates).Error)
	require.Len(t, candidates, 3)
	recovery := &Recovery{db: db, enqueuer: &recordingEnqueuer{}, config: DefaultConfig()}

	// The global six-slot window protects two Wiki slots while derivative also
	// waits. Those two slots are automatically split 1:1 for Map and Commit.
	_, err := recovery.reserveMapDispatch(
		context.Background(), candidates[0], "pool-a", 6, 2, time.Minute,
	)
	require.NoError(t, err)
	_, err = recovery.reserveMapDispatch(
		context.Background(), candidates[1], "pool-a", 6, 2, time.Minute,
	)
	require.ErrorIs(t, err, errMapDispatchWindowFull)
}
