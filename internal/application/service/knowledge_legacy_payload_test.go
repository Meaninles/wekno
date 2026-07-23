package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type legacyPayloadRepairRepo struct {
	interfaces.KnowledgeRepository
	knowledge *types.Knowledge
	repairs   int
}

func (r *legacyPayloadRepairRepo) GetKnowledgeByID(
	context.Context, uint64, string,
) (*types.Knowledge, error) {
	copyKnowledge := *r.knowledge
	return &copyKnowledge, nil
}

func (r *legacyPayloadRepairRepo) RepairLegacyProcessing(
	_ context.Context,
	_ uint64,
	_ string,
	_ string,
	expectedStatus string,
	_ bool,
	repairGeneration string,
	_ bool,
	message string,
	_ time.Time,
) (bool, error) {
	if r.knowledge.ParseStatus != expectedStatus || r.knowledge.ProcessingGeneration != "" ||
		r.knowledge.ProcessingOwner != "" {
		return false, nil
	}
	r.repairs++
	r.knowledge.ParseStatus = types.ParseStatusFailed
	r.knowledge.ProcessingGeneration = repairGeneration
	r.knowledge.ErrorMessage = message
	return true, nil
}

func legacyOwnershipTask(t *testing.T, taskType string, payload any) *asynq.Task {
	t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	return asynq.NewTask(taskType, encoded)
}

func TestLegacyGenerationlessPayloadsNeverAcknowledgeActiveRow(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *legacyPayloadRepairRepo) error
	}{
		{
			name: "document",
			run: func(ctx context.Context, repo *legacyPayloadRepairRepo) error {
				service := &knowledgeService{repo: repo}
				return service.ProcessDocument(ctx, legacyOwnershipTask(t, types.TypeDocumentProcess,
					types.DocumentProcessPayload{TenantID: 7, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1"}))
			},
		},
		{
			name: "manual",
			run: func(ctx context.Context, repo *legacyPayloadRepairRepo) error {
				service := &knowledgeService{repo: repo}
				return service.ProcessManualUpdate(ctx, legacyOwnershipTask(t, types.TypeManualProcess,
					types.ManualProcessPayload{TenantID: 7, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1"}))
			},
		},
		{
			name: "summary",
			run: func(ctx context.Context, repo *legacyPayloadRepairRepo) error {
				service := &knowledgeService{repo: repo}
				return service.ProcessSummaryGeneration(ctx, legacyOwnershipTask(t, types.TypeSummaryGeneration,
					types.SummaryGenerationPayload{TenantID: 7, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1"}))
			},
		},
		{
			name: "question",
			run: func(ctx context.Context, repo *legacyPayloadRepairRepo) error {
				service := &knowledgeService{repo: repo}
				return service.ProcessQuestionGeneration(ctx, legacyOwnershipTask(t, types.TypeQuestionGeneration,
					types.QuestionGenerationPayload{TenantID: 7, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1"}))
			},
		},
		{
			name: "postprocess",
			run: func(ctx context.Context, repo *legacyPayloadRepairRepo) error {
				service := &KnowledgePostProcessService{knowledgeRepo: repo}
				return service.Handle(ctx, legacyOwnershipTask(t, types.TypeKnowledgePostProcess,
					types.KnowledgePostProcessPayload{TenantID: 7, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1"}))
			},
		},
		{
			name: "image multimodal",
			run: func(ctx context.Context, repo *legacyPayloadRepairRepo) error {
				service := &ImageMultimodalService{knowledgeRepo: repo}
				return service.Handle(ctx, legacyOwnershipTask(t, types.TypeImageMultimodal,
					types.ImageMultimodalPayload{TenantID: 7, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1", ChunkID: "chunk-1"}))
			},
		},
		{
			name: "data table",
			run: func(ctx context.Context, repo *legacyPayloadRepairRepo) error {
				service := &DataTableSummaryService{knowledgeRepo: repo}
				return service.Handle(ctx, legacyOwnershipTask(t, types.TypeDataTableSummary,
					types.DataTableSummaryPayload{TenantID: 7, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1"}))
			},
		},
		{
			name: "graph extraction",
			run: func(ctx context.Context, repo *legacyPayloadRepairRepo) error {
				service := &ChunkExtractService{knowledgeRepo: repo}
				return service.Handle(ctx, legacyOwnershipTask(t, types.TypeChunkExtract,
					types.ExtractChunkPayload{TenantID: 7, ChunkID: "chunk-1", KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1"}))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &legacyPayloadRepairRepo{knowledge: &types.Knowledge{
				ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
				ParseStatus: types.ParseStatusPending,
			}}
			err := tc.run(context.Background(), repo)
			require.ErrorIs(t, err, processownership.ErrLegacyTaskRepaired)
			assert.Equal(t, 1, repo.repairs)
			assert.Equal(t, types.ParseStatusFailed, repo.knowledge.ParseStatus)
			assert.NotEmpty(t, repo.knowledge.ProcessingGeneration)
		})
	}
}

func TestLegacyPayloadCannotMutateNewGeneration(t *testing.T) {
	repo := &legacyPayloadRepairRepo{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: "generation-new", ProcessingOwner: "owner-new",
	}}
	service := &knowledgeService{repo: repo}
	err := service.ProcessDocument(context.Background(), legacyOwnershipTask(
		t, types.TypeDocumentProcess,
		types.DocumentProcessPayload{TenantID: 7, KnowledgeID: "knowledge-1", KnowledgeBaseID: "kb-1"},
	))
	require.NoError(t, err)
	assert.Zero(t, repo.repairs)
	assert.Equal(t, types.ParseStatusProcessing, repo.knowledge.ParseStatus)
	assert.Equal(t, "generation-new", repo.knowledge.ProcessingGeneration)
	assert.Equal(t, "owner-new", repo.knowledge.ProcessingOwner)
}

func TestMalformedOwnershipPayloadsReturnErrors(t *testing.T) {
	service := &knowledgeService{}
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "document", run: func() error {
			return service.ProcessDocument(context.Background(), asynq.NewTask(types.TypeDocumentProcess, []byte("{")))
		}},
		{name: "manual", run: func() error {
			return service.ProcessManualUpdate(context.Background(), asynq.NewTask(types.TypeManualProcess, []byte("{")))
		}},
		{name: "summary", run: func() error {
			return service.ProcessSummaryGeneration(context.Background(), asynq.NewTask(types.TypeSummaryGeneration, []byte("{")))
		}},
		{name: "question", run: func() error {
			return service.ProcessQuestionGeneration(context.Background(), asynq.NewTask(types.TypeQuestionGeneration, []byte("{")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, tc.run())
		})
	}
}
