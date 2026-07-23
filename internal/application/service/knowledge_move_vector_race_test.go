package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type moveRaceKnowledgeRepository struct {
	interfaces.KnowledgeRepository
	current              *types.Knowledge
	finalizeCalls        int
	rollbackStateCalls   int
	recoveryMarkerCalls  int
	moveScopeCalls       int
	restoredTagIDBatches [][]string
}

func (r *moveRaceKnowledgeRepository) WithActiveKnowledgeMoveScope(
	_ context.Context,
	_ uint64,
	_ string,
	_ string,
	work func() error,
) error {
	r.moveScopeCalls++
	return work()
}

func (r *moveRaceKnowledgeRepository) GetKnowledgeTags(
	_ context.Context,
	_ []string,
) (map[string][]*types.KnowledgeTag, error) {
	return map[string][]*types.KnowledgeTag{
		r.current.ID: {{ID: "tag-source"}},
	}, nil
}

func (r *moveRaceKnowledgeRepository) DeleteKnowledgeTagRelations(context.Context, string) error {
	return nil
}

func (r *moveRaceKnowledgeRepository) SetKnowledgeTags(
	_ context.Context,
	_ string,
	tagIDs []string,
) error {
	r.restoredTagIDBatches = append(r.restoredTagIDBatches, append([]string(nil), tagIDs...))
	return nil
}

func (r *moveRaceKnowledgeRepository) CompareAndSwapKnowledgeState(
	_ context.Context,
	_ uint64,
	_ string,
	_ string,
	_ string,
	values map[string]interface{},
) (bool, error) {
	if _, finalizing := values["knowledge_base_id"]; finalizing {
		r.finalizeCalls++
		return false, nil // concurrent delete wins before the final row CAS
	}
	if _, rollingBack := values["parse_status"]; rollingBack {
		r.rollbackStateCalls++
	} else if marker, markingRecovery := values["error_message"]; markingRecovery {
		r.recoveryMarkerCalls++
		r.current.ErrorMessage, _ = marker.(string)
	}
	return true, nil
}

func (r *moveRaceKnowledgeRepository) CompareAndSwapDocumentProcessing(
	_ context.Context,
	_ uint64,
	_ string,
	_ string,
	_ string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
) (bool, error) {
	if r.current.ProcessingGeneration != expectedGeneration || r.current.ProcessingOwner != expectedOwner {
		return false, nil
	}
	if _, rollingBack := values["parse_status"]; rollingBack {
		r.rollbackStateCalls++
		if status, ok := values["parse_status"].(string); ok {
			r.current.ParseStatus = status
		}
	} else if marker, markingRecovery := values["error_message"]; markingRecovery {
		r.recoveryMarkerCalls++
		r.current.ErrorMessage, _ = marker.(string)
	}
	if owner, ok := values["processing_owner"].(string); ok {
		r.current.ProcessingOwner = owner
	}
	return true, nil
}

func (r *moveRaceKnowledgeRepository) FinalizeReuseVectorKnowledgeMove(
	_ context.Context,
	_ uint64,
	_ string,
	_ string,
	_ string,
	_ string,
	_ string,
	_ string,
	_ time.Time,
) (bool, error) {
	r.finalizeCalls++
	return false, nil // concurrent delete wins before the atomic move commit
}

func (r *moveRaceKnowledgeRepository) FinalizeReparseKnowledgeMove(
	_ context.Context,
	_ uint64,
	_ string,
	_ string,
	_ string,
	_ string,
	_ string,
	_ string,
	_ string,
	_ string,
	_ func(*gorm.DB, func(*gorm.DB) error) error,
	_ time.Time,
) (bool, error) {
	return false, nil
}

func (r *moveRaceKnowledgeRepository) GetKnowledgeByID(
	_ context.Context,
	_ uint64,
	_ string,
) (*types.Knowledge, error) {
	copy := *r.current
	return &copy, nil
}

type moveRaceChunkRepository struct {
	interfaces.ChunkRepository
	chunks []*types.Chunk
	moves  []string
}

func (r *moveRaceChunkRepository) ListChunksByKnowledgeID(
	_ context.Context,
	_ uint64,
	_ string,
) ([]*types.Chunk, error) {
	return r.chunks, nil
}

func (r *moveRaceChunkRepository) MoveChunksByKnowledgeID(
	_ context.Context,
	_ uint64,
	_ string,
	targetKBID string,
) error {
	r.moves = append(r.moves, targetKBID)
	return nil
}

type moveRaceEmbedder struct{ embedding.Embedder }

func (*moveRaceEmbedder) GetDimensions() int { return 768 }

type moveRaceModelService struct{ interfaces.ModelService }

func (*moveRaceModelService) GetEmbeddingModel(context.Context, string) (embedding.Embedder, error) {
	return &moveRaceEmbedder{}, nil
}

type moveRaceEngine struct {
	interfaces.RetrieveEngineService
	moves       [][2]string
	failReverse bool
}

func (*moveRaceEngine) EngineType() types.RetrieverEngineType {
	return types.PostgresRetrieverEngineType
}

func (*moveRaceEngine) Support() []types.RetrieverType {
	return []types.RetrieverType{types.VectorRetrieverType}
}

func (*moveRaceEngine) SupportsKnowledgeIndexMove() bool { return true }

func (e *moveRaceEngine) MoveKnowledgeIndices(
	_ context.Context,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	_ string,
	_ []string,
	_ int,
	_ string,
) error {
	e.moves = append(e.moves, [2]string{sourceKnowledgeBaseID, targetKnowledgeBaseID})
	if e.failReverse && sourceKnowledgeBaseID == "kb-target" {
		return errors.New("reverse index move failed")
	}
	return nil
}

type moveRaceRegistry struct {
	interfaces.RetrieveEngineRegistry
	engine interfaces.RetrieveEngineService
}

func (r *moveRaceRegistry) GetByStoreID(string) (interfaces.RetrieveEngineService, error) {
	return r.engine, nil
}

type moveRaceOwnership struct{}

func (moveRaceOwnership) StoreOwnedBy(context.Context, string, uint64) (bool, error) {
	return true, nil
}

func newMoveRaceService(failReverse bool) (
	*knowledgeService,
	*moveRaceKnowledgeRepository,
	*moveRaceChunkRepository,
	*moveRaceEngine,
	*types.Knowledge,
	*types.KnowledgeBase,
	*types.KnowledgeBase,
) {
	knowledge := &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             7,
		KnowledgeBaseID:      "kb-source",
		Type:                 types.KnowledgeTypeManual,
		ParseStatus:          types.ParseStatusProcessing,
		EmbeddingModelID:     "embedding-1",
		ProcessingGeneration: "move-generation-1",
		ProcessingOwner:      knowledgeMoveTestAttemptID,
		ErrorMessage:         knowledgeMoveClaimMarker(knowledgeMoveTestAttemptID, "kb-target"),
	}
	repo := &moveRaceKnowledgeRepository{current: knowledge}
	chunks := &moveRaceChunkRepository{chunks: []*types.Chunk{{ID: "chunk-1"}}}
	engine := &moveRaceEngine{failReverse: failReverse}
	registry := &moveRaceRegistry{engine: engine}
	service := &knowledgeService{
		repo:           repo,
		chunkRepo:      chunks,
		modelService:   &moveRaceModelService{},
		retrieveEngine: registry,
		ownership:      moveRaceOwnership{},
	}
	storeID := "store-1"
	source := &types.KnowledgeBase{
		ID:            "kb-source",
		Type:          types.KnowledgeTypeManual,
		VectorStoreID: &storeID,
	}
	target := &types.KnowledgeBase{
		ID:            "kb-target",
		Type:          types.KnowledgeTypeManual,
		VectorStoreID: &storeID,
	}
	return service, repo, chunks, engine, knowledge, source, target
}

func TestMoveKnowledgeReuseVectorsFinalCASConflictCompensatesTarget(t *testing.T) {
	service, repo, chunks, engine, knowledge, source, target := newMoveRaceService(false)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	err := service.moveKnowledgeReuseVectors(ctx, knowledge, source, target, knowledgeMoveTestAttemptID)
	require.ErrorContains(t, err, "finalize reuse-vectors knowledge move")
	var moveFailure *knowledgeMoveFailure
	require.ErrorAs(t, err, &moveFailure)
	require.True(t, moveFailure.compensated)
	require.False(t, moveFailure.recoveryRequired)
	require.Equal(t, [][2]string{
		{"kb-source", "kb-target"},
		{"kb-target", "kb-source"},
	}, engine.moves, "target indices must be moved back when the knowledge-row CAS loses")
	require.Equal(t, []string{"kb-target", "kb-source"}, chunks.moves)
	require.Equal(t, 1, repo.finalizeCalls)
	require.Equal(t, 1, repo.moveScopeCalls)
	require.Equal(t, 1, repo.rollbackStateCalls)
	require.Equal(t, [][]string{{"tag-source"}}, repo.restoredTagIDBatches)
}

func TestMoveKnowledgeReuseVectorsRollbackFailureIsExplicitAndNotCompleted(t *testing.T) {
	service, repo, chunks, engine, knowledge, source, target := newMoveRaceService(true)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	err := service.moveKnowledgeReuseVectors(ctx, knowledge, source, target, knowledgeMoveTestAttemptID)
	require.ErrorContains(t, err, "reverse index move failed")
	require.ErrorContains(t, err, "rollback moved indices")
	var moveFailure *knowledgeMoveFailure
	require.ErrorAs(t, err, &moveFailure)
	require.False(t, moveFailure.compensated)
	require.True(t, moveFailure.recoveryRequired)
	require.Equal(t, []string{"kb-target", "kb-source"}, chunks.moves)
	require.Zero(t, repo.rollbackStateCalls,
		"a failed external compensation must leave the row non-terminal for delete/recovery cleanup")
	require.Equal(t, 1, repo.recoveryMarkerCalls)
	require.Equal(t, 1, repo.moveScopeCalls)
	require.Contains(t, knowledge.ErrorMessage, knowledgeMoveRecoveryRequired)
	require.Equal(t, types.ParseStatusProcessing, knowledge.ParseStatus)
	require.Len(t, engine.moves, 2)
}

type unsupportedMoveEngine struct {
	interfaces.RetrieveEngineService
}

func (*unsupportedMoveEngine) EngineType() types.RetrieverEngineType {
	return types.QdrantRetrieverEngineType
}

func (*unsupportedMoveEngine) Support() []types.RetrieverType {
	return []types.RetrieverType{types.VectorRetrieverType}
}

func TestMoveOneKnowledgeUnsupportedBackendRejectedBeforeClaim(t *testing.T) {
	service, repo, _, _, knowledge, source, target := newMoveRaceService(false)
	knowledge.ParseStatus = types.ParseStatusCompleted
	service.retrieveEngine = &moveRaceRegistry{engine: &unsupportedMoveEngine{}}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

	err := service.moveOneKnowledge(ctx, knowledge.ID, source, target, "reuse_vectors", knowledgeMoveTestAttemptID)
	require.ErrorContains(t, err, "use reparse mode")
	require.Zero(t, repo.finalizeCalls)
	require.Zero(t, repo.rollbackStateCalls)
	require.Equal(t, types.ParseStatusCompleted, knowledge.ParseStatus)
}

func TestFinalizeKnowledgeMoveAttemptPartialFailureNeverCompletes(t *testing.T) {
	progress := &types.KnowledgeMoveProgress{Total: 3, Processed: 3, Failed: 1}
	err := finalizeKnowledgeMoveAttempt(progress, []error{errors.New("one item failed")}, false)
	require.ErrorContains(t, err, "one item failed")
	require.Equal(t, types.KBCloneStatusProcessing, progress.Status)
	require.NotEqual(t, types.KBCloneStatusCompleted, progress.Status)
	require.Contains(t, progress.Message, "will retry")

	err = finalizeKnowledgeMoveAttempt(progress, []error{errors.New("still failed")}, true)
	require.ErrorContains(t, err, "still failed")
	require.Equal(t, types.KBCloneStatusFailed, progress.Status)
}

func TestKnowledgeMoveReparseTaskIDIsStableAcrossRetries(t *testing.T) {
	knowledge := &types.Knowledge{
		ID:          "knowledge-1",
		ParseStatus: types.ParseStatusPending,
		UpdatedAt:   time.Unix(1_700_000_000, 987_654_321),
	}
	first := knowledgeMoveRecoveryTaskID(knowledge)
	knowledge.UpdatedAt = time.Unix(1_700_000_000, 0) // database precision may truncate sub-seconds
	require.Equal(t, first, knowledgeMoveRecoveryTaskID(knowledge))

	knowledge.ErrorMessage = knowledgeMoveRecoveryReparseRequired + "generation-1"
	requiredID := knowledgeMoveRecoveryTaskID(knowledge)
	knowledge.ErrorMessage = knowledgeMoveRecoveryReparseQueued + "generation-1"
	require.Equal(t, requiredID, knowledgeMoveRecoveryTaskID(knowledge))
}

var _ retriever.TenantStoreOwnership = moveRaceOwnership{}
