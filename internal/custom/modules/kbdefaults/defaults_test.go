package kbdefaults

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type fakeModelLister struct {
	models []*types.Model
	err    error
}

func (f fakeModelLister) ListModels(context.Context) ([]*types.Model, error) {
	return f.models, f.err
}

func testModels() []*types.Model {
	return []*types.Model{
		{ID: "chat-derivative", Name: "derivative", Type: types.ModelTypeKnowledgeQA, WorkloadScope: types.ModelWorkloadDerivativeOnly, IsDefault: true, Status: types.ModelStatusActive},
		{ID: "chat-default", Name: "chat", Type: types.ModelTypeKnowledgeQA, IsDefault: true, Status: types.ModelStatusActive},
		{ID: "embedding-default", Name: "embedding", Type: types.ModelTypeEmbedding, IsDefault: true, Status: types.ModelStatusActive},
		{ID: "vlm-default", Name: "vision", Type: types.ModelTypeVLLM, IsDefault: true, Status: types.ModelStatusActive},
		{ID: "asr-omni", Name: "Qwen2.5-Omni-7B-local", DisplayName: "Qwen2.5-Omni-7B-local（语音转写）", Type: types.ModelTypeASR, Status: types.ModelStatusActive},
	}
}

func TestApplyFillsDocumentDefaults(t *testing.T) {
	kb := &types.KnowledgeBase{Type: types.KnowledgeBaseTypeDocument}

	err := NewService(fakeModelLister{models: testModels()}).Apply(context.Background(), kb)

	require.NoError(t, err)
	require.Equal(t, "chat-default", kb.SummaryModelID)
	require.Equal(t, "embedding-default", kb.EmbeddingModelID)
	require.Equal(t, "vlm-default", kb.VLMConfig.ModelID)
	require.True(t, kb.VLMConfig.Enabled)
	require.Equal(t, "asr-omni", kb.ASRConfig.ModelID)
	require.True(t, kb.ASRConfig.Enabled)
	require.Equal(t, 512, kb.ChunkingConfig.ChunkSize)
	require.Equal(t, 80, kb.ChunkingConfig.ChunkOverlap)
	require.True(t, kb.ChunkingConfig.EnableParentChild)
	require.Equal(t, "auto", kb.ChunkingConfig.Strategy)
}

func TestApplyPreservesExplicitConfiguration(t *testing.T) {
	kb := &types.KnowledgeBase{
		Type:              types.KnowledgeBaseTypeDocument,
		SummaryModelID:    "caller-chat",
		EmbeddingModelID:  "caller-embedding",
		VLMConfig:         types.VLMConfig{Enabled: false},
		VLMConfigProvided: true,
		ASRConfig:         types.ASRConfig{Enabled: false},
		ASRConfigProvided: true,
		ChunkingConfig:    types.ChunkingConfig{ChunkSize: 1024},
	}

	err := NewService(fakeModelLister{models: testModels()}).Apply(context.Background(), kb)

	require.NoError(t, err)
	require.Equal(t, "caller-chat", kb.SummaryModelID)
	require.Equal(t, "caller-embedding", kb.EmbeddingModelID)
	require.False(t, kb.VLMConfig.Enabled)
	require.False(t, kb.ASRConfig.Enabled)
	require.Equal(t, 1024, kb.ChunkingConfig.ChunkSize)
	require.Equal(t, 0, kb.ChunkingConfig.ChunkOverlap)
}

func TestApplyFailsWhenRequiredOmniModelIsMissing(t *testing.T) {
	kb := &types.KnowledgeBase{Type: types.KnowledgeBaseTypeDocument}

	err := NewService(fakeModelLister{models: testModels()[:4]}).Apply(context.Background(), kb)

	require.ErrorContains(t, err, "qwen2.5-omni-7b")
}

func TestNormalizeModelNameSupportsLocalAliases(t *testing.T) {
	require.Equal(t, defaultASRModelName, normalizeModelName("/models/Qwen2.5_Omni_7B-local:latest"))
}
