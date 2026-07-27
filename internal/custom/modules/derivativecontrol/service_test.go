package derivativecontrol

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	appservice "github.com/Tencent/WeKnora/internal/application/service"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type derivativeSettingsStub struct {
	interfaces.SystemSettingService
	mu  sync.RWMutex
	tpm int64
}

func (s *derivativeSettingsStub) GetInt(
	_ context.Context, key, _ string, fallback int64,
) int64 {
	if key != "derivative.tpm" {
		return fallback
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.tpm == 0 {
		return fallback
	}
	return s.tpm
}

func (s *derivativeSettingsStub) Update(
	_ context.Context, key string, rawValue any,
) (*types.SystemSetting, error) {
	if key != "derivative.tpm" {
		return nil, errors.New("unexpected setting")
	}
	value, ok := rawValue.(int64)
	if !ok || value < minTPM || value > maxTPM {
		return nil, errors.New("invalid derivative TPM")
	}
	s.mu.Lock()
	s.tpm = value
	s.mu.Unlock()
	return &types.SystemSetting{
		Key:       key,
		Value:     types.JSON(strconv.FormatInt(value, 10)),
		ValueType: "int",
	}, nil
}

type modelReferenceStub struct {
	interfaces.KnowledgeBaseRepository
	count int64
	err   error
}

func (s *modelReferenceStub) CountByModelID(
	context.Context, uint64, string,
) (int64, error) {
	return s.count, s.err
}

type agentReferenceStub struct {
	interfaces.CustomAgentRepository
	count int64
	err   error
}

func (s *agentReferenceStub) CountByModelID(
	context.Context, uint64, string,
) (int64, error) {
	return s.count, s.err
}

type staticChat struct {
	id   string
	name string
}

func (c *staticChat) Chat(
	context.Context, []chat.Message, *chat.ChatOptions,
) (*types.ChatResponse, error) {
	return &types.ChatResponse{
		Content: "ok",
		Usage:   types.TokenUsage{TotalTokens: 2},
	}, nil
}

func (c *staticChat) ChatStream(
	context.Context, []chat.Message, *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	out := make(chan types.StreamResponse)
	close(out)
	return out, nil
}

func (c *staticChat) GetModelName() string { return c.name }
func (c *staticChat) GetModelID() string   { return c.id }

type guardedModelService struct {
	interfaces.ModelService
	control *Service
	models  map[string]*types.Model

	mu         sync.Mutex
	seenTenant uint64
}

func (s *guardedModelService) GetChatModel(
	ctx context.Context, modelID string,
) (chat.Chat, error) {
	model := s.models[modelID]
	if model == nil {
		return nil, fmt.Errorf("model %s not found", modelID)
	}
	if err := s.control.GuardChatModel(ctx, model); err != nil {
		return nil, err
	}
	tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
	if tenantID != model.TenantID {
		return nil, fmt.Errorf("tenant context = %d, want %d", tenantID, model.TenantID)
	}
	s.mu.Lock()
	s.seenTenant = tenantID
	s.mu.Unlock()
	return &staticChat{id: model.ID, name: model.Name}, nil
}

func newDerivativeServiceTest(
	t *testing.T,
) (*gorm.DB, *Service, *derivativeSettingsStub, *modelReferenceStub, *agentReferenceStub) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "-") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Model{}, &types.KnowledgeBase{}))

	settings := &derivativeSettingsStub{}
	kbReferences := &modelReferenceStub{}
	agentReferences := &agentReferenceStub{}
	service := NewService(
		db, nil, settings, nil, kbReferences, agentReferences,
	)
	require.NoError(t, service.Migrate(context.Background()))
	return db, service, settings, kbReferences, agentReferences
}

func remoteChatModel(id string, tenantID uint64, baseURL string) *types.Model {
	now := time.Now().UTC()
	return &types.Model{
		ID: id, TenantID: tenantID, Name: id, DisplayName: id,
		Type: types.ModelTypeKnowledgeQA, Source: types.ModelSourceRemote,
		Status:        types.ModelStatusActive,
		WorkloadScope: types.ModelWorkloadInteractive,
		Parameters: types.ModelParameters{
			BaseURL: baseURL, Provider: "generic",
		},
		CreatedAt: now, UpdatedAt: now,
	}
}

func remoteDerivativeModel(id string, tenantID uint64, baseURL string) *types.Model {
	model := remoteChatModel(id, tenantID, baseURL)
	model.WorkloadScope = types.ModelWorkloadDerivativeOnly
	return model
}

func appErrorStatus(err error) int {
	appError, ok := apperrors.IsAppError(err)
	if !ok {
		return 0
	}
	return appError.HTTPCode
}

func TestDerivativeControlAdminCandidatesFollowActiveTenant(t *testing.T) {
	db, service, _, _, _ := newDerivativeServiceTest(t)
	current := remoteDerivativeModel("current-tenant-model", 10001, "http://current-derivative:4000/v1")
	other := remoteDerivativeModel("other-tenant-model", 10002, "http://other-derivative:4000/v1")
	interactive := remoteChatModel("interactive-model", 10001, "http://interactive:4000/v1")
	require.NoError(t, db.Create(current).Error)
	require.NoError(t, db.Create(other).Error)
	require.NoError(t, db.Create(interactive).Error)

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(10001))
	config, err := service.AdminConfig(ctx)
	require.NoError(t, err)
	require.Len(t, config.Candidates, 1)
	require.Equal(t, current.ID, config.Candidates[0].ID)
	require.Equal(t, current.TenantID, config.Candidates[0].TenantID)
	require.True(t, config.Candidates[0].Eligible)
}

func TestDerivativeControlPublishesIsolatedModelAndNeverFallsBack(t *testing.T) {
	db, service, settings, _, _ := newDerivativeServiceTest(t)
	ctx := context.WithValue(context.Background(), types.UserIDContextKey, "system-admin")
	ctx = context.WithValue(ctx, types.SystemAdminContextKey, true)

	status, err := service.Status(ctx)
	require.NoError(t, err)
	require.False(t, status.Configured)
	require.Equal(t, DefaultTPM, status.TPM)
	require.Empty(t, status.Models)

	interactive := remoteChatModel("chat-model", 10001, "http://interactive-api:4000/v1")
	conflicting := remoteDerivativeModel("conflicting-model", 10002, "http://interactive-api:4000/openai")
	dedicated := remoteDerivativeModel("derivative-model", 10002, "http://derivative-api:4000/v1")
	require.NoError(t, db.Create(interactive).Error)
	require.NoError(t, db.Create(conflicting).Error)
	require.NoError(t, db.Create(dedicated).Error)

	err = service.Publish(ctx, conflicting.ID, conflicting.TenantID)
	require.Error(t, err)
	require.Equal(t, 400, appErrorStatus(err))
	require.Contains(t, err.Error(), "物理隔离")

	err = service.Publish(ctx, interactive.ID, interactive.TenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "“衍生任务”分类")

	require.NoError(t, service.Publish(ctx, dedicated.ID, dedicated.TenantID))
	require.NoError(t, db.First(dedicated, "id = ?", dedicated.ID).Error)
	require.Equal(t, types.ModelWorkloadDerivativeOnly, dedicated.WorkloadScope)

	status, err = service.Status(ctx)
	require.NoError(t, err)
	require.True(t, status.Configured)
	require.Equal(t, dedicated.ID, status.DefaultModelID)
	require.Len(t, status.Models, 1)
	require.True(t, status.Models[0].IsDefault)

	modelService := &guardedModelService{
		control: service,
		models: map[string]*types.Model{
			interactive.ID: interactive,
			dedicated.ID:   dedicated,
		},
	}
	resolved, err := service.ResolveChatModel(ctx, modelService, "")
	require.NoError(t, err)
	require.Equal(t, dedicated.ID, resolved.GetModelID())
	modelService.mu.Lock()
	require.Equal(t, dedicated.TenantID, modelService.seenTenant)
	modelService.mu.Unlock()

	_, err = modelService.GetChatModel(
		context.WithValue(context.Background(), types.TenantIDContextKey, dedicated.TenantID),
		dedicated.ID,
	)
	require.Error(t, err)
	require.Equal(t, 400, appErrorStatus(err))
	require.Contains(t, err.Error(), "不能用于对话")

	_, err = service.ResolveChatModel(ctx, modelService, interactive.ID)
	var deferred *DeferredError
	require.ErrorAs(t, err, &deferred)
	require.Contains(t, err.Error(), "not published")

	require.NoError(t, service.ValidateKnowledgeBase(ctx, &types.KnowledgeBase{
		TenantID: 10001, SummaryModelID: interactive.ID,
		DerivativeModelID: dedicated.ID,
	}))
	err = service.ValidateKnowledgeBase(ctx, &types.KnowledgeBase{
		TenantID: 10001, SummaryModelID: interactive.ID,
		DerivativeModelID: interactive.ID,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "已下发")
	err = service.ValidateKnowledgeBase(ctx, &types.KnowledgeBase{
		TenantID: 10002, SummaryModelID: dedicated.ID,
		DerivativeModelID: dedicated.ID,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "不能作为知识库对话模型")

	conflictAtRuntime := remoteChatModel(
		"late-interactive-conflict", 10003, "http://derivative-api:4000/another-path",
	)
	err = service.GuardChatModel(ctx, conflictAtRuntime)
	require.Error(t, err)
	require.Contains(t, err.Error(), "端点已被衍生任务模型池独占")

	value, err := service.UpdateTPM(ctx, 12_345)
	require.NoError(t, err)
	require.EqualValues(t, 12_345, value)
	settings.mu.RLock()
	require.EqualValues(t, 12_345, settings.tpm)
	settings.mu.RUnlock()
}

func TestDerivativeControlRequiresDedicatedScopeAndGuardsRevocation(t *testing.T) {
	db, service, _, _, _ := newDerivativeServiceTest(t)
	ctx := context.WithValue(context.Background(), types.UserIDContextKey, "system-admin")
	ctx = context.WithValue(ctx, types.SystemAdminContextKey, true)
	interactive := remoteChatModel("interactive-model", 10001, "https://interactive.example/v1")
	model := remoteDerivativeModel("derivative-model", 10001, "https://derivative.example/v1")
	require.NoError(t, db.Create(interactive).Error)
	require.NoError(t, db.Create(model).Error)

	err := service.Publish(ctx, interactive.ID, interactive.TenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "“衍生任务”分类")

	require.NoError(t, service.Publish(ctx, model.ID, model.TenantID))

	// An idempotent publish must still revalidate live eligibility.
	require.NoError(t, db.Model(&types.Model{}).
		Where("id = ?", model.ID).
		Update("status", types.ModelStatusDownloadFailed).Error)
	err = service.Publish(ctx, model.ID, model.TenantID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "active")
	err = service.SetDefault(ctx, model.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "active")
	require.NoError(t, db.Model(&types.Model{}).
		Where("id = ?", model.ID).
		Update("status", types.ModelStatusActive).Error)

	kb := &types.KnowledgeBase{
		ID: "kb-1", Name: "kb", Type: types.KnowledgeBaseTypeDocument,
		TenantID: model.TenantID, DerivativeModelID: model.ID,
	}
	require.NoError(t, db.Create(kb).Error)
	err = service.Unpublish(ctx, model.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "仍被 1 个知识库使用")
	require.NoError(t, db.Delete(kb).Error)
	require.NoError(t, service.Unpublish(ctx, model.ID))

	var refreshed types.Model
	require.NoError(t, db.First(&refreshed, "id = ?", model.ID).Error)
	require.Equal(t, types.ModelWorkloadDerivativeOnly, refreshed.WorkloadScope)
	status, err := service.Status(ctx)
	require.NoError(t, err)
	require.False(t, status.Configured)
	require.Empty(t, status.DefaultModelID)

	modelService := &guardedModelService{
		control: service,
		models:  map[string]*types.Model{model.ID: &refreshed},
	}
	_, err = service.ResolveChatModel(ctx, modelService, "")
	var deferred *DeferredError
	require.ErrorAs(t, err, &deferred)
	require.Contains(t, err.Error(), "no derivative model is configured")
}

func TestDerivativeControlMutationPolicyFailsClosed(t *testing.T) {
	db, service, _, _, _ := newDerivativeServiceTest(t)
	dedicated := remoteChatModel("dedicated", 10001, "https://derive.example/v1")
	dedicated.WorkloadScope = types.ModelWorkloadDerivativeOnly

	err := service.GuardModelMutation(
		context.Background(), "update", dedicated, dedicated,
	)
	require.Error(t, err)
	require.Equal(t, 403, appErrorStatus(err))

	adminCtx := context.WithValue(context.Background(), types.SystemAdminContextKey, true)
	err = service.GuardModelMutation(
		adminCtx, appservice.ModelMutationCreate, nil, dedicated,
	)
	require.NoError(t, err)

	interactive := remoteChatModel("chat", 10001, "https://derive.example/other-path")
	require.NoError(t, db.Create(interactive).Error)
	err = service.GuardModelMutation(
		adminCtx, appservice.ModelMutationCreate, nil, dedicated,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "物理隔离")
}
