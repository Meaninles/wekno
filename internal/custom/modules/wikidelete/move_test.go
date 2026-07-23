package wikidelete

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func movePendingOp(tenantID uint64, kbID, knowledgeID, operation, moveTargetKBID string) *types.TaskPendingOp {
	payload := map[string]interface{}{
		"op":           operation,
		"knowledge_id": knowledgeID,
	}
	if operation == wikiOpRetract {
		payload["move_target_knowledge_base_id"] = moveTargetKBID
	} else {
		payload["processing_generation"] = "generation-1"
	}
	payloadBytes, _ := json.Marshal(payload)
	dedupKey := knowledgeID
	if operation == wikiOpIngest {
		dedupKey, _ = wikiqueue.IngestDedupKey(knowledgeID, "generation-1")
	}
	return &types.TaskPendingOp{
		TenantID: tenantID,
		TaskType: types.TypeWikiIngest,
		Scope:    types.TaskScopeKnowledgeBase,
		ScopeID:  kbID,
		Op:       operation,
		DedupKey: dedupKey,
		Payload:  payloadBytes,
	}
}

func moveRequest(
	tenantID uint64,
	knowledgeID, sourceKBID, targetKBID string,
	targetWikiEnabled bool,
) MoveRequest {
	marker := "wiki-pending:" + sourceKBID + ":" + targetKBID
	return MoveRequest{
		TenantID:               tenantID,
		KnowledgeID:            knowledgeID,
		SourceKnowledgeBaseID:  sourceKBID,
		TargetKnowledgeBaseID:  targetKBID,
		TargetWikiEnabled:      targetWikiEnabled,
		ExpectedMarker:         marker,
		SourceRetractPendingOp: movePendingOp(tenantID, sourceKBID, knowledgeID, wikiOpRetract, targetKBID),
		TargetIngestPendingOp:  movePendingOp(tenantID, targetKBID, knowledgeID, wikiOpIngest, ""),
	}
}

func setMoveWorkflowBinding(
	request *MoveRequest,
	workflowID, generation, owner string,
) {
	request.TargetProcessingWorkflowID = workflowID
	request.ExpectedProcessingGeneration = generation
	request.ExpectedProcessingOwner = owner
	request.BindTargetWorkflowTx = func(tx *gorm.DB, transition func(*gorm.DB) error) error {
		if err := transition(tx); err != nil {
			return err
		}
		var bound int64
		if err := tx.Table("knowledges").Where(
			"id = ? AND tenant_id = ? AND knowledge_base_id = ? AND parse_status = ? AND processing_generation = ? AND processing_owner = ? AND processing_workflow_id = ?",
			request.KnowledgeID, request.TenantID, request.TargetKnowledgeBaseID,
			types.ParseStatusPending, generation, owner, workflowID,
		).Count(&bound).Error; err != nil {
			return err
		}
		if bound != 1 {
			return errors.New("test workflow binding validation failed")
		}
		return nil
	}
}

func primeMoveMarker(t *testing.T, db *gorm.DB, request MoveRequest) {
	t.Helper()
	insertKnowledgeBase(t, db, request.TenantID, request.SourceKnowledgeBaseID)
	insertKnowledgeBase(t, db, request.TenantID, request.TargetKnowledgeBaseID)
	require.NoError(t, db.Table("knowledges").Where("id = ?", request.KnowledgeID).
		Update("error_message", request.ExpectedMarker).Error)
}

func scopedPendingCount(t *testing.T, db *gorm.DB, kbID, knowledgeID, operation string) int64 {
	t.Helper()
	var count int64
	query := db.Model(&types.TaskPendingOp{}).Where("scope_id = ? AND op = ?", kbID, operation)
	if operation == wikiOpIngest {
		prefix, _ := wikiqueue.IngestDedupPrefix(knowledgeID)
		query = query.Where("dedup_key LIKE ?", prefix+"%")
	} else {
		query = query.Where("dedup_key = ?", knowledgeID)
	}
	require.NoError(t, query.Count(&count).Error)
	return count
}

func TestCoordinatorPrepareMoveWikiEnablementCombinations(t *testing.T) {
	for _, tc := range []struct {
		name              string
		sourceWikiEnabled bool
		targetWikiEnabled bool
		wantTargetIngest  bool
	}{
		{name: "source_on_target_on", sourceWikiEnabled: true, targetWikiEnabled: true, wantTargetIngest: true},
		{name: "source_on_target_off", sourceWikiEnabled: true, targetWikiEnabled: false},
		{name: "source_off_target_on", sourceWikiEnabled: false, targetWikiEnabled: true, wantTargetIngest: true},
		{name: "source_off_target_off", sourceWikiEnabled: false, targetWikiEnabled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newCoordinatorDB(t)
			insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusCompleted)
			request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", tc.targetWikiEnabled)
			primeMoveMarker(t, db, request)
			insertPending(t, db, movePendingOp(7, "kb-source", "knowledge-1", wikiOpIngest, ""))
			insertPending(t, db, movePendingOp(7, "kb-target", "knowledge-1", wikiOpIngest, ""))

			result, err := New(db).PrepareMove(context.Background(), request)
			require.NoError(t, err)
			assert.True(t, result.SourceRetractPersisted,
				"source cleanup is required even when source Wiki is currently disabled")
			assert.Equal(t, tc.wantTargetIngest, result.TargetIngestPersisted)
			assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpIngest))
			assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
			if tc.wantTargetIngest {
				assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))
			} else {
				assert.Zero(t, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))
			}

			// Retrying after a trigger failure refreshes, rather than duplicates,
			// both durable operations.
			result, err = New(db).PrepareMove(context.Background(), request)
			require.NoError(t, err)
			assert.True(t, result.AlreadySettled)
			assert.False(t, result.TargetIngestPersisted)
			assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
			if tc.wantTargetIngest {
				assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))
			}
		})
	}
}

func TestCoordinatorPrepareMoveDefersTargetIngestUntilReparseCompletes(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusPending)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	setMoveWorkflowBinding(&request, "workflow-1", "generation-1", "owner-1")
	primeMoveMarker(t, db, request)
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").
		Update("processing_owner", "owner-1").Error)

	result, err := New(db).PrepareMove(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, result.SourceRetractPersisted)
	assert.False(t, result.TargetIngestPersisted)
	assert.True(t, result.TargetWorkflowBound)
	assert.Zero(t, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))
	var workflowID string
	require.NoError(t, db.Table("knowledges").Select("processing_workflow_id").
		Where("id = ?", "knowledge-1").Scan(&workflowID).Error)
	assert.Equal(t, "workflow-1", workflowID)

	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").
		Updates(map[string]interface{}{
			"parse_status":           types.ParseStatusCompleted,
			"processed_at":           time.Now().UTC(),
			"error_message":          request.ExpectedMarker,
			"processing_workflow_id": "",
			"updated_at":             time.Now().UTC(),
		}).Error)
	request.TargetProcessingWorkflowID = ""
	request.ExpectedProcessingGeneration = ""
	request.ExpectedProcessingOwner = ""
	request.BindTargetWorkflowTx = nil
	result, err = New(db).PrepareMove(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, result.TargetIngestPersisted)
	assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))
}

func TestCoordinatorPrepareMovePendingTargetRequiresWorkflowBinding(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusPending)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	primeMoveMarker(t, db, request)

	_, err := New(db).PrepareMove(context.Background(), request)
	require.ErrorIs(t, err, ErrInvalidRequest)
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	var marker, workflowID string
	require.NoError(t, db.Table("knowledges").Select("error_message", "processing_workflow_id").
		Where("id = ?", "knowledge-1").Row().Scan(&marker, &workflowID))
	assert.Equal(t, request.ExpectedMarker, marker)
	assert.Empty(t, workflowID)
}

func TestCoordinatorPrepareMoveWorkflowBindingFailureRollsBackWikiTransition(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusPending)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	setMoveWorkflowBinding(&request, "workflow-1", "generation-1", "owner-1")
	primeMoveMarker(t, db, request)
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").
		Update("processing_owner", "owner-1").Error)
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_move_workflow_binding
		BEFORE UPDATE OF processing_workflow_id ON knowledges
		WHEN NEW.processing_workflow_id <> ''
		BEGIN SELECT RAISE(ABORT, 'injected workflow binding failure'); END`).Error)

	_, err := New(db).PrepareMove(context.Background(), request)
	require.ErrorContains(t, err, "injected workflow binding failure")
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	var marker, workflowID string
	require.NoError(t, db.Table("knowledges").Select("error_message", "processing_workflow_id").
		Where("id = ?", "knowledge-1").Row().Scan(&marker, &workflowID))
	assert.Equal(t, request.ExpectedMarker, marker)
	assert.Empty(t, workflowID)
}

func TestCoordinatorPrepareMoveChangedMarkerRollsBackWorkflowBindingAttempt(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusPending)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	setMoveWorkflowBinding(&request, "workflow-1", "generation-1", "owner-1")
	primeMoveMarker(t, db, request)
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").
		Updates(map[string]interface{}{
			"processing_owner": "owner-1",
			"error_message":    "newer-move-marker",
		}).Error)

	result, err := New(db).PrepareMove(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, result.AlreadySettled)
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	var marker, workflowID string
	require.NoError(t, db.Table("knowledges").Select("error_message", "processing_workflow_id").
		Where("id = ?", "knowledge-1").Row().Scan(&marker, &workflowID))
	assert.Equal(t, "newer-move-marker", marker)
	assert.Empty(t, workflowID, "a stale settlement must never bind its prepared workflow")
}

func TestCoordinatorPrepareMoveRejectsStaleTargetIngestGeneration(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusCompleted)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	primeMoveMarker(t, db, request)

	stalePayload, err := json.Marshal(map[string]interface{}{
		"op":                    wikiOpIngest,
		"knowledge_id":          "knowledge-1",
		"processing_generation": "generation-stale",
	})
	require.NoError(t, err)
	staleKey, err := wikiqueue.IngestDedupKey("knowledge-1", "generation-stale")
	require.NoError(t, err)
	request.TargetIngestPendingOp.Payload = stalePayload
	request.TargetIngestPendingOp.DedupKey = staleKey

	_, err = New(db).PrepareMove(context.Background(), request)
	require.ErrorIs(t, err, ErrKnowledgeIdentity)
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	assert.Zero(t, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))
	var marker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", "knowledge-1").Scan(&marker).Error)
	assert.Equal(t, request.ExpectedMarker, marker)
}

func TestCoordinatorPrepareMoveFailedOrCompensatedMoveIsNoOp(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-source", types.ParseStatusCompleted)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	primeMoveMarker(t, db, request)
	insertPending(t, db, movePendingOp(7, "kb-source", "knowledge-1", wikiOpIngest, ""))

	_, err := New(db).PrepareMove(context.Background(), request)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrKnowledgeIdentity))
	assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpIngest))
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	assert.Zero(t, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))
}

func TestCoordinatorPrepareMoveThatHasMovedOnOnlyCleansOriginalSource(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-third", types.ParseStatusCompleted)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	primeMoveMarker(t, db, request)
	insertPending(t, db, movePendingOp(7, "kb-target", "knowledge-1", wikiOpIngest, ""))

	result, err := New(db).PrepareMove(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, result.SourceRetractPersisted)
	assert.False(t, result.TargetIngestPersisted)
	assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	assert.Zero(t, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest),
		"an old move retry must not revive its no-longer-current target")
}

func TestCoordinatorPrepareMoveQueueFailureRollsBackEveryQueueMutation(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusCompleted)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	primeMoveMarker(t, db, request)
	insertPending(t, db, movePendingOp(7, "kb-source", "knowledge-1", wikiOpIngest, ""))
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_target_ingest
		BEFORE INSERT ON task_pending_ops
		WHEN NEW.op = 'ingest' AND NEW.scope_id = 'kb-target'
		BEGIN SELECT RAISE(ABORT, 'target ingest unavailable'); END`).Error)

	_, err := New(db).PrepareMove(context.Background(), request)
	require.ErrorContains(t, err, "insert target Wiki ingest")
	assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpIngest),
		"source ingest deletion must roll back when the target durable row cannot be persisted")
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	var marker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", "knowledge-1").Scan(&marker).Error)
	assert.Equal(t, request.ExpectedMarker, marker)
}

func TestCoordinatorPrepareMoveSourceTombstoneSkipsRetractAndSettlesTarget(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusCompleted)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	primeMoveMarker(t, db, request)
	insertPending(t, db, movePendingOp(7, "kb-source", "knowledge-1", wikiOpIngest, ""))
	insertPending(t, db, movePendingOp(7, "kb-source", "knowledge-1", wikiOpRetract, "kb-target"))
	require.NoError(t, db.Table("knowledge_bases").Where("id = ?", "kb-source").
		Update("deleted_at", time.Now().UTC()).Error)

	result, err := New(db).PrepareMove(context.Background(), request)
	require.NoError(t, err)
	assert.False(t, result.SourceRetractPersisted,
		"a tombstoned source must never recreate queue state after KB-delete recovery stops scanning it")
	assert.True(t, result.TargetIngestPersisted)
	assert.True(t, result.SourceKnowledgeBaseDeleted)
	assert.False(t, result.TargetKnowledgeBaseDeleted)
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpIngest))
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))

	var marker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", "knowledge-1").Scan(&marker).Error)
	assert.Empty(t, marker)
}

func TestCoordinatorPrepareMoveSourceTombstoneWithDeleteIntentStillSkipsRetract(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusCompleted)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	primeMoveMarker(t, db, request)
	insertPending(t, db, &types.TaskPendingOp{
		TenantID: 7, TaskType: types.TypeKBDelete, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: "kb-source", Op: knowledgeBaseDeleteOperation, DedupKey: "kb-source",
		Payload: []byte(`{"tenant_id":7,"knowledge_base_id":"kb-source"}`),
	})
	require.NoError(t, db.Table("knowledge_bases").Where("id = ?", "kb-source").
		Update("deleted_at", time.Now().UTC()).Error)

	result, err := New(db).PrepareMove(context.Background(), request)
	require.NoError(t, err)
	assert.False(t, result.SourceRetractPersisted)
	assert.True(t, result.TargetIngestPersisted)
	assert.Zero(t, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	var marker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", "knowledge-1").Scan(&marker).Error)
	assert.Empty(t, marker)
}

func TestCoordinatorPrepareMoveTargetTombstoneSkipsIngestAndSettlesSource(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-target", types.ParseStatusCompleted)
	request := moveRequest(7, "knowledge-1", "kb-source", "kb-target", true)
	primeMoveMarker(t, db, request)
	insertPending(t, db, movePendingOp(7, "kb-target", "knowledge-1", wikiOpIngest, ""))
	require.NoError(t, db.Table("knowledge_bases").Where("id = ?", "kb-target").
		Update("deleted_at", time.Now().UTC()).Error)

	result, err := New(db).PrepareMove(context.Background(), request)
	require.NoError(t, err)
	assert.True(t, result.SourceRetractPersisted)
	assert.False(t, result.TargetIngestPersisted,
		"a tombstoned target must never receive a new ingest")
	assert.False(t, result.SourceKnowledgeBaseDeleted)
	assert.True(t, result.TargetKnowledgeBaseDeleted)
	assert.EqualValues(t, 1, scopedPendingCount(t, db, "kb-source", "knowledge-1", wikiOpRetract))
	assert.Zero(t, scopedPendingCount(t, db, "kb-target", "knowledge-1", wikiOpIngest))

	var marker string
	require.NoError(t, db.Table("knowledges").Select("error_message").
		Where("id = ?", "knowledge-1").Scan(&marker).Error)
	assert.Empty(t, marker)
}
