package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type summaryFinalizerFailureRepo struct {
	interfaces.KnowledgeRepository
	knowledge     *types.Knowledge
	finalizeErr   error
	casValues     map[string]interface{}
	finalizeCalls int
}

func (r *summaryFinalizerFailureRepo) GetKnowledgeByID(
	context.Context,
	uint64,
	string,
) (*types.Knowledge, error) {
	copy := *r.knowledge
	return &copy, nil
}

func (r *summaryFinalizerFailureRepo) FinalizeSubtaskGenerationItem(
	context.Context,
	uint64,
	string,
	string,
	string,
	string,
) (int, bool, error) {
	r.finalizeCalls++
	return 1, false, r.finalizeErr
}

func (r *summaryFinalizerFailureRepo) FinalizeSubtaskGenerationItemOutcome(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, knowledgeBaseID, generation, itemID, _, _ string,
) (int, bool, error) {
	return r.FinalizeSubtaskGenerationItem(
		ctx, tenantID, knowledgeID, knowledgeBaseID, generation, itemID,
	)
}

func (r *summaryFinalizerFailureRepo) CompareAndSwapKnowledgeProcessingGeneration(
	_ context.Context,
	_ uint64,
	_ string,
	_ string,
	_ string,
	_ []string,
	values map[string]interface{},
) (bool, error) {
	r.casValues = values
	return true, nil
}

type summaryFinalizerFailureKBService struct {
	interfaces.KnowledgeBaseService
	kb *types.KnowledgeBase
}

func (s *summaryFinalizerFailureKBService) GetKnowledgeBaseByID(
	context.Context,
	string,
) (*types.KnowledgeBase, error) {
	return s.kb, nil
}

func TestGenerationArtifactIDsAreStableAndGenerationScoped(t *testing.T) {
	summaryID := summaryGenerationChunkID("knowledge-1", "generation-1")
	require.NotEmpty(t, summaryID)
	assert.Equal(t, summaryID, summaryGenerationChunkID("knowledge-1", "generation-1"))
	assert.NotEqual(t, summaryID, summaryGenerationChunkID("knowledge-1", "generation-2"))

	questionID := questionGenerationID("generation-1", "chunk-1", 0)
	require.NotEmpty(t, questionID)
	assert.LessOrEqual(t, len(questionVectorSourceID(questionID)), 64,
		"generated-question vector source IDs must fit embeddings.source_id")
	assert.Equal(t, questionID, questionVectorSourceID(questionID))
	assert.Equal(t, questionID, questionGenerationID("generation-1", "chunk-1", 0))
	assert.NotEqual(t, questionID, questionGenerationID("generation-2", "chunk-1", 0))
	assert.NotEqual(t, questionID, questionGenerationID("generation-1", "chunk-1", 1))
	assert.NotEqual(t, questionID, questionGenerationID("generation-1", "chunk-2", 0))
}

func TestSummaryHandlerRetriesWhenDurableSubtaskFinalizerFails(t *testing.T) {
	dbErr := errors.New("completion ledger unavailable")
	repo := &summaryFinalizerFailureRepo{
		knowledge: &types.Knowledge{
			ID:                   "knowledge-1",
			TenantID:             7,
			KnowledgeBaseID:      "kb-1",
			ParseStatus:          types.ParseStatusFinalizing,
			ProcessingGeneration: "generation-1",
		},
		finalizeErr: dbErr,
	}
	service := &knowledgeService{
		repo: repo,
		kbService: &summaryFinalizerFailureKBService{kb: &types.KnowledgeBase{
			ID:             "kb-1",
			SummaryModelID: "", // terminal no-op body; the deferred drain still must persist
		}},
	}
	payload, err := json.Marshal(types.SummaryGenerationPayload{
		TenantID:             7,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "generation-1",
	})
	require.NoError(t, err)

	err = service.ProcessSummaryGeneration(
		context.Background(),
		asynq.NewTask(types.TypeSummaryGeneration, payload),
	)
	require.ErrorIs(t, err, dbErr, "the task must not ACK before its durable slot is drained")
}

func TestProviderOutageNeverDrainsGenerationItemAtHistoricalFinalAttempt(t *testing.T) {
	repo := &summaryFinalizerFailureRepo{}
	providerErr := &modeladmission.ProviderUnavailableError{
		Kind:       modeladmission.KindChat,
		RetryAfter: time.Minute,
		Cause:      errors.New("upstream 503"),
	}
	err := finalizeSubtaskDetached(
		context.Background(),
		repo,
		7,
		"knowledge-1",
		"kb-1",
		"generation-1",
		"summary",
		providerErr,
		providerErr,
		false,
		false,
		true,
	)
	require.NoError(t, err)
	assert.Zero(t, repo.finalizeCalls)
}

func TestShutdownCancellationNeverDrainsGenerationItemAtHistoricalFinalAttempt(t *testing.T) {
	repo := &summaryFinalizerFailureRepo{}
	err := finalizeSubtaskDetached(
		context.Background(),
		repo,
		7,
		"knowledge-1",
		"kb-1",
		"generation-1",
		"graph_chunk[0]",
		context.Canceled,
		context.Canceled,
		false,
		false,
		true,
	)
	require.NoError(t, err)
	assert.Zero(t, repo.finalizeCalls)
}

func TestSummaryWithoutModelClearsPendingStatusInExactGeneration(t *testing.T) {
	repo := &summaryFinalizerFailureRepo{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             7,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusFinalizing,
		ProcessingGeneration: "generation-1",
	}}
	service := &knowledgeService{
		repo: repo,
		kbService: &summaryFinalizerFailureKBService{kb: &types.KnowledgeBase{
			ID: "kb-1",
		}},
	}
	payload, err := json.Marshal(types.SummaryGenerationPayload{
		TenantID:             7,
		KnowledgeID:          "knowledge-1",
		KnowledgeBaseID:      "kb-1",
		ProcessingGeneration: "generation-1",
	})
	require.NoError(t, err)
	require.NoError(t, service.ProcessSummaryGeneration(
		context.Background(), asynq.NewTask(types.TypeSummaryGeneration, payload),
	))
	require.NotNil(t, repo.casValues)
	assert.Equal(t, types.SummaryStatusNone, repo.casValues["summary_status"])
}
