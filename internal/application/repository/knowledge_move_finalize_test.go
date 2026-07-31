package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupKnowledgeMoveFinalizeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:move-finalize-%s?mode=memory&cache=shared", uuid.NewString())), &gorm.Config{})
	require.NoError(t, err)
	for _, statement := range []string{
		`CREATE TABLE knowledge_bases (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL, deleted_at DATETIME
		)`,
		`INSERT INTO knowledge_bases (id, tenant_id) VALUES
			('kb-source', 7), ('kb-target', 7)`,
		`CREATE TABLE knowledges (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, parse_status TEXT NOT NULL,
			processing_generation TEXT NOT NULL DEFAULT '',
			processing_owner TEXT NOT NULL DEFAULT '',
			processing_workflow_id TEXT NOT NULL DEFAULT '', processing_fanout JSON,
			embedding_model_id TEXT, error_message TEXT NOT NULL DEFAULT '',
			file_path TEXT NOT NULL DEFAULT '',
			enable_status TEXT NOT NULL DEFAULT 'enabled', description TEXT,
			processed_at DATETIME, pending_subtasks_count INTEGER NOT NULL DEFAULT 0,
			core_status TEXT NOT NULL DEFAULT 'pending',
			core_completed_at DATETIME,
			enrichment_status TEXT NOT NULL DEFAULT 'none',
			enrichment_completed_at DATETIME,
			enrichment_error_summary TEXT NOT NULL DEFAULT '',
			wiki_status TEXT NOT NULL DEFAULT 'none',
			wiki_error_message TEXT NOT NULL DEFAULT '',
			storage_size INTEGER NOT NULL DEFAULT 0, updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE knowledge_fanout_completions (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			knowledge_base_id TEXT NOT NULL, processing_generation TEXT NOT NULL,
			item_id TEXT NOT NULL, completed_at DATETIME NOT NULL,
			PRIMARY KEY (tenant_id, knowledge_id, processing_generation, item_id)
		)`,
		`CREATE TABLE custom_enrichment_outcomes (
			tenant_id INTEGER NOT NULL, knowledge_id TEXT NOT NULL,
			knowledge_base_id TEXT NOT NULL, processing_generation TEXT NOT NULL,
			item_id TEXT NOT NULL, status TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '',
			completed_at DATETIME NOT NULL,
			PRIMARY KEY (tenant_id, knowledge_id, processing_generation, item_id)
		)`,
		`CREATE TABLE task_pending_ops (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tenant_id INTEGER NOT NULL, task_type TEXT NOT NULL,
			scope TEXT NOT NULL, scope_id TEXT NOT NULL, op TEXT NOT NULL,
			dedup_key TEXT NOT NULL, payload JSON NOT NULL,
			fail_count INTEGER NOT NULL DEFAULT 0,
			enqueued_at DATETIME NOT NULL, claimed_at DATETIME
		)`,
		`CREATE UNIQUE INDEX uq_test_knowledge_aux_owned
			ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
			WHERE task_type = 'knowledge:aux_object' AND scope = 'knowledge_base' AND op = 'owned'`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	return db
}

func seedKnowledgeMoveFinalize(
	t *testing.T,
	db *gorm.DB,
	knowledgeID, sourceKBID, generation, owner string,
) {
	t.Helper()
	filePath := fmt.Sprintf("dummy://7/%s/source.pdf", knowledgeID)
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (
			id, tenant_id, knowledge_base_id, parse_status,
			processing_generation, processing_owner, processed_at,
			pending_subtasks_count, storage_size, file_path
		) VALUES (?, 7, ?, 'processing', ?, ?, CURRENT_TIMESTAMP, 2, 123, ?)
	`, knowledgeID, sourceKBID, generation, owner, filePath).Error)
	binding, err := storagebinding.Normalize(storagebinding.Binding{
		Provider: storagebinding.ProviderDummy, ConfigSource: storagebinding.ConfigSourceDirect,
		CredentialScope: storagebinding.CredentialScopeNone,
	})
	require.NoError(t, err)
	object := knowledgeaux.Object{
		TenantID: 7, KnowledgeBaseID: sourceKBID, KnowledgeID: knowledgeID,
		ProcessingGeneration: generation, Path: filePath, FallbackProvider: "dummy",
		Kind: knowledgeaux.KindSourceFile, Binding: &binding,
	}
	payload, err := json.Marshal(object)
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(filePath))
	require.NoError(t, db.Create(&types.TaskPendingOp{
		TenantID: 7, TaskType: knowledgeaux.TaskType, Scope: types.TaskScopeKnowledgeBase,
		ScopeID: sourceKBID, Op: "owned", DedupKey: knowledgeID + ":" + hex.EncodeToString(digest[:]),
		Payload: payload, EnqueuedAt: time.Now(),
	}).Error)
	for _, row := range []struct {
		generation string
		itemID     string
	}{
		{generation: "older-generation", itemID: "image:old"},
		{generation: generation, itemID: "enrichment:summary"},
	} {
		require.NoError(t, db.Exec(`
			INSERT INTO knowledge_fanout_completions
				(tenant_id, knowledge_id, knowledge_base_id, processing_generation, item_id, completed_at)
			VALUES (7, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		`, knowledgeID, sourceKBID, row.generation, row.itemID).Error)
	}
}

func assertPersistentOwnershipScope(
	t *testing.T,
	db *gorm.DB,
	knowledgeBaseID string,
	processingGeneration string,
) {
	t.Helper()
	var ownership types.TaskPendingOp
	require.NoError(t, db.Where("task_type = ?", knowledgeaux.TaskType).Take(&ownership).Error)
	assert.Equal(t, knowledgeBaseID, ownership.ScopeID)
	var ownershipObject knowledgeaux.Object
	require.NoError(t, json.Unmarshal(ownership.Payload, &ownershipObject))
	assert.Equal(t, knowledgeBaseID, ownershipObject.KnowledgeBaseID)
	assert.Equal(t, processingGeneration, ownershipObject.ProcessingGeneration)
}

func seedPreparedMoveWorkflow(
	t *testing.T,
	db *gorm.DB,
	knowledgeID, knowledgeBaseID, generation, owner string,
) (*documentqueue.Coordinator, documentqueue.WorkflowBinding) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&documentqueue.Workflow{}))
	payload, err := json.Marshal(types.DocumentProcessPayload{
		TenantID: 7, KnowledgeID: knowledgeID, KnowledgeBaseID: knowledgeBaseID,
		ProcessingGeneration: generation, ProcessingOwner: owner,
	})
	require.NoError(t, err)
	now := time.Now().UTC()
	workflow := &documentqueue.Workflow{
		ID: uuid.NewString(), TenantID: 7, KnowledgeID: knowledgeID,
		KnowledgeBaseID: knowledgeBaseID, ProcessingGeneration: generation,
		TaskType: types.TypeDocumentProcess, Payload: payload, PlanHash: "repository-move-plan",
		State: documentqueue.StatePreparing, Stage: documentqueue.StagePreparing,
		DispatchEpoch: 1, EnqueuedAt: now, LastProgressAt: &now, Version: 1,
	}
	require.NoError(t, db.Create(workflow).Error)
	binding, err := documentqueue.BindingForWorkflow(workflow)
	require.NoError(t, err)
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, nil, "repository-move-test", "boot-1", 1, documentqueue.DefaultConfig(),
	)
	return coordinator, binding
}

func TestFinalizeReuseVectorKnowledgeMoveMovesLedgerAtomically(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-1", "")

	moved, err := repo.FinalizeReuseVectorKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-target",
		"generation-1", "", "move_recovery:wiki_pending:kb-source:kb-target", time.Now(),
	)
	require.NoError(t, err)
	require.True(t, moved)

	var knowledge struct {
		KnowledgeBaseID string
		ParseStatus     string
		ProcessingOwner string
	}
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").Take(&knowledge).Error)
	assert.Equal(t, "kb-target", knowledge.KnowledgeBaseID)
	assert.Equal(t, types.ParseStatusCompleted, knowledge.ParseStatus)
	assert.Empty(t, knowledge.ProcessingOwner)
	var sourceCount, targetCount int64
	require.NoError(t, db.Table("knowledge_fanout_completions").
		Where("knowledge_id = ? AND knowledge_base_id = ?", "knowledge-1", "kb-source").Count(&sourceCount).Error)
	require.NoError(t, db.Table("knowledge_fanout_completions").
		Where("knowledge_id = ? AND knowledge_base_id = ?", "knowledge-1", "kb-target").Count(&targetCount).Error)
	assert.Zero(t, sourceCount)
	assert.EqualValues(t, 2, targetCount, "every generation must follow the authoritative KB")
	var sourceOwnership, targetOwnership int64
	require.NoError(t, db.Table("task_pending_ops").
		Where("task_type = ? AND scope_id = ?", knowledgeaux.TaskType, "kb-source").Count(&sourceOwnership).Error)
	require.NoError(t, db.Table("task_pending_ops").
		Where("task_type = ? AND scope_id = ?", knowledgeaux.TaskType, "kb-target").Count(&targetOwnership).Error)
	assert.Zero(t, sourceOwnership)
	assert.EqualValues(t, 1, targetOwnership, "persistent source ownership must follow the moved document")
}

func TestPrepareKnowledgeMoveReparseRecoveryFencesMarkerAndReplacesGeneration(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "old-generation", "old-owner")
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").
		Update("error_message", "move_recovery:required:uncertain").Error)

	prepared, err := repo.PrepareKnowledgeMoveReparseRecovery(
		context.Background(), 7, "knowledge-1", "kb-source",
		"old-generation", "old-owner", "move_recovery:required:uncertain",
		"new-generation", "new-owner", "move_recovery:source_reparse_cleanup_required:recovery", time.Now(),
	)
	require.NoError(t, err)
	require.True(t, prepared)

	var generation, owner, marker string
	require.NoError(t, db.Raw(`
		SELECT processing_generation, processing_owner, error_message
		FROM knowledges WHERE id = ?
	`, "knowledge-1").Row().Scan(&generation, &owner, &marker))
	assert.Equal(t, "new-generation", generation)
	assert.Equal(t, "new-owner", owner)
	assert.Equal(t, "move_recovery:source_reparse_cleanup_required:recovery", marker)

	prepared, err = repo.PrepareKnowledgeMoveReparseRecovery(
		context.Background(), 7, "knowledge-1", "kb-source",
		"old-generation", "old-owner", "move_recovery:required:uncertain",
		"other-generation", "other-owner", "other-marker", time.Now(),
	)
	require.NoError(t, err)
	assert.False(t, prepared, "a concurrent retry cannot overwrite the winning recovery identity")
}

func TestFinalizeReuseVectorKnowledgeMoveLedgerFailureRollsBackKnowledge(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-1", "")
	require.NoError(t, db.Exec(`
		CREATE TRIGGER fail_fanout_scope_move
		BEFORE UPDATE OF knowledge_base_id ON knowledge_fanout_completions
		BEGIN SELECT RAISE(ABORT, 'injected fanout move failure'); END
	`).Error)

	moved, err := repo.FinalizeReuseVectorKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-target",
		"generation-1", "", "move_recovery:wiki_pending:kb-source:kb-target", time.Now(),
	)
	require.ErrorContains(t, err, "injected fanout move failure")
	assert.False(t, moved)

	var kbID, status string
	require.NoError(t, db.Raw("SELECT knowledge_base_id, parse_status FROM knowledges WHERE id = ?", "knowledge-1").
		Row().Scan(&kbID, &status))
	assert.Equal(t, "kb-source", kbID)
	assert.Equal(t, types.ParseStatusProcessing, status)
	var mismatched int64
	require.NoError(t, db.Table("knowledge_fanout_completions").
		Where("knowledge_id = ? AND knowledge_base_id <> ?", "knowledge-1", "kb-source").Count(&mismatched).Error)
	assert.Zero(t, mismatched)
	assertPersistentOwnershipScope(t, db, "kb-source", "generation-1")
}

func TestFinalizeReuseVectorKnowledgeMoveDeleteClaimLeavesLedgerUntouched(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-1", "")
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-1").
		Update("parse_status", types.ParseStatusDeleting).Error)

	moved, err := repo.FinalizeReuseVectorKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-target",
		"generation-1", "", "move_recovery:wiki_pending:kb-source:kb-target", time.Now(),
	)
	require.NoError(t, err)
	assert.False(t, moved)

	var targetCount int64
	require.NoError(t, db.Table("knowledge_fanout_completions").
		Where("knowledge_id = ? AND knowledge_base_id = ?", "knowledge-1", "kb-target").Count(&targetCount).Error)
	assert.Zero(t, targetCount)
}

func TestFinalizeReuseVectorKnowledgeMoveRejectsDeletedTargetKB(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-1", "")
	require.NoError(t, db.Exec(
		"UPDATE knowledge_bases SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'kb-target'",
	).Error)

	moved, err := repo.FinalizeReuseVectorKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-target",
		"generation-1", "", "move_recovery:wiki_pending:kb-source:kb-target", time.Now(),
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, kbwritefence.ErrKnowledgeBaseUnavailable))
	assert.False(t, moved)

	var kbID, status string
	require.NoError(t, db.Raw(
		"SELECT knowledge_base_id, parse_status FROM knowledges WHERE id = ?", "knowledge-1",
	).Row().Scan(&kbID, &status))
	assert.Equal(t, "kb-source", kbID)
	assert.Equal(t, types.ParseStatusProcessing, status)
}

func TestFinalizeReparseKnowledgeMoveDeletesLedgerAtomically(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-2", "owner-2")

	moved, err := repo.FinalizeReparseKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-target",
		"generation-2", "owner-2", "embedding-target",
		"move_recovery:wiki_pending:kb-source:kb-target", "", nil, time.Now(),
	)
	require.NoError(t, err)
	require.True(t, moved)

	var kbID, status, owner string
	require.NoError(t, db.Raw(`
		SELECT knowledge_base_id, parse_status, processing_owner
		FROM knowledges WHERE id = ?
	`, "knowledge-1").Row().Scan(&kbID, &status, &owner))
	assert.Equal(t, "kb-target", kbID)
	assert.Equal(t, types.ParseStatusPending, status)
	assert.Equal(t, "owner-2", owner, "the target document task retains the planned owner")
	var ledgerCount int64
	require.NoError(t, db.Table("knowledge_fanout_completions").Where("knowledge_id = ?", "knowledge-1").Count(&ledgerCount).Error)
	assert.Zero(t, ledgerCount)
	assertPersistentOwnershipScope(t, db, "kb-target", "generation-2")
}

func TestFinalizeSourceRecoveryBindsWorkflowWithPendingTransition(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-2", "owner-2")
	coordinator, binding := seedPreparedMoveWorkflow(
		t, db, "knowledge-1", "kb-source", "generation-2", "owner-2",
	)

	moved, err := repo.FinalizeReparseKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-source",
		"generation-2", "owner-2", "embedding-source",
		"move_recovery:source_reparse_enqueue_required:generation-2", binding.WorkflowID,
		func(tx *gorm.DB, transition func(*gorm.DB) error) error {
			return coordinator.BindPreparedWorkflowTransitionTx(tx, binding, transition)
		}, time.Now(),
	)
	require.NoError(t, err)
	require.True(t, moved)

	var status, workflowID string
	require.NoError(t, db.Raw(`
		SELECT parse_status, processing_workflow_id FROM knowledges WHERE id = ?
	`, "knowledge-1").Row().Scan(&status, &workflowID))
	assert.Equal(t, types.ParseStatusPending, status)
	assert.Equal(t, binding.WorkflowID, workflowID)
	require.ErrorContains(t,
		coordinator.AbortPreparedWorkflow(context.Background(), binding, "late producer abort"),
		"bound prepared workflow cannot be aborted",
	)
}

func TestFinalizeSourceRecoveryFailureRollsBackBindingAndAllowsSafeAbort(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-2", "owner-2")
	coordinator, binding := seedPreparedMoveWorkflow(
		t, db, "knowledge-1", "kb-source", "generation-2", "owner-2",
	)
	require.NoError(t, db.Exec(`
		CREATE TRIGGER fail_source_recovery_ledger_delete
		BEFORE DELETE ON knowledge_fanout_completions
		BEGIN SELECT RAISE(ABORT, 'injected source recovery failure'); END
	`).Error)

	moved, err := repo.FinalizeReparseKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-source",
		"generation-2", "owner-2", "embedding-source",
		"move_recovery:source_reparse_enqueue_required:generation-2", binding.WorkflowID,
		func(tx *gorm.DB, transition func(*gorm.DB) error) error {
			return coordinator.BindPreparedWorkflowTransitionTx(tx, binding, transition)
		}, time.Now(),
	)
	require.ErrorContains(t, err, "injected source recovery failure")
	assert.False(t, moved)

	var status, workflowID string
	require.NoError(t, db.Raw(`
		SELECT parse_status, processing_workflow_id FROM knowledges WHERE id = ?
	`, "knowledge-1").Row().Scan(&status, &workflowID))
	assert.Equal(t, types.ParseStatusProcessing, status)
	assert.Empty(t, workflowID)
	require.NoError(t,
		coordinator.AbortPreparedWorkflow(context.Background(), binding, "business transaction rolled back"),
	)
	var workflow documentqueue.Workflow
	require.NoError(t, db.Where("id = ?", binding.WorkflowID).Take(&workflow).Error)
	assert.Equal(t, documentqueue.StateCancelled, workflow.State)
}

func TestFinalizeReparseKnowledgeMoveRejectsDeletedTargetKB(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-2", "owner-2")
	require.NoError(t, db.Exec(
		"UPDATE knowledge_bases SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'kb-target'",
	).Error)

	moved, err := repo.FinalizeReparseKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-target",
		"generation-2", "owner-2", "embedding-target",
		"move_recovery:wiki_pending:kb-source:kb-target", "", nil, time.Now(),
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, kbwritefence.ErrKnowledgeBaseUnavailable))
	assert.False(t, moved)

	var kbID, status string
	require.NoError(t, db.Raw(
		"SELECT knowledge_base_id, parse_status FROM knowledges WHERE id = ?", "knowledge-1",
	).Row().Scan(&kbID, &status))
	assert.Equal(t, "kb-source", kbID)
	assert.Equal(t, types.ParseStatusProcessing, status)
}

func TestFinalizeReparseKnowledgeMoveLedgerFailureRollsBackKnowledge(t *testing.T) {
	db := setupKnowledgeMoveFinalizeDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	seedKnowledgeMoveFinalize(t, db, "knowledge-1", "kb-source", "generation-2", "owner-2")
	require.NoError(t, db.Exec(`
		CREATE TRIGGER fail_fanout_scope_delete
		BEFORE DELETE ON knowledge_fanout_completions
		BEGIN SELECT RAISE(ABORT, 'injected fanout delete failure'); END
	`).Error)

	moved, err := repo.FinalizeReparseKnowledgeMove(
		context.Background(), 7, "knowledge-1", "kb-source", "kb-target",
		"generation-2", "owner-2", "embedding-target",
		"move_recovery:wiki_pending:kb-source:kb-target", "", nil, time.Now(),
	)
	require.ErrorContains(t, err, "injected fanout delete failure")
	assert.False(t, moved)

	var kbID, status string
	require.NoError(t, db.Raw("SELECT knowledge_base_id, parse_status FROM knowledges WHERE id = ?", "knowledge-1").
		Row().Scan(&kbID, &status))
	assert.Equal(t, "kb-source", kbID)
	assert.Equal(t, types.ParseStatusProcessing, status)
	var ledgerCount int64
	require.NoError(t, db.Table("knowledge_fanout_completions").Where("knowledge_id = ?", "knowledge-1").Count(&ledgerCount).Error)
	assert.EqualValues(t, 2, ledgerCount)
	assertPersistentOwnershipScope(t, db, "kb-source", "generation-2")
}
