package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbdeletequeue"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestWikiMaterializationCannotReappearAfterKnowledgeBaseTombstone(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.KnowledgeBase{}, &types.Knowledge{}, &types.TaskPendingOp{},
		&types.WikiPage{}, &types.WikiFolder{}, &types.WikiPageIssue{}, &types.WikiLogEntry{},
		&wikilease.Lease{},
	))
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "test"}).Error)
	pageRepo := NewWikiPageRepository(db)
	existingPage := &types.WikiPage{
		ID: "existing-page", TenantID: 7, KnowledgeBaseID: "kb-1",
		Slug: "entity/existing", Title: "existing", Content: "before", Version: 1,
	}
	require.NoError(t, pageRepo.Create(context.Background(), existingPage))

	// These objects model a Wiki worker's in-memory reduce result after a long
	// LLM call. The KB is deleted before that worker reaches persistence.
	latePage := &types.WikiPage{
		ID: "late-page", TenantID: 7, KnowledgeBaseID: "kb-1",
		Slug: "entity/late", Title: "late", Version: 1,
	}
	lateFolder := &types.WikiFolder{
		ID: "late-folder", TenantID: 7, KnowledgeBaseID: "kb-1", Name: "late",
	}
	lateIssue := &types.WikiPageIssue{
		ID: "late-issue", TenantID: 7, KnowledgeBaseID: "kb-1", Slug: "entity/late",
		IssueType: "stale", Description: "late", ReportedBy: "test",
	}
	lateLog := &types.WikiLogEntry{
		TenantID: 7, KnowledgeBaseID: "kb-1", Action: "ingest", KnowledgeID: "knowledge-1",
	}
	require.NoError(t, kbdeletequeue.New(db).Prepare(
		context.Background(), 7, "kb-1", []byte(`{"tenant_id":7,"knowledge_base_id":"kb-1"}`),
	))

	existingPage.Content = "late overwrite"
	for _, write := range []func() error{
		func() error { return pageRepo.Create(context.Background(), latePage) },
		func() error { return pageRepo.Update(context.Background(), existingPage) },
		func() error { return pageRepo.UpdateMeta(context.Background(), existingPage) },
		func() error { return pageRepo.UpdateAutoLinkedContent(context.Background(), existingPage) },
		func() error { return pageRepo.CreateFolder(context.Background(), lateFolder) },
		func() error { return pageRepo.CreateIssue(context.Background(), lateIssue) },
		func() error {
			return NewWikiLogEntryRepository(db).AppendBatch(context.Background(), []*types.WikiLogEntry{lateLog})
		},
	} {
		require.ErrorIs(t, write(), kbwritefence.ErrKnowledgeBaseUnavailable)
	}
	require.NoError(t, kbdeletequeue.New(db).PurgeWikiState(context.Background(), 7, "kb-1"))

	for table, model := range map[string]interface{}{
		"wiki_pages":       &types.WikiPage{},
		"wiki_folders":     &types.WikiFolder{},
		"wiki_page_issues": &types.WikiPageIssue{},
		"wiki_log_entries": &types.WikiLogEntry{},
	} {
		var count int64
		require.NoError(t, db.Unscoped().Model(model).
			Where("tenant_id = ? AND knowledge_base_id = ?", 7, "kb-1").Count(&count).Error, table)
		require.Zero(t, count, table)
	}

	// The sentinel remains detectable through the log repository's contextual
	// wrapping, which lets callers classify this as terminal stale work.
	require.True(t, errors.Is(
		NewWikiLogEntryRepository(db).AppendBatch(context.Background(), []*types.WikiLogEntry{lateLog}),
		kbwritefence.ErrKnowledgeBaseUnavailable,
	))
}
