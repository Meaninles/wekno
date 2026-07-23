package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type chatModelLookupRepoStub struct {
	interfaces.ModelRepository
	model *types.Model
	err   error
}

func (r *chatModelLookupRepoStub) GetByID(context.Context, uint64, string) (*types.Model, error) {
	return r.model, r.err
}

func TestGetChatModelClassifiesConstructionErrorAsConfiguration(t *testing.T) {
	repo := &chatModelLookupRepoStub{model: &types.Model{
		ID:       "bad-chat-model",
		TenantID: 42,
		Name:     "bad-chat-model",
		Source:   types.ModelSource("unsupported-source"),
	}}
	svc := &modelService{repo: repo}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(42))

	model, err := svc.GetChatModel(ctx, "bad-chat-model")
	if model != nil {
		t.Fatalf("GetChatModel() model = %T, want nil", model)
	}
	if !errors.Is(err, ErrChatModelConfiguration) {
		t.Fatalf("GetChatModel() error = %v, want ErrChatModelConfiguration", err)
	}
}

func TestGetChatModelLeavesRepositoryErrorTransient(t *testing.T) {
	repositoryErr := errors.New("model repository connection reset")
	svc := &modelService{repo: &chatModelLookupRepoStub{err: repositoryErr}}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(42))

	model, err := svc.GetChatModel(ctx, "chat-model")
	if model != nil {
		t.Fatalf("GetChatModel() model = %T, want nil", model)
	}
	if !errors.Is(err, repositoryErr) {
		t.Fatalf("GetChatModel() error = %v, want repository error", err)
	}
	if errors.Is(err, ErrChatModelConfiguration) {
		t.Fatalf("repository error was misclassified as permanent configuration error: %v", err)
	}
}
