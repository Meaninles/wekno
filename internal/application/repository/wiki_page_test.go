package repository

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// wikiPagesTestDDL is a minimal SQLite-compatible subset of the
// production wiki_pages DDL (migrations/versioned/000037_wiki_and_indexing.up.sql).
// JSONB is stored as TEXT in SQLite; the StringArray Scan/Value pair
// handles the JSON round-trip unchanged.
const wikiPagesTestDDL = `
CREATE TABLE IF NOT EXISTS wiki_pages (
    id                VARCHAR(36) PRIMARY KEY,
    tenant_id         INTEGER NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    slug              VARCHAR(255) NOT NULL,
    title             VARCHAR(512) NOT NULL DEFAULT '',
    page_type         VARCHAR(32) NOT NULL DEFAULT 'summary',
    status            VARCHAR(32) NOT NULL DEFAULT 'published',
    content           TEXT NOT NULL DEFAULT '',
    summary           TEXT NOT NULL DEFAULT '',
    parent_slug       VARCHAR(255) NOT NULL DEFAULT '',
    folder_id         VARCHAR(36) NOT NULL DEFAULT '',
    category_path     TEXT DEFAULT '[]',
    wiki_path         VARCHAR(1024) NOT NULL DEFAULT '',
    depth             INTEGER NOT NULL DEFAULT 0,
    sort_order        INTEGER NOT NULL DEFAULT 0,
    source_refs       TEXT DEFAULT '[]',
    chunk_refs        TEXT DEFAULT '[]',
    in_links          TEXT DEFAULT '[]',
    out_links         TEXT DEFAULT '[]',
    page_metadata     TEXT DEFAULT '{}',
    aliases           TEXT DEFAULT '[]',
    version           INTEGER NOT NULL DEFAULT 1,
    created_at        DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at        DATETIME DEFAULT CURRENT_TIMESTAMP,
    deleted_at        DATETIME
);
`

// wikiFoldersTestDDL mirrors the production wiki_folders DDL for SQLite.
const wikiFoldersTestDDL = `
CREATE TABLE IF NOT EXISTS wiki_folders (
    id                VARCHAR(36) PRIMARY KEY,
    tenant_id         INTEGER NOT NULL DEFAULT 0,
    knowledge_base_id VARCHAR(36) NOT NULL,
    parent_id         VARCHAR(36) NOT NULL DEFAULT '',
    name              VARCHAR(255) NOT NULL,
    path              VARCHAR(1024) NOT NULL DEFAULT '',
    depth             INTEGER NOT NULL DEFAULT 0,
    sort_order        INTEGER NOT NULL DEFAULT 0,
    created_at        DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at        DATETIME DEFAULT CURRENT_TIMESTAMP,
    deleted_at        DATETIME
);
`

func setupWikiPagesTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(wikiPagesTestDDL).Error)
	require.NoError(t, db.Exec(wikiFoldersTestDDL).Error)
	require.NoError(t, db.AutoMigrate(&types.KnowledgeBase{}))
	for _, kbID := range []string{
		"kb-a", "kb-auto-link-cas", "kb-cap", "kb-f", "kb-other",
		"kb-quarantine", "kb-quarantine-meta", "kb-quarantine-meta-clear",
		"kb-quarantine-meta-fresh", "kb-quarantine-meta-stale-clear",
	} {
		require.NoError(t, db.Create(&types.KnowledgeBase{ID: kbID, TenantID: 1, Name: kbID}).Error)
	}
	return db
}

func TestDeletePermanentlyRemovesWikiPageContent(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	page := makeWikiPage("kb-a", "entity/private-delete", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	page.Content = "private generated prose"
	page.Summary = "private summary"
	page.SourceRefs = types.StringArray{"knowledge-private|Private document"}
	require.NoError(t, repo.Create(ctx, page))

	require.NoError(t, repo.Delete(ctx, page.KnowledgeBaseID, page.Slug))

	var rawCount int64
	require.NoError(t, db.Unscoped().Model(&types.WikiPage{}).
		Where("id = ?", page.ID).Count(&rawCount).Error)
	require.Zero(t, rawCount, "deleted Wiki content must not survive as a GORM tombstone")
}

func TestListSourceProvenanceBySourceRefUsesExactDocumentOwnership(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	exact := makeWikiPage("kb-a", "entity/exact", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	exact.Content = strings.Repeat("large body must not be projected", 20)
	exact.SourceRefs = types.StringArray{"source_%"}
	exact.ChunkRefs = types.StringArray{"exact-old-1", "exact-old-2"}
	require.NoError(t, repo.Create(ctx, exact))

	legacyTitle := makeWikiPage("kb-a", "concept/legacy-title", types.WikiPageTypeConcept, types.WikiPageStatusPublished)
	legacyTitle.Content = strings.Repeat("another large body", 20)
	legacyTitle.SourceRefs = types.StringArray{"source_%|Legacy title"}
	legacyTitle.ChunkRefs = types.StringArray{"legacy-old"}
	require.NoError(t, repo.Create(ctx, legacyTitle))

	likeNeighbour := makeWikiPage("kb-a", "entity/not-owned", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	likeNeighbour.SourceRefs = types.StringArray{"source-AB|Other document"}
	likeNeighbour.ChunkRefs = types.StringArray{"other-source"}
	require.NoError(t, repo.Create(ctx, likeNeighbour))

	otherKB := makeWikiPage("kb-other", "entity/other-kb", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	otherKB.SourceRefs = types.StringArray{"source_%"}
	otherKB.ChunkRefs = types.StringArray{"other-kb"}
	require.NoError(t, repo.Create(ctx, otherKB))

	rows, err := repo.ListSourceProvenanceBySourceRef(ctx, "kb-a", "source_%")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "concept/legacy-title", rows[0].Slug)
	assert.Equal(t, types.WikiPageTypeConcept, rows[0].PageType)
	assert.Equal(t, types.StringArray{"legacy-old"}, rows[0].ChunkRefs)
	assert.Equal(t, "entity/exact", rows[1].Slug)
	assert.Equal(t, types.StringArray{"exact-old-1", "exact-old-2"}, rows[1].ChunkRefs)

	slugs, err := repo.ListSlugsBySourceRef(ctx, "kb-a", "source_%")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"entity/exact", "concept/legacy-title"}, slugs)

	fullPages, err := repo.ListBySourceRef(ctx, "kb-a", "source_%")
	require.NoError(t, err)
	require.Len(t, fullPages, 2)
	assert.NotEmpty(t, fullPages[0].Content,
		"the legacy full-row API remains available to callers that need page bodies")
}

func TestQuarantineForDeleteUnionsSourcesAndFencesStaleWriter(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	page := makeWikiPage("kb-quarantine", "entity/shared", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	page.SourceRefs = types.StringArray{"source-1", "source-2", "survivor"}
	require.NoError(t, repo.Create(ctx, page))

	stale := *page
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-1"))
	stale.Content = "stale writer must not publish"
	require.ErrorIs(t, repo.Update(ctx, &stale), ErrWikiPageConflict)
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-2"))

	got, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	assert.Equal(t, types.WikiPageStatusArchived, got.Status)
	sources, err := wikidelete.PendingSources(got)
	require.NoError(t, err)
	assert.Equal(t, []string{"source-1", "source-2"}, sources)
	assert.Equal(t, 3, got.Version)
}

func TestUpdateMetaRejectsStaleWriterAfterDeleteQuarantine(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	page := makeWikiPage("kb-quarantine-meta", "concept/shared", types.WikiPageTypeConcept, types.WikiPageStatusPublished)
	page.SourceRefs = types.StringArray{"source-1", "survivor"}
	require.NoError(t, repo.Create(ctx, page))
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-1"))

	incoming := *page
	incoming.Status = types.WikiPageStatusPublished
	incoming.PageMetadata = types.JSON([]byte(`{}`))
	incoming.SourceRefs = types.StringArray{"survivor"}
	require.ErrorIs(t, repo.UpdateMeta(ctx, &incoming), ErrWikiPageConflict)

	got, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	assert.Equal(t, types.WikiPageStatusArchived, got.Status)
	sources, err := wikidelete.PendingSources(got)
	require.NoError(t, err)
	assert.Equal(t, []string{"source-1"}, sources)
	assert.Equal(t, types.StringArray{"source-1", "survivor"}, got.SourceRefs)
}

func TestUpdateMetaFreshOrdinaryWritePreservesDeleteQuarantine(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	page := makeWikiPage("kb-quarantine-meta-fresh", "concept/shared", types.WikiPageTypeConcept, types.WikiPageStatusPublished)
	page.SourceRefs = types.StringArray{"source-1", "survivor"}
	require.NoError(t, repo.Create(ctx, page))
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-1"))

	incoming, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	incoming.Status = types.WikiPageStatusPublished
	incoming.PageMetadata = types.JSON([]byte(`{"caller":"kept"}`))
	incoming.SourceRefs = types.StringArray{"survivor"}
	require.NoError(t, repo.UpdateMeta(ctx, incoming))

	got, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	assert.Equal(t, types.WikiPageStatusArchived, got.Status)
	sources, err := wikidelete.PendingSources(got)
	require.NoError(t, err)
	assert.Equal(t, []string{"source-1"}, sources)
	assert.Equal(t, types.StringArray{"survivor"}, got.SourceRefs)
}

func TestUpdateMetaTrustedClearUsesCurrentMarkerAndExactSources(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	page := makeWikiPage("kb-quarantine-meta-clear", "concept/shared", types.WikiPageTypeConcept, types.WikiPageStatusPublished)
	page.SourceRefs = types.StringArray{"source-1", "source-2", "survivor"}
	require.NoError(t, wikidelete.MarkApplied(page, 101))
	require.NoError(t, repo.Create(ctx, page))
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-1"))
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-2"))

	incoming, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	version := incoming.Version
	// Simulate a reducer payload whose marker is absent. Trusted completion
	// must rebuild from the current row and remove only source-1.
	incoming.Status = types.WikiPageStatusPublished
	incoming.PageMetadata = types.JSON([]byte(`{"applied":"retained"}`))
	require.NoError(t, wikidelete.MarkApplied(incoming, 202))
	require.NoError(t, repo.UpdateMeta(
		wikidelete.WithQuarantineClear(ctx, "source-1"), incoming,
	))

	got, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	assert.Equal(t, version, got.Version, "metadata completion must not bump the user revision")
	assert.Equal(t, types.WikiPageStatusArchived, got.Status)
	sources, err := wikidelete.PendingSources(got)
	require.NoError(t, err)
	assert.Equal(t, []string{"source-2"}, sources)
	assert.Contains(t, string(got.PageMetadata), `"applied":"retained"`)
	for _, opID := range []int64{101, 202} {
		applied, appliedErr := wikidelete.IsApplied(got, opID)
		require.NoError(t, appliedErr)
		assert.True(t, applied, "applied operation %d must survive trusted metadata merge", opID)
	}

	// A context carrying no source IDs has no clear authority.
	noAuthority := *got
	noAuthority.Status = types.WikiPageStatusPublished
	noAuthority.PageMetadata = types.JSON([]byte(`{}`))
	require.NoError(t, repo.UpdateMeta(wikidelete.WithQuarantineClear(ctx), &noAuthority))
	stillQuarantined, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	sources, err = wikidelete.PendingSources(stillQuarantined)
	require.NoError(t, err)
	assert.Equal(t, []string{"source-2"}, sources)
	assert.Equal(t, types.WikiPageStatusArchived, stillQuarantined.Status)
	for _, opID := range []int64{101, 202} {
		applied, appliedErr := wikidelete.IsApplied(stillQuarantined, opID)
		require.NoError(t, appliedErr)
		assert.True(t, applied, "ordinary metadata writes must preserve applied operation %d", opID)
	}

	final := *stillQuarantined
	final.Status = types.WikiPageStatusPublished
	final.PageMetadata = types.JSON([]byte(`{}`))
	require.NoError(t, repo.UpdateMeta(
		wikidelete.WithQuarantineClear(ctx, "source-2"), &final,
	))
	cleared, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	sources, err = wikidelete.PendingSources(cleared)
	require.NoError(t, err)
	assert.Empty(t, sources)
	assert.Equal(t, types.WikiPageStatusPublished, cleared.Status)
	assert.Equal(t, version, cleared.Version)
	for _, opID := range []int64{101, 202} {
		applied, appliedErr := wikidelete.IsApplied(cleared, opID)
		require.NoError(t, appliedErr)
		assert.True(t, applied, "trusted clear must preserve applied operation %d", opID)
	}
}

func TestUpdateMetaTrustedClearRejectsStaleVersionWithoutLosingNewMarker(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	page := makeWikiPage("kb-quarantine-meta-stale-clear", "concept/shared", types.WikiPageTypeConcept, types.WikiPageStatusPublished)
	page.SourceRefs = types.StringArray{"source-1", "source-2", "survivor"}
	require.NoError(t, repo.Create(ctx, page))
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-1"))

	stale, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	stale.Status = types.WikiPageStatusPublished
	stale.PageMetadata = types.JSON([]byte(`{}`))
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-2"))

	err = repo.UpdateMeta(wikidelete.WithQuarantineClear(ctx, "source-1"), stale)
	require.ErrorIs(t, err, ErrWikiPageConflict)
	got, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	sources, err := wikidelete.PendingSources(got)
	require.NoError(t, err)
	assert.Equal(t, []string{"source-1", "source-2"}, sources)
	assert.Equal(t, types.WikiPageStatusArchived, got.Status)
}

func TestUpdateAutoLinkedContentRejectsVersionChangedByQuarantine(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	page := makeWikiPage("kb-auto-link-cas", "entity/shared", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	page.SourceRefs = types.StringArray{"source-1", "survivor"}
	require.NoError(t, repo.Create(ctx, page))

	stale := *page
	stale.Content = "stale auto-linked body"
	stale.OutLinks = types.StringArray{"entity/stale"}
	require.NoError(t, repo.QuarantineForDelete(ctx, page.KnowledgeBaseID, page.Slug, "source-1"))
	require.ErrorIs(t, repo.UpdateAutoLinkedContent(ctx, &stale), ErrWikiPageConflict)

	got, err := repo.GetBySlug(ctx, page.KnowledgeBaseID, page.Slug)
	require.NoError(t, err)
	assert.Equal(t, page.Content, got.Content)
	assert.Empty(t, got.OutLinks)
	assert.Equal(t, types.WikiPageStatusArchived, got.Status)
}

func TestAtomicInLinkMutationsMergeConcurrentSourcesWithoutVersionConflicts(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// SQLite local mode has one writer. Keeping one connection also prevents
	// :memory: from creating an independent database per goroutine; the test
	// still exercises concurrent callers and is repeated under -race.
	sqlDB.SetMaxOpenConns(1)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	target := makeWikiPage(
		"kb-a", "entity/atomic-target",
		types.WikiPageTypeEntity, types.WikiPageStatusPublished,
	)
	require.NoError(t, repo.Create(ctx, target))

	const sourceCount = 48
	var wg sync.WaitGroup
	for i := 0; i < sourceCount; i++ {
		source := "entity/source-" + strconv.Itoa(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, repo.AddInLink(
				context.Background(), 1, "kb-a", target.Slug, source,
			))
			// Duplicate delivery must remain a set membership.
			require.NoError(t, repo.AddInLink(
				context.Background(), 1, "kb-a", target.Slug, source,
			))
		}()
	}
	wg.Wait()

	got, err := repo.GetBySlug(ctx, "kb-a", target.Slug)
	require.NoError(t, err)
	assert.Len(t, got.InLinks, sourceCount)
	assert.Equal(t, 1, got.Version,
		"reverse-link bookkeeping must not advance the user revision")

	for i := 0; i < sourceCount; i += 2 {
		source := "entity/source-" + strconv.Itoa(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, repo.RemoveInLink(
				context.Background(), 1, "kb-a", target.Slug, source,
			))
			require.NoError(t, repo.RemoveInLink(
				context.Background(), 1, "kb-a", target.Slug, source,
			))
		}()
	}
	wg.Wait()

	got, err = repo.GetBySlug(ctx, "kb-a", target.Slug)
	require.NoError(t, err)
	assert.Len(t, got.InLinks, sourceCount/2)
	for i := 0; i < sourceCount; i++ {
		source := "entity/source-" + strconv.Itoa(i)
		assert.Equal(t, i%2 == 1, slices.Contains(got.InLinks, source))
	}
	assert.Equal(t, 1, got.Version)
}

func TestSyncInLinksForTargetRepairsSourceBeforeTargetCreation(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	for _, sourceSlug := range []string{"entity/source-b", "entity/source-a"} {
		source := makeWikiPage(
			"kb-a", sourceSlug,
			types.WikiPageTypeEntity, types.WikiPageStatusPublished,
		)
		source.OutLinks = types.StringArray{"concept/future-target"}
		require.NoError(t, repo.Create(ctx, source))
	}
	archived := makeWikiPage(
		"kb-a", "entity/archived-source",
		types.WikiPageTypeEntity, types.WikiPageStatusArchived,
	)
	archived.OutLinks = types.StringArray{"concept/future-target"}
	require.NoError(t, repo.Create(ctx, archived))
	otherKB := makeWikiPage(
		"kb-other", "entity/other-kb-source",
		types.WikiPageTypeEntity, types.WikiPageStatusPublished,
	)
	otherKB.OutLinks = types.StringArray{"concept/future-target"}
	require.NoError(t, repo.Create(ctx, otherKB))

	target := makeWikiPage(
		"kb-a", "concept/future-target",
		types.WikiPageTypeConcept, types.WikiPageStatusPublished,
	)
	require.NoError(t, repo.Create(ctx, target))
	require.NoError(t, repo.SyncInLinksForTarget(
		ctx, 1, "kb-a", target.Slug,
	))

	got, err := repo.GetBySlug(ctx, "kb-a", target.Slug)
	require.NoError(t, err)
	assert.Equal(t, types.StringArray{
		"entity/source-a",
		"entity/source-b",
	}, got.InLinks)
	assert.Equal(t, 1, got.Version)
}

// makeWikiPage builds a minimal WikiPage suitable for insert. Title is
// derived from the slug so ORDER BY title ASC yields a predictable
// test ordering without callers having to spell out both fields.
func makeWikiPage(kbID, slug, pageType, status string) *types.WikiPage {
	title := slug
	if idx := strings.LastIndex(slug, "/"); idx >= 0 {
		title = slug[idx+1:]
	}
	return &types.WikiPage{
		ID:              uuid.New().String(),
		TenantID:        1,
		KnowledgeBaseID: kbID,
		Slug:            slug,
		Title:           title,
		PageType:        pageType,
		Status:          status,
		Content:         "body of " + slug,
		Summary:         "summary of " + slug,
		WikiPath:        pageType + "/" + title,
		Version:         1,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
}

func makeCategorizedWikiPage(kbID, slug, pageType, status string, categoryPath ...string) *types.WikiPage {
	page := makeWikiPage(kbID, slug, pageType, status)
	page.CategoryPath = types.StringArray(categoryPath)
	if len(categoryPath) > 0 {
		page.WikiPath = pageType + "/" + strings.Join(categoryPath, "/") + "/" + page.Title
		page.Depth = len(categoryPath)
	}
	return page
}

// TestList_WikiPathSortReturnsCategorizedPagesFirst protects the sidebar's
// IDE-like tree contract. Pagination happens in the repository, so the DB
// must return pages with category_path before loose root pages; otherwise the
// frontend cannot know about directories hiding on later pages.
func TestList_WikiPathSortReturnsCategorizedPagesFirst(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	pages := []*types.WikiPage{
		makeWikiPage("kb-a", "entity/000-root", types.WikiPageTypeEntity, types.WikiPageStatusPublished),
		makeCategorizedWikiPage("kb-a", "entity/999-child", types.WikiPageTypeEntity, types.WikiPageStatusPublished, "zzz-folder"),
		makeWikiPage("kb-a", "entity/001-root", types.WikiPageTypeEntity, types.WikiPageStatusPublished),
	}
	for _, p := range pages {
		require.NoError(t, repo.Create(ctx, p))
	}

	got, total, err := repo.List(ctx, &types.WikiPageListRequest{
		KnowledgeBaseID: "kb-a",
		PageType:        types.WikiPageTypeEntity,
		Page:            1,
		PageSize:        10,
		SortBy:          "wiki_path",
		SortOrder:       "asc",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	require.Len(t, got, 3)
	assert.Equal(t, "entity/999-child", got[0].Slug)
	assert.Equal(t, "entity/000-root", got[1].Slug)
	assert.Equal(t, "entity/001-root", got[2].Slug)
}

func TestList_DefaultHidesDeleteQuarantineButExplicitArchivedRemainsInspectable(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	live := makeWikiPage(
		"kb-quarantine",
		"entity/live",
		types.WikiPageTypeEntity,
		types.WikiPageStatusPublished,
	)
	quarantined := makeWikiPage(
		"kb-quarantine",
		"entity/quarantined",
		types.WikiPageTypeEntity,
		types.WikiPageStatusPublished,
	)
	quarantined.SourceRefs = types.StringArray{"knowledge-being-deleted"}
	require.NoError(t, repo.Create(ctx, live))
	require.NoError(t, repo.Create(ctx, quarantined))
	require.NoError(t, repo.QuarantineForDelete(
		ctx,
		quarantined.KnowledgeBaseID,
		quarantined.Slug,
		"knowledge-being-deleted",
	))

	pages, total, err := repo.List(ctx, &types.WikiPageListRequest{
		KnowledgeBaseID: "kb-quarantine",
		Page:            1,
		PageSize:        20,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, pages, 1)
	assert.Equal(t, live.Slug, pages[0].Slug)

	archived, archivedTotal, err := repo.List(ctx, &types.WikiPageListRequest{
		KnowledgeBaseID: "kb-quarantine",
		Status:          types.WikiPageStatusArchived,
		Page:            1,
		PageSize:        20,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, archivedTotal)
	require.Len(t, archived, 1)
	assert.Equal(t, quarantined.Slug, archived[0].Slug)
}

// TestFolderTree_CRUDAndChildListing exercises the wiki_folders repository:
// child listing ordered by sort_order/name, find-by-name, page counting under
// a folder, and that ListDistinctCategoryPaths reflects the folder paths.
func TestFolderTree_CRUDAndChildListing(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	mk := func(id, parentID, name, path string, depth int) *types.WikiFolder {
		return &types.WikiFolder{
			ID: id, TenantID: 1, KnowledgeBaseID: "kb-f",
			ParentID: parentID, Name: name, Path: path, Depth: depth,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}
	ai := mk("f-ai", types.WikiFolderRootID, "AI", "AI", 1)
	people := mk("f-people", types.WikiFolderRootID, "人物", "人物", 1)
	llm := mk("f-llm", "f-ai", "LLM", "AI/LLM", 2)
	for _, f := range []*types.WikiFolder{ai, people, llm} {
		require.NoError(t, repo.CreateFolder(ctx, f))
	}

	// Root children: AI, 人物 (ordered by name within equal sort_order).
	roots, err := repo.ListChildFolders(ctx, "kb-f", types.WikiFolderRootID)
	require.NoError(t, err)
	require.Len(t, roots, 2)

	// Direct child of AI is LLM.
	child, err := repo.GetChildFolderByName(ctx, "kb-f", "f-ai", "LLM")
	require.NoError(t, err)
	assert.Equal(t, "f-llm", child.ID)

	// Missing child surfaces the typed not-found error.
	_, err = repo.GetChildFolderByName(ctx, "kb-f", "f-ai", "Nope")
	assert.ErrorIs(t, err, ErrWikiFolderNotFound)

	// Pages filed into folders are counted (archived excluded).
	pAI := makeCategorizedWikiPage("kb-f", "entity/a1", types.WikiPageTypeEntity, types.WikiPageStatusPublished, "AI")
	pAI.FolderID = "f-ai"
	pLLM := makeCategorizedWikiPage("kb-f", "entity/a2", types.WikiPageTypeEntity, types.WikiPageStatusPublished, "AI", "LLM")
	pLLM.FolderID = "f-llm"
	pArch := makeCategorizedWikiPage("kb-f", "entity/a3", types.WikiPageTypeEntity, types.WikiPageStatusArchived, "AI")
	pArch.FolderID = "f-ai"
	for _, p := range []*types.WikiPage{pAI, pLLM, pArch} {
		require.NoError(t, repo.Create(ctx, p))
	}

	aiCount, err := repo.CountPagesInFolder(ctx, "kb-f", "f-ai")
	require.NoError(t, err)
	assert.Equal(t, int64(1), aiCount, "archived page excluded; LLM page is under f-llm, not f-ai")

	// ListDistinctCategoryPaths returns the folder paths split into segments.
	paths, err := repo.ListDistinctCategoryPaths(ctx, "kb-f", 100)
	require.NoError(t, err)
	assert.Contains(t, paths, []string{"AI"})
	assert.Contains(t, paths, []string{"AI", "LLM"})
	assert.Contains(t, paths, []string{"人物"})

	// Pages can be fetched by their folder ids for subtree recompute.
	pages, err := repo.ListPagesByFolderIDs(ctx, "kb-f", []string{"f-ai", "f-llm"})
	require.NoError(t, err)
	assert.Len(t, pages, 3)
}

// TestListByTypeLight_ProjectsNarrowColumnsAndExcludesArchived verifies
// that the index-view projection only emits the slug/title/summary
// triples and respects the archived-status filter. This is the whole
// point of splitting the method off ListByType — the index reader must
// not pay for TEXT content transport.
func TestListByTypeLight_ProjectsNarrowColumnsAndExcludesArchived(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	pages := []*types.WikiPage{
		makeWikiPage("kb-a", "entity/alpha", types.WikiPageTypeEntity, types.WikiPageStatusPublished),
		makeWikiPage("kb-a", "entity/beta", types.WikiPageTypeEntity, types.WikiPageStatusDraft),
		makeWikiPage("kb-a", "entity/gamma", types.WikiPageTypeEntity, types.WikiPageStatusArchived),
		makeWikiPage("kb-a", "concept/delta", types.WikiPageTypeConcept, types.WikiPageStatusPublished),
		makeWikiPage("kb-other", "entity/leaked", types.WikiPageTypeEntity, types.WikiPageStatusPublished),
	}
	for _, p := range pages {
		require.NoError(t, repo.Create(ctx, p))
	}

	entries, total, err := repo.ListByTypeLight(ctx, "kb-a", types.WikiPageTypeEntity, 50, 0)
	require.NoError(t, err)

	// Archived is excluded; sibling KB is excluded; everything else
	// surfaces regardless of draft/published status (the index shows
	// both so admins notice newly-drafted pages).
	assert.Equal(t, int64(2), total)
	require.Len(t, entries, 2)

	// ORDER BY title ASC => alpha, beta.
	assert.Equal(t, "entity/alpha", entries[0].Slug)
	assert.Equal(t, "alpha", entries[0].Title)
	assert.Equal(t, "summary of entity/alpha", entries[0].Summary)
	assert.Equal(t, "entity/beta", entries[1].Slug)
}

// TestListByTypeLight_Pagination walks the type list using offsets and
// asserts the count stays stable regardless of where in the list we
// are — the index handler uses total to render "showing N of M".
func TestListByTypeLight_Pagination(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	for _, s := range []string{"entity/a", "entity/b", "entity/c", "entity/d", "entity/e"} {
		require.NoError(t, repo.Create(ctx, makeWikiPage("kb-a", s, types.WikiPageTypeEntity, types.WikiPageStatusPublished)))
	}

	page1, total1, err := repo.ListByTypeLight(ctx, "kb-a", types.WikiPageTypeEntity, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(5), total1)
	require.Len(t, page1, 2)
	assert.Equal(t, "entity/a", page1[0].Slug)
	assert.Equal(t, "entity/b", page1[1].Slug)

	page2, total2, err := repo.ListByTypeLight(ctx, "kb-a", types.WikiPageTypeEntity, 2, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(5), total2, "total should be stable across pages")
	require.Len(t, page2, 2)
	assert.Equal(t, "entity/c", page2[0].Slug)
	assert.Equal(t, "entity/d", page2[1].Slug)

	page3, total3, err := repo.ListByTypeLight(ctx, "kb-a", types.WikiPageTypeEntity, 2, 4)
	require.NoError(t, err)
	assert.Equal(t, int64(5), total3)
	require.Len(t, page3, 1)
	assert.Equal(t, "entity/e", page3[0].Slug)

	// Offset past the end yields an empty list, not an error — the
	// handler relies on this to short-circuit pagination tails.
	page4, total4, err := repo.ListByTypeLight(ctx, "kb-a", types.WikiPageTypeEntity, 2, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(5), total4)
	assert.Empty(t, page4)
}

// TestListByTypeLight_EmptyType_ReturnsZero exercises the "no rows"
// short-circuit. We skip the SELECT entirely when count is zero, so
// a KB with no pages of a type shouldn't burn a pointless query.
func TestListByTypeLight_EmptyType_ReturnsZero(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	entries, total, err := repo.ListByTypeLight(ctx, "kb-empty", types.WikiPageTypeSynthesis, 50, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), total)
	assert.Empty(t, entries)
}

// TestListByTypeLight_ClampsLimit verifies the [1, 200] clamp. We don't
// want a client passing limit=100000 and forcing the DB to return a
// multi-MB response.
func TestListByTypeLight_ClampsLimit(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	for i := 0; i < 250; i++ {
		slug := "entity/bulk-"
		// Use a stable, zero-padded suffix so title ordering is
		// deterministic for the cap assertion below.
		slug += string(rune('a'+(i/26)%26)) + string(rune('a'+i%26))
		require.NoError(t, repo.Create(ctx, makeWikiPage("kb-cap", slug, types.WikiPageTypeEntity, types.WikiPageStatusPublished)))
	}

	// limit=0 falls back to the default of 50.
	defaultEntries, _, err := repo.ListByTypeLight(ctx, "kb-cap", types.WikiPageTypeEntity, 0, 0)
	require.NoError(t, err)
	assert.Len(t, defaultEntries, 50)

	// limit=5000 clamps to 200.
	clampedEntries, _, err := repo.ListByTypeLight(ctx, "kb-cap", types.WikiPageTypeEntity, 5000, 0)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(clampedEntries), 200)
}

func TestListGraphNeighborsPaginatesFiltersAndNeverCrossesKnowledgeBase(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	center := makeWikiPage("kb-a", "entity/center", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	center.InLinks = types.StringArray{"concept/c", "entity/a", "entity/archived", "entity/missing"}
	center.OutLinks = types.StringArray{"entity/b", "concept/c", "entity/a", "entity/other-kb"}
	require.NoError(t, repo.Create(ctx, center))

	a := makeWikiPage("kb-a", "entity/a", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	a.InLinks = types.StringArray{"x/1", "x/2", "x/3"}
	a.OutLinks = types.StringArray{"entity/center", "x/4"}
	b := makeWikiPage("kb-a", "entity/b", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	b.OutLinks = types.StringArray{"entity/center"}
	c := makeWikiPage("kb-a", "concept/c", types.WikiPageTypeConcept, types.WikiPageStatusPublished)
	c.InLinks = types.StringArray{"entity/center", "x/5"}
	archived := makeWikiPage("kb-a", "entity/archived", types.WikiPageTypeEntity, types.WikiPageStatusArchived)
	otherKB := makeWikiPage("kb-other", "entity/other-kb", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	for _, page := range []*types.WikiPage{a, b, c, archived, otherKB} {
		require.NoError(t, repo.Create(ctx, page))
	}

	first, total, err := repo.ListGraphNeighbors(ctx, "kb-a", center.Slug, nil, 2, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	require.Len(t, first, 2)
	assert.Equal(t, "entity/a", first[0].Slug, "highest-degree node must sort first")
	assert.Equal(t, 5, first[0].LinkCount)
	assert.Equal(t, "concept/c", first[1].Slug)

	second, total, err := repo.ListGraphNeighbors(ctx, "kb-a", center.Slug, nil, 2, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	require.Len(t, second, 1)
	assert.Equal(t, "entity/b", second[0].Slug)

	entities, total, err := repo.ListGraphNeighbors(
		ctx,
		"kb-a",
		center.Slug,
		[]string{types.WikiPageTypeEntity},
		20,
		0,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	assert.Equal(t, []string{"entity/a", "entity/b"}, []string{entities[0].Slug, entities[1].Slug})
}

func TestGetGraphPageBySlugReturnsOnlyGraphProjection(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	page := makeWikiPage("kb-a", "entity/large-center", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	page.Content = strings.Repeat("large body ", 10_000)
	page.Summary = "summary must not be loaded"
	page.SourceRefs = types.StringArray{"source-1"}
	page.InLinks = types.StringArray{"entity/in"}
	page.OutLinks = types.StringArray{"entity/out"}
	require.NoError(t, repo.Create(ctx, page))

	got, err := repo.GetGraphPageBySlug(ctx, "kb-a", page.Slug)
	require.NoError(t, err)
	assert.Equal(t, page.Slug, got.Slug)
	assert.Equal(t, page.Title, got.Title)
	assert.Equal(t, page.PageType, got.PageType)
	assert.Equal(t, page.Status, got.Status)
	assert.Equal(t, page.InLinks, got.InLinks)
	assert.Equal(t, page.OutLinks, got.OutLinks)
	assert.Empty(t, got.Content)
	assert.Empty(t, got.Summary)
	assert.Empty(t, got.SourceRefs)
	assert.Empty(t, got.PageMetadata)
}

func TestListGraphProjectionSearchesNamesAndDoesNotLoadBodies(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()

	target := makeWikiPage("kb-a", "entity/needle-slug", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	target.Title = "Needle Title"
	target.Content = strings.Repeat("large body ", 10_000)
	target.InLinks = types.StringArray{"entity/in"}
	target.OutLinks = types.StringArray{"entity/out"}
	require.NoError(t, repo.Create(ctx, target))

	contentOnly := makeWikiPage("kb-a", "entity/content-only", types.WikiPageTypeEntity, types.WikiPageStatusPublished)
	contentOnly.Title = "Unrelated"
	contentOnly.Content = "Needle only exists in the body"
	require.NoError(t, repo.Create(ctx, contentOnly))

	pages, total, err := repo.List(ctx, &types.WikiPageListRequest{
		KnowledgeBaseID: "kb-a",
		PageType:        types.WikiPageTypeEntity,
		Query:           "Needle",
		Projection:      "graph",
		Page:            1,
		PageSize:        50,
		SortBy:          "title",
		SortOrder:       "asc",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "graph search must not scan/match page content")
	require.Len(t, pages, 1)
	assert.Equal(t, target.Slug, pages[0].Slug)
	assert.Empty(t, pages[0].Content)
	assert.Empty(t, pages[0].Summary)
	assert.Empty(t, pages[0].InLinks)
	assert.Empty(t, pages[0].OutLinks)
}
