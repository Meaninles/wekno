package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openLegacyProcessingRepairDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf(
		"file:legacy-processing-%s?mode=memory&cache=shared", uuid.NewString(),
	)), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Knowledge{}, &types.Chunk{}))
	return db
}

func TestRepairLegacyProcessingInstallsFenceAndFailsUncommittedCore(t *testing.T) {
	db := openLegacyProcessingRepairDB(t)
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ParseStatus: types.ParseStatusPending, SummaryStatus: types.SummaryStatusPending,
	}
	require.NoError(t, db.Create(knowledge).Error)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)

	repaired, err := repo.RepairLegacyProcessing(
		context.Background(), 7, knowledge.ID, knowledge.KnowledgeBaseID,
		types.ParseStatusPending, false, "repair-generation", false,
		"legacy task requires reparse", time.Now(),
	)
	require.NoError(t, err)
	require.True(t, repaired)
	require.NoError(t, db.First(knowledge, "id = ?", knowledge.ID).Error)
	assert.Equal(t, types.ParseStatusFailed, knowledge.ParseStatus)
	assert.Equal(t, "repair-generation", knowledge.ProcessingGeneration)
	assert.Equal(t, types.SummaryStatusFailed, knowledge.SummaryStatus)
	assert.Equal(t, "legacy task requires reparse", knowledge.ErrorMessage)

	repaired, err = repo.RepairLegacyProcessing(
		context.Background(), 7, knowledge.ID, knowledge.KnowledgeBaseID,
		types.ParseStatusPending, false, "other-generation", false,
		"must not overwrite", time.Now(),
	)
	require.NoError(t, err)
	assert.False(t, repaired, "replay must not consume the installed repair generation")
}

func TestRepairLegacyProcessingCompletesOnlyCommittedCoreWithLiveChunk(t *testing.T) {
	for _, tc := range []struct {
		name        string
		chunkTenant uint64
		want        string
	}{
		{name: "live core artifact", chunkTenant: 7, want: types.ParseStatusCompleted},
		{name: "missing core artifact", want: types.ParseStatusFailed},
		{name: "same knowledge id in another tenant", chunkTenant: 8, want: types.ParseStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openLegacyProcessingRepairDB(t)
			now := time.Now()
			knowledge := &types.Knowledge{
				ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
				ParseStatus: types.ParseStatusFinalizing, ProcessedAt: &now,
				SummaryStatus: types.SummaryStatusProcessing, PendingSubtasksCount: 4,
			}
			require.NoError(t, db.Create(knowledge).Error)
			if tc.chunkTenant != 0 {
				require.NoError(t, db.Create(&types.Chunk{
					ID: uuid.NewString(), TenantID: tc.chunkTenant, KnowledgeID: knowledge.ID,
					KnowledgeBaseID: knowledge.KnowledgeBaseID, Content: "usable",
				}).Error)
			}
			repo := NewKnowledgeRepository(db).(*knowledgeRepository)
			repaired, err := repo.RepairLegacyProcessing(
				context.Background(), 7, knowledge.ID, knowledge.KnowledgeBaseID,
				types.ParseStatusFinalizing, true, "repair-generation", true,
				"legacy task requires reparse", time.Now(),
			)
			require.NoError(t, err)
			require.True(t, repaired)
			require.NoError(t, db.First(knowledge, "id = ?", knowledge.ID).Error)
			assert.Equal(t, tc.want, knowledge.ParseStatus)
			assert.Zero(t, knowledge.PendingSubtasksCount)
			assert.Equal(t, types.SummaryStatusFailed, knowledge.SummaryStatus)
			if tc.want == types.ParseStatusCompleted {
				assert.Empty(t, knowledge.ErrorMessage)
			} else {
				assert.Contains(t, knowledge.ErrorMessage, "reparse")
			}
		})
	}
}
