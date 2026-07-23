package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type moveWikiKnowledgeRepoStub struct {
	interfaces.KnowledgeRepository
	knowledge *types.Knowledge
	err       error
}

func (r *moveWikiKnowledgeRepoStub) GetKnowledgeByID(context.Context, uint64, string) (*types.Knowledge, error) {
	if r.err != nil {
		return nil, r.err
	}
	copyKnowledge := *r.knowledge
	return &copyKnowledge, nil
}

func (r *moveWikiKnowledgeRepoStub) CompareAndSwapDocumentProcessing(
	_ context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
) (bool, error) {
	if r.knowledge == nil || r.knowledge.TenantID != tenantID || r.knowledge.ID != id ||
		r.knowledge.KnowledgeBaseID != expectedKnowledgeBaseID ||
		r.knowledge.ParseStatus != expectedParseStatus ||
		r.knowledge.ProcessingGeneration != expectedGeneration ||
		r.knowledge.ProcessingOwner != expectedOwner {
		return false, nil
	}
	if marker, ok := values["error_message"].(string); ok {
		r.knowledge.ErrorMessage = marker
	}
	if status, ok := values["parse_status"].(string); ok {
		r.knowledge.ParseStatus = status
	}
	return true, nil
}

type moveWikiPageRepoStub struct {
	interfaces.WikiPageRepository
	pages         []*types.WikiPage
	index         *types.WikiPage
	indexErr      error
	err           error
	quarantineErr error
	quarantined   []string
}

func (r *moveWikiPageRepoStub) ListBySourceRef(context.Context, string, string) ([]*types.WikiPage, error) {
	return r.pages, r.err
}

func (r *moveWikiPageRepoStub) GetBySlug(context.Context, string, string) (*types.WikiPage, error) {
	return r.index, r.indexErr
}

func (r *moveWikiPageRepoStub) QuarantineForDelete(_ context.Context, _, slug, knowledgeID string) error {
	r.quarantined = append(r.quarantined, slug)
	if r.quarantineErr != nil {
		return r.quarantineErr
	}
	for _, page := range append(append([]*types.WikiPage(nil), r.pages...), r.index) {
		if page != nil && page.Slug == slug {
			return wikidelete.Quarantine(page, knowledgeID)
		}
	}
	return nil
}

func TestMoveWikiQuarantineFailureKeepsRecoveryMarkerAndQueuesEmpty(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		ParseStatus:  types.ParseStatusCompleted,
		ErrorMessage: knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target"),
	}
	service, db := newMoveWikiService(t, knowledge, &wikiQueueTaskEnqueuerStub{})
	quarantineErr := errors.New("wiki page update unavailable")
	service.wikiRepo.(*moveWikiPageRepoStub).quarantineErr = quarantineErr
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	err := service.moveOneKnowledge(ctx, knowledge.ID,
		&types.KnowledgeBase{ID: "kb-source"},
		&types.KnowledgeBase{ID: "kb-target", IndexingStrategy: types.IndexingStrategy{WikiEnabled: true}},
		"reuse_vectors",
		knowledgeMoveTestAttemptID,
	)
	require.ErrorIs(t, err, quarantineErr)
	assert.Zero(t, moveWikiQueueCount(t, db, "kb-source", knowledge.ID, WikiOpRetract))
	assert.Zero(t, moveWikiQueueCount(t, db, "kb-target", knowledge.ID, WikiOpIngest))
	var marker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", knowledge.ID).Scan(&marker).Error)
	assert.Equal(t, knowledge.ErrorMessage, marker)
}

func TestMoveWikiMissingSourceIndexDoesNotBlockDurableCleanup(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		ParseStatus:  types.ParseStatusCompleted,
		ErrorMessage: knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target"),
	}
	service, db := newMoveWikiService(t, knowledge, &wikiQueueTaskEnqueuerStub{})
	pageRepo := service.wikiRepo.(*moveWikiPageRepoStub)
	pageRepo.index = nil
	pageRepo.indexErr = apprepo.ErrWikiPageNotFound
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	require.NoError(t, service.moveOneKnowledge(ctx, knowledge.ID,
		&types.KnowledgeBase{ID: "kb-source"},
		&types.KnowledgeBase{ID: "kb-target"},
		"reuse_vectors",
		knowledgeMoveTestAttemptID,
	))
	assert.EqualValues(t, 1, moveWikiQueueCount(t, db, "kb-source", knowledge.ID, WikiOpRetract))
}

func newMoveWikiDB(t *testing.T, knowledge *types.Knowledge) *gorm.DB {
	t.Helper()
	if knowledge.ProcessingGeneration == "" {
		knowledge.ProcessingGeneration = "generation-1"
	}
	if knowledge.ParseStatus == types.ParseStatusCompleted && knowledge.ProcessedAt == nil {
		now := time.Unix(1_700_000_000, 0)
		knowledge.ProcessedAt = &now
	}
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:move-wiki-%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	require.NoError(t, err)
	for _, statement := range []string{
		`CREATE TABLE knowledge_bases (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL, deleted_at DATETIME
		)`,
		`CREATE TABLE knowledges (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, parse_status TEXT NOT NULL,
			processing_generation TEXT NOT NULL DEFAULT '',
			processing_owner TEXT NOT NULL DEFAULT '',
			processing_workflow_id TEXT NOT NULL DEFAULT '', processed_at DATETIME,
			error_message TEXT NOT NULL DEFAULT '', updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE task_pending_ops (
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id INTEGER NOT NULL,
			task_type TEXT NOT NULL, scope TEXT NOT NULL, scope_id TEXT NOT NULL,
			op TEXT NOT NULL, dedup_key TEXT NOT NULL DEFAULT '', payload JSON NOT NULL DEFAULT '{}',
			fail_count INTEGER NOT NULL DEFAULT 0, enqueued_at DATETIME NOT NULL, claimed_at DATETIME
		)`,
		`CREATE UNIQUE INDEX uq_task_pending_ops_wiki_retract
			ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
			WHERE task_type = 'wiki:ingest' AND scope = 'knowledge_base' AND op = 'retract'`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	require.NoError(t, db.Exec(
		`INSERT INTO knowledge_bases (id, tenant_id) VALUES ('kb-source', 7), ('kb-target', 7)`,
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO knowledges (id, tenant_id, knowledge_base_id, parse_status, processing_generation, processing_owner, processing_workflow_id, processed_at, error_message) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		knowledge.ID, knowledge.TenantID, knowledge.KnowledgeBaseID, knowledge.ParseStatus,
		knowledge.ProcessingGeneration, knowledge.ProcessingOwner, knowledge.ProcessingWorkflowID,
		knowledge.ProcessedAt, knowledge.ErrorMessage,
	).Error)
	return db
}

func moveWikiQueueCount(t *testing.T, db *gorm.DB, kbID, knowledgeID, operation string) int64 {
	t.Helper()
	var count int64
	query := db.Model(&types.TaskPendingOp{}).Where("scope_id = ? AND op = ?", kbID, operation)
	if operation == WikiOpIngest {
		prefix, _ := wikiqueue.IngestDedupPrefix(knowledgeID)
		query = query.Where("dedup_key LIKE ?", prefix+"%")
	} else {
		query = query.Where("dedup_key = ?", knowledgeID)
	}
	require.NoError(t, query.Count(&count).Error)
	return count
}

func newMoveWikiService(t *testing.T, knowledge *types.Knowledge, task *wikiQueueTaskEnqueuerStub) (*knowledgeService, *gorm.DB) {
	t.Helper()
	db := newMoveWikiDB(t, knowledge)
	pageRepo := &moveWikiPageRepoStub{
		pages: []*types.WikiPage{{
			Slug: "entity/source", PageType: types.WikiPageTypeEntity,
			Status: types.WikiPageStatusPublished, SourceRefs: types.StringArray{knowledge.ID, "knowledge-other"},
		}},
		index: &types.WikiPage{
			Slug: "index", PageType: types.WikiPageTypeIndex, Status: types.WikiPageStatusPublished,
		},
	}
	return &knowledgeService{
		repo:            &moveWikiKnowledgeRepoStub{knowledge: knowledge},
		wikiRepo:        pageRepo,
		chunkRepo:       &wikiQueueChunkRepoStub{ids: []string{"chunk-source"}},
		task:            task,
		wikiDeleteCoord: wikidelete.New(db),
	}, db
}

func TestMoveOneKnowledgeCompletedTargetRepairsWikiQueueAfterTriggerFailure(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		ParseStatus: types.ParseStatusCompleted, Title: "Moved document",
		ErrorMessage: knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target"),
	}
	triggerErr := errors.New("redis unavailable")
	task := &wikiQueueTaskEnqueuerStub{err: triggerErr}
	service, db := newMoveWikiService(t, knowledge, task)
	pageRepo := service.wikiRepo.(*moveWikiPageRepoStub)
	require.NoError(t, wikidelete.Quarantine(pageRepo.pages[0], "knowledge-other"))
	pageRepo.pages[0].Status = types.WikiPageStatusPublished
	source := &types.KnowledgeBase{ID: "kb-source"}
	target := &types.KnowledgeBase{
		ID: "kb-target", IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	require.NoError(t, service.moveOneKnowledge(
		ctx, knowledge.ID, source, target, "reuse_vectors", knowledgeMoveTestAttemptID,
	))
	assert.EqualValues(t, 1, moveWikiQueueCount(t, db, source.ID, knowledge.ID, WikiOpRetract))
	assert.EqualValues(t, 1, moveWikiQueueCount(t, db, target.ID, knowledge.ID, WikiOpIngest))
	assert.Equal(t, types.WikiPageStatusArchived, pageRepo.pages[0].Status)
	pendingSources, markerErr := wikidelete.PendingSources(pageRepo.pages[0])
	require.NoError(t, markerErr)
	assert.ElementsMatch(t, []string{"knowledge-1", "knowledge-other"}, pendingSources)
	assert.Equal(t, types.WikiPageStatusArchived, pageRepo.index.Status)
	var persistedMarker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", knowledge.ID).Scan(&persistedMarker).Error)
	assert.Empty(t, persistedMarker)

	// A batch-level retry after another item fails sees the cleared marker and
	// must not recreate either operation after a worker may have consumed it.
	require.NoError(t, db.Where("dedup_key = ? OR dedup_key LIKE ?", knowledge.ID, knowledge.ID+":%").Delete(&types.TaskPendingOp{}).Error)
	task.err = nil
	require.NoError(t, service.moveOneKnowledge(
		ctx, knowledge.ID, source, target, "reuse_vectors", knowledgeMoveTestAttemptID,
	))
	assert.Zero(t, moveWikiQueueCount(t, db, source.ID, knowledge.ID, WikiOpRetract))
	assert.Zero(t, moveWikiQueueCount(t, db, target.ID, knowledge.ID, WikiOpIngest))
}

func TestMoveOneKnowledgeReparseTargetRetryStates(t *testing.T) {
	for _, tc := range []struct {
		status  string
		wantErr bool
	}{
		{status: types.ParseStatusProcessing},
		{status: types.ParseStatusFinalizing},
		{status: types.ParseStatusCompleted},
		{status: types.ParseStatusFailed, wantErr: true},
		{status: types.ParseStatusCancelled, wantErr: true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			knowledge := &types.Knowledge{
				ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target", ParseStatus: tc.status,
			}
			service, _ := newMoveWikiService(t, knowledge, &wikiQueueTaskEnqueuerStub{})
			ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
			err := service.moveOneKnowledge(ctx, knowledge.ID,
				&types.KnowledgeBase{ID: "kb-source"},
				&types.KnowledgeBase{ID: "kb-target"},
				"reparse",
				knowledgeMoveTestAttemptID,
			)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMoveOneKnowledgeDeletionTakeoverIsControllerNoOp(t *testing.T) {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
	for _, currentKB := range []string{"kb-source", "kb-target"} {
		t.Run(currentKB, func(t *testing.T) {
			service := &knowledgeService{repo: &moveWikiKnowledgeRepoStub{knowledge: &types.Knowledge{
				ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: currentKB,
				ParseStatus: types.ParseStatusDeleting,
			}}}
			require.NoError(t, service.moveOneKnowledge(ctx, "knowledge-1",
				&types.KnowledgeBase{ID: "kb-source"},
				&types.KnowledgeBase{ID: "kb-target"},
				"reuse_vectors",
				knowledgeMoveTestAttemptID,
			))
		})
	}

	t.Run("tenant scoped row already deleted", func(t *testing.T) {
		service := &knowledgeService{repo: &moveWikiKnowledgeRepoStub{err: apprepo.ErrKnowledgeNotFound}}
		require.NoError(t, service.moveOneKnowledge(ctx, "knowledge-1",
			&types.KnowledgeBase{ID: "kb-source"},
			&types.KnowledgeBase{ID: "kb-target"},
			"reuse_vectors",
			knowledgeMoveTestAttemptID,
		))
	})
}

func TestMoveOneKnowledgeTargetDeletionTakesOverPendingSourceWikiRetract(t *testing.T) {
	marker := knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target")
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		ParseStatus: types.ParseStatusDeleting, ErrorMessage: marker,
	}
	service, db := newMoveWikiService(t, knowledge, &wikiQueueTaskEnqueuerStub{})
	pageRepo := service.wikiRepo.(*moveWikiPageRepoStub)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	require.NoError(t, service.moveOneKnowledge(ctx, knowledge.ID,
		&types.KnowledgeBase{ID: "kb-source"},
		&types.KnowledgeBase{ID: "kb-target", IndexingStrategy: types.IndexingStrategy{WikiEnabled: true}},
		"reuse_vectors",
		knowledgeMoveTestAttemptID,
	))
	assert.EqualValues(t, 1, moveWikiQueueCount(t, db, "kb-source", knowledge.ID, WikiOpRetract))
	assert.Zero(t, moveWikiQueueCount(t, db, "kb-target", knowledge.ID, WikiOpIngest),
		"deletion owns the target; move takeover must never revive target Wiki ingest")
	assert.Equal(t, types.WikiPageStatusArchived, pageRepo.pages[0].Status)
	var persistedMarker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", knowledge.ID).Scan(&persistedMarker).Error)
	assert.Empty(t, persistedMarker)
}

func TestMoveOneKnowledgePendingReparseRepairsSourceWikiBeforeProcessingEnqueue(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		ParseStatus: types.ParseStatusPending, Type: types.KnowledgeTypeManual,
		FileHash: "move-generation", ProcessingGeneration: "generation-1",
		ProcessingOwner: processownership.DocumentOwner("knowledge-1", "generation-1"),
		UpdatedAt:       time.Unix(1_700_000_000, 0),
		ErrorMessage:    knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target"),
	}
	require.NoError(t, knowledge.SetManualMetadata(types.NewManualKnowledgeMetadata("body", "published", 1)))
	task := &wikiQueueTaskEnqueuerStub{}
	service, db := newMoveWikiService(t, knowledge, task)
	source := &types.KnowledgeBase{ID: "kb-source"}
	target := &types.KnowledgeBase{
		ID: "kb-target", IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	require.NoError(t, service.moveOneKnowledge(
		ctx, knowledge.ID, source, target, "reparse", knowledgeMoveTestAttemptID,
	))
	assert.EqualValues(t, 1, moveWikiQueueCount(t, db, source.ID, knowledge.ID, WikiOpRetract))
	assert.Zero(t, moveWikiQueueCount(t, db, target.ID, knowledge.ID, WikiOpIngest),
		"target ingest must wait for the new parse generation to complete")
	require.Len(t, task.tasks, 2)
	assert.Equal(t, types.TypeWikiIngest, task.tasks[0].Type())
	assert.Equal(t, types.TypeManualProcess, task.tasks[1].Type())
}

func TestMovePendingReparseTransientBindingFailureKeepsStablePreparationForRetry(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		ParseStatus: types.ParseStatusPending, Type: types.KnowledgeTypeManual,
		FileHash: "move-generation", ProcessingGeneration: "generation-1",
		ProcessingOwner: processownership.DocumentOwner("knowledge-1", "generation-1"),
		UpdatedAt:       time.Unix(1_700_000_000, 0),
		ErrorMessage:    knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target"),
	}
	require.NoError(t, knowledge.SetManualMetadata(types.NewManualKnowledgeMetadata("body", "published", 1)))
	task := &wikiQueueTaskEnqueuerStub{}
	service, db := newMoveWikiService(t, knowledge, task)
	source := &types.KnowledgeBase{ID: "kb-source"}
	target := &types.KnowledgeBase{ID: "kb-target"}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_move_binding_once
		BEFORE UPDATE OF processing_workflow_id ON knowledges
		WHEN NEW.processing_workflow_id <> ''
		BEGIN SELECT RAISE(ABORT, 'transient move binding failure'); END`).Error)

	err := service.reconcileWikiAfterKnowledgeMove(ctx, knowledge, source, target)
	require.ErrorContains(t, err, "transient move binding failure")
	require.Len(t, task.prepared, 1)
	assert.Zero(t, task.aborts, "stable-generation move preparation must survive a retryable business failure")
	var marker, workflowID string
	require.NoError(t, db.Table("knowledges").Select("error_message", "processing_workflow_id").
		Where("id = ?", knowledge.ID).Row().Scan(&marker, &workflowID))
	assert.Equal(t, knowledge.ErrorMessage, marker)
	assert.Empty(t, workflowID)

	require.NoError(t, db.Exec("DROP TRIGGER reject_move_binding_once").Error)
	require.NoError(t, service.reconcileWikiAfterKnowledgeMove(ctx, knowledge, source, target))
	assert.Zero(t, task.aborts)
	assert.GreaterOrEqual(t, task.binds, 2)
	require.NoError(t, db.Table("knowledges").Select("error_message", "processing_workflow_id").
		Where("id = ?", knowledge.ID).Row().Scan(&marker, &workflowID))
	assert.Empty(t, marker)
	assert.Equal(t, "workflow-knowledge-1-generation-1", workflowID)
}

func TestKnowledgeMoveDeadLetterSettlesCompletedReuseAfterSourceTombstone(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "generation-1",
		ErrorMessage: knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target"),
	}
	taskQueue := &wikiQueueTaskEnqueuerStub{}
	service, db := newMoveWikiService(t, knowledge, taskQueue)
	source := &types.KnowledgeBase{ID: "kb-source", TenantID: 7}
	source.DeletedAt.Time = time.Now().UTC()
	source.DeletedAt.Valid = true
	target := &types.KnowledgeBase{
		ID: "kb-target", TenantID: 7,
		IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
	}
	service.kbService = &reparseMoveKBService{source: source, target: target}
	service.tenantRepo = &reparseMoveTenantRepo{tenant: &types.Tenant{ID: 7}}
	require.NoError(t, db.Table("knowledge_bases").Where("id = ?", source.ID).
		Update("deleted_at", source.DeletedAt.Time).Error)
	payload, err := json.Marshal(types.KnowledgeMovePayload{
		TenantID: 7, TaskID: knowledgeMoveTestAttemptID, AttemptID: knowledgeMoveTestAttemptID,
		KnowledgeIDs: []string{knowledge.ID}, SourceKBID: source.ID, TargetKBID: target.ID,
		Mode: "reuse_vectors",
	})
	require.NoError(t, err)

	require.NoError(t, service.RepairKnowledgeMoveDeadLetter(
		context.Background(), asynq.NewTask(types.TypeKnowledgeMove, payload), errors.New("exhausted"),
	))
	assert.Zero(t, moveWikiQueueCount(t, db, source.ID, knowledge.ID, WikiOpRetract))
	assert.EqualValues(t, 1, moveWikiQueueCount(t, db, target.ID, knowledge.ID, WikiOpIngest))
	var marker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", knowledge.ID).Scan(&marker).Error)
	assert.Empty(t, marker)
}

func TestKnowledgeMoveDeadLetterHandsOffPendingReparseAfterSourceTombstone(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		Type: types.KnowledgeTypeManual, ParseStatus: types.ParseStatusPending,
		ProcessingGeneration: "generation-1",
		ProcessingOwner:      processownership.DocumentOwner("knowledge-1", "generation-1"),
		ErrorMessage:         knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target"),
	}
	require.NoError(t, knowledge.SetManualMetadata(types.NewManualKnowledgeMetadata("body", "published", 1)))
	taskQueue := &wikiQueueTaskEnqueuerStub{}
	service, db := newMoveWikiService(t, knowledge, taskQueue)
	source := &types.KnowledgeBase{ID: "kb-source", TenantID: 7}
	source.DeletedAt.Time = time.Now().UTC()
	source.DeletedAt.Valid = true
	target := &types.KnowledgeBase{ID: "kb-target", TenantID: 7}
	service.kbService = &reparseMoveKBService{source: source, target: target}
	service.tenantRepo = &reparseMoveTenantRepo{tenant: &types.Tenant{ID: 7}}
	require.NoError(t, db.Table("knowledge_bases").Where("id = ?", source.ID).
		Update("deleted_at", source.DeletedAt.Time).Error)
	payload, err := json.Marshal(types.KnowledgeMovePayload{
		TenantID: 7, TaskID: knowledgeMoveTestAttemptID, AttemptID: knowledgeMoveTestAttemptID,
		KnowledgeIDs: []string{knowledge.ID}, SourceKBID: source.ID, TargetKBID: target.ID,
		Mode: "reparse",
	})
	require.NoError(t, err)

	require.NoError(t, service.RepairKnowledgeMoveDeadLetter(
		context.Background(), asynq.NewTask(types.TypeKnowledgeMove, payload), errors.New("exhausted"),
	))
	assert.Zero(t, moveWikiQueueCount(t, db, source.ID, knowledge.ID, WikiOpRetract))
	require.Len(t, taskQueue.tasks, 1)
	assert.Equal(t, types.TypeManualProcess, taskQueue.tasks[0].Type())
	var persistedMarker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", knowledge.ID).Scan(&persistedMarker).Error)
	assert.Empty(t, persistedMarker)
}

func TestMoveOneKnowledgeTargetTombstoneSettlesSourceWithoutParserHandoff(t *testing.T) {
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-target",
		Type: types.KnowledgeTypeManual, ParseStatus: types.ParseStatusPending,
		ProcessingGeneration: "generation-1",
		ProcessingOwner:      processownership.DocumentOwner("knowledge-1", "generation-1"),
		ErrorMessage:         knowledgeMoveWikiPendingMarker(knowledgeMoveTestAttemptID, "kb-source", "kb-target"),
	}
	require.NoError(t, knowledge.SetManualMetadata(types.NewManualKnowledgeMetadata("body", "published", 1)))
	taskQueue := &wikiQueueTaskEnqueuerStub{}
	service, db := newMoveWikiService(t, knowledge, taskQueue)
	source := &types.KnowledgeBase{ID: "kb-source", TenantID: 7}
	target := &types.KnowledgeBase{ID: "kb-target", TenantID: 7}
	tombstonedAt := time.Now().UTC()
	require.NoError(t, db.Table("knowledge_bases").Where("id = ?", target.ID).
		Update("deleted_at", tombstonedAt).Error)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	require.NoError(t, service.moveOneKnowledge(
		ctx, knowledge.ID, source, target, "reparse", knowledgeMoveTestAttemptID,
	))
	assert.EqualValues(t, 1, moveWikiQueueCount(t, db, source.ID, knowledge.ID, WikiOpRetract))
	assert.Zero(t, moveWikiQueueCount(t, db, target.ID, knowledge.ID, WikiOpIngest))
	require.Len(t, taskQueue.tasks, 1)
	assert.Equal(t, types.TypeWikiIngest, taskQueue.tasks[0].Type())
	assert.Equal(t, types.ParseStatusPending, service.repo.(*moveWikiKnowledgeRepoStub).knowledge.ParseStatus)
	assert.True(t, target.DeletedAt.Valid,
		"the locked coordinator tombstone observation must suppress the parser handoff")
}

func TestKnowledgeMoveReparseTaskIDIsStableAcrossAmbiguousEnqueueRetry(t *testing.T) {
	first := &types.Knowledge{ID: "knowledge-1", ParseStatus: types.ParseStatusPending, ProcessingGeneration: "generation-1", UpdatedAt: time.Unix(100, 123)}
	reloaded := &types.Knowledge{ID: "knowledge-1", ParseStatus: types.ParseStatusPending, ProcessingGeneration: "generation-1", UpdatedAt: time.Unix(200, 456)}
	assert.Equal(t,
		knowledgeMoveReparseTaskID(first, "kb-target"),
		knowledgeMoveReparseTaskID(reloaded, "kb-target"),
	)
	assert.Equal(t, "move-reparse-knowledge-1-kb-target-generation-1", knowledgeMoveReparseTaskID(first, "kb-target"))

	recovery := &types.Knowledge{
		ID: "knowledge-1", ErrorMessage: knowledgeMoveAttemptMarker(
			knowledgeMoveTestAttemptID,
			knowledgeMoveRecoveryReparseQueued+"generation-1",
		),
	}
	assert.Equal(t, "move-recovery-knowledge-1-"+knowledgeMoveTestAttemptID+"-generation-1",
		knowledgeMoveReparseTaskID(recovery, "kb-target"))
}
