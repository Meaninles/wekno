package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type deleteImageInfoRepoStub struct {
	interfaces.ChunkRepository
	rows []interfaces.ChunkImageInfo
	err  error
}

func (s *deleteImageInfoRepoStub) ListImageInfoByKnowledgeIDsUnscoped(
	context.Context, uint64, []string,
) ([]interfaces.ChunkImageInfo, error) {
	return s.rows, s.err
}

func TestRemoveChunkRefs(t *testing.T) {
	got := removeChunkRefs(
		types.StringArray{"chunk-a", "chunk-b", "chunk-c"},
		map[string]bool{"chunk-b": true},
	)

	require.Equal(t, types.StringArray{"chunk-a", "chunk-c"}, got)
}

func TestRemoveChunkRefsNoRemovedSet(t *testing.T) {
	refs := types.StringArray{"chunk-a", "chunk-b"}

	got := removeChunkRefs(refs, nil)

	require.Equal(t, refs, got)
}

func TestListDeleteImageInfoPropagatesSnapshotFailure(t *testing.T) {
	snapshotErr := errors.New("database unavailable")
	svc := &knowledgeService{chunkRepo: &deleteImageInfoRepoStub{err: snapshotErr}}

	_, err := svc.listDeleteImageInfo(context.Background(), 7, []string{"knowledge-1"})
	require.ErrorIs(t, err, snapshotErr)
}

func TestBuildKnowledgeDeleteVectorGroupsKeepsBoundStoresSeparate(t *testing.T) {
	storeA := "store-a"
	storeB := "store-b"
	knowledges := []*types.Knowledge{
		{ID: "a-1", KnowledgeBaseID: "kb-a", EmbeddingModelID: "embed", Type: "file"},
		{ID: "a-2", KnowledgeBaseID: "kb-a", EmbeddingModelID: "embed", Type: "file"},
		{ID: "b-1", KnowledgeBaseID: "kb-b", EmbeddingModelID: "embed", Type: "file"},
	}
	kbs := map[string]*types.KnowledgeBase{
		"kb-a": {ID: "kb-a", VectorStoreID: &storeA},
		"kb-b": {ID: "kb-b", VectorStoreID: &storeB},
	}

	groups, err := buildKnowledgeDeleteVectorGroups(knowledges, kbs)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, []string{"a-1", "a-2"}, groups[knowledgeDeleteVectorGroupKey{
		KnowledgeBaseID: "kb-a", VectorStoreID: storeA, EmbeddingModelID: "embed", KnowledgeType: "file",
	}])
	assert.Equal(t, []string{"b-1"}, groups[knowledgeDeleteVectorGroupKey{
		KnowledgeBaseID: "kb-b", VectorStoreID: storeB, EmbeddingModelID: "embed", KnowledgeType: "file",
	}])
}

func TestBuildKnowledgeDeleteVectorGroupsRejectsMissingKBSnapshot(t *testing.T) {
	_, err := buildKnowledgeDeleteVectorGroups(
		[]*types.Knowledge{{ID: "a", KnowledgeBaseID: "missing"}},
		map[string]*types.KnowledgeBase{},
	)
	require.ErrorContains(t, err, "disappeared from routing snapshot")
}

type cleanupKBServiceStub struct {
	interfaces.KnowledgeBaseService
	kb  *types.KnowledgeBase
	err error
}

func (s *cleanupKBServiceStub) GetKnowledgeBaseByID(context.Context, string) (*types.KnowledgeBase, error) {
	return s.kb, s.err
}

type cleanupChunkServiceStub struct {
	interfaces.ChunkService
	repo        interfaces.ChunkRepository
	deleteCalls int
}

func (s *cleanupChunkServiceStub) GetRepository() interfaces.ChunkRepository { return s.repo }
func (s *cleanupChunkServiceStub) DeleteChunksByKnowledgeID(context.Context, string) error {
	s.deleteCalls++
	return nil
}

type cleanupFileServiceStub struct {
	interfaces.FileService
	deleteErr   error
	deleteCalls int
}

func (s *cleanupFileServiceStub) DeleteFile(context.Context, string) error {
	s.deleteCalls++
	return s.deleteErr
}

type cleanupGraphStub struct {
	interfaces.RetrieveGraphRepository
}

func (cleanupGraphStub) DelGraph(context.Context, []types.NameSpace) error { return nil }

func cleanupTestContext() context.Context {
	tenant := &types.Tenant{ID: 7}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, tenant.ID)
	return context.WithValue(ctx, types.TenantInfoContextKey, tenant)
}

func cleanupAuxRegistry(t *testing.T, fileSvc interfaces.FileService) *knowledgeaux.Registry {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:cleanup-aux-%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Tenant{}, &types.KnowledgeBase{}, &types.TaskPendingOp{}))
	require.NoError(t, db.Create(&types.Tenant{ID: 7, Name: "tenant"}).Error)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "kb"}).Error)
	return knowledgeaux.NewWithResolver(db, func(
		context.Context, *types.Tenant, string,
	) (interfaces.FileService, string, error) {
		return fileSvc, "local", nil
	})
}

func TestCleanupKnowledgeResourcesFailsClosedWhenKBLookupFails(t *testing.T) {
	loadErr := errors.New("database unavailable")
	chunkRepo := &deleteImageInfoRepoStub{}
	chunkSvc := &cleanupChunkServiceStub{repo: chunkRepo}
	fileSvc := &cleanupFileServiceStub{}
	svc := &knowledgeService{
		kbService:    &cleanupKBServiceStub{err: loadErr},
		chunkRepo:    chunkRepo,
		chunkService: chunkSvc,
		fileSvc:      fileSvc,
		auxObjects:   cleanupAuxRegistry(t, fileSvc),
		graphEngine:  cleanupGraphStub{},
	}

	err := svc.cleanupKnowledgeResources(cleanupTestContext(), &types.Knowledge{
		ID: "knowledge-1", KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusCompleted,
	})
	require.ErrorIs(t, err, loadErr)
	assert.Zero(t, chunkSvc.deleteCalls)
	assert.Zero(t, fileSvc.deleteCalls)
}

func TestCleanupKnowledgeResourcesReturnsImageDeleteErrorAndRetainsRetrySnapshot(t *testing.T) {
	imageDeleteErr := errors.New("object store unavailable")
	chunkRepo := &deleteImageInfoRepoStub{rows: []interfaces.ChunkImageInfo{{
		KnowledgeID: "knowledge-1",
		ImageInfo:   `[{"url":"local://7/knowledge-1/image.png"}]`,
	}}}
	chunkSvc := &cleanupChunkServiceStub{repo: chunkRepo}
	fileSvc := &cleanupFileServiceStub{deleteErr: imageDeleteErr}
	svc := &knowledgeService{
		kbService:    &cleanupKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1", TenantID: 7}},
		chunkRepo:    chunkRepo,
		chunkService: chunkSvc,
		fileSvc:      fileSvc,
		auxObjects:   cleanupAuxRegistry(t, fileSvc),
		graphEngine:  cleanupGraphStub{},
	}
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1", ParseStatus: types.ParseStatusCompleted,
	}

	err := svc.cleanupKnowledgeResources(cleanupTestContext(), knowledge)
	require.ErrorIs(t, err, knowledgeaux.ErrBindingMissing)
	err = svc.cleanupKnowledgeResources(cleanupTestContext(), knowledge)
	require.ErrorIs(t, err, knowledgeaux.ErrBindingMissing)
	assert.Equal(t, 2, chunkSvc.deleteCalls)
	assert.Zero(t, fileSvc.deleteCalls)
}
