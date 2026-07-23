package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupKnowledgeStorageFinalizeTest(t *testing.T) (*knowledgeRepository, string, string) {
	t.Helper()
	db := setupKnowledgeTestDB(t)
	require.NoError(t, db.Exec(`
		CREATE TABLE tenants (
			id INTEGER PRIMARY KEY,
			storage_used BIGINT NOT NULL DEFAULT 0,
			deleted_at DATETIME
		)
	`).Error)
	require.NoError(t, db.Exec(`INSERT INTO tenants (id, storage_used) VALUES (7, 10)`).Error)
	id, kbID := insertKnowledgeStateFenceRow(t, db, types.ParseStatusProcessing)
	return NewKnowledgeRepository(db).(*knowledgeRepository), id, kbID
}

func TestFinalizeKnowledgeWithStorageCommitsStateAndChargeAtomically(t *testing.T) {
	repo, id, kbID := setupKnowledgeStorageFinalizeTest(t)
	knowledge, err := repo.GetKnowledgeByID(context.Background(), 7, id)
	require.NoError(t, err)
	knowledge.KnowledgeBaseID = kbID
	knowledge.ParseStatus = types.ParseStatusCompleted
	knowledge.StorageSize = 25

	finalized, err := repo.FinalizeKnowledgeWithStorage(
		context.Background(), knowledge, types.ParseStatusProcessing, 25,
	)
	require.NoError(t, err)
	require.True(t, finalized)

	var stored struct {
		ParseStatus string
		StorageSize int64
	}
	require.NoError(t, repo.db.Table("knowledges").Select("parse_status, storage_size").Where("id = ?", id).Take(&stored).Error)
	assert.Equal(t, types.ParseStatusCompleted, stored.ParseStatus)
	assert.EqualValues(t, 25, stored.StorageSize)
	var storageUsed int64
	require.NoError(t, repo.db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 35, storageUsed)
}

func TestFinalizeKnowledgeWithStorageStateConflictDoesNotCharge(t *testing.T) {
	repo, id, _ := setupKnowledgeStorageFinalizeTest(t)
	knowledge, err := repo.GetKnowledgeByID(context.Background(), 7, id)
	require.NoError(t, err)
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Update("parse_status", types.ParseStatusDeleting).Error)
	knowledge.ParseStatus = types.ParseStatusCompleted
	knowledge.StorageSize = 25

	finalized, err := repo.FinalizeKnowledgeWithStorage(
		context.Background(), knowledge, types.ParseStatusProcessing, 25,
	)
	require.NoError(t, err)
	assert.False(t, finalized)

	var stored struct {
		ParseStatus string
		StorageSize int64
	}
	require.NoError(t, repo.db.Table("knowledges").Select("parse_status, storage_size").Where("id = ?", id).Take(&stored).Error)
	assert.Equal(t, types.ParseStatusDeleting, stored.ParseStatus)
	assert.Zero(t, stored.StorageSize)
	var storageUsed int64
	require.NoError(t, repo.db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 10, storageUsed)
}

func TestFinalizeKnowledgeWithStorageTenantFailureRollsBackKnowledge(t *testing.T) {
	repo, id, _ := setupKnowledgeStorageFinalizeTest(t)
	knowledge, err := repo.GetKnowledgeByID(context.Background(), 7, id)
	require.NoError(t, err)
	require.NoError(t, repo.db.Exec(`DELETE FROM tenants WHERE id = 7`).Error)
	knowledge.ParseStatus = types.ParseStatusCompleted
	knowledge.StorageSize = 25

	finalized, err := repo.FinalizeKnowledgeWithStorage(
		context.Background(), knowledge, types.ParseStatusProcessing, 25,
	)
	require.Error(t, err)
	assert.False(t, finalized)

	var stored struct {
		ParseStatus string
		StorageSize int64
	}
	require.NoError(t, repo.db.Table("knowledges").Select("parse_status, storage_size").Where("id = ?", id).Take(&stored).Error)
	assert.Equal(t, types.ParseStatusProcessing, stored.ParseStatus)
	assert.Zero(t, stored.StorageSize)
}

func TestResetKnowledgeStorageCommitsRowAndTenantDecrementAtomically(t *testing.T) {
	repo, id, kbID := setupKnowledgeStorageFinalizeTest(t)
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Update("storage_size", 8).Error)

	reset, err := repo.ResetKnowledgeStorage(
		context.Background(), 7, id, kbID, types.ParseStatusProcessing, "", 8,
	)
	require.NoError(t, err)
	require.True(t, reset)

	var storageSize int64
	require.NoError(t, repo.db.Table("knowledges").Select("storage_size").Where("id = ?", id).Scan(&storageSize).Error)
	assert.Zero(t, storageSize)
	var storageUsed int64
	require.NoError(t, repo.db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 2, storageUsed)
}

func TestResetKnowledgeStorageDeleteWinnerDoesNotDoubleDecrement(t *testing.T) {
	repo, id, kbID := setupKnowledgeStorageFinalizeTest(t)
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Updates(map[string]interface{}{
			"storage_size": 8,
			"parse_status": types.ParseStatusDeleting,
		}).Error)

	reset, err := repo.ResetKnowledgeStorage(
		context.Background(), 7, id, kbID, types.ParseStatusProcessing, "", 8,
	)
	require.NoError(t, err)
	assert.False(t, reset)
	var storageUsed int64
	require.NoError(t, repo.db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 10, storageUsed)
}

func TestFinalizeKnowledgeWithStorageDocumentGenerationConflictDoesNotCharge(t *testing.T) {
	repo, id, _ := setupKnowledgeStorageFinalizeTest(t)
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Update("file_hash", "generation-old").Error)
	knowledge, err := repo.GetKnowledgeByID(context.Background(), 7, id)
	require.NoError(t, err)
	require.Equal(t, "generation-old", knowledge.FileHash)

	// A newer edit can cycle parse_status back to processing. Status-only CAS
	// would let the old worker charge and publish here; generation must reject it.
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Update("file_hash", "generation-new").Error)
	knowledge.ParseStatus = types.ParseStatusCompleted
	knowledge.StorageSize = 25

	finalized, err := repo.FinalizeKnowledgeWithStorage(
		context.Background(), knowledge, types.ParseStatusProcessing, 25,
	)
	require.NoError(t, err)
	assert.False(t, finalized)

	var storageUsed int64
	require.NoError(t, repo.db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 10, storageUsed)
}

func TestFinalizeKnowledgeWithStorageFAQGenerationConflictDoesNotCharge(t *testing.T) {
	repo, id, _ := setupKnowledgeStorageFinalizeTest(t)
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Updates(map[string]interface{}{
			"type":      types.KnowledgeTypeFAQ,
			"file_hash": "faq-generation-old",
		}).Error)
	knowledge, err := repo.GetKnowledgeByID(context.Background(), 7, id)
	require.NoError(t, err)
	require.Equal(t, "faq-generation-old", knowledge.FileHash)

	// indexFAQChunks uses the same atomic finalizer as ProcessDocument. A
	// newer FAQ row generation must reject the old vector batch and its charge.
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Update("file_hash", "faq-generation-new").Error)
	knowledge.StorageSize = 25

	finalized, err := repo.FinalizeKnowledgeWithStorage(
		context.Background(), knowledge, types.ParseStatusProcessing, 25,
	)
	require.NoError(t, err)
	assert.False(t, finalized)

	var storageUsed int64
	require.NoError(t, repo.db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 10, storageUsed)
}

func TestResetKnowledgeStorageGenerationConflictPreservesCharge(t *testing.T) {
	repo, id, kbID := setupKnowledgeStorageFinalizeTest(t)
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Updates(map[string]interface{}{
			"processing_generation": "generation-new",
			"storage_size":          8,
		}).Error)

	reset, err := repo.ResetKnowledgeStorage(
		context.Background(), 7, id, kbID, types.ParseStatusProcessing, "generation-old", 8,
	)
	require.NoError(t, err)
	assert.False(t, reset)

	var row struct {
		StorageSize int64
	}
	require.NoError(t, repo.db.Table("knowledges").Select("storage_size").Where("id = ?", id).Take(&row).Error)
	assert.EqualValues(t, 8, row.StorageSize)
	var storageUsed int64
	require.NoError(t, repo.db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 10, storageUsed)
}

func TestFailedReparsePreservesStorageOwnershipForLaterRecovery(t *testing.T) {
	repo, id, kbID := setupKnowledgeStorageFinalizeTest(t)
	require.NoError(t, repo.db.Table("knowledges").Where("id = ?", id).
		Updates(map[string]interface{}{
			"file_hash":             "generation-1",
			"processing_generation": "generation-1",
			"storage_size":          8,
		}).Error)

	// A cleanup failure marks the generation failed but deliberately leaves
	// storage_size untouched. That row remains the durable owner of the tenant
	// charge, so a later delete/reparse can recover it exactly once.
	failed, err := repo.CompareAndSwapKnowledgeGeneration(
		context.Background(),
		7,
		id,
		kbID,
		types.ParseStatusProcessing,
		"generation-1",
		map[string]interface{}{
			"parse_status":  types.ParseStatusFailed,
			"error_message": "external cleanup failed",
		},
	)
	require.NoError(t, err)
	require.True(t, failed)

	var before struct {
		ParseStatus string
		StorageSize int64
	}
	require.NoError(t, repo.db.Table("knowledges").Select("parse_status, storage_size").Where("id = ?", id).Take(&before).Error)
	assert.Equal(t, types.ParseStatusFailed, before.ParseStatus)
	assert.EqualValues(t, 8, before.StorageSize)

	reset, err := repo.ResetKnowledgeStorage(
		context.Background(), 7, id, kbID, types.ParseStatusFailed, "generation-1", 8,
	)
	require.NoError(t, err)
	require.True(t, reset)

	var storageUsed int64
	require.NoError(t, repo.db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 2, storageUsed)
}
