package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestWikiSearchExcludesStoredNavigationBeforeCandidateLimit(t *testing.T) {
	db := testsupport.Postgres(t, &types.WikiPage{})
	ctx := context.Background()
	kbID := uuid.NewString()
	pages := []*types.WikiPage{
		{ID: uuid.NewString(), KnowledgeBaseID: kbID, Slug: "index", PageType: types.WikiPageTypeIndex, Title: "Orion removed", Content: "Orion no longer exists", Status: "published"},
		{ID: uuid.NewString(), KnowledgeBaseID: kbID, Slug: "log", PageType: types.WikiPageTypeLog, Title: "Orion history", Content: "Orion removed", Status: "published"},
		{ID: uuid.NewString(), KnowledgeBaseID: kbID, Slug: "current", PageType: types.WikiPageTypeConcept, Title: "Current schedule", Content: "Orion is scheduled tomorrow", Status: "published"},
		{ID: uuid.NewString(), KnowledgeBaseID: uuid.NewString(), Slug: "other", PageType: types.WikiPageTypeConcept, Title: "Orion unrelated", Status: "published"},
	}
	require.NoError(t, db.Create(&pages).Error)
	repo := &wikiPageRepository{db: db}
	results, err := repo.Search(ctx, kbID, "Orion", 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "current", results[0].Slug)
}
