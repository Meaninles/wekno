package knowledgefolders

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newFolderTestService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.KnowledgeBase{}, &types.Knowledge{}))
	service := NewService(db, nil, nil, nil)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
	ctx = context.WithValue(ctx, types.UserIDContextKey, "user-1")
	require.NoError(t, service.Migrate(ctx))
	return service, ctx
}

func createTestKnowledge(
	t *testing.T,
	service *Service,
	folderID, id, status string,
	pendingSubtasks int,
	wikiStatus, enrichmentStatus string,
) {
	t.Helper()
	now := time.Now()
	require.NoError(t, service.db.Create(&types.Knowledge{
		ID:                   id,
		TenantID:             7,
		KnowledgeBaseID:      "kb-1",
		FolderID:             folderID,
		Type:                 "file",
		Title:                id + ".txt",
		FileName:             id + ".txt",
		FileType:             "txt",
		ParseStatus:          status,
		PendingSubtasksCount: pendingSubtasks,
		WikiStatus:           wikiStatus,
		EnrichmentStatus:     enrichmentStatus,
		CreatedAt:            now,
		UpdatedAt:            now,
	}).Error)
}

func TestFolderHierarchyStatisticsAndPagination(t *testing.T) {
	service, ctx := newFolderTestService(t)
	root, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "研发", Description: "根目录"})
	require.NoError(t, err)
	child, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{ParentID: root.ID, Name: "项目 A"})
	require.NoError(t, err)

	createTestKnowledge(t, service, root.ID, "doc-root", types.ParseStatusPending, 0, types.WikiStatusNone, types.EnrichmentStatusNone)
	createTestKnowledge(t, service, child.ID, "doc-child", types.ParseStatusFinalizing, 3, types.WikiStatusPending, types.EnrichmentStatusPending)

	root, err = service.GetFolder(ctx, "kb-1", root.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), root.Stats.SubtreeDocumentCount)
	require.Equal(t, int64(1), root.Stats.DirectChildFolderCount)
	require.Equal(t, int64(1), root.Stats.ParsePendingCount)
	require.Equal(t, int64(3), root.Stats.EnrichmentPendingTaskCount)
	require.Equal(t, int64(1), root.Stats.WikiPendingTaskCount)

	first, err := service.ListNodes(ctx, "kb-1", root.ID, 1, 1, types.KnowledgeListFilter{})
	require.NoError(t, err)
	require.Equal(t, int64(2), first.Total)
	require.Len(t, first.Data, 1)
	require.Equal(t, "folder", first.Data[0].NodeType)
	require.Equal(t, child.ID, first.Data[0].Folder.ID)

	second, err := service.ListNodes(ctx, "kb-1", root.ID, 2, 1, types.KnowledgeListFilter{})
	require.NoError(t, err)
	require.Len(t, second.Data, 1)
	require.Equal(t, "document", second.Data[0].NodeType)
	require.Equal(t, "doc-root", second.Data[0].Document.ID)
}

func TestFolderStatsFollowStatusAndLocationChanges(t *testing.T) {
	service, ctx := newFolderTestService(t)
	parent, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "父级"})
	require.NoError(t, err)
	child, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{ParentID: parent.ID, Name: "子级"})
	require.NoError(t, err)
	createTestKnowledge(t, service, child.ID, "doc-1", types.ParseStatusPending, 0, types.WikiStatusNone, types.EnrichmentStatusNone)

	require.NoError(t, service.db.Model(&types.Knowledge{}).Where("id = ?", "doc-1").Updates(map[string]any{
		"parse_status":           types.ParseStatusProcessing,
		"pending_subtasks_count": 2,
		"wiki_status":            types.WikiStatusPending,
	}).Error)
	parent, err = service.GetFolder(ctx, "kb-1", parent.ID)
	require.NoError(t, err)
	require.Zero(t, parent.Stats.ParsePendingCount)
	require.Equal(t, int64(1), parent.Stats.ParseRunningCount)
	require.Equal(t, int64(2), parent.Stats.EnrichmentPendingTaskCount)
	require.Equal(t, int64(1), parent.Stats.WikiPendingTaskCount)

	affected, err := service.MoveDocuments(ctx, "kb-1", MoveDocumentsRequest{
		KnowledgeIDs: []string{"doc-1"}, TargetFolderID: parent.ID,
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)
	child, err = service.GetFolder(ctx, "kb-1", child.ID)
	require.NoError(t, err)
	require.Zero(t, child.Stats.SubtreeDocumentCount)
	parent, err = service.GetFolder(ctx, "kb-1", parent.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), parent.Stats.SubtreeDocumentCount)

	require.NoError(t, service.db.Delete(&types.Knowledge{}, "id = ?", "doc-1").Error)
	parent, err = service.GetFolder(ctx, "kb-1", parent.ID)
	require.NoError(t, err)
	require.Zero(t, parent.Stats.SubtreeDocumentCount)
	require.Zero(t, parent.Stats.ParseRunningCount)
}

func TestFolderStatsSeparateAbnormalAndTerminalFailedDocuments(t *testing.T) {
	service, ctx := newFolderTestService(t)
	folder, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "状态拆分"})
	require.NoError(t, err)

	// A failed derivative branch is still only abnormal while another branch
	// remains active; the document-level workflow has not reached failed yet.
	createTestKnowledge(
		t, service, folder.ID, "doc-retrying", types.ParseStatusCompleted, 1,
		types.WikiStatusPending, types.EnrichmentStatusFailed,
	)
	createTestKnowledge(
		t, service, folder.ID, "doc-degraded", types.ParseStatusCompleted, 0,
		types.WikiStatusCompleted, types.EnrichmentStatusDegraded,
	)
	createTestKnowledge(
		t, service, folder.ID, "doc-failed", types.ParseStatusFailed, 0,
		types.WikiStatusNone, types.EnrichmentStatusNone,
	)

	folder, err = service.GetFolder(ctx, "kb-1", folder.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), folder.Stats.AbnormalDocumentCount)
	require.Equal(t, int64(1), folder.Stats.FailedDocumentCount)

	// Once the remaining branch settles, the failed derivative becomes a true
	// document-level failure and moves between the two counters atomically.
	require.NoError(t, service.db.Model(&types.Knowledge{}).
		Where("id = ?", "doc-retrying").
		Updates(map[string]any{
			"pending_subtasks_count": 0,
			"wiki_status":            types.WikiStatusCompleted,
		}).Error)
	folder, err = service.GetFolder(ctx, "kb-1", folder.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), folder.Stats.AbnormalDocumentCount)
	require.Equal(t, int64(2), folder.Stats.FailedDocumentCount)

	// Reconciliation uses the same projection as the incremental trigger.
	require.NoError(t, service.ReconcileAll(ctx))
	folder, err = service.GetFolder(ctx, "kb-1", folder.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), folder.Stats.AbnormalDocumentCount)
	require.Equal(t, int64(2), folder.Stats.FailedDocumentCount)
}

func TestFolderMoveCycleAndRecursiveDelete(t *testing.T) {
	service, ctx := newFolderTestService(t)
	parent, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "父级"})
	require.NoError(t, err)
	child, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{ParentID: parent.ID, Name: "子级"})
	require.NoError(t, err)
	grandchild, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{ParentID: child.ID, Name: "孙级"})
	require.NoError(t, err)
	createTestKnowledge(t, service, grandchild.ID, "doc-deep", types.ParseStatusCompleted, 0, types.WikiStatusCompleted, types.EnrichmentStatusCompleted)

	badParent := grandchild.ID
	_, err = service.UpdateFolder(ctx, "kb-1", parent.ID, UpdateFolderRequest{ParentID: &badParent})
	require.ErrorIs(t, err, ErrFolderCycle)

	operation, err := service.RequestDeleteFolder(ctx, "kb-1", child.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), operation.TotalDocumentCount)
	require.Equal(t, FolderDeleteOperationPending, operation.Status)

	_, err = service.GetFolder(ctx, "kb-1", child.ID)
	require.ErrorIs(t, err, ErrFolderNotFound)
	_, err = service.GetFolder(ctx, "kb-1", grandchild.ID)
	require.ErrorIs(t, err, ErrFolderNotFound)
	var deleting types.Knowledge
	require.NoError(t, service.db.Where("id = ?", "doc-deep").First(&deleting).Error)
	require.Equal(t, types.ParseStatusDeleting, deleting.ParseStatus)

	parent, err = service.GetFolder(ctx, "kb-1", parent.ID)
	require.NoError(t, err)
	require.Zero(t, parent.Stats.SubtreeDocumentCount)
	require.Zero(t, parent.Stats.DirectChildFolderCount)

	require.NoError(t, service.db.Delete(&types.Knowledge{}, "id = ?", "doc-deep").Error)
	require.NoError(t, service.OnKnowledgeDeleteCompleted(ctx, 7, "kb-1", []string{"doc-deep"}))
	operation, err = service.getDeleteOperation(ctx, 7, "kb-1", operation.ID)
	require.NoError(t, err)
	require.Equal(t, FolderDeleteOperationCompleted, operation.Status)
	var remainingFolders int64
	require.NoError(t, service.db.Model(&Folder{}).
		Where("id IN ?", []string{child.ID, grandchild.ID}).Count(&remainingFolders).Error)
	require.Zero(t, remainingFolders)
}

func TestListNodesProjectsMatchingDocumentsThroughAncestorFolders(t *testing.T) {
	service, ctx := newFolderTestService(t)
	matchingRoot, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "匹配树"})
	require.NoError(t, err)
	matchingChild, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{ParentID: matchingRoot.ID, Name: "深层"})
	require.NoError(t, err)
	nonMatchingRoot, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "其它树"})
	require.NoError(t, err)
	createTestKnowledge(t, service, matchingChild.ID, "failed-deep", types.ParseStatusFailed, 0, types.WikiStatusNone, types.EnrichmentStatusNone)
	createTestKnowledge(t, service, nonMatchingRoot.ID, "completed-other", types.ParseStatusCompleted, 0, types.WikiStatusCompleted, types.EnrichmentStatusCompleted)

	filter := types.KnowledgeListFilter{WorkflowStatus: "failed"}
	rootPage, err := service.ListNodes(ctx, "kb-1", "", 1, 20, filter)
	require.NoError(t, err)
	require.Equal(t, int64(1), rootPage.Total)
	require.Len(t, rootPage.Data, 1)
	require.Equal(t, matchingRoot.ID, rootPage.Data[0].Folder.ID)

	childPage, err := service.ListNodes(ctx, "kb-1", matchingRoot.ID, 1, 20, filter)
	require.NoError(t, err)
	require.Equal(t, int64(1), childPage.Total)
	require.Equal(t, matchingChild.ID, childPage.Data[0].Folder.ID)

	documentPage, err := service.ListNodes(ctx, "kb-1", matchingChild.ID, 1, 20, filter)
	require.NoError(t, err)
	require.Equal(t, int64(1), documentPage.Total)
	require.Equal(t, "failed-deep", documentPage.Data[0].Document.ID)
}

func TestKnowledgeBaseTaskStatsIncludeRootAndExcludeDeleting(t *testing.T) {
	service, ctx := newFolderTestService(t)
	folder, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "任务"})
	require.NoError(t, err)
	createTestKnowledge(t, service, "", "root-pending", types.ParseStatusPending, 0, types.WikiStatusNone, types.EnrichmentStatusNone)
	createTestKnowledge(t, service, folder.ID, "folder-running", types.ParseStatusProcessing, 2, types.WikiStatusPending, types.EnrichmentStatusPending)
	createTestKnowledge(t, service, folder.ID, "hidden-deleting", types.ParseStatusDeleting, 0, types.WikiStatusNone, types.EnrichmentStatusNone)

	stats, err := service.GetKnowledgeBaseTaskStats(ctx, "kb-1")
	require.NoError(t, err)
	require.Equal(t, int64(2), stats.DocumentCount)
	require.Equal(t, int64(1), stats.ParsePendingCount)
	require.Equal(t, int64(1), stats.ParseRunningCount)
	require.Equal(t, int64(2), stats.EnrichmentPendingTaskCount)
	require.Equal(t, int64(1), stats.WikiPendingTaskCount)
}

func TestFolderValidationIsolationAndDuplicateHandling(t *testing.T) {
	service, ctx := newFolderTestService(t)
	_, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "../bad"})
	require.ErrorIs(t, err, ErrFolderNameInvalid)
	_, err = service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "同名"})
	require.NoError(t, err)
	_, err = service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: " 同名 "})
	require.ErrorIs(t, err, ErrFolderNameExists)

	foreign, err := service.CreateFolder(ctx, "kb-2", CreateFolderRequest{Name: "其他库"})
	require.NoError(t, err)
	_, err = service.CreateFolder(ctx, "kb-1", CreateFolderRequest{ParentID: foreign.ID, Name: "越权"})
	require.ErrorIs(t, err, ErrFolderNotFound)

	otherCtx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(8))
	_, err = service.GetFolder(otherCtx, "kb-1", foreign.ID)
	require.True(t, errors.Is(err, ErrFolderNotFound))
}

func TestKnowledgeBaseDeletionCleansFolderMetadata(t *testing.T) {
	service, ctx := newFolderTestService(t)
	require.NoError(t, service.db.Create(&types.KnowledgeBase{
		ID: "kb-1", TenantID: 7, Name: "待删除知识库", Type: types.KnowledgeBaseTypeDocument,
	}).Error)
	folder, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "目录"})
	require.NoError(t, err)
	createTestKnowledge(
		t, service, folder.ID, "doc-cleanup", types.ParseStatusCompleted, 0,
		types.WikiStatusCompleted, types.EnrichmentStatusCompleted,
	)
	operation, err := service.RequestDeleteFolder(ctx, "kb-1", folder.ID)
	require.NoError(t, err)
	require.NotEmpty(t, operation.ID)

	require.NoError(t, service.db.Delete(&types.KnowledgeBase{}, "id = ?", "kb-1").Error)
	var folderCount, closureCount, statsCount, operationCount, itemCount int64
	require.NoError(t, service.db.Model(&Folder{}).Where("knowledge_base_id = ?", "kb-1").Count(&folderCount).Error)
	require.NoError(t, service.db.Model(&FolderClosure{}).Where("knowledge_base_id = ?", "kb-1").Count(&closureCount).Error)
	require.NoError(t, service.db.Model(&FolderStats{}).Where("knowledge_base_id = ?", "kb-1").Count(&statsCount).Error)
	require.NoError(t, service.db.Model(&FolderDeleteOperation{}).Where("knowledge_base_id = ?", "kb-1").Count(&operationCount).Error)
	require.NoError(t, service.db.Model(&FolderDeleteOperationItem{}).Where("operation_id = ?", operation.ID).Count(&itemCount).Error)
	require.Zero(t, folderCount)
	require.Zero(t, closureCount)
	require.Zero(t, statsCount)
	require.Zero(t, operationCount)
	require.Zero(t, itemCount)
}

func TestRecursiveSearchFindsFoldersAndDocumentsAndHonorsScope(t *testing.T) {
	service, ctx := newFolderTestService(t)
	root, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "产品资料"})
	require.NoError(t, err)
	child, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{ParentID: root.ID, Name: "Needle 方案"})
	require.NoError(t, err)
	outside, err := service.CreateFolder(ctx, "kb-1", CreateFolderRequest{Name: "其他"})
	require.NoError(t, err)
	createTestKnowledge(t, service, child.ID, "needle-document", types.ParseStatusCompleted, 0, types.WikiStatusNone, types.EnrichmentStatusNone)
	require.NoError(t, service.db.Model(&types.Knowledge{}).Where("id = ?", "needle-document").
		Updates(map[string]any{"title": "Needle 文档", "file_name": "Needle 文档.txt"}).Error)
	createTestKnowledge(t, service, outside.ID, "outside-document", types.ParseStatusCompleted, 0, types.WikiStatusNone, types.EnrichmentStatusNone)
	require.NoError(t, service.db.Model(&types.Knowledge{}).Where("id = ?", "outside-document").
		Updates(map[string]any{"title": "Needle 外部", "file_name": "Needle 外部.txt"}).Error)

	all, err := service.SearchKnowledgeBase(
		ctx, "kb-1", "", "needle", 1, 20, types.KnowledgeListFilter{},
	)
	require.NoError(t, err)
	require.Equal(t, int64(3), all.Total)
	require.Equal(t, "folder", all.Data[0].NodeType)

	scoped, err := service.SearchKnowledgeBase(
		ctx, "kb-1", root.ID, "needle", 1, 20, types.KnowledgeListFilter{},
	)
	require.NoError(t, err)
	require.Equal(t, int64(2), scoped.Total)
	require.ElementsMatch(
		t,
		[]string{"folder", "document"},
		[]string{scoped.Data[0].NodeType, scoped.Data[1].NodeType},
	)

	empty, err := service.SearchKnowledgeBase(
		ctx, "kb-1", "", "", 1, 20, types.KnowledgeListFilter{},
	)
	require.NoError(t, err)
	require.Empty(t, empty.Data)
	require.Zero(t, empty.Total)
}

func TestRelativeUploadPathValidation(t *testing.T) {
	directories, filename, err := parseRelativeUploadPath(`项目\子目录\文档.txt`)
	require.NoError(t, err)
	require.Equal(t, []string{"项目", "子目录"}, directories)
	require.Equal(t, "文档.txt", filename)

	for _, invalid := range []string{
		"single.txt",
		"../secret.txt",
		"folder/../secret.txt",
		`C:\folder\secret.txt`,
		"folder//secret.txt",
	} {
		_, _, err := parseRelativeUploadPath(invalid)
		require.Error(t, err, invalid)
	}
}
