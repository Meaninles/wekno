package retriever

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type inPlaceMoveRepository struct {
	interfaces.RetrieveEngineRepository
	calls []string
	err   error
}

type knownInPlaceMoveError struct {
	sourceIntact bool
}

func (*knownInPlaceMoveError) Error() string        { return "in-place move failed" }
func (e *knownInPlaceMoveError) SourceIntact() bool { return e.sourceIntact }

func (r *inPlaceMoveRepository) EngineType() types.RetrieverEngineType {
	return types.PostgresRetrieverEngineType
}

func (r *inPlaceMoveRepository) MoveKnowledgeIndicesInPlace(
	_ context.Context,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	_ string,
	_ []string,
	_ int,
	_ string,
) error {
	r.calls = append(r.calls, fmt.Sprintf("move:%s:%s", sourceKnowledgeBaseID, targetKnowledgeBaseID))
	return r.err
}

func TestMoveKnowledgeIndicesDelegatesOnlyToAtomicInPlacePrimitive(t *testing.T) {
	repo := &inPlaceMoveRepository{}
	service := &KeywordsVectorHybridRetrieveEngineService{indexRepository: repo}

	require.True(t, service.SupportsKnowledgeIndexMove())
	require.NoError(t, service.MoveKnowledgeIndices(
		context.Background(),
		"kb-source",
		"kb-target",
		"knowledge-1",
		[]string{"chunk-1"},
		768,
		"manual",
	))
	require.Equal(t, []string{"move:kb-source:kb-target"}, repo.calls)
}

func TestMoveKnowledgeIndicesRejectsCopyOnlyRepository(t *testing.T) {
	repo := &copyOnlyMoveRepository{}
	service := &KeywordsVectorHybridRetrieveEngineService{indexRepository: repo}

	require.False(t, service.SupportsKnowledgeIndexMove())
	err := service.MoveKnowledgeIndices(
		context.Background(),
		"kb-source",
		"kb-target",
		"knowledge-1",
		[]string{"chunk-1"},
		768,
		"manual",
	)
	require.ErrorContains(t, err, "use reparse mode")
	require.True(t, KnowledgeIndexMoveRollbackComplete(err), "capability rejection happens before mutation")
	require.Zero(t, repo.copyCalls, "ordinary CopyIndices must never be used as a reuse move")
}

func TestMoveKnowledgeIndicesRequiresReadBackBeforeLifecycleRollback(t *testing.T) {
	for _, tc := range []struct {
		name             string
		err              error
		rollbackComplete bool
	}{
		{
			name:             "source ownership proven",
			err:              &knownInPlaceMoveError{sourceIntact: true},
			rollbackComplete: true,
		},
		{
			name:             "ownership uncertain",
			err:              &knownInPlaceMoveError{sourceIntact: false},
			rollbackComplete: false,
		},
		{
			name:             "untyped transport error",
			err:              errors.New("connection lost"),
			rollbackComplete: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &inPlaceMoveRepository{err: tc.err}
			service := &KeywordsVectorHybridRetrieveEngineService{indexRepository: repo}
			err := service.MoveKnowledgeIndices(
				context.Background(),
				"kb-source",
				"kb-target",
				"knowledge-1",
				[]string{"chunk-1"},
				768,
				"manual",
			)
			require.Error(t, err)
			require.Equal(t, tc.rollbackComplete, KnowledgeIndexMoveRollbackComplete(err))
		})
	}
}

type copyOnlyMoveRepository struct {
	interfaces.RetrieveEngineRepository
	copyCalls int
}

func (*copyOnlyMoveRepository) EngineType() types.RetrieverEngineType {
	return types.QdrantRetrieverEngineType
}

func (r *copyOnlyMoveRepository) CopyIndices(
	context.Context,
	string,
	map[string]string,
	map[string]string,
	string,
	int,
	string,
) error {
	r.copyCalls++
	return nil
}

type recordingIndexMover struct {
	interfaces.RetrieveEngineService
	name        types.RetrieverEngineType
	calls       *[]string
	failForward bool
}

func (m *recordingIndexMover) EngineType() types.RetrieverEngineType { return m.name }
func (m *recordingIndexMover) SupportsKnowledgeIndexMove() bool      { return true }
func (m *recordingIndexMover) MoveKnowledgeIndices(
	_ context.Context,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	_ string,
	_ []string,
	_ int,
	_ string,
) error {
	*m.calls = append(*m.calls, fmt.Sprintf("%s:%s:%s", m.name, sourceKnowledgeBaseID, targetKnowledgeBaseID))
	if m.failForward && sourceKnowledgeBaseID == "kb-source" {
		return errors.New("engine forward failure")
	}
	return nil
}

func TestCompositeMoveKnowledgeIndicesRollsBackEarlierEngines(t *testing.T) {
	var calls []string
	first := &recordingIndexMover{name: types.PostgresRetrieverEngineType, calls: &calls}
	second := &recordingIndexMover{
		name:        types.SQLiteRetrieverEngineType,
		calls:       &calls,
		failForward: true,
	}
	composite := &CompositeRetrieveEngine{engineInfos: []*engineInfo{
		{retrieveEngine: first},
		{retrieveEngine: second},
	}}

	require.True(t, composite.SupportsKnowledgeIndexMove())
	err := composite.MoveKnowledgeIndices(
		context.Background(),
		"kb-source",
		"kb-target",
		"knowledge-1",
		[]string{"chunk-1"},
		768,
		"manual",
	)
	require.ErrorContains(t, err, "engine forward failure")
	require.False(t, KnowledgeIndexMoveRollbackComplete(err),
		"an engine that returns an unclassified error cannot be assumed compensated")
	require.Equal(t, []string{
		"postgres:kb-source:kb-target",
		"sqlite:kb-source:kb-target",
		"postgres:kb-target:kb-source",
	}, calls)
}
