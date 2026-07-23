package kbdeletequeue

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openQueueTestDB(t *testing.T, withOutbox bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.KnowledgeBase{}, &types.Knowledge{},
		&types.WikiPage{}, &types.WikiFolder{}, &types.WikiPageIssue{}, &types.WikiLogEntry{},
		&wikilease.Lease{},
	))
	if withOutbox {
		require.NoError(t, db.AutoMigrate(&types.TaskPendingOp{}))
	}
	return db
}

func openQueueRaceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "kb-delete-race.db")) +
		"?_busy_timeout=5000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(
		&types.KnowledgeBase{}, &types.Knowledge{}, &types.TaskPendingOp{},
		&types.WikiPage{}, &types.WikiFolder{}, &types.WikiPageIssue{}, &types.WikiLogEntry{},
		&wikilease.Lease{},
	))
	return db
}

func TestPrepareAtomicallySoftDeletesAndPersistsOneIntent(t *testing.T) {
	db := openQueueTestDB(t, true)
	kb := &types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}
	require.NoError(t, db.Create(kb).Error)
	payload := []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`)

	coordinator := New(db)
	require.NoError(t, coordinator.Prepare(context.Background(), 7, "kb-1", payload))
	require.NoError(t, coordinator.Prepare(context.Background(), 7, "kb-1", payload))

	var tombstone types.KnowledgeBase
	require.NoError(t, db.Unscoped().First(&tombstone, "id = ?", "kb-1").Error)
	require.True(t, tombstone.DeletedAt.Valid)
	var intents []types.TaskPendingOp
	require.NoError(t, db.Find(&intents).Error)
	require.Len(t, intents, 1)
	require.Equal(t, types.TypeKBDelete, intents[0].TaskType)
	require.JSONEq(t, string(payload), string(intents[0].Payload))
}

func TestPrepareRollsBackSoftDeleteWhenOutboxWriteFails(t *testing.T) {
	db := openQueueTestDB(t, false)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)

	err := New(db).Prepare(context.Background(), 7, "kb-1", []byte(`{"ok":true}`))
	require.Error(t, err)

	var active types.KnowledgeBase
	require.NoError(t, db.First(&active, "id = ?", "kb-1").Error)
	require.False(t, active.DeletedAt.Valid)
}

func TestPrepareWaitsForActiveKnowledgeMoveScope(t *testing.T) {
	db := openQueueRaceTestDB(t)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-source", TenantID: 7, Name: "source"}).Error)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-target", TenantID: 7, Name: "target"}).Error)

	entered := make(chan struct{})
	release := make(chan struct{})
	moveDone := make(chan error, 1)
	go func() {
		moveDone <- kbwritefence.WithActiveSharedSet(
			context.Background(), db, 7, []string{"kb-target", "kb-source"}, func() error {
				close(entered)
				<-release
				return nil
			},
		)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("move scope did not acquire its parent KB fence")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- New(db).Prepare(
			context.Background(), 7, "kb-target",
			[]byte(`{"tenant_id":7,"knowledge_base_id":"kb-target"}`),
		)
	}()
	select {
	case err := <-deleteDone:
		t.Fatalf("KB delete crossed the active move scope: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-moveDone)
	require.NoError(t, <-deleteDone)
}

type queueEnqueuerStub struct {
	err   error
	tasks []*asynq.Task
}

func (s *queueEnqueuerStub) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	s.tasks = append(s.tasks, task)
	if s.err != nil {
		return nil, s.err
	}
	return &asynq.TaskInfo{ID: "trigger"}, nil
}

func TestRecoveryRetainsIntentWhenTriggerFailsAndRetriesLater(t *testing.T) {
	db := openQueueTestDB(t, true)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	payload := []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`)
	coordinator := New(db)
	require.NoError(t, coordinator.Prepare(context.Background(), 7, "kb-1", payload))

	wakeErr := errors.New("redis unavailable")
	enqueuer := &queueEnqueuerStub{err: wakeErr}
	recovery := NewRecovery(db, enqueuer)
	require.ErrorIs(t, recovery.RecoverNow(context.Background()), wakeErr)

	exists, err := coordinator.IntentExists(context.Background(), 7, "kb-1", payload)
	require.NoError(t, err)
	require.True(t, exists)
	enqueuer.err = nil
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Len(t, enqueuer.tasks, 2)
	require.Equal(t, types.TypeKBDelete, enqueuer.tasks[1].Type())
}

func TestCompleteConsumesOnlyExactIntent(t *testing.T) {
	db := openQueueTestDB(t, true)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	payload := []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`)
	coordinator := New(db)
	require.NoError(t, coordinator.Prepare(context.Background(), 7, "kb-1", payload))
	require.NoError(t, coordinator.Complete(context.Background(), 7, "kb-1"))
	require.NoError(t, coordinator.Complete(context.Background(), 7, "kb-1"))

	exists, err := coordinator.IntentExists(context.Background(), 7, "kb-1", payload)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestCompletePurgesWikiDatabaseLeaseBeforeConsumingDeleteIntent(t *testing.T) {
	db := openQueueTestDB(t, true)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	coordinator := New(db)
	require.NoError(t, coordinator.Prepare(
		context.Background(), 7, "kb-1", []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`),
	))
	require.NoError(t, db.Create(&wikilease.Lease{
		TenantID: 7, KnowledgeBaseID: "kb-1", Epoch: 3,
		Token:      "0123456789012345678901234567890123456789012",
		AcquiredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Error)

	require.NoError(t, coordinator.Complete(context.Background(), 7, "kb-1"))
	var count int64
	require.NoError(t, db.Model(&wikilease.Lease{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", 7, "kb-1").Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("task_type = ? AND scope_id = ?", types.TypeKBDelete, "kb-1").Count(&count).Error)
	require.Zero(t, count)
}

func TestCompleteRefusesPreCreateAuxiliaryOwnership(t *testing.T) {
	db := openQueueTestDB(t, true)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	payload := []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`)
	coordinator := New(db)
	require.NoError(t, coordinator.Prepare(context.Background(), 7, "kb-1", payload))
	auxPayload := []byte(`{
		"tenant_id":7,
		"knowledge_base_id":"kb-1",
		"knowledge_id":"not-yet-created",
		"processing_generation":"generation-1",
		"path":"local://7/not-yet-created/source.pdf",
		"fallback_provider":"local",
		"kind":"source_file"
	}`)
	require.NoError(t, db.Create(&types.TaskPendingOp{
		TenantID: 7, TaskType: knowledgeaux.TaskType, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb-1", Op: "owned", DedupKey: "not-yet-created:proof", Payload: auxPayload,
	}).Error)

	err := coordinator.Complete(context.Background(), 7, "kb-1")
	require.ErrorContains(t, err, "auxiliary ownership")
	require.NoError(t, db.Where("task_type = ?", knowledgeaux.TaskType).Delete(&types.TaskPendingOp{}).Error)
	require.NoError(t, coordinator.Complete(context.Background(), 7, "kb-1"))
}

func TestCompleteFailsClosedUntilKnowledgeAndWikiStateAreDrained(t *testing.T) {
	db := openQueueTestDB(t, true)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusCompleted,
	}).Error)
	payload := []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`)
	coordinator := New(db)
	require.NoError(t, coordinator.Prepare(context.Background(), 7, "kb-1", payload))

	require.ErrorContains(t, coordinator.Complete(context.Background(), 7, "kb-1"), "active knowledge")
	exists, err := coordinator.IntentExists(context.Background(), 7, "kb-1", payload)
	require.NoError(t, err)
	require.True(t, exists, "failed assertion must retain the durable retry anchor")

	require.NoError(t, db.Delete(&types.Knowledge{}, "id = ?", "knowledge-1").Error)
	require.NoError(t, db.Create(&types.WikiPage{
		ID: "page-1", TenantID: 7, KnowledgeBaseID: "kb-1", Slug: "late", Title: "late",
	}).Error)
	require.ErrorContains(t, coordinator.Complete(context.Background(), 7, "kb-1"), "Wiki pages")
	require.NoError(t, coordinator.PurgeWikiState(context.Background(), 7, "kb-1"))
	require.NoError(t, coordinator.Complete(context.Background(), 7, "kb-1"))
	exists, err = coordinator.IntentExists(context.Background(), 7, "kb-1", payload)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestCompleteSerializesAndRejectsLateWikiPendingEnqueue(t *testing.T) {
	db := openQueueRaceTestDB(t)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	payload := []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`)
	coordinator := New(db)
	require.NoError(t, coordinator.Prepare(context.Background(), 7, "kb-1", payload))

	completeAtDelete := make(chan struct{})
	releaseComplete := make(chan struct{})
	var blockOnce sync.Once
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(
		"test:block_kb_delete_complete",
		func(tx *gorm.DB) {
			if tx.Statement.Table != "task_pending_ops" {
				return
			}
			blockOnce.Do(func() {
				close(completeAtDelete)
				<-releaseComplete
			})
		},
	))

	completeDone := make(chan error, 1)
	go func() { completeDone <- coordinator.Complete(context.Background(), 7, "kb-1") }()
	select {
	case <-completeAtDelete:
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not reach its outbox delete while holding the KB lock")
	}

	lateDone := make(chan error, 1)
	pendingRepo := repository.NewTaskPendingOpsRepository(db)
	go func() {
		lateDone <- pendingRepo.Enqueue(context.Background(), &types.TaskPendingOp{
			TenantID: 7, TaskType: types.TypeWikiIngest,
			Scope: types.TaskScopeKnowledgeBase, ScopeID: "kb-1",
			Op: "ingest", DedupKey: "late", Payload: []byte(`{"op":"ingest"}`),
		})
	}()
	select {
	case err := <-lateDone:
		t.Fatalf("late Wiki enqueue escaped the completion lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseComplete)
	require.NoError(t, <-completeDone)
	require.ErrorIs(t, <-lateDone, kbwritefence.ErrKnowledgeBaseUnavailable)

	var wikiPending int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("task_type = ? AND scope_id = ?", types.TypeWikiIngest, "kb-1").
		Count(&wikiPending).Error)
	require.Zero(t, wikiPending)
}

func TestIntentExistsUsesSemanticJSONEquality(t *testing.T) {
	db := openQueueTestDB(t, true)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	coordinator := New(db)
	require.NoError(t, coordinator.Prepare(
		context.Background(), 7, "kb-1",
		[]byte(`{"tenant_id":7,"knowledge_base_id":"kb-1","effective_engines":[]}`),
	))

	exists, err := coordinator.IntentExists(
		context.Background(), 7, "kb-1",
		[]byte(`{ "effective_engines": [], "knowledge_base_id": "kb-1", "tenant_id": 7 }`),
	)
	require.NoError(t, err)
	require.True(t, exists)
}

func TestPurgeWikiStateRefusesActiveOrCrossTenantKB(t *testing.T) {
	db := openQueueTestDB(t, true)
	require.NoError(t, db.AutoMigrate(
		&types.WikiPage{}, &types.WikiFolder{}, &types.WikiPageIssue{}, &types.WikiLogEntry{},
	))
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	require.NoError(t, db.Create(&types.WikiPage{
		ID: "page-1", TenantID: 7, KnowledgeBaseID: "kb-1", Slug: "page", Title: "page",
	}).Error)
	coordinator := New(db)

	require.ErrorContains(t, coordinator.PurgeWikiState(context.Background(), 7, "kb-1"), "still active")
	require.Error(t, coordinator.PurgeWikiState(context.Background(), 8, "kb-1"))
	var count int64
	require.NoError(t, db.Model(&types.WikiPage{}).Where("id = ?", "page-1").Count(&count).Error)
	require.EqualValues(t, 1, count)
}
