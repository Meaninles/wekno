package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type sourceReadKnowledgeRepositoryStub struct {
	interfaces.KnowledgeRepository
	knowledge *types.Knowledge
}

func (s *sourceReadKnowledgeRepositoryStub) GetKnowledgeByID(
	_ context.Context, tenantID uint64, knowledgeID string,
) (*types.Knowledge, error) {
	if s.knowledge == nil || s.knowledge.TenantID != tenantID || s.knowledge.ID != knowledgeID {
		return nil, fmt.Errorf("knowledge not found")
	}
	return s.knowledge, nil
}

type sourceReadKnowledgeBaseServiceStub struct {
	interfaces.KnowledgeBaseService
	knowledgeBase *types.KnowledgeBase
}

func (s *sourceReadKnowledgeBaseServiceStub) GetKnowledgeBaseByID(
	_ context.Context, knowledgeBaseID string,
) (*types.KnowledgeBase, error) {
	if s.knowledgeBase == nil || s.knowledgeBase.ID != knowledgeBaseID {
		return nil, fmt.Errorf("knowledge base not found")
	}
	return s.knowledgeBase, nil
}

type sourceReadFileServiceStub struct {
	interfaces.FileService
	content  string
	getCalls int
}

func (s *sourceReadFileServiceStub) BindingForPath(path string) (storagebinding.Binding, error) {
	if !strings.HasPrefix(path, "local://") {
		return storagebinding.Binding{}, knowledgeaux.ErrBindingMismatch
	}
	return storagebinding.Normalize(storagebinding.Binding{
		Provider:           storagebinding.ProviderLocal,
		CanonicalLocalBase: "/test/files",
		LocalRootIdentity:  "test:source-read",
		ConfigSource:       storagebinding.ConfigSourceDirect,
		CredentialScope:    storagebinding.CredentialScopeNone,
	})
}

func (s *sourceReadFileServiceStub) GetFile(
	_ context.Context, _ string,
) (io.ReadCloser, error) {
	s.getCalls++
	return io.NopCloser(strings.NewReader(s.content)), nil
}

func TestGetKnowledgeFileReadsLegacySourceWithoutOwnershipLedger(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.Tenant{}, &types.KnowledgeBase{}, &types.Knowledge{}, &types.TaskPendingOp{},
	))

	tenant := &types.Tenant{ID: 7, Name: "tenant"}
	knowledgeBase := &types.KnowledgeBase{ID: "kb-1", TenantID: tenant.ID, Name: "kb"}
	knowledgeBase.SetStorageProvider("local")
	knowledge := &types.Knowledge{
		ID: "knowledge-1", TenantID: tenant.ID, KnowledgeBaseID: knowledgeBase.ID,
		Type: "file", FileName: "source.pdf",
		FilePath: "local://7/knowledge-1/source.pdf",
	}
	require.NoError(t, db.Create(tenant).Error)
	require.NoError(t, db.Create(knowledgeBase).Error)
	require.NoError(t, db.Create(knowledge).Error)

	fileService := &sourceReadFileServiceStub{content: "legacy source"}
	registry := knowledgeaux.NewWithResolver(db, func(
		context.Context, *types.Tenant, string,
	) (interfaces.FileService, string, error) {
		return fileService, "local", nil
	})
	service := &knowledgeService{
		repo:       &sourceReadKnowledgeRepositoryStub{knowledge: knowledge},
		kbService:  &sourceReadKnowledgeBaseServiceStub{knowledgeBase: knowledgeBase},
		auxObjects: registry,
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, tenant.ID)

	file, filename, err := service.GetKnowledgeFile(ctx, knowledge.ID)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, file.Close())
	})
	body, err := io.ReadAll(file)
	require.NoError(t, err)
	require.Equal(t, "source.pdf", filename)
	require.Equal(t, "legacy source", string(body))
	require.Equal(t, 1, fileService.getCalls)

	var ownershipCount int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("task_type = ?", knowledgeaux.TaskType).
		Count(&ownershipCount).Error)
	require.Zero(t, ownershipCount, "read fallback must not create ownership")
}
