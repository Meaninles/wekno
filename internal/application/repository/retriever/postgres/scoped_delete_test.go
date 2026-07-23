package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDeleteByKnowledgeBaseAndKnowledgeIDListKeepsTargetCopy(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:postgres-scoped-delete?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
		CREATE TABLE embeddings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id TEXT NOT NULL,
			source_type INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL,
			knowledge_id TEXT NOT NULL,
			updated_at DATETIME,
			UNIQUE (source_id, source_type)
		)
	`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO embeddings (source_id, source_type, knowledge_base_id, knowledge_id) VALUES
			('source-1', 1, 'kb-source', 'knowledge-1'),
			('source-2', 1, 'kb-target', 'knowledge-1'),
			('source-3', 1, 'kb-source', 'knowledge-2')
	`).Error)

	repo := &pgRepository{db: db}
	require.NoError(t, repo.DeleteByKnowledgeBaseAndKnowledgeIDList(
		context.Background(),
		"kb-source",
		[]string{"knowledge-1"},
		768,
		"manual",
	))

	var sourceMovedCount int64
	require.NoError(t, db.Table("embeddings").
		Where("knowledge_base_id = ? AND knowledge_id = ?", "kb-source", "knowledge-1").
		Count(&sourceMovedCount).Error)
	require.Zero(t, sourceMovedCount)

	var targetCopyCount int64
	require.NoError(t, db.Table("embeddings").
		Where("knowledge_base_id = ? AND knowledge_id = ?", "kb-target", "knowledge-1").
		Count(&targetCopyCount).Error)
	require.EqualValues(t, 1, targetCopyCount, "source-scoped cleanup must not delete the target copy")

	var unrelatedSourceCount int64
	require.NoError(t, db.Table("embeddings").
		Where("knowledge_base_id = ? AND knowledge_id = ?", "kb-source", "knowledge-2").
		Count(&unrelatedSourceCount).Error)
	require.EqualValues(t, 1, unrelatedSourceCount)
}

func TestMoveKnowledgeIndicesInPlacePreservesUniqueSourceIdentity(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:postgres-in-place-move?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
		CREATE TABLE embeddings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id TEXT NOT NULL,
			source_type INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL,
			knowledge_id TEXT NOT NULL,
			updated_at DATETIME,
			UNIQUE (source_id, source_type)
		)
	`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO embeddings (source_id, source_type, knowledge_base_id, knowledge_id)
		VALUES ('stable-source', 7, 'kb-source', 'knowledge-1')
	`).Error)

	repo := &pgRepository{db: db}
	require.NoError(t, repo.MoveKnowledgeIndicesInPlace(
		context.Background(),
		"kb-source",
		"kb-target",
		"knowledge-1",
		[]string{"chunk-1"},
		768,
		"manual",
	))

	var targetRows int64
	require.NoError(t, db.Table("embeddings").
		Where("knowledge_base_id = ? AND knowledge_id = ? AND source_id = ? AND source_type = ?",
			"kb-target", "knowledge-1", "stable-source", 7).
		Count(&targetRows).Error)
	require.EqualValues(t, 1, targetRows)

	var totalRows int64
	require.NoError(t, db.Table("embeddings").Count(&totalRows).Error)
	require.EqualValues(t, 1, totalRows, "in-place move must not copy or delete the unique source row")
}

func TestClassifyInPlaceMoveResultReadBack(t *testing.T) {
	newDB := func(t *testing.T, name string, kbRows ...string) *gorm.DB {
		t.Helper()
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		require.NoError(t, err)
		require.NoError(t, db.Exec(`
			CREATE TABLE embeddings (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				source_id TEXT NOT NULL,
				source_type INTEGER NOT NULL,
				knowledge_base_id TEXT NOT NULL,
				knowledge_id TEXT NOT NULL,
				UNIQUE (source_id, source_type)
			)
		`).Error)
		for i, kbID := range kbRows {
			require.NoError(t, db.Exec(
				"INSERT INTO embeddings (source_id, source_type, knowledge_base_id, knowledge_id) VALUES (?, ?, ?, ?)",
				"source-"+string(rune('a'+i)), i+1, kbID, "knowledge-1",
			).Error)
		}
		return db
	}

	t.Run("target commit proved despite transport error", func(t *testing.T) {
		repo := &pgRepository{db: newDB(t, "pg-readback-target", "kb-target")}
		err := repo.classifyInPlaceMoveResult(
			context.Background(), "kb-source", "kb-target", "knowledge-1", errors.New("response lost"),
		)
		require.NoError(t, err)
	})

	t.Run("source intact proved", func(t *testing.T) {
		repo := &pgRepository{db: newDB(t, "pg-readback-source", "kb-source")}
		err := repo.classifyInPlaceMoveResult(
			context.Background(), "kb-source", "kb-target", "knowledge-1", errors.New("write rejected"),
		)
		require.Error(t, err)
		var stateErr interfaces.KnowledgeIndexInPlaceMoveError
		require.ErrorAs(t, err, &stateErr)
		require.True(t, stateErr.SourceIntact())
	})

	t.Run("mixed ownership remains uncertain", func(t *testing.T) {
		repo := &pgRepository{db: newDB(t, "pg-readback-mixed", "kb-source", "kb-target")}
		err := repo.classifyInPlaceMoveResult(
			context.Background(), "kb-source", "kb-target", "knowledge-1", errors.New("write uncertain"),
		)
		require.Error(t, err)
		var stateErr interfaces.KnowledgeIndexInPlaceMoveError
		require.ErrorAs(t, err, &stateErr)
		require.False(t, stateErr.SourceIntact())
	})
}
