package repository

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type wikiLeaseAcquirerForTest interface {
	AcquireWikiIngestLease(context.Context, uint64, string) (wikilease.Identity, error)
}

type wikiPayloadCheckpointerForTest interface {
	UpdateWikiPayload(context.Context, int64, uint64, string, []byte) (bool, error)
}

type wikiPublisherFenceForTest interface {
	WithActiveWikiKnowledgeBase(context.Context, uint64, string, func() error) error
}

func openWikiLeaseRepositoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "wiki-lease-fence.db")) +
		"?_busy_timeout=5000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(
		&types.KnowledgeBase{}, &types.Knowledge{},
		&types.TaskPendingOp{}, &types.TaskDeadLetter{},
		&types.WikiPage{}, &types.WikiFolder{}, &types.WikiLogEntry{},
		&wikilease.Lease{},
	))
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_test_wiki_log_source_op
		ON wiki_log_entries (source_op_id)`).Error)
	return db
}

// TestWikiDatabaseLeaseFencesPausedWorkerAcrossEveryDurableBoundary models
// the exact TTL race: worker A owns epoch 1 and pauses longer than its Redis
// lease; worker B acquires the replacement Redis lock and advances to epoch 2;
// A then resumes. Every A-side mutation must be typed fenced and commit zero
// page/folder/log/checkpoint/settlement/dead-letter changes, while B proceeds.
func TestWikiDatabaseLeaseFencesPausedWorkerAcrossEveryDurableBoundary(t *testing.T) {
	db := openWikiLeaseRepositoryTestDB(t)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	processedAt := time.Now().UTC()
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		Title: "source", Source: "source.md", Type: "file",
		ParseStatus: types.ParseStatusCompleted, ProcessedAt: &processedAt,
		ProcessingGeneration: "generation-1",
	}).Error)

	pending := &types.TaskPendingOp{
		TenantID: 7, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb-1", Op: "ingest", DedupKey: "knowledge-1:generation-1",
		Payload: json.RawMessage(`{
			"op":"ingest",
			"knowledge_id":"knowledge-1",
			"processing_generation":"generation-1",
			"prepared":{"doc_title":"source","updates":[{"Slug":"entity/acme"}]}
		}`),
		EnqueuedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create(pending).Error)
	originalPayload := append([]byte(nil), pending.Payload...)

	pageRepo := NewWikiPageRepository(db)
	logRepo := NewWikiLogEntryRepository(db)
	pendingRepo := NewTaskPendingOpsRepository(db)
	acquirer, ok := pendingRepo.(wikiLeaseAcquirerForTest)
	require.True(t, ok)
	checkpointer, ok := pendingRepo.(wikiPayloadCheckpointerForTest)
	require.True(t, ok)
	publisher, ok := pendingRepo.(wikiPublisherFenceForTest)
	require.True(t, ok)

	index := &types.WikiPage{
		ID: "index-page", TenantID: 7, KnowledgeBaseID: "kb-1",
		Slug: "index", Title: "Index", Content: "before", Summary: "before",
		PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusPublished, Version: 1,
	}
	require.NoError(t, pageRepo.Create(context.Background(), index))

	workerA, err := acquirer.AcquireWikiIngestLease(context.Background(), 7, "kb-1")
	require.NoError(t, err)
	ctxA := wikilease.WithIdentity(context.Background(), workerA)
	identity := wikiingestguard.Identity{
		TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1",
	}

	// A is paused here beyond Redis/Lite TTL. B becomes the only authoritative
	// coordinator by advancing the database epoch before A resumes.
	workerB, err := acquirer.AcquireWikiIngestLease(context.Background(), 7, "kb-1")
	require.NoError(t, err)
	require.Greater(t, workerB.Epoch, workerA.Epoch)
	ctxB := wikilease.WithIdentity(context.Background(), workerB)

	assertFenced := func(err error) {
		t.Helper()
		var typed *wikilease.FencedError
		require.ErrorAs(t, err, &typed)
		require.ErrorIs(t, err, wikilease.ErrFenced)
	}
	validationA := wikiingestguard.WithValidation(ctxA, identity)
	pageApplicationA := wikiingestguard.WithPageApplication(ctxA, "entity/acme", wikiingestguard.Operation{
		PendingOpID: pending.ID, Identity: identity,
	})
	assertFenced(pageRepo.Create(pageApplicationA, &types.WikiPage{
		ID: "page-a", TenantID: 7, KnowledgeBaseID: "kb-1",
		Slug: "entity/acme", Title: "A must not commit", Version: 1,
	}))
	assertFenced(pageRepo.CreateFolder(validationA, &types.WikiFolder{
		ID: "folder-a", TenantID: 7, KnowledgeBaseID: "kb-1", Name: "A",
	}))
	indexA := *index
	indexA.Content = "stale index overwrite"
	assertFenced(pageRepo.Update(validationA, &indexA))
	indexMetaA := *index
	indexMetaA.Status = types.WikiPageStatusDraft
	assertFenced(pageRepo.UpdateMeta(validationA, &indexMetaA))
	assertFenced(logRepo.AppendBatch(validationA, []*types.WikiLogEntry{{
		TenantID: 7, KnowledgeBaseID: "kb-1", Action: "ingest",
		KnowledgeID: "knowledge-1", SourceOpID: &pending.ID,
	}}))
	updated, err := checkpointer.UpdateWikiPayload(
		validationA, pending.ID, 7, "kb-1", []byte(`{"stale":"payload"}`),
	)
	require.False(t, updated)
	assertFenced(err)
	assertFenced(pendingRepo.DeleteByIDs(ctxA, []int64{pending.ID}))
	_, err = pendingRepo.IncrFailCount(ctxA, pending.ID)
	assertFenced(err)
	assertFenced(pendingRepo.ArchiveToDeadLetter(ctxA, pending.ID, &types.TaskDeadLetter{
		TenantID: 7, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb-1", RelatedID: "knowledge-1", Payload: []byte(`{}`), FailCount: 0,
	}))
	stalePublished := false
	assertFenced(publisher.WithActiveWikiKnowledgeBase(ctxA, 7, "kb-1", func() error {
		stalePublished = true
		return nil
	}))
	require.False(t, stalePublished)

	var storedPending types.TaskPendingOp
	require.NoError(t, db.First(&storedPending, pending.ID).Error)
	require.JSONEq(t, string(originalPayload), string(storedPending.Payload))
	require.Zero(t, storedPending.FailCount)
	var count int64
	require.NoError(t, db.Model(&types.WikiPage{}).Count(&count).Error)
	require.EqualValues(t, 1, count, "A must not create a page")
	require.NoError(t, db.Model(&types.WikiFolder{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Model(&types.WikiLogEntry{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Model(&types.TaskDeadLetter{}).Count(&count).Error)
	require.Zero(t, count)
	var storedIndex types.WikiPage
	require.NoError(t, db.First(&storedIndex, "id = ?", index.ID).Error)
	require.Equal(t, "before", storedIndex.Content)
	require.EqualValues(t, 1, storedIndex.Version)
	require.Equal(t, types.WikiPageStatusPublished, storedIndex.Status)

	// B retains the same durable operation and can finish every boundary.
	validationB := wikiingestguard.WithValidation(ctxB, identity)
	require.NoError(t, pageRepo.CreateFolder(validationB, &types.WikiFolder{
		ID: "folder-b", TenantID: 7, KnowledgeBaseID: "kb-1", Name: "B",
	}))
	pageApplicationB := wikiingestguard.WithPageApplication(ctxB, "entity/acme", wikiingestguard.Operation{
		PendingOpID: pending.ID, Identity: identity,
	})
	pageB := &types.WikiPage{
		ID: "page-b", TenantID: 7, KnowledgeBaseID: "kb-1",
		Slug: "entity/acme", Title: "B committed", PageType: types.WikiPageTypeEntity,
		Status: types.WikiPageStatusDraft, Version: 1,
	}
	require.NoError(t, pageRepo.Create(pageApplicationB, pageB))
	require.NoError(t, db.First(&storedPending, pending.ID).Error)
	require.Contains(t, string(storedPending.Payload), "applied_page_slugs")
	pageB.Status = types.WikiPageStatusPublished
	require.NoError(t, pageRepo.UpdateMeta(validationB, pageB))
	indexB := storedIndex
	indexB.Content = "B index"
	require.NoError(t, pageRepo.Update(validationB, &indexB))
	require.NoError(t, logRepo.AppendBatch(validationB, []*types.WikiLogEntry{{
		TenantID: 7, KnowledgeBaseID: "kb-1", Action: "ingest",
		KnowledgeID: "knowledge-1", SourceOpID: &pending.ID,
	}}))
	currentPublished := false
	require.NoError(t, publisher.WithActiveWikiKnowledgeBase(ctxB, 7, "kb-1", func() error {
		currentPublished = true
		return nil
	}))
	require.True(t, currentPublished)
	require.NoError(t, pendingRepo.DeleteByIDs(ctxB, []int64{pending.ID}))
	require.ErrorIs(t, db.First(&types.TaskPendingOp{}, pending.ID).Error, gorm.ErrRecordNotFound)
	require.NoError(t, db.Model(&types.TaskDeadLetter{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Model(&types.WikiLogEntry{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.NoError(t, db.Model(&types.WikiFolder{}).Count(&count).Error)
	require.EqualValues(t, 1, count)

	var committedPage types.WikiPage
	require.NoError(t, db.First(&committedPage, "id = ?", pageB.ID).Error)
	require.Equal(t, types.WikiPageStatusPublished, committedPage.Status)
	require.Equal(t, "B committed", committedPage.Title)
	// Compile-time assurance that the public generic interface remains narrow:
	// database leasing is a mandatory production extension, not a generic queue
	// concern inherited by unrelated adapters.
	var _ interfaces.TaskPendingOpsRepository = pendingRepo
}
