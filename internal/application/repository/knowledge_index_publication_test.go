package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func exerciseDeferredIndexPublication(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	kbID, id := uuid.NewString(), uuid.NewString()
	require.NoError(t, db.Create(&types.Tenant{ID: 19}).Error)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: kbID, TenantID: 19}).Error)
	k := &types.Knowledge{ID: id, TenantID: 19, KnowledgeBaseID: kbID,
		ParseStatus: types.ParseStatusProcessing, CoreStatus: types.CoreStatusProcessing,
		ProcessingGeneration: "second", ProcessingOwner: "worker", PublishedGeneration: "first",
		EmbeddingModelID: "old-model", PublishedEmbeddingModelID: "old-model"}
	require.NoError(t, db.Create(k).Error)
	oldID, textID, imageID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, chunk := range []*types.Chunk{
		{ID: oldID, ProcessingGeneration: "first"}, {ID: textID, ProcessingGeneration: "second"},
	} {
		chunk.TenantID, chunk.KnowledgeID, chunk.KnowledgeBaseID = 19, id, kbID
		chunk.IsEnabled = chunk.ID == oldID
		require.NoError(t, NewChunkRepository(db).CreateChunks(ctx, []*types.Chunk{chunk}))
	}
	r := &knowledgeRepository{db: db}
	k.EmbeddingModelID, k.ProcessingOwner = "new-model", ""
	ok, err := r.FinalizeKnowledgeWithStorageOwned(ctx, k, types.ParseStatusProcessing, "second", "worker", 0)
	require.NoError(t, err)
	require.True(t, ok)
	visible := func() []string {
		var ids []string
		require.NoError(t, db.Model(&types.Chunk{}).Where("knowledge_id = ? AND is_enabled = ?", id, true).Pluck("id", &ids).Error)
		return ids
	}
	require.Equal(t, []string{oldID}, visible(), "text commit must not hide the previous complete generation while images are pending")
	require.NoError(t, NewChunkRepository(db).CreateChunks(ctx, []*types.Chunk{{ID: imageID, TenantID: 19, KnowledgeID: id, KnowledgeBaseID: kbID, ProcessingGeneration: "second", ChunkType: types.ChunkTypeImageOCR, IsEnabled: false}}))
	require.Equal(t, []string{oldID}, visible())
	for _, generation := range []string{"stale", "second"} {
		ok, err = r.CompareAndSwapKnowledgeProcessingGeneration(ctx, 19, id, kbID, generation, []string{types.ParseStatusProcessing}, map[string]interface{}{"parse_status": types.ParseStatusFinalizing, "core_status": types.CoreStatusReady})
		require.NoError(t, err)
		require.Equal(t, generation == "second", ok)
		if generation == "stale" {
			require.Equal(t, []string{oldID}, visible())
		}
	}
	require.ElementsMatch(t, []string{textID, imageID}, visible())
	var live types.Knowledge
	require.NoError(t, db.First(&live, "id = ?", id).Error)
	require.Equal(t, "second", live.PublishedGeneration)
	require.Equal(t, "new-model", live.PublishedEmbeddingModelID)
}

func TestRepairDeferredIndexPublication(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Tenant{}, &types.KnowledgeBase{}))
	exerciseDeferredIndexPublication(t, db)
}

func TestRepairPostgresDeferredIndexPublication(t *testing.T) {
	exerciseDeferredIndexPublication(t, testsupport.Postgres(t, &types.Tenant{}, &types.KnowledgeBase{}, &types.Knowledge{}, &types.Chunk{}, &types.TaskPendingOp{}))
}
