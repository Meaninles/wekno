package processownership

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type legacyRepairRepositoryStub struct {
	interfaces.KnowledgeRepository
	knowledge *types.Knowledge
	calls     int
}

func (r *legacyRepairRepositoryStub) GetKnowledgeByID(
	context.Context, uint64, string,
) (*types.Knowledge, error) {
	if r.knowledge == nil {
		return nil, nil
	}
	copyKnowledge := *r.knowledge
	return &copyKnowledge, nil
}

func (r *legacyRepairRepositoryStub) RepairLegacyProcessing(
	_ context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	expectedStatus string,
	_ bool,
	repairGeneration string,
	completeCore bool,
	message string,
	_ time.Time,
) (bool, error) {
	r.calls++
	if r.knowledge == nil || r.knowledge.TenantID != tenantID || r.knowledge.ID != knowledgeID ||
		r.knowledge.KnowledgeBaseID != knowledgeBaseID || r.knowledge.ParseStatus != expectedStatus ||
		r.knowledge.ProcessingGeneration != "" || r.knowledge.ProcessingOwner != "" {
		return false, nil
	}
	r.knowledge.ProcessingGeneration = repairGeneration
	r.knowledge.ProcessingOwner = ""
	r.knowledge.PendingSubtasksCount = 0
	r.knowledge.ErrorMessage = message
	r.knowledge.ParseStatus = types.ParseStatusFailed
	if completeCore {
		r.knowledge.ParseStatus = types.ParseStatusCompleted
		r.knowledge.ErrorMessage = ""
	}
	return true, nil
}

func TestRepairLegacyTaskTerminalizesOnlyGenerationlessActiveRow(t *testing.T) {
	repo := &legacyRepairRepositoryStub{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ParseStatus: types.ParseStatusPending,
	}}
	err := RepairLegacyTask(
		context.Background(), repo, 7, "knowledge-1", "kb-1", "document processing",
	)
	require.ErrorIs(t, err, ErrLegacyTaskRepaired)
	assert.Equal(t, types.ParseStatusFailed, repo.knowledge.ParseStatus)
	assert.NotEmpty(t, repo.knowledge.ProcessingGeneration)
	assert.Contains(t, repo.knowledge.ErrorMessage, "reparse is required")
	assert.Equal(t, 1, repo.calls)
}

func TestRepairLegacyTaskTreatsCurrentGenerationAsStaleWithoutWrite(t *testing.T) {
	repo := &legacyRepairRepositoryStub{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ParseStatus: types.ParseStatusProcessing, ProcessingGeneration: "generation-new",
		ProcessingOwner: "owner-new",
	}}
	require.NoError(t, RepairLegacyTask(
		context.Background(), repo, 7, "knowledge-1", "kb-1", "document processing",
	))
	assert.Equal(t, 0, repo.calls)
	assert.Equal(t, types.ParseStatusProcessing, repo.knowledge.ParseStatus)
	assert.Equal(t, "generation-new", repo.knowledge.ProcessingGeneration)
	assert.Equal(t, "owner-new", repo.knowledge.ProcessingOwner)
}

func TestRepairLegacyTaskCASConflictNeverAcknowledges(t *testing.T) {
	repo := &legacyRepairRepositoryStub{knowledge: &types.Knowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ParseStatus: types.ParseStatusPending,
	}}
	// Return a stale snapshot from Get and make the exact repair lose.
	repo.KnowledgeRepository = nil
	original := repo.knowledge
	repo.knowledge = original
	conflict := &legacyRepairConflictRepository{legacyRepairRepositoryStub: repo}
	err := RepairLegacyTask(
		context.Background(), conflict, 7, "knowledge-1", "kb-1", "document processing",
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLegacyTaskConflict))
}

type legacyRepairConflictRepository struct {
	*legacyRepairRepositoryStub
}

func (*legacyRepairConflictRepository) RepairLegacyProcessing(
	context.Context, uint64, string, string, string, bool, string, bool, string, time.Time,
) (bool, error) {
	return false, nil
}
