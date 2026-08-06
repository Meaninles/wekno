package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbdeletequeue"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type kbDeleteKnowledgeRepoStub struct {
	interfaces.KnowledgeRepository
	db *gorm.DB
}

func (s *kbDeleteKnowledgeRepoStub) ListKnowledgeByKnowledgeBaseID(
	context.Context, uint64, string,
) ([]*types.Knowledge, error) {
	var rows []*types.Knowledge
	err := s.db.Find(&rows).Error
	return rows, err
}

type kbDeleteChunkRepoStub struct {
	interfaces.ChunkRepository
	deleteCalls int
}

func (s *kbDeleteChunkRepoStub) ListImageInfoByKnowledgeIDsUnscoped(
	context.Context, uint64, []string,
) ([]interfaces.ChunkImageInfo, error) {
	return nil, nil
}

func (s *kbDeleteChunkRepoStub) DeleteChunksByKnowledgeID(context.Context, uint64, string) error {
	s.deleteCalls++
	return nil
}

type kbDeleteTenantRepoStub struct {
	interfaces.TenantRepository
	tenant *types.Tenant
}

func (s *kbDeleteTenantRepoStub) GetTenantByID(context.Context, uint64) (*types.Tenant, error) {
	return s.tenant, nil
}

type kbDeleteInspectorStub struct {
	interfaces.TaskInspector
	cancelErr        error
	purgeErr         error
	purgedIDs        []string
	wikiLiveSequence []bool
	wikiCancelCalls  int
	wikiProbeCalls   int
}

func (s *kbDeleteInspectorStub) CancelTasksForKnowledge(context.Context, string) (int, int, error) {
	return 0, 0, s.cancelErr
}

func (s *kbDeleteInspectorStub) DocumentLifecycleTaskKnowledgeIDs(
	context.Context, []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (s *kbDeleteInspectorStub) DeletionTaskKnowledgeIDs(
	context.Context, []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (s *kbDeleteInspectorStub) PurgeTaskHistoryForKnowledge(
	_ context.Context, knowledgeID string,
) (int, error) {
	s.purgedIDs = append(s.purgedIDs, knowledgeID)
	if s.purgeErr != nil {
		return 0, s.purgeErr
	}
	return 1, nil
}

func (s *kbDeleteInspectorStub) CancelWikiTasksForKnowledgeBase(
	context.Context, string,
) (int, int, error) {
	s.wikiCancelCalls++
	return 0, 0, s.cancelErr
}

func (s *kbDeleteInspectorStub) HasWikiTasksForKnowledgeBase(
	context.Context, string,
) (bool, error) {
	s.wikiProbeCalls++
	if len(s.wikiLiveSequence) == 0 {
		return false, nil
	}
	value := s.wikiLiveSequence[0]
	s.wikiLiveSequence = s.wikiLiveSequence[1:]
	return value, nil
}

type kbDeleteGraphStub struct {
	interfaces.RetrieveGraphRepository
	err   error
	calls int
}

func (s *kbDeleteGraphStub) DelGraph(context.Context, []types.NameSpace) error {
	s.calls++
	return s.err
}

type kbDeleteWakeStub struct {
	calls int
	err   error
}

type kbDeleteResidueStub struct{}

func (*kbDeleteResidueStub) QuiesceDerivativeTasks(
	context.Context, uint64, string, []string,
) error {
	return nil
}

func (*kbDeleteResidueStub) CleanupCompletedKnowledgeBase(
	context.Context, uint64, string,
) error {
	return nil
}

func (s *kbDeleteWakeStub) Enqueue(*asynq.Task, ...asynq.Option) (*asynq.TaskInfo, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &asynq.TaskInfo{ID: "wake"}, nil
}

func newKBDeleteWorkerHarness(t *testing.T) (
	*knowledgeBaseService,
	*gorm.DB,
	[]byte,
	*kbDeleteChunkRepoStub,
	*kbDeleteGraphStub,
	*kbDeleteInspectorStub,
	*kbdeletequeue.Coordinator,
) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.KnowledgeBase{}, &types.Knowledge{}, &types.TaskPendingOp{}, &types.Tenant{},
		&types.TaskDeadLetter{},
		&types.KnowledgeTagRelation{}, &types.KnowledgeFanoutCompletion{},
		&types.WikiPage{}, &types.WikiFolder{}, &types.WikiPageIssue{}, &types.WikiLogEntry{},
		&wikilease.Lease{},
	))
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_test_wiki_retract
		ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
		WHERE task_type = 'wiki:ingest' AND scope = 'knowledge_base' AND op = 'retract'`).Error)
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS custom_enrichment_outcomes (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_generated_question_claims (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_processing_spans_v2 (
			knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_document_split_plans (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_document_split_parts (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_content_cache_entries (
			tenant_id INTEGER NOT NULL, cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL, version_hash TEXT NOT NULL,
			ref_count INTEGER NOT NULL DEFAULT 0, updated_at DATETIME NOT NULL,
			PRIMARY KEY (tenant_id, cache_kind, content_hash, version_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS custom_content_cache_refs (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL, cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL, version_hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS embeddings (
			knowledge_id TEXT NOT NULL, knowledge_base_id TEXT,
			source_id TEXT, content TEXT NOT NULL DEFAULT '', deleted_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS chunks (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '', deleted_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS custom_derivative_work_items (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_derivative_provider_calls (
			id TEXT PRIMARY KEY, work_item_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_derivative_results (
			id TEXT PRIMARY KEY, work_item_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_document_queue_workflows (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS custom_document_queue_schedule_groups (
			tenant_id INTEGER NOT NULL, knowledge_base_id TEXT NOT NULL,
			PRIMARY KEY (tenant_id, knowledge_base_id)
		)`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}

	kb := &types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}
	kb.SetStorageProvider("local")
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: kb.ID,
		ParseStatus: types.ParseStatusCompleted, StorageSize: 20,
	}
	tenant := &types.Tenant{ID: 7, StorageUsed: 100}
	require.NoError(t, db.Create(kb).Error)
	require.NoError(t, db.Create(knowledge).Error)
	require.NoError(t, db.Create(tenant).Error)
	payload := types.KBDeletePayload{
		TenantID: 7, KnowledgeBaseID: kb.ID, StorageProvider: "local",
	}
	payloadBytes, err := json.Marshal(payload)
	require.NoError(t, err)
	outbox := kbdeletequeue.New(db)
	require.NoError(t, outbox.Prepare(context.Background(), 7, kb.ID, payloadBytes))

	chunkRepo := &kbDeleteChunkRepoStub{}
	graph := &kbDeleteGraphStub{}
	inspector := &kbDeleteInspectorStub{}
	wake := &kbDeleteWakeStub{}
	svc := &knowledgeBaseService{
		repo:             repository.NewKnowledgeBaseRepository(db),
		kgRepo:           &kbDeleteKnowledgeRepoStub{db: db},
		chunkRepo:        chunkRepo,
		tenantRepo:       &kbDeleteTenantRepoStub{tenant: tenant},
		graphEngine:      graph,
		asynqClient:      wake,
		taskInspector:    inspector,
		kbDeleteQueue:    outbox,
		wikiDeleteCoord:  wikidelete.New(db),
		auxObjects:       knowledgeaux.NewWithResolver(db, nil),
		purgeCoordinator: &kbDeleteResidueStub{},
	}
	return svc, db, payloadBytes, chunkRepo, graph, inspector, outbox
}

func TestProcessKBDeleteExternalFailurePreservesChunksKnowledgeStorageAndOutbox(t *testing.T) {
	svc, db, payload, chunks, graph, _, outbox := newKBDeleteWorkerHarness(t)
	graphErr := errors.New("neo4j unavailable")
	graph.err = graphErr

	err := svc.ProcessKBDelete(context.Background(), asynq.NewTask(types.TypeKBDelete, payload))
	require.ErrorIs(t, err, graphErr)
	assert.Zero(t, chunks.deleteCalls)

	var knowledge types.Knowledge
	require.NoError(t, db.First(&knowledge, "id = ?", "knowledge-1").Error)
	assert.Equal(t, types.ParseStatusDeleting, knowledge.ParseStatus)
	var tenant types.Tenant
	require.NoError(t, db.First(&tenant, "id = ?", 7).Error)
	assert.EqualValues(t, 100, tenant.StorageUsed)
	exists, inspectErr := outbox.IntentExists(context.Background(), 7, "kb-1", payload)
	require.NoError(t, inspectErr)
	assert.True(t, exists)
}

func TestProcessKBDeleteQuiescenceFailureStopsBeforeExternalCleanup(t *testing.T) {
	svc, _, payload, chunks, graph, inspector, _ := newKBDeleteWorkerHarness(t)
	quiesceErr := errors.New("redis inspection unavailable")
	inspector.cancelErr = quiesceErr

	err := svc.ProcessKBDelete(context.Background(), asynq.NewTask(types.TypeKBDelete, payload))
	require.ErrorIs(t, err, quiesceErr)
	assert.Zero(t, graph.calls)
	assert.Zero(t, chunks.deleteCalls)
}

func TestProcessKBDeleteTaskHistoryPurgeFailureStopsBeforeFinalize(t *testing.T) {
	svc, db, payload, chunks, _, inspector, outbox := newKBDeleteWorkerHarness(t)
	purgeErr := errors.New("redis terminal history unavailable")
	inspector.purgeErr = purgeErr

	err := svc.ProcessKBDelete(context.Background(), asynq.NewTask(types.TypeKBDelete, payload))
	require.ErrorIs(t, err, purgeErr)
	assert.Equal(t, 1, chunks.deleteCalls)
	assert.Equal(t, []string{"knowledge-1"}, inspector.purgedIDs)

	var knowledge types.Knowledge
	require.NoError(t, db.First(&knowledge, "id = ?", "knowledge-1").Error)
	assert.Equal(t, types.ParseStatusDeleting, knowledge.ParseStatus)
	exists, inspectErr := outbox.IntentExists(context.Background(), 7, "kb-1", payload)
	require.NoError(t, inspectErr)
	assert.True(t, exists)
}

func TestProcessKBDeleteFinalizesStorageExactlyOnceAndConsumesOutbox(t *testing.T) {
	svc, db, payload, chunks, _, inspector, outbox := newKBDeleteWorkerHarness(t)
	require.NoError(t, db.Create(&types.WikiFolder{
		ID: "folder-1", TenantID: 7, KnowledgeBaseID: "kb-1", Name: "folder",
	}).Error)
	require.NoError(t, db.Create(&types.WikiPage{
		ID: "page-1", TenantID: 7, KnowledgeBaseID: "kb-1", Slug: "page", Title: "page",
	}).Error)
	require.NoError(t, db.Create(&types.WikiPageIssue{
		ID: "issue-1", TenantID: 7, KnowledgeBaseID: "kb-1", Slug: "page",
		IssueType: "stale", Description: "stale", ReportedBy: "test",
	}).Error)
	require.NoError(t, db.Create(&types.WikiLogEntry{
		TenantID: 7, KnowledgeBaseID: "kb-1", Action: "ingest", DocTitle: "sensitive title",
	}).Error)
	require.NoError(t, db.Create(&types.TaskPendingOp{
		TenantID: 7, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb-1", Op: "ingest", DedupKey: "orphan-generation", Payload: []byte(`{"op":"ingest"}`),
	}).Error)
	task := asynq.NewTask(types.TypeKBDelete, payload)

	require.NoError(t, svc.ProcessKBDelete(context.Background(), task))
	require.NoError(t, svc.ProcessKBDelete(context.Background(), task))
	assert.Equal(t, 1, chunks.deleteCalls)
	assert.Equal(t, []string{"knowledge-1"}, inspector.purgedIDs)
	assert.GreaterOrEqual(t, inspector.wikiCancelCalls, 6,
		"first delivery must fence Wiki after claim and on both sides of the durable purge")

	var knowledgeCount int64
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", "knowledge-1").Count(&knowledgeCount).Error)
	assert.Zero(t, knowledgeCount)
	var tenant types.Tenant
	require.NoError(t, db.First(&tenant, "id = ?", 7).Error)
	assert.EqualValues(t, 80, tenant.StorageUsed)
	exists, err := outbox.IntentExists(context.Background(), 7, "kb-1", payload)
	require.NoError(t, err)
	assert.False(t, exists)
	for tableName, model := range map[string]interface{}{
		"wiki_pages":       &types.WikiPage{},
		"wiki_folders":     &types.WikiFolder{},
		"wiki_page_issues": &types.WikiPageIssue{},
		"wiki_log_entries": &types.WikiLogEntry{},
	} {
		var count int64
		require.NoError(t, db.Unscoped().Model(model).
			Where("tenant_id = ? AND knowledge_base_id = ?", 7, "kb-1").Count(&count).Error, tableName)
		assert.Zero(t, count, tableName)
	}
	var wikiPending int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("tenant_id = ? AND task_type = ? AND scope_id = ?", 7, types.TypeWikiIngest, "kb-1").
		Count(&wikiPending).Error)
	assert.Zero(t, wikiPending)
}

func TestDeleteKnowledgeBaseTriggerFailureIsDurablyAccepted(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.KnowledgeBase{}, &types.TaskPendingOp{}))
	kb := &types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}
	kb.SetStorageProvider("local")
	require.NoError(t, db.Create(kb).Error)
	outbox := kbdeletequeue.New(db)
	wakeErr := errors.New("redis unavailable")
	svc := &knowledgeBaseService{
		repo:          repository.NewKnowledgeBaseRepository(db),
		asynqClient:   &kbDeleteWakeStub{err: wakeErr},
		kbDeleteQueue: outbox,
	}

	require.NoError(t, svc.DeleteKnowledgeBase(ctxWithTenantStorage(7, "local"), kb.ID))
	var tombstone types.KnowledgeBase
	require.NoError(t, db.Unscoped().First(&tombstone, "id = ?", kb.ID).Error)
	require.True(t, tombstone.DeletedAt.Valid)
	var count int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("task_type = ? AND scope_id = ?", types.TypeKBDelete, kb.ID).Count(&count).Error)
	require.EqualValues(t, 1, count)
}
