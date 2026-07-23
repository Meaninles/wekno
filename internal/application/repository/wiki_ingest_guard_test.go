package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupWikiIngestGuardRepositoryTest(t *testing.T) (*gorm.DB, wikiingestguard.Identity) {
	t.Helper()
	db := setupWikiPagesTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Knowledge{}))
	require.NoError(t, db.Exec(taskPendingOpsTestDDL).Error)
	processedAt := time.Now().UTC()
	require.NoError(t, db.Create(&types.Knowledge{
		ID:                   "knowledge-guard",
		TenantID:             1,
		KnowledgeBaseID:      "kb-a",
		ProcessingGeneration: "generation-1",
		ParseStatus:          types.ParseStatusCompleted,
		ProcessedAt:          &processedAt,
	}).Error)
	return db, wikiingestguard.Identity{
		TenantID:             1,
		KnowledgeBaseID:      "kb-a",
		KnowledgeID:          "knowledge-guard",
		ProcessingGeneration: "generation-1",
	}
}

func TestWikiPageUpdateRejectsStaleGenerationInWriteTransaction(t *testing.T) {
	db, identity := setupWikiIngestGuardRepositoryTest(t)
	repo := NewWikiPageRepository(db)
	page := makeWikiPage("kb-a", "entity/guarded", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	require.NoError(t, repo.Create(context.Background(), page))

	loaded, err := repo.GetBySlug(context.Background(), page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	loaded.Content = "must not commit"
	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id = ?", identity.KnowledgeID).
		Update("processing_generation", "generation-2").Error)

	err = repo.Update(wikiingestguard.WithValidation(context.Background(), identity), loaded)
	require.NotEmpty(t, wikiingestguard.StaleIdentities(err))

	got, err := repo.GetBySlug(context.Background(), page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	assert.Equal(t, page.Content, got.Content)
	assert.Equal(t, 1, got.Version)
}

func TestWikiPageAndAppliedCheckpointRollBackTogetherWhenPendingRowDisappears(t *testing.T) {
	db, identity := setupWikiIngestGuardRepositoryTest(t)
	repo := NewWikiPageRepository(db)
	page := makeWikiPage("kb-a", "entity/atomic", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	require.NoError(t, repo.Create(context.Background(), page))

	loaded, err := repo.GetBySlug(context.Background(), page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	loaded.Content = "must roll back"
	ctx := wikiingestguard.WithPageApplication(context.Background(), page.Slug, wikiingestguard.Operation{
		PendingOpID: 999,
		Identity:    identity,
	})
	err = repo.Update(ctx, loaded)
	require.NotEmpty(t, wikiingestguard.StaleIdentities(err))

	got, err := repo.GetBySlug(context.Background(), page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	assert.Equal(t, page.Content, got.Content)
	assert.Equal(t, 1, got.Version)
}

func TestWikiPageAndAppliedCheckpointCommitAtomically(t *testing.T) {
	db, identity := setupWikiIngestGuardRepositoryTest(t)
	pageRepo := NewWikiPageRepository(db)
	pendingRepo := NewTaskPendingOpsRepository(db)
	page := makeWikiPage("kb-a", "entity/exact-once", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	require.NoError(t, pageRepo.Create(context.Background(), page))

	payload := json.RawMessage(`{
		"op":"ingest",
		"knowledge_id":"knowledge-guard",
		"processing_generation":"generation-1",
		"prepared":{"doc_title":"kept"}
	}`)
	pending := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-a", "ingest",
		"knowledge-guard:generation-1", payload,
	)
	require.NoError(t, pendingRepo.Enqueue(context.Background(), pending))

	loaded, err := pageRepo.GetBySlug(context.Background(), page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	loaded.Content = "committed exactly once"
	ctx := wikiingestguard.WithPageApplication(context.Background(), page.Slug, wikiingestguard.Operation{
		PendingOpID: pending.ID,
		Identity:    identity,
	})
	require.NoError(t, pageRepo.Update(ctx, loaded))

	got, err := pageRepo.GetBySlug(context.Background(), page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	assert.Equal(t, "committed exactly once", got.Content)
	assert.Equal(t, 2, got.Version)

	var stored types.TaskPendingOp
	require.NoError(t, db.First(&stored, pending.ID).Error)
	var decoded struct {
		Applied  []string `json:"applied_page_slugs"`
		Prepared struct {
			DocTitle string `json:"doc_title"`
		} `json:"prepared"`
	}
	require.NoError(t, json.Unmarshal(stored.Payload, &decoded))
	assert.Equal(t, []string{page.Slug}, decoded.Applied)
	assert.Equal(t, "kept", decoded.Prepared.DocTitle)
}

func TestWikiFolderWritesRejectStaleGenerationInWriteTransaction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(context.Context, *gorm.DB, wikiingestguard.Identity) error
	}{
		{
			name: "create",
			write: func(ctx context.Context, db *gorm.DB, _ wikiingestguard.Identity) error {
				return NewWikiPageRepository(db).CreateFolder(ctx, &types.WikiFolder{
					ID: "folder-new", TenantID: 1, KnowledgeBaseID: "kb-a",
					Name: "New", Path: "New", Depth: 1,
				})
			},
		},
		{
			name: "update",
			write: func(ctx context.Context, db *gorm.DB, _ wikiingestguard.Identity) error {
				repo := NewWikiPageRepository(db)
				folder := &types.WikiFolder{
					ID: "folder-existing", TenantID: 1, KnowledgeBaseID: "kb-a",
					Name: "Existing", Path: "Existing", Depth: 1,
				}
				if err := repo.CreateFolder(context.Background(), folder); err != nil {
					return err
				}
				folder.Name = "Must Not Commit"
				folder.Path = "Must Not Commit"
				return repo.UpdateFolder(ctx, folder)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, identity := setupWikiIngestGuardRepositoryTest(t)
			require.NoError(t, db.Model(&types.Knowledge{}).
				Where("id = ?", identity.KnowledgeID).
				Update("processing_generation", "generation-2").Error)
			ctx := wikiingestguard.WithValidation(context.Background(), identity)

			err := tc.write(ctx, db, identity)
			require.NotEmpty(t, wikiingestguard.StaleIdentities(err))

			var forbidden int64
			require.NoError(t, db.Model(&types.WikiFolder{}).
				Where("knowledge_base_id = ? AND (id = ? OR name = ?)", "kb-a", "folder-new", "Must Not Commit").
				Count(&forbidden).Error)
			assert.Zero(t, forbidden)
		})
	}
}
