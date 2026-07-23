package repository

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func insertKnowledgeStateFenceRow(t *testing.T, db *gorm.DB, status string) (string, string) {
	t.Helper()
	id := uuid.New().String()
	kbID := uuid.New().String()
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (
			id, tenant_id, knowledge_base_id, type, title, source,
			parse_status, enable_status, pending_subtasks_count
		) VALUES (?, 7, ?, 'document', 'state-fence', 'manual', ?, 'enabled', 3)
	`, id, kbID, status).Error)
	return id, kbID
}

type knowledgeStateFenceSnapshot struct {
	KnowledgeBaseID      string
	ParseStatus          string
	EnableStatus         string
	PendingSubtasksCount int
}

func loadKnowledgeStateFenceSnapshot(t *testing.T, db *gorm.DB, id string) knowledgeStateFenceSnapshot {
	t.Helper()
	var snapshot knowledgeStateFenceSnapshot
	require.NoError(t, db.Table("knowledges").
		Select("knowledge_base_id, parse_status, enable_status, pending_subtasks_count").
		Where("id = ?", id).
		Take(&snapshot).Error)
	return snapshot
}

func TestCompareAndSwapKnowledgeState_MoveClaimHasSingleWinner(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	id, kbID := insertKnowledgeStateFenceRow(t, db, types.ParseStatusCompleted)

	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			swapped, err := repo.CompareAndSwapKnowledgeState(
				context.Background(),
				7,
				id,
				kbID,
				types.ParseStatusCompleted,
				map[string]interface{}{
					"parse_status": types.ParseStatusProcessing,
					"updated_at":   time.Now(),
				},
			)
			require.NoError(t, err)
			if swapped {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 1, winners.Load())
	assert.Equal(t, types.ParseStatusProcessing, loadKnowledgeStateFenceSnapshot(t, db, id).ParseStatus)
}

func TestCompareAndSwapKnowledgeState_DeleteClaimBlocksMoveFinalWrite(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	id, sourceKBID := insertKnowledgeStateFenceRow(t, db, types.ParseStatusProcessing)
	targetKBID := uuid.New().String()

	require.NoError(t, db.Table("knowledges").Where("id = ?", id).
		Updates(map[string]interface{}{
			"parse_status": types.ParseStatusDeleting,
			"updated_at":   time.Now(),
		}).Error)

	swapped, err := repo.CompareAndSwapKnowledgeState(
		context.Background(),
		7,
		id,
		sourceKBID,
		types.ParseStatusProcessing,
		map[string]interface{}{
			"knowledge_base_id": targetKBID,
			"parse_status":      types.ParseStatusCompleted,
			"updated_at":        time.Now(),
		},
	)
	require.NoError(t, err)
	assert.False(t, swapped)

	snapshot := loadKnowledgeStateFenceSnapshot(t, db, id)
	assert.Equal(t, sourceKBID, snapshot.KnowledgeBaseID)
	assert.Equal(t, types.ParseStatusDeleting, snapshot.ParseStatus)
}

func TestCompareAndSwapKnowledgeState_KnowledgeBaseChangeBlocksMoveFinalWrite(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	id, sourceKBID := insertKnowledgeStateFenceRow(t, db, types.ParseStatusProcessing)
	concurrentKBID := uuid.New().String()
	targetKBID := uuid.New().String()

	require.NoError(t, db.Table("knowledges").Where("id = ?", id).
		Update("knowledge_base_id", concurrentKBID).Error)

	swapped, err := repo.CompareAndSwapKnowledgeState(
		context.Background(),
		7,
		id,
		sourceKBID,
		types.ParseStatusProcessing,
		map[string]interface{}{
			"knowledge_base_id": targetKBID,
			"parse_status":      types.ParseStatusCompleted,
		},
	)
	require.NoError(t, err)
	assert.False(t, swapped)

	snapshot := loadKnowledgeStateFenceSnapshot(t, db, id)
	assert.Equal(t, concurrentKBID, snapshot.KnowledgeBaseID)
	assert.Equal(t, types.ParseStatusProcessing, snapshot.ParseStatus)
}

func TestCompareAndSwapKnowledgeState_DeleteClaimBlocksReparsePendingAndFailedWrites(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	id, kbID := insertKnowledgeStateFenceRow(t, db, types.ParseStatusDeleting)

	for name, updates := range map[string]map[string]interface{}{
		"pending": {
			"parse_status":           types.ParseStatusPending,
			"enable_status":          "disabled",
			"pending_subtasks_count": 0,
		},
		"failed": {
			"parse_status":           types.ParseStatusFailed,
			"error_message":          "stale reparse failure",
			"pending_subtasks_count": 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			swapped, err := repo.CompareAndSwapKnowledgeState(
				context.Background(),
				7,
				id,
				kbID,
				types.ParseStatusProcessing,
				updates,
			)
			require.NoError(t, err)
			assert.False(t, swapped)
		})
	}

	snapshot := loadKnowledgeStateFenceSnapshot(t, db, id)
	assert.Equal(t, types.ParseStatusDeleting, snapshot.ParseStatus)
	assert.Equal(t, "enabled", snapshot.EnableStatus)
	assert.Equal(t, 3, snapshot.PendingSubtasksCount)
}

func TestCompareAndSwapKnowledgeGeneration_ABAStatusRejectsStaleWorker(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	id, kbID := insertKnowledgeStateFenceRow(t, db, types.ParseStatusProcessing)
	require.NoError(t, db.Table("knowledges").Where("id = ?", id).
		Update("file_hash", "generation-new").Error)

	swapped, err := repo.CompareAndSwapKnowledgeGeneration(
		context.Background(),
		7,
		id,
		kbID,
		types.ParseStatusProcessing,
		"generation-old",
		map[string]interface{}{
			"parse_status": types.ParseStatusCompleted,
		},
	)
	require.NoError(t, err)
	assert.False(t, swapped)
	assert.Equal(t, types.ParseStatusProcessing, loadKnowledgeStateFenceSnapshot(t, db, id).ParseStatus)
}

func TestCompareAndSwapKnowledgeGeneration_DuplicateManualWorkersHaveOneWinner(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	id, kbID := insertKnowledgeStateFenceRow(t, db, types.ParseStatusPending)
	require.NoError(t, db.Table("knowledges").Where("id = ?", id).
		Update("file_hash", "generation-1").Error)

	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			swapped, err := repo.CompareAndSwapKnowledgeGeneration(
				context.Background(),
				7,
				id,
				kbID,
				types.ParseStatusPending,
				"generation-1",
				map[string]interface{}{
					"parse_status": types.ParseStatusProcessing,
					"updated_at":   time.Now(),
				},
			)
			require.NoError(t, err)
			if swapped {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 1, winners.Load())
	assert.Equal(t, types.ParseStatusProcessing, loadKnowledgeStateFenceSnapshot(t, db, id).ParseStatus)
}
