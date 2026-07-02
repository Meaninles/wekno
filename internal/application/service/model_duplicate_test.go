package service

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

type fakeModelRepository struct {
	models  []*types.Model
	created []*types.Model
}

func (r *fakeModelRepository) Create(_ context.Context, model *types.Model) error {
	if model.ID == "" {
		model.ID = "created-model"
	}
	r.created = append(r.created, model)
	r.models = append(r.models, model)
	return nil
}

func (r *fakeModelRepository) GetByID(_ context.Context, tenantID uint64, id string) (*types.Model, error) {
	for _, model := range r.models {
		if model.ID == id && (model.TenantID == tenantID || model.IsBuiltin) {
			return model, nil
		}
	}
	return nil, nil
}

func (r *fakeModelRepository) List(
	_ context.Context,
	tenantID uint64,
	modelType types.ModelType,
	source types.ModelSource,
) ([]*types.Model, error) {
	var result []*types.Model
	for _, model := range r.models {
		if model.TenantID != tenantID && !model.IsBuiltin {
			continue
		}
		if modelType != "" && model.Type != modelType {
			continue
		}
		if source != "" && model.Source != source {
			continue
		}
		result = append(result, model)
	}
	return result, nil
}

func (r *fakeModelRepository) Update(_ context.Context, _ *types.Model) error {
	return nil
}

func (r *fakeModelRepository) Delete(_ context.Context, _ uint64, _ string) error {
	return nil
}

func (r *fakeModelRepository) ClearDefaultByType(
	_ context.Context,
	_ uint,
	_ types.ModelType,
	_ string,
) error {
	return nil
}

func TestCreateModelRejectsDuplicateModelConfig(t *testing.T) {
	existing := &types.Model{
		ID:       "existing-model",
		TenantID: 42,
		Name:     "GLM-4.7",
		Type:     types.ModelTypeKnowledgeQA,
		Source:   types.ModelSourceRemote,
		Parameters: types.ModelParameters{
			BaseURL:  "https://open.bigmodel.cn/api/coding/paas/v4/",
			Provider: "zhipu",
		},
		Status: types.ModelStatusActive,
	}
	repo := &fakeModelRepository{models: []*types.Model{existing}}
	svc := &modelService{repo: repo}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(42))

	candidate := &types.Model{
		Name:   " glm-4.7 ",
		Type:   types.ModelTypeKnowledgeQA,
		Source: types.ModelSourceRemote,
		Parameters: types.ModelParameters{
			BaseURL:  "https://open.bigmodel.cn/api/coding/paas/v4",
			Provider: "ZHIPU",
		},
	}

	err := svc.CreateModel(ctx, candidate)
	if !stderrors.Is(err, ErrModelAlreadyExists) {
		t.Fatalf("expected ErrModelAlreadyExists, got %v", err)
	}
	if len(repo.created) != 0 {
		t.Fatalf("duplicate model should not be created, created=%d", len(repo.created))
	}
	if candidate.ID != existing.ID {
		t.Fatalf("candidate should be populated with existing model id, got %q", candidate.ID)
	}
}

func TestCreateModelAllowsDistinctDisplayNameAndFillsTenant(t *testing.T) {
	repo := &fakeModelRepository{models: []*types.Model{
		{
			ID:       "existing-model",
			TenantID: 42,
			Name:     "GLM-4.7",
			Type:     types.ModelTypeKnowledgeQA,
			Source:   types.ModelSourceRemote,
			Parameters: types.ModelParameters{
				BaseURL:  "https://open.bigmodel.cn/api/coding/paas/v4",
				Provider: "zhipu",
			},
			Status: types.ModelStatusActive,
		},
	}}
	svc := &modelService{repo: repo}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(42))

	candidate := &types.Model{
		Name:        "GLM-4.7",
		DisplayName: "GLM backup",
		Type:        types.ModelTypeKnowledgeQA,
		Source:      types.ModelSourceRemote,
		Parameters: types.ModelParameters{
			BaseURL:  "https://open.bigmodel.cn/api/coding/paas/v4",
			Provider: "zhipu",
		},
	}

	if err := svc.CreateModel(ctx, candidate); err != nil {
		t.Fatalf("expected distinct display name to create, got %v", err)
	}
	if len(repo.created) != 1 {
		t.Fatalf("expected one created model, got %d", len(repo.created))
	}
	if candidate.TenantID != 42 {
		t.Fatalf("expected tenant id to be filled from context, got %d", candidate.TenantID)
	}
	if candidate.Status != types.ModelStatusActive {
		t.Fatalf("expected remote model to become active, got %q", candidate.Status)
	}
}
