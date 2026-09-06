package knowledgeaux

import (
	"context"
	"errors"
	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"testing"
)

func exerciseIndexPublication(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	kbID, documentID := uuid.NewString(), uuid.NewString()
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: kbID, TenantID: 7}).Error)
	before := &types.Knowledge{ID: documentID, TenantID: 7, KnowledgeBaseID: kbID, PublishedGeneration: "old", ProcessingGeneration: "new", EmbeddingModelID: "new-model", PublishedEmbeddingModelID: "old-model"}
	require.NoError(t, db.Create(before).Error)
	oldID, newID := uuid.NewString(), uuid.NewString()
	old := &types.Chunk{ID: oldID, TenantID: 7, KnowledgeBaseID: kbID, KnowledgeID: documentID, ProcessingGeneration: "old", IsEnabled: true, Content: "original"}
	draft := &types.Chunk{ID: newID, TenantID: 7, KnowledgeBaseID: kbID, KnowledgeID: documentID, ProcessingGeneration: "new", IsEnabled: true, Content: "replacement"}
	require.NoError(t, db.Create([]*types.Chunk{old, draft}).Error)
	require.NoError(t, db.Model(draft).Update("is_enabled", false).Error)
	next := *before
	next.EmbeddingModelID = "new-model"
	visible := func() []string {
		var ids []string
		require.NoError(t, db.Model(&types.Chunk{}).Where("knowledge_id = ? AND is_enabled = ?", documentID, true).Order("id").Pluck("id", &ids).Error)
		return ids
	}
	require.Equal(t, []string{oldID}, visible())
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := PublishChunksTx(tx, before, &next); err != nil {
			return err
		}
		return errors.New("injected commit failure")
	})
	require.Error(t, err)
	require.Equal(t, []string{oldID}, visible(), "a failed publication must preserve old evidence")
	var count int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error { return PublishChunksTx(tx, before, &next) }))
	require.Equal(t, []string{newID}, visible())
	r := &Registry{db: db}
	ids, err := r.DraftChunkIDs(ctx, &next)
	require.NoError(t, err)
	require.Empty(t, ids, "uncertain commit cleanup must not erase published data")
	require.Error(t, DrainIndexRetirements(ctx, db, func(context.Context, uint64, IndexRetirement) error { return errors.New("provider unavailable") }))
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	calls := 0
	require.NoError(t, DrainIndexRetirements(ctx, db, func(_ context.Context, tenant uint64, receipt IndexRetirement) error {
		calls++
		require.EqualValues(t, 7, tenant)
		require.Equal(t, "old-model", receipt.EmbeddingModelID)
		require.Equal(t, []string{oldID}, receipt.ChunkIDs)
		return nil
	}))
	require.NoError(t, DrainIndexRetirements(ctx, db, func(context.Context, uint64, IndexRetirement) error {
		t.Fatal("completed receipt replayed")
		return nil
	}))
	require.Equal(t, 1, calls)
	require.Equal(t, []string{newID}, visible())
	require.NoError(t, db.Model(&types.Chunk{}).Where("id = ?", oldID).Count(&count).Error)
	require.Zero(t, count)
}

func TestRepairIndexPublicationRetainsOldOnFailureAndResumesCleanup(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sql, _ := db.DB()
	sql.SetMaxOpenConns(1)
	defer sql.Close()
	require.NoError(t, db.AutoMigrate(&types.KnowledgeBase{}, &types.Knowledge{}, &types.Chunk{}, &types.TaskPendingOp{}))
	exerciseIndexPublication(t, db)
}

func TestRepairPostgresIndexPublicationRetainsOldOnFailureAndResumesCleanup(t *testing.T) {
	exerciseIndexPublication(t, testsupport.Postgres(t, &types.KnowledgeBase{}, &types.Knowledge{}, &types.Chunk{}, &types.TaskPendingOp{}))
}

func TestRepairRetirementIncludesGenerationWithoutTextChunks(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sql, _ := db.DB()
	sql.SetMaxOpenConns(1)
	defer sql.Close()
	require.NoError(t, db.AutoMigrate(&types.KnowledgeBase{}, &types.Knowledge{}, &types.Chunk{}, &types.TaskPendingOp{}))
	kbID, docID := uuid.NewString(), uuid.NewString()
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: kbID, TenantID: 7}).Error)
	k := &types.Knowledge{ID: docID, TenantID: 7, KnowledgeBaseID: kbID, PublishedGeneration: "images-only", ProcessingGeneration: "new"}
	require.NoError(t, db.Create(k).Error)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error { return PublishChunksTx(tx, k, k) }))
	calls := 0
	require.NoError(t, DrainIndexRetirements(context.Background(), db, func(_ context.Context, tenant uint64, receipt IndexRetirement) error {
		calls++
		require.Equal(t, "images-only", receipt.Generation)
		require.Empty(t, receipt.ChunkIDs)
		return nil
	}))
	require.Equal(t, 1, calls)
}
