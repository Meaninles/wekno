package knowledgeaux

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestAdoptMigratedSourceCreatesLedgerAndSwitchesPathAtomically(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "")
	require.NoError(t, db.Model(owner).UpdateColumn(
		"file_path",
		"obs://bucket-a/legacy/source.pdf",
	).Error)

	service := &fakeFileService{provider: "obs", failures: map[string]int{}}
	registry := New(db, service)
	adopted, err := registry.AdoptMigratedSource(
		context.Background(),
		"obs://bucket-a/legacy/source.pdf",
		"",
		Object{
			TenantID:         7,
			KnowledgeBaseID:  "kb-1",
			KnowledgeID:      "knowledge-1",
			Path:             "obs://bucket-a/current/7/knowledge-1/source.pdf",
			FallbackProvider: "obs",
			Kind:             KindSourceFile,
		},
		service,
	)
	require.NoError(t, err)
	require.NotEmpty(t, adopted.ProcessingGeneration)

	var persisted types.Knowledge
	require.NoError(t, db.First(&persisted, "id = ?", owner.ID).Error)
	require.Equal(t, adopted.Path, persisted.FilePath)
	require.Equal(t, adopted.ProcessingGeneration, persisted.ProcessingGeneration)

	var rows []types.TaskPendingOp
	require.NoError(t, db.Where(
		"tenant_id = ? AND task_type = ? AND scope_id = ?",
		uint64(7),
		TaskType,
		"kb-1",
	).Find(&rows).Error)
	require.Len(t, rows, 1)
	var payload Object
	require.NoError(t, json.Unmarshal(rows[0].Payload, &payload))
	require.Equal(t, adopted.Path, payload.Path)
	require.Equal(t, adopted.ProcessingGeneration, payload.ProcessingGeneration)
	require.NotNil(t, payload.Binding)
	require.NotEmpty(t, payload.Binding.Fingerprint)
}

func TestAdoptMigratedSourceRollsBackLedgerWhenPathSwitchFails(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-1")
	require.NoError(t, db.Model(owner).UpdateColumn(
		"file_path",
		"obs://bucket-a/legacy/source.pdf",
	).Error)
	require.NoError(t, db.Exec(`
		CREATE TRIGGER reject_source_migration
		BEFORE UPDATE OF file_path ON knowledges
		WHEN NEW.file_path = 'obs://bucket-a/current/7/knowledge-1/source.pdf'
		BEGIN
			SELECT RAISE(ABORT, 'injected path switch failure');
		END
	`).Error)

	service := &fakeFileService{provider: "obs", failures: map[string]int{}}
	registry := New(db, service)
	_, err := registry.AdoptMigratedSource(
		context.Background(),
		"obs://bucket-a/legacy/source.pdf",
		"generation-1",
		Object{
			TenantID:             7,
			KnowledgeBaseID:      "kb-1",
			KnowledgeID:          "knowledge-1",
			ProcessingGeneration: "generation-1",
			Path:                 "obs://bucket-a/current/7/knowledge-1/source.pdf",
			FallbackProvider:     "obs",
			Kind:                 KindSourceFile,
		},
		service,
	)
	require.Error(t, err)

	var persisted types.Knowledge
	require.NoError(t, db.First(&persisted, "id = ?", owner.ID).Error)
	require.Equal(t, "obs://bucket-a/legacy/source.pdf", persisted.FilePath)
	require.Equal(t, "generation-1", persisted.ProcessingGeneration)
	var ledgerCount int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("task_type = ?", TaskType).
		Count(&ledgerCount).Error)
	require.Zero(t, ledgerCount)
}

func TestAdoptMigratedSourceRejectsConcurrentGenerationChange(t *testing.T) {
	db := openRegistryTestDB(t)
	owner := createOwner(t, db, types.ParseStatusCompleted, "generation-2")
	require.NoError(t, db.Model(owner).UpdateColumn(
		"file_path",
		"obs://bucket-a/legacy/source.pdf",
	).Error)

	service := &fakeFileService{provider: "obs", failures: map[string]int{}}
	registry := New(db, service)
	_, err := registry.AdoptMigratedSource(
		context.Background(),
		"obs://bucket-a/legacy/source.pdf",
		"generation-1",
		Object{
			TenantID:             7,
			KnowledgeBaseID:      "kb-1",
			KnowledgeID:          "knowledge-1",
			ProcessingGeneration: "generation-1",
			Path:                 "obs://bucket-a/current/7/knowledge-1/source.pdf",
			FallbackProvider:     "obs",
			Kind:                 KindSourceFile,
		},
		service,
	)
	require.ErrorIs(t, err, ErrSourceMigrationConflict)

	var ledgerCount int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("task_type = ?", TaskType).
		Count(&ledgerCount).Error)
	require.Zero(t, ledgerCount)
}
