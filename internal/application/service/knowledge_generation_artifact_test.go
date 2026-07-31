package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/derivativequeue"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/models/chat"
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

type emptySummaryChunkService struct {
	interfaces.ChunkService
}

func (*emptySummaryChunkService) ListChunksByKnowledgeID(
	context.Context,
	string,
) ([]*types.Chunk, error) {
	return nil, nil
}

type oneSummaryChunkService struct {
	interfaces.ChunkService
}

func (*oneSummaryChunkService) ListChunksByKnowledgeID(
	context.Context,
	string,
) ([]*types.Chunk, error) {
	return []*types.Chunk{{
		ID: "chunk-1", ChunkType: types.ChunkTypeText, Content: "制度正文",
	}}, nil
}

type derivativeWaitTestError struct{}

func (derivativeWaitTestError) Error() string                  { return "derivative model is not configured" }
func (derivativeWaitTestError) ModelWorkDeferred() bool        { return true }
func (derivativeWaitTestError) ModelRetryAfter() time.Duration { return time.Minute }

func installDerivativeWaitResolver(t *testing.T, requestedModelID *string) {
	t.Helper()
	derivativeModelHooks.Lock()
	previous := derivativeModelHooks.resolver
	derivativeModelHooks.resolver = func(
		_ context.Context,
		_ interfaces.ModelService,
		modelID string,
	) (chat.Chat, error) {
		if requestedModelID != nil {
			*requestedModelID = modelID
		}
		return nil, derivativeWaitTestError{}
	}
	derivativeModelHooks.Unlock()
	t.Cleanup(func() {
		derivativeModelHooks.Lock()
		derivativeModelHooks.resolver = previous
		derivativeModelHooks.Unlock()
	})
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
		repo:         repo,
		chunkService: &emptySummaryChunkService{},
		kbService: &summaryFinalizerFailureKBService{kb: &types.KnowledgeBase{
			ID:             "kb-1",
			SummaryModelID: "", // No chunks: terminal no-op body; the deferred drain still must persist.
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

func TestDurableDerivativeHandlerLeavesGenerationItemForPlanFinalizer(t *testing.T) {
	repo := &summaryFinalizerFailureRepo{}
	ctx := derivativequeue.WithExecution(
		context.Background(),
		&derivativequeue.Repository{},
		&derivativequeue.WorkItem{ID: "work-1", LeaseToken: "lease-1"},
	)
	err := finalizeSubtaskDetached(
		ctx,
		repo,
		7,
		"knowledge-1",
		"kb-1",
		"generation-1",
		"summary",
		nil,
		nil,
		false,
		false,
		true,
	)
	require.NoError(t, err)
	assert.Zero(t, repo.finalizeCalls)
}

func TestSummaryWithoutDerivativeModelWaitsWithoutFallingBack(t *testing.T) {
	requestedModelID := ""
	installDerivativeWaitResolver(t, &requestedModelID)
	repo := &summaryFinalizerFailureRepo{knowledge: &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             7,
		KnowledgeBaseID:      "kb-1",
		ParseStatus:          types.ParseStatusFinalizing,
		ProcessingGeneration: "generation-1",
	}}
	service := &knowledgeService{
		repo:         repo,
		chunkService: &oneSummaryChunkService{},
		kbService: &summaryFinalizerFailureKBService{kb: &types.KnowledgeBase{
			ID:             "kb-1",
			SummaryModelID: "conversation-model-must-not-be-used",
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
		context.Background(), asynq.NewTask(types.TypeSummaryGeneration, payload),
	)
	require.Error(t, err)
	assert.True(t, modeladmission.IsModelWorkDeferred(err))
	assert.Empty(t, requestedModelID, "interactive SummaryModelID must never be used as a fallback")
	require.NotNil(t, repo.casValues)
	assert.Equal(t, types.SummaryStatusProcessing, repo.casValues["summary_status"])
	assert.Zero(t, repo.finalizeCalls, "a waiting derivative task must remain durable")
}
