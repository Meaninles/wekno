package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const knowledgeMoveTestAttemptID = "move-attempt-1"

type reparseMoveRecoveryRepo struct {
	interfaces.KnowledgeRepository
	knowledge      *types.Knowledge
	finalizeCalls  int
	deadLetterFail int
	tagDeleteCalls int
	moveScope      func(func() error) error
	moveDB         *gorm.DB
}

// Production repositories hold a sorted shared fence over both knowledge
// bases while cleanup/external vector work and the target commit run. This
// state-only fake has no database lock manager, but it must preserve the same
// callback boundary so recovery tests reach the injected operation failures.
func (r *reparseMoveRecoveryRepo) WithActiveKnowledgeMoveScope(
	_ context.Context,
	_ uint64,
	_ string,
	_ string,
	work func() error,
) error {
	if r.moveScope != nil {
		return r.moveScope(work)
	}
	return work()
}

func (r *reparseMoveRecoveryRepo) PrepareKnowledgeMoveReparseRecovery(
	_ context.Context,
	tenantID uint64,
	id string,
	knowledgeBaseID string,
	expectedGeneration string,
	expectedOwner string,
	expectedMarker string,
	newGeneration string,
	newOwner string,
	newMarker string,
	_ time.Time,
) (bool, error) {
	if r.knowledge.TenantID != tenantID || r.knowledge.ID != id ||
		r.knowledge.KnowledgeBaseID != knowledgeBaseID || r.knowledge.ParseStatus != types.ParseStatusProcessing ||
		r.knowledge.ProcessingGeneration != expectedGeneration || r.knowledge.ProcessingOwner != expectedOwner ||
		r.knowledge.ErrorMessage != expectedMarker {
		return false, nil
	}
	r.knowledge.ProcessingGeneration = newGeneration
	r.knowledge.ProcessingOwner = newOwner
	r.knowledge.ProcessingFanout = nil
	r.knowledge.ErrorMessage = newMarker
	return true, nil
}

func (r *reparseMoveRecoveryRepo) GetKnowledgeByID(context.Context, uint64, string) (*types.Knowledge, error) {
	copyKnowledge := *r.knowledge
	return &copyKnowledge, nil
}

func applyReparseMoveValues(knowledge *types.Knowledge, values map[string]interface{}) {
	if value, ok := values["parse_status"].(string); ok {
		knowledge.ParseStatus = value
	}
	if value, ok := values["processing_generation"].(string); ok {
		knowledge.ProcessingGeneration = value
	}
	if value, ok := values["processing_owner"].(string); ok {
		knowledge.ProcessingOwner = value
	}
	if value, ok := values["processing_workflow_id"].(string); ok {
		knowledge.ProcessingWorkflowID = value
	}
	if value, ok := values["error_message"].(string); ok {
		knowledge.ErrorMessage = value
	}
	if value, ok := values["knowledge_base_id"].(string); ok {
		knowledge.KnowledgeBaseID = value
	}
}

func (r *reparseMoveRecoveryRepo) CompareAndSwapKnowledgeState(
	_ context.Context,
	tenantID uint64,
	id string,
	expectedKBID string,
	expectedStatus string,
	values map[string]interface{},
) (bool, error) {
	if r.knowledge.TenantID != tenantID || r.knowledge.ID != id ||
		r.knowledge.KnowledgeBaseID != expectedKBID || r.knowledge.ParseStatus != expectedStatus {
		return false, nil
	}
	applyReparseMoveValues(r.knowledge, values)
	return true, nil
}

func (r *reparseMoveRecoveryRepo) CompareAndSwapDocumentProcessing(
	_ context.Context,
	tenantID uint64,
	id string,
	expectedKBID string,
	expectedStatus string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
) (bool, error) {
	if r.knowledge.TenantID != tenantID || r.knowledge.ID != id ||
		r.knowledge.KnowledgeBaseID != expectedKBID || r.knowledge.ParseStatus != expectedStatus ||
		r.knowledge.ProcessingGeneration != expectedGeneration || r.knowledge.ProcessingOwner != expectedOwner {
		return false, nil
	}
	applyReparseMoveValues(r.knowledge, values)
	return true, nil
}

func (r *reparseMoveRecoveryRepo) FinalizeReuseVectorKnowledgeMove(
	context.Context, uint64, string, string, string, string, string, string, time.Time,
) (bool, error) {
	return false, nil
}

func (r *reparseMoveRecoveryRepo) FinalizeReparseKnowledgeMove(
	_ context.Context,
	tenantID uint64,
	id string,
	sourceKBID string,
	targetKBID string,
	expectedGeneration string,
	expectedOwner string,
	targetEmbeddingModelID string,
	moveMarker string,
	processingWorkflowID string,
	_ func(*gorm.DB, func(*gorm.DB) error) error,
	_ time.Time,
) (bool, error) {
	if r.knowledge.TenantID != tenantID || r.knowledge.ID != id ||
		r.knowledge.KnowledgeBaseID != sourceKBID || r.knowledge.ParseStatus != types.ParseStatusProcessing ||
		r.knowledge.ProcessingGeneration != expectedGeneration || r.knowledge.ProcessingOwner != expectedOwner {
		return false, nil
	}
	r.finalizeCalls++
	r.knowledge.KnowledgeBaseID = targetKBID
	r.knowledge.EmbeddingModelID = targetEmbeddingModelID
	r.knowledge.ParseStatus = types.ParseStatusPending
	r.knowledge.ErrorMessage = moveMarker
	r.knowledge.ProcessingWorkflowID = processingWorkflowID
	r.knowledge.EnableStatus = "disabled"
	r.knowledge.ProcessedAt = nil
	r.knowledge.StorageSize = 0
	if r.moveDB != nil {
		if err := r.moveDB.Table("knowledges").Where("id = ?", id).Updates(map[string]interface{}{
			"knowledge_base_id": targetKBID, "parse_status": types.ParseStatusPending,
			"processing_generation": expectedGeneration, "processing_owner": expectedOwner,
			"processing_workflow_id": processingWorkflowID, "error_message": moveMarker,
		}).Error; err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *reparseMoveRecoveryRepo) DeleteKnowledgeTagRelations(context.Context, string) error {
	r.tagDeleteCalls++
	return nil
}

func (r *reparseMoveRecoveryRepo) FailKnowledgeMoveGeneration(
	_ context.Context,
	tenantID uint64,
	id string,
	expectedKBID string,
	expectedGeneration string,
	expectedOwner string,
	expectedMarker string,
	errorMessage string,
) (bool, error) {
	if r.knowledge.TenantID != tenantID || r.knowledge.ID != id ||
		r.knowledge.KnowledgeBaseID != expectedKBID || r.knowledge.ParseStatus != types.ParseStatusProcessing ||
		r.knowledge.ProcessingGeneration != expectedGeneration || r.knowledge.ProcessingOwner != expectedOwner ||
		r.knowledge.ErrorMessage != expectedMarker {
		return false, nil
	}
	r.deadLetterFail++
	r.knowledge.ParseStatus = types.ParseStatusFailed
	r.knowledge.ProcessingOwner = ""
	r.knowledge.ErrorMessage = errorMessage
	return true, nil
}

type reparseMoveChunkRepo struct{ interfaces.ChunkRepository }

func (*reparseMoveChunkRepo) ListImageInfoByKnowledgeIDs(context.Context, uint64, []string) ([]interfaces.ChunkImageInfo, error) {
	return nil, nil
}

func (*reparseMoveChunkRepo) ListImageInfoByKnowledgeIDsUnscoped(context.Context, uint64, []string) ([]interfaces.ChunkImageInfo, error) {
	return nil, nil
}

func (*reparseMoveChunkRepo) ListChunkIDsByKnowledgeIDUnscoped(context.Context, uint64, string) ([]string, error) {
	return nil, nil
}

type reparseMoveChunkService struct {
	interfaces.ChunkService
	repo        interfaces.ChunkRepository
	deleteError error
	deleteCalls int
}

func (s *reparseMoveChunkService) GetRepository() interfaces.ChunkRepository { return s.repo }

func (s *reparseMoveChunkService) DeleteChunksByKnowledgeID(context.Context, string) error {
	s.deleteCalls++
	return s.deleteError
}

type reparseMoveGraphRepo struct {
	interfaces.RetrieveGraphRepository
}

func (*reparseMoveGraphRepo) DelGraph(context.Context, []types.NameSpace) error { return nil }

type reparseMoveKBService struct {
	interfaces.KnowledgeBaseService
	source *types.KnowledgeBase
	target *types.KnowledgeBase
}

func (s *reparseMoveKBService) GetKnowledgeBaseByID(_ context.Context, id string) (*types.KnowledgeBase, error) {
	if s.source != nil && s.source.ID == id {
		if s.source.DeletedAt.Valid {
			return nil, apprepo.ErrKnowledgeBaseNotFound
		}
		return s.source, nil
	}
	if s.target != nil && s.target.ID == id {
		if s.target.DeletedAt.Valid {
			return nil, apprepo.ErrKnowledgeBaseNotFound
		}
		return s.target, nil
	}
	return nil, apprepo.ErrKnowledgeBaseNotFound
}

func (s *reparseMoveKBService) GetKnowledgeBaseByIDForMoveRecovery(
	_ context.Context,
	id string,
	tenantID uint64,
) (*types.KnowledgeBase, error) {
	for _, kb := range []*types.KnowledgeBase{s.source, s.target} {
		if kb != nil && kb.ID == id {
			if kb.TenantID != tenantID {
				return nil, errors.New("knowledge base tenant mismatch")
			}
			copyKB := *kb
			return &copyKB, nil
		}
	}
	return nil, apprepo.ErrKnowledgeBaseNotFound
}

type reparseMoveTenantRepo struct {
	interfaces.TenantRepository
	tenant *types.Tenant
}

func (r *reparseMoveTenantRepo) GetTenantByID(context.Context, uint64) (*types.Tenant, error) {
	return r.tenant, nil
}

func newReparseMoveRecoveryService(
	t *testing.T,
	repo *reparseMoveRecoveryRepo,
	chunks *reparseMoveChunkService,
	task *wikiQueueTaskEnqueuerStub,
) (*knowledgeService, *types.KnowledgeBase, *types.KnowledgeBase) {
	t.Helper()
	source := &types.KnowledgeBase{ID: "kb-source", TenantID: 7, EmbeddingModelID: "embedding-1"}
	target := &types.KnowledgeBase{ID: "kb-target", TenantID: 7, EmbeddingModelID: "embedding-1"}
	coordinatorDB := newMoveWikiDB(t, repo.knowledge)
	repo.moveDB = coordinatorDB
	return &knowledgeService{
		repo:            repo,
		kbService:       &reparseMoveKBService{source: source, target: target},
		tenantRepo:      &reparseMoveTenantRepo{tenant: &types.Tenant{ID: 7}},
		chunkService:    chunks,
		chunkRepo:       chunks.repo,
		graphEngine:     &reparseMoveGraphRepo{},
		task:            task,
		wikiRepo:        &moveWikiPageRepoStub{indexErr: apprepo.ErrWikiPageNotFound},
		wikiDeleteCoord: wikidelete.New(coordinatorDB),
		auxObjects:      knowledgeaux.NewWithResolver(coordinatorDB, nil),
	}, source, target
}

func TestReparseMoveCleanupFailurePersistsMarkerAndRetryResumes(t *testing.T) {
	now := time.Now()
	repo := &reparseMoveRecoveryRepo{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-source",
		ParseStatus: types.ParseStatusCompleted, ProcessedAt: &now,
		Type: "document", FilePath: "local://test/document.pdf", FileName: "document.pdf",
		EmbeddingModelID: "", EnableStatus: "enabled",
	}}
	chunkRepo := &reparseMoveChunkRepo{}
	chunks := &reparseMoveChunkService{repo: chunkRepo, deleteError: errors.New("injected chunk cleanup failure")}
	task := &wikiQueueTaskEnqueuerStub{}
	service, source, target := newReparseMoveRecoveryService(t, repo, chunks, task)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, &types.Tenant{ID: 7})

	err := service.moveOneKnowledge(ctx, repo.knowledge.ID, source, target, "reparse", knowledgeMoveTestAttemptID)
	require.ErrorContains(t, err, "injected chunk cleanup failure")
	assert.Equal(t, types.ParseStatusProcessing, repo.knowledge.ParseStatus)
	assert.True(t, isKnowledgeMoveTargetCleanupMarker(repo.knowledge, target.ID, knowledgeMoveTestAttemptID))
	assert.NotEmpty(t, repo.knowledge.ProcessingOwner)
	assert.Zero(t, repo.finalizeCalls)

	chunks.deleteError = nil
	err = service.moveOneKnowledge(ctx, repo.knowledge.ID, source, target, "reparse", knowledgeMoveTestAttemptID)
	require.NoError(t, err)
	assert.Equal(t, target.ID, repo.knowledge.KnowledgeBaseID)
	assert.Equal(t, types.ParseStatusPending, repo.knowledge.ParseStatus)
	var persistedMarker string
	require.NoError(t, repo.moveDB.Table("knowledges").Select("error_message").
		Where("id = ?", repo.knowledge.ID).Scan(&persistedMarker).Error)
	assert.Empty(t, persistedMarker)
	assert.Equal(t, 1, repo.finalizeCalls)
	assert.Equal(t, 2, chunks.deleteCalls)
	require.Len(t, task.tasks, 2)
	assert.Equal(t, types.TypeWikiIngest, task.tasks[0].Type())
	assert.Equal(t, types.TypeDocumentProcess, task.tasks[1].Type())
}

func TestReparseMoveRejectsUnreconstructableSourceBeforeClaimOrCleanup(t *testing.T) {
	for _, tc := range []struct {
		name          string
		knowledgeType string
		filePath      string
	}{
		{name: "FAQ aggregate", knowledgeType: types.KnowledgeTypeFAQ},
		{name: "document without persistent source", knowledgeType: "document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			repo := &reparseMoveRecoveryRepo{knowledge: &types.Knowledge{
				ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-source",
				ParseStatus: types.ParseStatusCompleted, ProcessedAt: &now,
				Type: tc.knowledgeType, FilePath: tc.filePath,
			}}
			chunks := &reparseMoveChunkService{repo: &reparseMoveChunkRepo{}}
			service, source, target := newReparseMoveRecoveryService(
				t, repo, chunks, &wikiQueueTaskEnqueuerStub{},
			)
			ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))

			err := service.moveOneKnowledge(
				ctx, repo.knowledge.ID, source, target, "reparse", knowledgeMoveTestAttemptID,
			)
			require.Error(t, err)
			assert.Equal(t, types.ParseStatusCompleted, repo.knowledge.ParseStatus)
			assert.Zero(t, chunks.deleteCalls, "preflight rejection must precede destructive cleanup")
			assert.Zero(t, repo.finalizeCalls)
		})
	}
}

func TestKnowledgeMoveTargetTombstoneWinningFenceLeavesSourceCompletelyUnchanged(t *testing.T) {
	for _, mode := range []string{"reparse", "reuse_vectors"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			repo := &reparseMoveRecoveryRepo{knowledge: &types.Knowledge{
				ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-source",
				ParseStatus: types.ParseStatusCompleted, ProcessedAt: &now,
				Type: "document", FilePath: "local://test/document.pdf", FileName: "document.pdf",
				ProcessingGeneration: "completed-generation", ProcessingOwner: "",
				EmbeddingModelID: "embedding-1",
			}}
			chunks := &reparseMoveChunkService{repo: &reparseMoveChunkRepo{}}
			service, source, target := newReparseMoveRecoveryService(
				t, repo, chunks, &wikiQueueTaskEnqueuerStub{},
			)
			storeID := "store-1"
			source.VectorStoreID = &storeID
			target.VectorStoreID = &storeID
			moveEngine := &moveRaceEngine{}
			service.retrieveEngine = &moveRaceRegistry{engine: moveEngine}
			service.ownership = moveRaceOwnership{}
			service.modelService = &moveRaceModelService{}
			fenceRequested := make(chan struct{})
			deleteCommitted := make(chan struct{})
			repo.moveScope = func(work func() error) error {
				close(fenceRequested)
				<-deleteCommitted
				return kbwritefence.ErrKnowledgeBaseUnavailable
			}
			ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
			moveDone := make(chan error, 1)
			go func() {
				moveDone <- service.moveOneKnowledge(
					ctx, repo.knowledge.ID, source, target, mode, knowledgeMoveTestAttemptID,
				)
			}()

			select {
			case <-fenceRequested:
			case <-time.After(5 * time.Second):
				t.Fatal("move did not request the double-KB fence before claiming the source")
			}
			target.DeletedAt.Time = time.Now().UTC()
			target.DeletedAt.Valid = true
			close(deleteCommitted)
			err := <-moveDone
			require.ErrorIs(t, err, kbwritefence.ErrKnowledgeBaseUnavailable)
			assert.Equal(t, types.ParseStatusCompleted, repo.knowledge.ParseStatus)
			assert.Equal(t, "kb-source", repo.knowledge.KnowledgeBaseID)
			assert.Equal(t, "completed-generation", repo.knowledge.ProcessingGeneration)
			assert.Empty(t, repo.knowledge.ProcessingOwner)
			assert.Empty(t, repo.knowledge.ErrorMessage)
			assert.Zero(t, chunks.deleteCalls,
				"a target tombstone that wins the parent fence must precede every destructive cleanup")
			assert.Empty(t, moveEngine.moves)
			assert.Zero(t, repo.tagDeleteCalls)
			assert.Zero(t, repo.finalizeCalls)
		})
	}
}

func TestKnowledgeMoveDeadLetterFailsOnlySynchronousRecoveryPhase(t *testing.T) {
	generation := "generation-2"
	owner := "owner-2"
	repo := &reparseMoveRecoveryRepo{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-source",
		ParseStatus: types.ParseStatusProcessing, ProcessingGeneration: generation,
		ProcessingOwner: owner,
		ErrorMessage:    knowledgeMoveTargetCleanupMarker(knowledgeMoveTestAttemptID, "kb-target", generation),
	}}
	chunks := &reparseMoveChunkService{repo: &reparseMoveChunkRepo{}}
	service, _, _ := newReparseMoveRecoveryService(t, repo, chunks, &wikiQueueTaskEnqueuerStub{})
	payload, err := json.Marshal(types.KnowledgeMovePayload{
		TenantID: 7, KnowledgeIDs: []string{repo.knowledge.ID},
		SourceKBID: "kb-source", TargetKBID: "kb-target", Mode: "reparse",
		TaskID: knowledgeMoveTestAttemptID, AttemptID: knowledgeMoveTestAttemptID,
	})
	require.NoError(t, err)

	require.NoError(t, service.RepairKnowledgeMoveDeadLetter(
		context.Background(), asynq.NewTask(types.TypeKnowledgeMove, payload), errors.New("exhausted"),
	))
	assert.Equal(t, types.ParseStatusFailed, repo.knowledge.ParseStatus)
	assert.Empty(t, repo.knowledge.ProcessingOwner)
	assert.Equal(t, 1, repo.deadLetterFail)

	// Once the source child is enqueued, Processing belongs to that child. A
	// delayed parent dead letter must not terminalize it.
	repo.knowledge.ParseStatus = types.ParseStatusProcessing
	repo.knowledge.ProcessingOwner = owner
	repo.knowledge.ErrorMessage = knowledgeMoveAttemptMarker(
		knowledgeMoveTestAttemptID,
		knowledgeMoveRecoveryReparseRequired+"recovery-generation",
	)
	repo.deadLetterFail = 0
	require.NoError(t, service.RepairKnowledgeMoveDeadLetter(
		context.Background(), asynq.NewTask(types.TypeKnowledgeMove, payload), errors.New("late parent"),
	))
	assert.Equal(t, types.ParseStatusProcessing, repo.knowledge.ParseStatus)
	assert.Equal(t, 0, repo.deadLetterFail)
}

func TestKnowledgeMoveDeadLetterCannotFailNewerAttempt(t *testing.T) {
	const (
		oldAttempt = "move-attempt-old"
		newAttempt = "move-attempt-new"
	)
	generation := "generation-new"
	owner := "owner-new"
	repo := &reparseMoveRecoveryRepo{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-source",
		ParseStatus: types.ParseStatusProcessing, ProcessingGeneration: generation,
		ProcessingOwner: owner,
		ErrorMessage:    knowledgeMoveTargetCleanupMarker(newAttempt, "kb-target", generation),
	}}
	chunks := &reparseMoveChunkService{repo: &reparseMoveChunkRepo{}}
	service, _, _ := newReparseMoveRecoveryService(t, repo, chunks, &wikiQueueTaskEnqueuerStub{})
	moveTask := func(attemptID string) *asynq.Task {
		payload, err := json.Marshal(types.KnowledgeMovePayload{
			TenantID: 7, KnowledgeIDs: []string{repo.knowledge.ID},
			SourceKBID: "kb-source", TargetKBID: "kb-target", Mode: "reparse",
			TaskID: attemptID, AttemptID: attemptID,
		})
		require.NoError(t, err)
		return asynq.NewTask(types.TypeKnowledgeMove, payload)
	}

	require.NoError(t, service.RepairKnowledgeMoveDeadLetter(
		context.Background(), moveTask(oldAttempt), errors.New("delayed old attempt"),
	))
	assert.Equal(t, types.ParseStatusProcessing, repo.knowledge.ParseStatus)
	assert.Equal(t, owner, repo.knowledge.ProcessingOwner)
	assert.Equal(t, 0, repo.deadLetterFail)

	require.NoError(t, service.RepairKnowledgeMoveDeadLetter(
		context.Background(), moveTask(newAttempt), errors.New("current attempt exhausted"),
	))
	assert.Equal(t, types.ParseStatusFailed, repo.knowledge.ParseStatus)
	assert.Empty(t, repo.knowledge.ProcessingOwner)
	assert.Equal(t, 1, repo.deadLetterFail)
}

func TestReuseMoveRecoveryReplacesResidualCompletedGenerationBeforeCleanup(t *testing.T) {
	repo := &reparseMoveRecoveryRepo{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-source",
		ParseStatus: types.ParseStatusProcessing, ProcessingGeneration: "old-completed-generation",
		ProcessingOwner: "old-completed-owner", ErrorMessage: knowledgeMoveAttemptMarker(
			knowledgeMoveTestAttemptID,
			knowledgeMoveRecoveryRequired+"uncertain vector state",
		),
		Type: "document", FilePath: "local://test/document.pdf", FileName: "document.pdf",
	}}
	chunks := &reparseMoveChunkService{
		repo: &reparseMoveChunkRepo{}, deleteError: errors.New("injected cleanup failure"),
	}
	service, source, _ := newReparseMoveRecoveryService(t, repo, chunks, &wikiQueueTaskEnqueuerStub{})
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, &types.Tenant{ID: 7})
	knowledge, loadErr := repo.GetKnowledgeByID(ctx, 7, repo.knowledge.ID)
	require.NoError(t, loadErr)

	err := service.recoverMarkedKnowledgeMove(ctx, knowledge, source, knowledgeMoveTestAttemptID)
	require.ErrorContains(t, err, "injected cleanup failure")
	assert.NotEqual(t, "old-completed-generation", repo.knowledge.ProcessingGeneration)
	assert.NotEqual(t, "old-completed-owner", repo.knowledge.ProcessingOwner)
	_, marker, marked := parseKnowledgeMoveAttemptMarker(repo.knowledge.ErrorMessage)
	assert.True(t, marked)
	assert.True(t, strings.HasPrefix(marker, knowledgeMoveRecoveryCleanupRequired))
}
