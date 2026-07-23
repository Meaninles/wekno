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

func insertOwnedProcessingKnowledge(
	t *testing.T,
	db *gorm.DB,
	status string,
	generation string,
	owner string,
	pending int,
	processedAt interface{},
) (string, string) {
	t.Helper()
	id, kbID := uuid.NewString(), uuid.NewString()
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (
			id, tenant_id, knowledge_base_id, type, title, source,
			parse_status, processing_generation, processing_owner,
			pending_subtasks_count, processed_at
		) VALUES (?, 7, ?, 'document', 'owned-processing', 'manual', ?, ?, ?, ?, ?)
	`, id, kbID, status, generation, owner, pending, processedAt).Error)
	return id, kbID
}

func setupOwnedFinalizerRepository(t *testing.T) (*knowledgeRepository, *gorm.DB) {
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
	return NewKnowledgeRepository(db).(*knowledgeRepository), db
}

func TestCompareAndSwapDocumentProcessingRequiresExactIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		generation string
		owner      string
		wantSwap   bool
	}{
		{name: "happy", status: types.ParseStatusProcessing, generation: "generation-1", owner: "owner-1", wantSwap: true},
		{name: "wrong generation", status: types.ParseStatusProcessing, generation: "generation-old", owner: "owner-1"},
		{name: "wrong owner", status: types.ParseStatusProcessing, generation: "generation-1", owner: "owner-old"},
		{name: "wrong status", status: types.ParseStatusPending, generation: "generation-1", owner: "owner-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupKnowledgeTestDB(t)
			repo := NewKnowledgeRepository(db).(*knowledgeRepository)
			id, kbID := insertOwnedProcessingKnowledge(
				t, db, test.status, "generation-1", "owner-1", 0, nil,
			)
			swapped, err := repo.CompareAndSwapDocumentProcessing(
				context.Background(), 7, id, kbID, types.ParseStatusProcessing,
				test.generation, test.owner,
				map[string]interface{}{"error_message": "claimed"},
			)
			require.NoError(t, err)
			assert.Equal(t, test.wantSwap, swapped)
			var row struct {
				ErrorMessage *string
			}
			require.NoError(t, db.Table("knowledges").Select("error_message").Where("id = ?", id).Take(&row).Error)
			if test.wantSwap {
				require.NotNil(t, row.ErrorMessage)
				assert.Equal(t, "claimed", *row.ErrorMessage)
			} else {
				assert.Nil(t, row.ErrorMessage)
			}
		})
	}
}

func TestCompareAndSwapKnowledgeProcessingGenerationRequiresGenerationAndActiveStatus(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	id, kbID := insertOwnedProcessingKnowledge(
		t, db, types.ParseStatusFinalizing, "generation-1", "", 2, nil,
	)

	swapped, err := repo.CompareAndSwapKnowledgeProcessingGeneration(
		context.Background(), 7, id, kbID, "generation-old",
		[]string{types.ParseStatusFinalizing}, map[string]interface{}{"summary_status": "processing"},
	)
	require.NoError(t, err)
	assert.False(t, swapped)

	swapped, err = repo.CompareAndSwapKnowledgeProcessingGeneration(
		context.Background(), 7, id, kbID, "generation-1",
		[]string{types.ParseStatusProcessing}, map[string]interface{}{"summary_status": "processing"},
	)
	require.NoError(t, err)
	assert.False(t, swapped)

	swapped, err = repo.CompareAndSwapKnowledgeProcessingGeneration(
		context.Background(), 7, id, kbID, "generation-1",
		[]string{types.ParseStatusFinalizing}, map[string]interface{}{"summary_status": "processing"},
	)
	require.NoError(t, err)
	assert.True(t, swapped)

	_, err = repo.CompareAndSwapKnowledgeProcessingGeneration(
		context.Background(), 7, id, kbID, "generation-1",
		[]string{types.ParseStatusCompleted}, map[string]interface{}{"summary_status": "completed"},
	)
	require.Error(t, err)
}

func TestFailDocumentProcessingGenerationHonorsOwnerAndCoreCommit(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      string
		generation  string
		owner       string
		processedAt interface{}
		wantFailed  bool
	}{
		{name: "pending happy", status: types.ParseStatusPending, generation: "generation-1", owner: "owner-1", wantFailed: true},
		{name: "processing happy", status: types.ParseStatusProcessing, generation: "generation-1", owner: "owner-1", wantFailed: true},
		{name: "wrong generation", status: types.ParseStatusPending, generation: "generation-old", owner: "owner-1"},
		{name: "wrong owner", status: types.ParseStatusPending, generation: "generation-1", owner: "owner-old"},
		{name: "terminal status", status: types.ParseStatusFinalizing, generation: "generation-1", owner: "owner-1"},
		{name: "core committed", status: types.ParseStatusProcessing, generation: "generation-1", owner: "owner-1", processedAt: time.Now()},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupKnowledgeTestDB(t)
			repo := NewKnowledgeRepository(db).(*knowledgeRepository)
			id, kbID := insertOwnedProcessingKnowledge(
				t, db, test.status, "generation-1", "owner-1", 0, test.processedAt,
			)
			failed, err := repo.FailDocumentProcessingGeneration(
				context.Background(), 7, id, kbID, test.generation, test.owner,
				map[string]interface{}{"parse_status": types.ParseStatusFailed},
			)
			require.NoError(t, err)
			assert.Equal(t, test.wantFailed, failed)
			var status string
			require.NoError(t, db.Table("knowledges").Select("parse_status").Where("id = ?", id).Scan(&status).Error)
			if test.wantFailed {
				assert.Equal(t, types.ParseStatusFailed, status)
			} else {
				assert.Equal(t, test.status, status)
			}
		})
	}
}

func TestCompletePostProcessDeadLetterGenerationRequiresCommittedExactGeneration(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      string
		generation  string
		processedAt interface{}
		wantDone    bool
	}{
		{name: "processing committed", status: types.ParseStatusProcessing, generation: "generation-1", processedAt: time.Now(), wantDone: true},
		{name: "finalizing committed", status: types.ParseStatusFinalizing, generation: "generation-1", processedAt: time.Now(), wantDone: true},
		{name: "core not committed", status: types.ParseStatusProcessing, generation: "generation-1"},
		{name: "wrong generation", status: types.ParseStatusFinalizing, generation: "generation-old", processedAt: time.Now()},
		{name: "already completed", status: types.ParseStatusCompleted, generation: "generation-1", processedAt: time.Now()},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := setupKnowledgeTestDB(t)
			repo := NewKnowledgeRepository(db).(*knowledgeRepository)
			id, kbID := insertOwnedProcessingKnowledge(
				t, db, test.status, "generation-1", "owner-1", 3, test.processedAt,
			)
			require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", id).Updates(map[string]interface{}{
				"processing_fanout": types.JSON(`{"stage":"enrichment"}`),
				"summary_status":    types.SummaryStatusPending,
				"error_message":     "postprocess retry exhausted",
			}).Error)

			done, err := repo.CompletePostProcessDeadLetterGeneration(
				context.Background(), 7, id, kbID, test.generation,
			)
			require.NoError(t, err)
			assert.Equal(t, test.wantDone, done)

			var row struct {
				ParseStatus          string
				PendingSubtasksCount int
				ProcessingOwner      string
				ProcessingFanout     types.JSON
				SummaryStatus        string
				ErrorMessage         string
				ProcessedAt          *time.Time
			}
			require.NoError(t, db.Table("knowledges").Where("id = ?", id).Take(&row).Error)
			if test.wantDone {
				assert.Equal(t, types.ParseStatusCompleted, row.ParseStatus)
				assert.Zero(t, row.PendingSubtasksCount)
				assert.Empty(t, row.ProcessingOwner)
				assert.Empty(t, row.ProcessingFanout)
				assert.Equal(t, types.SummaryStatusFailed, row.SummaryStatus)
				assert.Empty(t, row.ErrorMessage)
				require.NotNil(t, row.ProcessedAt)
			} else {
				assert.Equal(t, test.status, row.ParseStatus)
				assert.Equal(t, 3, row.PendingSubtasksCount)
				assert.Equal(t, "owner-1", row.ProcessingOwner)
				assert.NotEmpty(t, row.ProcessingFanout)
				assert.Equal(t, types.SummaryStatusPending, row.SummaryStatus)
				assert.Equal(t, "postprocess retry exhausted", row.ErrorMessage)
			}
		})
	}
}

func TestFinalizeKnowledgeWithStorageOwnedCommitsAndChargesExactlyOnce(t *testing.T) {
	repo, db := setupOwnedFinalizerRepository(t)
	id, kbID := insertOwnedProcessingKnowledge(
		t, db, types.ParseStatusProcessing, "generation-1", "owner-1", 0, nil,
	)
	stored, err := repo.GetKnowledgeByID(context.Background(), 7, id)
	require.NoError(t, err)
	now := time.Now()
	stored.ParseStatus = types.ParseStatusCompleted
	stored.ProcessedAt = &now
	stored.ProcessingOwner = ""
	stored.StorageSize = 25

	var winners atomic.Int32
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := *stored
			finalized, finalizeErr := repo.FinalizeKnowledgeWithStorageOwned(
				context.Background(), &copy, types.ParseStatusProcessing,
				"generation-1", "owner-1", 25,
			)
			if finalizeErr != nil {
				errCh <- finalizeErr
				return
			}
			if finalized {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for finalizeErr := range errCh {
		require.NoError(t, finalizeErr)
	}
	assert.EqualValues(t, 1, winners.Load())

	var row struct {
		ParseStatus     string
		ProcessingOwner string
		StorageSize     int64
	}
	require.NoError(t, db.Table("knowledges").Select("parse_status, processing_owner, storage_size").Where("id = ?", id).Take(&row).Error)
	assert.Equal(t, types.ParseStatusCompleted, row.ParseStatus)
	assert.Empty(t, row.ProcessingOwner)
	assert.EqualValues(t, 25, row.StorageSize)
	var storageUsed int64
	require.NoError(t, db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
	assert.EqualValues(t, 35, storageUsed)
	assert.NotEmpty(t, kbID)
}

func TestFinalizeKnowledgeWithStorageOwnedRejectsWrongGenerationOwnerAndStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		rowStatus  string
		generation string
		owner      string
	}{
		{name: "wrong generation", rowStatus: types.ParseStatusProcessing, generation: "generation-old", owner: "owner-1"},
		{name: "wrong owner", rowStatus: types.ParseStatusProcessing, generation: "generation-1", owner: "owner-old"},
		{name: "wrong status", rowStatus: types.ParseStatusPending, generation: "generation-1", owner: "owner-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, db := setupOwnedFinalizerRepository(t)
			id, _ := insertOwnedProcessingKnowledge(
				t, db, test.rowStatus, "generation-1", "owner-1", 0, nil,
			)
			stored, err := repo.GetKnowledgeByID(context.Background(), 7, id)
			require.NoError(t, err)
			stored.ParseStatus = types.ParseStatusCompleted
			stored.ProcessingOwner = ""
			stored.StorageSize = 25
			finalized, err := repo.FinalizeKnowledgeWithStorageOwned(
				context.Background(), stored, types.ParseStatusProcessing,
				test.generation, test.owner, 25,
			)
			require.NoError(t, err)
			assert.False(t, finalized)
			var storageUsed int64
			require.NoError(t, db.Table("tenants").Select("storage_used").Where("id = ?", 7).Scan(&storageUsed).Error)
			assert.EqualValues(t, 10, storageUsed)
		})
	}
}

func TestFinalizeSubtaskGenerationRejectsStaleGenerationWithoutDecrement(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	id, kbID := insertOwnedProcessingKnowledge(
		t, db, types.ParseStatusFinalizing, "generation-new", "", 2, nil,
	)

	count, promoted, err := repo.FinalizeSubtaskGeneration(
		context.Background(), 7, id, kbID, "generation-old",
	)
	require.NoError(t, err)
	assert.False(t, promoted)
	assert.Zero(t, count, "a stale-generation diagnostic read must not masquerade as the current count")
	status, storedCount := reloadKnowledgeRow(t, db, id)
	assert.Equal(t, types.ParseStatusFinalizing, status)
	assert.Equal(t, 2, storedCount)
}

func TestFinalizeSubtaskGenerationConcurrentExactlyOnePromotes(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	const descendants = 12
	id, kbID := insertOwnedProcessingKnowledge(
		t, db, types.ParseStatusFinalizing, "generation-1", "", descendants, nil,
	)

	var promoteWins atomic.Int32
	errCh := make(chan error, descendants)
	var wg sync.WaitGroup
	for i := 0; i < descendants; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, promoted, err := repo.FinalizeSubtaskGeneration(
				context.Background(), 7, id, kbID, "generation-1",
			)
			if err != nil {
				errCh <- err
				return
			}
			if promoted {
				promoteWins.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, promoteWins.Load())
	status, count := reloadKnowledgeRow(t, db, id)
	assert.Equal(t, types.ParseStatusCompleted, status)
	assert.Zero(t, count)
}

func TestCleanupKnowledgeFanoutCompletionsBoundsReparseAndSoftDeleteLedger(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.KnowledgeFanoutCompletion{}))
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	id, kbID := insertOwnedProcessingKnowledge(
		t, db, types.ParseStatusCompleted, "generation-current", "", 0, time.Now(),
	)
	otherID, otherKBID := insertOwnedProcessingKnowledge(
		t, db, types.ParseStatusCompleted, "generation-other", "", 0, time.Now(),
	)
	for _, item := range []struct {
		knowledgeID string
		kbID        string
		generation  string
		itemID      string
	}{
		{knowledgeID: id, kbID: kbID, generation: "generation-old", itemID: "image:old"},
		{knowledgeID: id, kbID: kbID, generation: "generation-current", itemID: "summary"},
		{knowledgeID: otherID, kbID: otherKBID, generation: "generation-other", itemID: "summary"},
	} {
		require.NoError(t, db.Create(&types.KnowledgeFanoutCompletion{
			TenantID:             7,
			KnowledgeID:          item.knowledgeID,
			KnowledgeBaseID:      item.kbID,
			ProcessingGeneration: item.generation,
			ItemID:               item.itemID,
			CompletedAt:          time.Now(),
		}).Error)
	}

	require.NoError(t, repo.CleanupKnowledgeFanoutCompletions(ctx, 7, id, kbID, "generation-current"))
	var rows []types.KnowledgeFanoutCompletion
	require.NoError(t, db.Order("knowledge_id, processing_generation").Find(&rows).Error)
	require.Len(t, rows, 2)
	remaining := make(map[string]string, len(rows))
	for _, row := range rows {
		remaining[row.KnowledgeID] = row.ProcessingGeneration
	}
	assert.Equal(t, "generation-current", remaining[id])
	assert.Equal(t, "generation-other", remaining[otherID])

	require.NoError(t, repo.CleanupKnowledgeFanoutCompletions(ctx, 7, id, kbID, ""))
	rows = nil
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, otherID, rows[0].KnowledgeID, "soft-delete cleanup must remain scoped to one knowledge")
}
