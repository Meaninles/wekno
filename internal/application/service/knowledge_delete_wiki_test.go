package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type deleteWikiPageRepoStub struct {
	interfaces.WikiPageRepository
	pages       []*types.WikiPage
	err         error
	quarantined []*types.WikiPage
}

func (r *deleteWikiPageRepoStub) QuarantineForDelete(
	_ context.Context, kbID, slug, sourceKnowledgeID string,
) error {
	var target *types.WikiPage
	for _, page := range r.pages {
		if page != nil && page.KnowledgeBaseID == kbID && page.Slug == slug {
			target = page
			break
		}
	}
	if target == nil {
		target = &types.WikiPage{
			ID: slug, KnowledgeBaseID: kbID, Slug: slug,
			PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusPublished,
		}
	}
	if err := wikidelete.Quarantine(target, sourceKnowledgeID); err != nil {
		return err
	}
	clone := *target
	r.quarantined = append(r.quarantined, &clone)
	return nil
}

func (r *deleteWikiPageRepoStub) ListBySourceRef(
	context.Context, string, string,
) ([]*types.WikiPage, error) {
	return r.pages, r.err
}

type deleteWikiChunkRepoStub struct {
	interfaces.ChunkRepository
	ids []string
	err error
}

func (r *deleteWikiChunkRepoStub) ListChunkIDsByKnowledgeIDUnscoped(
	context.Context, uint64, string,
) ([]string, error) {
	return r.ids, r.err
}

func setupDeleteWikiDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:delete-wiki-%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	require.NoError(t, err)
	for _, statement := range []string{
		`CREATE TABLE knowledge_bases (
			id TEXT PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE knowledges (
			id TEXT PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL,
			parse_status TEXT NOT NULL,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE task_pending_ops (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tenant_id INTEGER NOT NULL,
			task_type TEXT NOT NULL,
			scope TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			op TEXT NOT NULL,
			dedup_key TEXT NOT NULL,
			payload JSON NOT NULL,
			fail_count INTEGER NOT NULL DEFAULT 0,
			enqueued_at DATETIME,
			claimed_at DATETIME,
			CONSTRAINT reject_boom CHECK (NOT (op = 'retract' AND dedup_key = 'boom'))
		)`,
		`CREATE UNIQUE INDEX uq_task_pending_ops_wiki_retract
			ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
			WHERE task_type = 'wiki:ingest'
			  AND scope = 'knowledge_base'
			  AND op = 'retract'`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	require.NoError(t, db.Exec(
		`INSERT INTO knowledge_bases (id, tenant_id) VALUES ('kb-delete', 7)`,
	).Error)
	return db
}

func insertDeleteWikiKnowledge(t *testing.T, db *gorm.DB, id string) *types.Knowledge {
	t.Helper()
	knowledge := &types.Knowledge{
		ID:              id,
		TenantID:        7,
		KnowledgeBaseID: "kb-delete",
		ParseStatus:     types.ParseStatusCompleted,
		Title:           "Policy",
	}
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges (id, tenant_id, knowledge_base_id, parse_status)
		 VALUES (?, ?, ?, ?)`,
		knowledge.ID, knowledge.TenantID, knowledge.KnowledgeBaseID, knowledge.ParseStatus,
	).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO task_pending_ops
		 (tenant_id, task_type, scope, scope_id, op, dedup_key, payload)
		 VALUES (?, ?, ?, ?, 'ingest', ?, '{}')`,
		knowledge.TenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
		knowledge.KnowledgeBaseID, knowledge.ID,
	).Error)
	return knowledge
}

func TestPrepareWikiDeleteTransactionFailureHasNoVisibleSideEffects(t *testing.T) {
	db := setupDeleteWikiDB(t)
	knowledge := insertDeleteWikiKnowledge(t, db, "boom")
	pageSvc := &wikiQueuePageServiceStub{indexPage: &types.WikiPage{
		ID: "index-page", KnowledgeBaseID: "kb-delete", Slug: "index", PageType: types.WikiPageTypeIndex,
		Status: types.WikiPageStatusPublished,
	}}
	task := &wikiQueueTaskEnqueuerStub{}
	pageRepo := &deleteWikiPageRepoStub{pages: []*types.WikiPage{{
		Slug: "policy", SourceRefs: types.StringArray{"boom"},
	}}}
	svc := &knowledgeService{
		wikiDeleteCoord: wikidelete.New(db),
		wikiRepo:        pageRepo,
		wikiService:     pageSvc,
		chunkRepo:       &deleteWikiChunkRepoStub{ids: []string{"chunk-1"}},
		task:            task,
	}

	err := svc.prepareWikiKnowledgeDeletion(context.Background(), []*types.Knowledge{knowledge})
	require.Error(t, err)
	var status string
	require.NoError(t, db.Raw("SELECT parse_status FROM knowledges WHERE id = 'boom'").Row().Scan(&status))
	assert.Equal(t, types.ParseStatusCompleted, status)
	var ingestCount, retractCount int64
	require.NoError(t, db.Table("task_pending_ops").Where("op = 'ingest'").Count(&ingestCount).Error)
	require.NoError(t, db.Table("task_pending_ops").Where("op = 'retract'").Count(&retractCount).Error)
	assert.EqualValues(t, 1, ingestCount)
	assert.Zero(t, retractCount)
	assert.Empty(t, pageSvc.deleted)
	assert.Empty(t, pageSvc.updated)
	assert.Empty(t, pageRepo.quarantined)
	assert.Empty(t, task.tasks)
}

func TestPrepareWikiDeleteTriggerFailureUsesDurableRecovery(t *testing.T) {
	db := setupDeleteWikiDB(t)
	knowledge := insertDeleteWikiKnowledge(t, db, "kid-delete")
	pageSvc := &wikiQueuePageServiceStub{indexPage: &types.WikiPage{
		ID: "index-page", KnowledgeBaseID: "kb-delete", Slug: "index", PageType: types.WikiPageTypeIndex,
		Status: types.WikiPageStatusPublished,
	}}
	task := &wikiQueueTaskEnqueuerStub{err: errors.New("redis unavailable")}
	pageRepo := &deleteWikiPageRepoStub{pages: []*types.WikiPage{{
		KnowledgeBaseID: "kb-delete", Slug: "policy", SourceRefs: types.StringArray{"kid-delete"},
	}}}
	svc := &knowledgeService{
		wikiDeleteCoord: wikidelete.New(db),
		wikiRepo:        pageRepo,
		wikiService:     pageSvc,
		chunkRepo:       &deleteWikiChunkRepoStub{ids: []string{"chunk-1"}},
		task:            task,
	}

	require.NoError(t, svc.prepareWikiKnowledgeDeletion(context.Background(), []*types.Knowledge{knowledge}))
	var status string
	require.NoError(t, db.Raw("SELECT parse_status FROM knowledges WHERE id = 'kid-delete'").Row().Scan(&status))
	assert.Equal(t, types.ParseStatusDeleting, status)
	var ingestCount, retractCount int64
	require.NoError(t, db.Table("task_pending_ops").Where("op = 'ingest'").Count(&ingestCount).Error)
	require.NoError(t, db.Table("task_pending_ops").Where("op = 'retract'").Count(&retractCount).Error)
	assert.Zero(t, ingestCount)
	assert.EqualValues(t, 1, retractCount)
	assert.Empty(t, pageSvc.deleted)
	assert.Empty(t, pageSvc.updated)
	require.Len(t, pageRepo.quarantined, 2)
	assert.Equal(t, "policy", pageRepo.quarantined[0].Slug)
	assert.Equal(t, types.WikiPageStatusArchived, pageRepo.quarantined[0].Status)
	assert.Equal(t, "index", pageRepo.quarantined[1].Slug)
	assert.Len(t, task.tasks, 1, "Redis wake-up failure is recoverable from the committed PG row")
}

func TestPrepareWikiDeleteQuarantinesSharedPageWithoutRemovingProvenance(t *testing.T) {
	db := setupDeleteWikiDB(t)
	knowledge := insertDeleteWikiKnowledge(t, db, "kid-shared")
	shared := &types.WikiPage{
		ID: "shared-page", KnowledgeBaseID: "kb-delete", Slug: "entity/shared",
		PageType: types.WikiPageTypeEntity, Status: types.WikiPageStatusPublished,
		Content:    "contains both sources",
		SourceRefs: types.StringArray{"kid-shared", "surviving-source"},
		ChunkRefs:  types.StringArray{"chunk-deleted", "chunk-surviving"},
	}
	pageSvc := &wikiQueuePageServiceStub{indexPage: &types.WikiPage{
		ID: "index-page", KnowledgeBaseID: "kb-delete", Slug: "index", PageType: types.WikiPageTypeIndex,
		Status: types.WikiPageStatusPublished,
	}}
	pageRepo := &deleteWikiPageRepoStub{pages: []*types.WikiPage{shared}}
	svc := &knowledgeService{
		wikiDeleteCoord: wikidelete.New(db),
		wikiRepo:        pageRepo,
		wikiService:     pageSvc,
		chunkRepo:       &deleteWikiChunkRepoStub{ids: []string{"chunk-deleted"}},
		task:            &wikiQueueTaskEnqueuerStub{},
	}

	require.NoError(t, svc.prepareWikiKnowledgeDeletion(context.Background(), []*types.Knowledge{knowledge}))
	assert.Empty(t, pageSvc.deleted)
	assert.Empty(t, pageSvc.updated)
	require.Len(t, pageRepo.quarantined, 2)
	assert.Equal(t, "entity/shared", pageRepo.quarantined[0].Slug)
	assert.Equal(t, types.WikiPageStatusArchived, pageRepo.quarantined[0].Status)
	assert.Equal(t, types.StringArray{"kid-shared", "surviving-source"}, pageRepo.quarantined[0].SourceRefs,
		"source refs must remain until the durable reducer commits the safe body")
	assert.Equal(t, types.StringArray{"chunk-deleted", "chunk-surviving"}, pageRepo.quarantined[0].ChunkRefs)
}
