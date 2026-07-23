package repository

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbdeletequeue"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openKnowledgeCreateFenceDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "fence.db")) +
		"?_busy_timeout=5000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	require.NoError(t, db.AutoMigrate(
		&types.KnowledgeBase{}, &types.Knowledge{}, &types.KnowledgeTagRelation{}, &types.TaskPendingOp{},
	))
	return db
}

func TestCreateKnowledgeCommitsWorkflowBindingAndTagsAtomically(t *testing.T) {
	db := openKnowledgeCreateFenceDB(t)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	repo := NewKnowledgeRepository(db)

	knowledge := createFenceKnowledge("bound-with-tags")
	knowledge.ProcessingGeneration = "generation-1"
	knowledge.ProcessingOwner = "owner-1"
	knowledge.ProcessingWorkflowID = "00000000-0000-0000-0000-000000000001"
	knowledge.InitialTagIDs = []string{"tag-1", "tag-2"}
	require.NoError(t, repo.CreateKnowledge(context.Background(), knowledge))

	var persisted types.Knowledge
	require.NoError(t, db.First(&persisted, "id = ?", knowledge.ID).Error)
	require.Equal(t, knowledge.ProcessingWorkflowID, persisted.ProcessingWorkflowID)
	var relations []types.KnowledgeTagRelation
	require.NoError(t, db.Order("tag_id").Where("knowledge_id = ?", knowledge.ID).Find(&relations).Error)
	require.Equal(t, []string{"tag-1", "tag-2"}, []string{relations[0].TagID, relations[1].TagID})

	require.NoError(t, db.Exec(`
		CREATE TRIGGER reject_initial_tag
		BEFORE INSERT ON knowledge_tag_relations
		WHEN NEW.tag_id = 'reject'
		BEGIN SELECT RAISE(ABORT, 'reject initial tag'); END;
	`).Error)
	rejected := createFenceKnowledge("must-roll-back")
	rejected.ProcessingGeneration = "generation-2"
	rejected.ProcessingOwner = "owner-2"
	rejected.ProcessingWorkflowID = "00000000-0000-0000-0000-000000000002"
	rejected.InitialTagIDs = []string{"reject"}
	require.Error(t, repo.CreateKnowledge(context.Background(), rejected))
	var leaked int64
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", rejected.ID).Count(&leaked).Error)
	require.Zero(t, leaked, "tag failure must roll back the workflow-bound knowledge row")
}

func createFenceKnowledge(id string) *types.Knowledge {
	return &types.Knowledge{
		ID: id, TenantID: 7, KnowledgeBaseID: "kb-1",
		ParseStatus: types.ParseStatusPending,
	}
}

func TestCreateKnowledgeRejectsTombstonedOrCrossTenantKnowledgeBase(t *testing.T) {
	db := openKnowledgeCreateFenceDB(t)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	repo := NewKnowledgeRepository(db)
	require.NoError(t, repo.CreateKnowledge(context.Background(), createFenceKnowledge("before-delete")))

	require.NoError(t, kbdeletequeue.New(db).Prepare(
		context.Background(), 7, "kb-1", []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`),
	))
	err := repo.CreateKnowledge(context.Background(), createFenceKnowledge("after-delete"))
	require.ErrorIs(t, err, kbwritefence.ErrKnowledgeBaseUnavailable)

	crossTenant := createFenceKnowledge("cross-tenant")
	crossTenant.TenantID = 8
	require.ErrorIs(t, repo.CreateKnowledge(context.Background(), crossTenant), kbwritefence.ErrKnowledgeBaseUnavailable)

	var leaked int64
	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id IN ?", []string{"after-delete", "cross-tenant"}).Count(&leaked).Error)
	require.Zero(t, leaked)
}

func TestCreateKnowledgeStartedFirstSerializesBeforeKnowledgeBaseDelete(t *testing.T) {
	db := openKnowledgeCreateFenceDB(t)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)

	insertReached := make(chan struct{})
	releaseInsert := make(chan struct{})
	var once sync.Once
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(
		"test:block_knowledge_insert", func(tx *gorm.DB) {
			if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "knowledges" {
				return
			}
			once.Do(func() { close(insertReached) })
			<-releaseInsert
		},
	))

	createDone := make(chan error, 1)
	go func() {
		createDone <- NewKnowledgeRepository(db).CreateKnowledge(
			context.Background(), createFenceKnowledge("ordered-before-delete"),
		)
	}()
	<-insertReached // parent KB lock is held in the same transaction

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- kbdeletequeue.New(db).Prepare(
			context.Background(), 7, "kb-1", []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`),
		)
	}()
	select {
	case err := <-deleteDone:
		t.Fatalf("KB delete crossed the in-flight create fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseInsert)
	require.NoError(t, <-createDone)
	require.NoError(t, <-deleteDone)

	var count int64
	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id = ?", "ordered-before-delete").Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestKnowledgeBaseDeleteStartedFirstRejectsConcurrentCreate(t *testing.T) {
	db := openKnowledgeCreateFenceDB(t)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)

	outboxInsertReached := make(chan struct{})
	releaseDelete := make(chan struct{})
	var once sync.Once
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(
		"test:block_kb_delete_outbox", func(tx *gorm.DB) {
			if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "task_pending_ops" {
				return
			}
			once.Do(func() { close(outboxInsertReached) })
			<-releaseDelete
		},
	))

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- kbdeletequeue.New(db).Prepare(
			context.Background(), 7, "kb-1", []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`),
		)
	}()
	<-outboxInsertReached // delete owns the parent KB lock, before commit

	createDone := make(chan error, 1)
	go func() {
		createDone <- NewKnowledgeRepository(db).CreateKnowledge(
			context.Background(), createFenceKnowledge("must-not-leak"),
		)
	}()
	select {
	case err := <-createDone:
		t.Fatalf("knowledge create crossed the in-flight delete fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseDelete)
	require.NoError(t, <-deleteDone)
	require.ErrorIs(t, <-createDone, kbwritefence.ErrKnowledgeBaseUnavailable)

	var count int64
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "must-not-leak").Count(&count).Error)
	require.Zero(t, count)

	var tombstone types.KnowledgeBase
	require.NoError(t, db.Unscoped().First(&tombstone, "id = ?", "kb-1").Error)
	require.True(t, tombstone.DeletedAt.Valid)

	// Preserve errors.Is through repository wrapping for API/service callers.
	require.True(t, errors.Is(
		NewKnowledgeRepository(db).CreateKnowledge(context.Background(), createFenceKnowledge("still-rejected")),
		kbwritefence.ErrKnowledgeBaseUnavailable,
	))
}
