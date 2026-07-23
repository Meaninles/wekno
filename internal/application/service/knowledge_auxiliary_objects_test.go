package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAuxiliaryFileServiceForPathAllowsNilKBWithExplicitIdentity(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&types.Tenant{}, &types.KnowledgeBase{}, &types.Knowledge{}, &types.TaskPendingOp{},
	))
	require.NoError(t, db.Create(&types.Tenant{ID: 7, Name: "tenant"}).Error)
	require.NoError(t, db.Create(&types.KnowledgeBase{ID: "kb-1", TenantID: 7, Name: "kb"}).Error)
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1", Type: types.KnowledgeTypeFAQ,
		ParseStatus: types.ParseStatusCompleted, ProcessingGeneration: "generation-1",
	}).Error)
	fileSvc := &cleanupFileServiceStub{}
	registry := knowledgeaux.NewWithResolver(db, func(
		context.Context, *types.Tenant, string,
	) (interfaces.FileService, string, error) {
		return fileSvc, "local", nil
	})
	object := knowledgeaux.Object{
		TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1", Path: "local://7/faq.json",
		FallbackProvider: "local", Kind: knowledgeaux.KindFAQEntries,
	}
	_, err = registry.Register(context.Background(), object, fileSvc)
	require.NoError(t, err)
	svc := &knowledgeService{auxObjects: registry}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	resolved, err := svc.auxiliaryFileServiceForPath(
		ctx, nil, "kb-1", "knowledge-1", object.Path,
	)
	require.NoError(t, err)
	require.Same(t, fileSvc, resolved)
	_, err = svc.auxiliaryFileServiceForPath(ctx, nil, "", "knowledge-1", object.Path)
	require.Error(t, err)
	_, err = svc.auxiliaryFileServiceForPath(ctx, nil, "wrong-kb", "knowledge-1", object.Path)
	require.ErrorIs(t, err, knowledgeaux.ErrReservationLost)
}
