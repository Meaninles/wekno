package derivativequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

type derivativeTestKnowledge struct {
	ID                   string `gorm:"primaryKey"`
	TenantID             uint64
	KnowledgeBaseID      string
	ProcessingGeneration string
	ParseStatus          string
	DeletedAt            gorm.DeletedAt
}

func (derivativeTestKnowledge) TableName() string { return "knowledges" }

type derivativeTestWikiDemand struct {
	ID                    int64 `gorm:"primaryKey"`
	TaskType              string
	Scope                 string
	Op                    string
	MapReadyAt            *time.Time
	MapResourcePoolID     string
	MapDispatchLeaseUntil *time.Time
	NextAttemptAt         *time.Time
}

func (derivativeTestWikiDemand) TableName() string { return "task_pending_ops" }

func derivativeRepositoryForTest(t *testing.T) (*Repository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&derivativeTestKnowledge{}))
	repository := NewRepository(db)
	require.NoError(t, repository.Migrate(context.Background()))
	return repository, db
}

func derivativePlan() PlanItem {
	return PlanItem{
		TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1", ProcessingAttempt: 1,
		ItemID: "question:batch:0", WorkKind: WorkQuestion,
		Payload: types.JSON(`{"batch":0}`), ResourcePoolID: "pool-a",
	}
}

func TestRepositoryPlanDispatchClaimAndResultAreIdempotent(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusFinalizing,
	}).Error)

	rows, err := repository.UpsertPlan(ctx, []PlanItem{derivativePlan(), derivativePlan()})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Nil(t, rows[0].ResultID, "unmaterialized work must persist a SQL NULL result_id")
	var count int64
	require.NoError(t, db.Model(&WorkItem{}).Count(&count).Error)
	require.EqualValues(t, 1, count)

	wake, err := repository.MarkDispatched(ctx, rows[0].ID, rows[0].Version)
	require.NoError(t, err)
	require.EqualValues(t, 1, wake.DispatchEpoch)
	claimed, err := repository.Claim(ctx, wake, "worker-a", time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, claimed.LeaseToken)
	_, err = repository.Claim(ctx, wake, "worker-b", time.Minute)
	require.ErrorIs(t, err, ErrInvalidState)

	running, err := repository.BeginProvider(ctx, claimed.ID, claimed.LeaseToken)
	require.NoError(t, err)
	require.Equal(t, StateProviderRunning, running.State)
	require.Equal(t, 1, running.ProviderAttempts)
	require.NotEmpty(t, running.ProviderRequestKey)

	first, err := repository.SaveProviderResult(ctx, running.ID, running.LeaseToken, ProviderResult{
		Content: `{"questions":["q1"]}`, Usage: types.JSON(`{"total_tokens":12}`),
	})
	require.NoError(t, err)
	second, err := repository.SaveProviderResult(ctx, running.ID, running.LeaseToken, ProviderResult{
		Content: `{"questions":["q1"]}`, Usage: types.JSON(`{"total_tokens":12}`),
	})
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.NoError(t, db.Model(&Result{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestAdmissionDeferralDoesNotConsumeProviderAttempt(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusFinalizing,
	}).Error)
	rows, err := repository.UpsertPlan(ctx, []PlanItem{derivativePlan()})
	require.NoError(t, err)
	wake, err := repository.MarkDispatched(ctx, rows[0].ID, rows[0].Version)
	require.NoError(t, err)
	claimed, err := repository.Claim(ctx, wake, "worker-a", time.Minute)
	require.NoError(t, err)
	require.NoError(t, repository.DeferWithoutProviderAttempt(
		ctx, claimed.ID, claimed.LeaseToken, "admission", "pool_full", "busy", time.Second,
	))
	var row WorkItem
	require.NoError(t, db.First(&row, "id = ?", claimed.ID).Error)
	require.Equal(t, StateQueued, row.State)
	require.Zero(t, row.ProviderAttempts)
	require.Empty(t, row.LeaseToken)
}

func TestAdmissionDeferralAfterCheckpointDoesNotConsumeAnyRetryBudget(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusFinalizing,
	}).Error)
	rows, err := repository.UpsertPlan(ctx, []PlanItem{derivativePlan()})
	require.NoError(t, err)
	wake, err := repository.MarkDispatched(ctx, rows[0].ID, rows[0].Version)
	require.NoError(t, err)
	claimed, err := repository.Claim(ctx, wake, "worker-a", time.Minute)
	require.NoError(t, err)
	running, err := repository.BeginProvider(ctx, claimed.ID, claimed.LeaseToken)
	require.NoError(t, err)
	require.NoError(t, db.Create(&ProviderCall{
		ID: "00000000-0000-0000-0000-000000000001", WorkItemID: running.ID,
		RequestHash: "hash-1", Attempt: 1, ProviderRequestKey: "provider-key-1",
		ProcessingGeneration: "generation-1", Response: types.JSON(`{"ok":true}`),
		ContentChecksum: "checksum-1", Disposition: ProviderCallCheckpointed,
		CreatedAt: time.Now().UTC(),
	}).Error)

	require.NoError(t, repository.DeferForAdmission(
		ctx, running.ID, running.LeaseToken, "model_deferred", "pool is busy", time.Second,
	))
	var row WorkItem
	require.NoError(t, db.First(&row, "id = ?", running.ID).Error)
	require.Equal(t, StateQueued, row.State)
	require.Equal(t, 1, row.ProviderAttempts)
	require.Zero(t, row.MaterializeAttempts)
	require.Zero(t, row.FinalizeAttempts)
	require.Empty(t, row.LeaseToken)
}

func TestDispatchLeasePreventsDuplicateWakeUntilExpiry(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	rows, err := repository.UpsertPlan(ctx, []PlanItem{derivativePlan()})
	require.NoError(t, err)

	first, err := repository.markDispatched(ctx, rows[0].ID, rows[0].Version, "pool-a", 2, 2, time.Minute)
	require.NoError(t, err)
	_, err = repository.markDispatched(ctx, rows[0].ID, rows[0].Version+1, "pool-a", 2, 2, time.Minute)
	require.ErrorIs(t, err, ErrInvalidState)

	require.NoError(t, db.Model(&WorkItem{}).Where("id = ?", rows[0].ID).
		Update("dispatch_lease_until", time.Now().UTC().Add(-time.Second)).Error)
	var current WorkItem
	require.NoError(t, db.First(&current, "id = ?", rows[0].ID).Error)
	second, err := repository.markDispatched(ctx, current.ID, current.Version, "pool-a", 2, 2, time.Minute)
	require.NoError(t, err)
	require.Equal(t, first.DispatchEpoch+1, second.DispatchEpoch)
}

func TestDispatchWindowBoundsPublishedModelWork(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	firstPlan := derivativePlan()
	secondPlan := derivativePlan()
	secondPlan.ItemID = "question:batch:1"
	rows, err := repository.UpsertPlan(ctx, []PlanItem{firstPlan, secondPlan})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	_, err = repository.markDispatched(ctx, rows[0].ID, rows[0].Version, "pool-a", 1, 1, time.Minute)
	require.NoError(t, err)
	_, err = repository.markDispatched(ctx, rows[1].ID, rows[1].Version, "pool-a", 1, 1, time.Minute)
	require.ErrorIs(t, err, ErrDispatchWindowFull)

	require.NoError(t, db.Model(&WorkItem{}).Where("id = ?", rows[0].ID).
		Update("dispatch_lease_until", time.Now().UTC().Add(-time.Second)).Error)
	_, err = repository.markDispatched(ctx, rows[1].ID, rows[1].Version, "pool-a", 1, 1, time.Minute)
	require.NoError(t, err)
}

func TestDispatchWindowProtectsWikiShareAndBorrowsWhenWikiIsIdle(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.AutoMigrate(&derivativeTestWikiDemand{}))
	now := time.Now().UTC().Add(-time.Second)
	require.NoError(t, db.Create(&derivativeTestWikiDemand{
		ID: 1, TaskType: types.TypeWikiIngest, Scope: "knowledge_base", Op: "ingest",
		MapResourcePoolID: "pool-a", NextAttemptAt: &now,
	}).Error)

	plans := make([]PlanItem, 0, 3)
	for index := 0; index < 3; index++ {
		item := derivativePlan()
		item.ItemID = fmt.Sprintf("question:batch:%d", index)
		plans = append(plans, item)
	}
	rows, err := repository.UpsertPlan(ctx, plans)
	require.NoError(t, err)
	for index := 0; index < 2; index++ {
		_, err = repository.markDispatched(
			ctx, rows[index].ID, rows[index].Version, "pool-a", 3, 2, time.Minute,
		)
		require.NoError(t, err)
	}
	_, err = repository.markDispatched(
		ctx, rows[2].ID, rows[2].Version, "pool-a", 3, 2, time.Minute,
	)
	require.ErrorIs(t, err, ErrDispatchWindowFull)

	require.NoError(t, db.Delete(&derivativeTestWikiDemand{}, 1).Error)
	_, err = repository.markDispatched(
		ctx, rows[2].ID, rows[2].Version, "pool-a", 3, 2, time.Minute,
	)
	require.NoError(t, err, "derivative lane should borrow the idle Wiki share")
}

func TestGenerationFenceCancelsOldWork(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-2", ParseStatus: types.ParseStatusFinalizing,
	}).Error)
	rows, err := repository.UpsertPlan(ctx, []PlanItem{derivativePlan()})
	require.NoError(t, err)
	wake, err := repository.MarkDispatched(ctx, rows[0].ID, rows[0].Version)
	require.NoError(t, err)
	_, err = repository.Claim(ctx, wake, "worker-a", time.Minute)
	require.True(t, errors.Is(err, ErrGenerationFence))
	var row WorkItem
	require.NoError(t, db.First(&row, "id = ?", rows[0].ID).Error)
	require.Equal(t, StateCancelled, row.State)
}

func TestMaterializationGenerationFencePersistsTerminalCancellation(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusCompleted,
	}).Error)
	rows, err := repository.UpsertPlan(ctx, []PlanItem{derivativePlan()})
	require.NoError(t, err)
	wake, err := repository.MarkDispatched(ctx, rows[0].ID, rows[0].Version)
	require.NoError(t, err)
	claimed, err := repository.Claim(ctx, wake, "worker-a", time.Minute)
	require.NoError(t, err)
	running, err := repository.BeginProvider(ctx, claimed.ID, claimed.LeaseToken)
	require.NoError(t, err)
	_, err = repository.SaveProviderResult(ctx, running.ID, running.LeaseToken, ProviderResult{
		Content: `{"results":[]}`,
	})
	require.NoError(t, err)
	require.NoError(t, db.Model(&derivativeTestKnowledge{}).
		Where("id = ?", "knowledge-1").
		Update("processing_generation", "generation-2").Error)
	require.NoError(t, repository.CompleteMaterializationOutcome(
		ctx, running.ID, running.LeaseToken, "degraded", "coverage incomplete",
	))
	var row WorkItem
	require.NoError(t, db.First(&row, "id = ?", running.ID).Error)
	require.Equal(t, StateCancelled, row.State)
	require.Equal(t, "generation_changed", row.LastErrorCode)
	require.Empty(t, row.LeaseToken)
}

func TestCompletedMaterializationPersistsDegradedBusinessOutcome(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusCompleted,
	}).Error)
	rows, err := repository.UpsertPlan(ctx, []PlanItem{derivativePlan()})
	require.NoError(t, err)
	wake, err := repository.MarkDispatched(ctx, rows[0].ID, rows[0].Version)
	require.NoError(t, err)
	claimed, err := repository.Claim(ctx, wake, "worker-a", time.Minute)
	require.NoError(t, err)
	running, err := repository.BeginProvider(ctx, claimed.ID, claimed.LeaseToken)
	require.NoError(t, err)
	_, err = repository.SaveProviderResult(ctx, running.ID, running.LeaseToken, ProviderResult{
		Content: `{"results":[]}`,
	})
	require.NoError(t, err)
	require.NoError(t, repository.CompleteMaterializationOutcome(
		ctx, running.ID, running.LeaseToken, "degraded", "coverage incomplete",
	))
	var row WorkItem
	require.NoError(t, db.First(&row, "id = ?", running.ID).Error)
	require.Equal(t, StateCompleted, row.State)
	require.Equal(t, "degraded", row.OutcomeStatus)
	require.Equal(t, "coverage incomplete", row.OutcomeDetail)
}

type derivativeWakeCapture struct {
	task *asynq.Task
	opts []asynq.Option
}

func (c *derivativeWakeCapture) Enqueue(
	task *asynq.Task,
	opts ...asynq.Option,
) (*asynq.TaskInfo, error) {
	c.task = task
	c.opts = opts
	return &asynq.TaskInfo{ID: "wake"}, nil
}

type checkpointGraphHandler struct {
	repository       *Repository
	checkpointBefore bool
}

func (h *checkpointGraphHandler) Handle(ctx context.Context, _ *asynq.Task) error {
	messages := []chat.Message{{Role: "user", Content: "stable graph request"}}
	if err := BeginProviderForContext(ctx); err != nil {
		return err
	}
	if err := CheckpointChatResponse(
		ctx, "model-a", messages, nil,
		&types.ChatResponse{Content: `{"nodes":["a"]}`},
	); err != nil {
		return err
	}
	var count int64
	if err := h.repository.db.WithContext(ctx).Model(&ProviderCall{}).Count(&count).Error; err != nil {
		return err
	}
	h.checkpointBefore = count == 1
	return nil
}

type checkpointReplayFailureHandler struct{}

func (*checkpointReplayFailureHandler) Handle(ctx context.Context, _ *asynq.Task) error {
	messages := []chat.Message{{Role: "user", Content: "stable replay request"}}
	decodeErr := errors.New("decode structured response")
	if _, found, err := LookupChatCheckpoint(ctx, "model-a", messages, nil); err != nil {
		return err
	} else if !found {
		if err := BeginProviderForContext(ctx); err != nil {
			return err
		}
		if err := CheckpointChatResponse(
			ctx, "model-a", messages, nil,
			&types.ChatResponse{Content: "invalid structured result"},
		); err != nil {
			return err
		}
	}
	if err := RejectChatCheckpoint(ctx, "model-a", messages, nil, decodeErr); err != nil {
		return err
	}
	return ProviderContractRejected(decodeErr)
}

func TestWakePayloadContainsOnlyIdentityAndWorkerPersistsResultBeforeCompletion(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusFinalizing,
	}).Error)
	enqueuer := &derivativeWakeCapture{}
	rows, err := repository.PublishPlan(ctx, enqueuer, []PlanItem{{
		TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1", ProcessingAttempt: 1,
		ItemID: "graph_chunk[0]", WorkKind: WorkGraph,
		Payload: types.JSON(`{"chunk_ids":["chunk-secret-business-payload"]}`),
		ModelID: "model-a", ModelTenantID: 9, ResourcePoolID: "pool-a",
	}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, enqueuer.task)
	require.Equal(t, TypeWake, enqueuer.task.Type())
	require.NotContains(t, string(enqueuer.task.Payload()), "chunk-secret-business-payload")
	var wake WakePayload
	require.NoError(t, json.Unmarshal(enqueuer.task.Payload(), &wake))
	require.Equal(t, rows[0].ID, wake.WorkItemID)

	graphHandler := &checkpointGraphHandler{repository: repository}
	worker := &Worker{
		repository: repository, chunkExtractor: graphHandler,
		owner: "worker-test",
	}
	require.NoError(t, worker.Handle(ctx, enqueuer.task))
	require.True(t, graphHandler.checkpointBefore)

	var item WorkItem
	require.NoError(t, db.First(&item, "id = ?", rows[0].ID).Error)
	require.Equal(t, StateCompleted, item.State)
	require.Equal(t, 1, item.ProviderAttempts)
	var resultCount int64
	require.NoError(t, db.Model(&Result{}).Where("work_item_id = ?", item.ID).Count(&resultCount).Error)
	require.EqualValues(t, 1, resultCount)
	var callCount int64
	require.NoError(t, db.Model(&ProviderCall{}).Where("work_item_id = ?", item.ID).Count(&callCount).Error)
	require.EqualValues(t, 1, callCount)

	// Duplicate delivery is fenced by state/dispatch epoch and never invokes
	// the business handler or creates another provider checkpoint.
	graphHandler.checkpointBefore = false
	require.NoError(t, worker.Handle(ctx, enqueuer.task))
	require.False(t, graphHandler.checkpointBefore)
	require.NoError(t, db.Model(&ProviderCall{}).Where("work_item_id = ?", item.ID).Count(&callCount).Error)
	require.EqualValues(t, 1, callCount)
}

func TestContractRejectedCheckpointAdvancesProviderBudgetAndAllowsFreshCall(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusFinalizing,
	}).Error)
	enqueuer := &derivativeWakeCapture{}
	rows, err := repository.PublishPlan(ctx, enqueuer, []PlanItem{{
		TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1", ProcessingAttempt: 1,
		ItemID: "graph_chunk[0]", WorkKind: WorkGraph, Payload: types.JSON(`{}`),
		ModelID: "model-a", ModelTenantID: 9, ResourcePoolID: "pool-a",
	}})
	require.NoError(t, err)
	worker := &Worker{
		repository: repository, chunkExtractor: &checkpointReplayFailureHandler{},
		owner: "worker-test",
	}

	require.NoError(t, worker.Handle(ctx, enqueuer.task))
	var first WorkItem
	require.NoError(t, db.First(&first, "id = ?", rows[0].ID).Error)
	require.Equal(t, StateRetryWait, first.State)
	require.Equal(t, 1, first.ProviderAttempts)
	require.Zero(t, first.MaterializeAttempts)

	require.NoError(t, db.Model(&WorkItem{}).Where("id = ?", first.ID).
		Updates(map[string]any{
			"next_attempt_at": time.Now().UTC().Add(-time.Minute),
			"updated_at":      time.Now().UTC().Add(-time.Minute),
		}).Error)
	require.NoError(t, db.First(&first, "id = ?", first.ID).Error)
	secondWake, err := repository.MarkDispatched(ctx, first.ID, first.Version)
	require.NoError(t, err)
	raw, err := json.Marshal(secondWake)
	require.NoError(t, err)
	require.NoError(t, worker.Handle(ctx, asynq.NewTask(TypeWake, raw)))

	var replayed WorkItem
	require.NoError(t, db.First(&replayed, "id = ?", first.ID).Error)
	require.Equal(t, StateRetryWait, replayed.State)
	require.Zero(t, replayed.MaterializeAttempts)
	require.Equal(t, 2, replayed.ProviderAttempts)
	var calls []ProviderCall
	require.NoError(t, db.Where("work_item_id = ?", first.ID).Order("attempt").Find(&calls).Error)
	require.Len(t, calls, 2)
	require.Equal(t, []int{1, 2}, []int{calls[0].Attempt, calls[1].Attempt})
	require.Equal(t, ProviderCallInvalidContract, calls[0].Disposition)
	require.Equal(t, ProviderCallInvalidContract, calls[1].Disposition)
}

func TestExpiredProviderLeaseWithoutCheckpointBecomesUnknown(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusFinalizing,
	}).Error)
	rows, err := repository.UpsertPlan(ctx, []PlanItem{derivativePlan()})
	require.NoError(t, err)
	wake, err := repository.MarkDispatched(ctx, rows[0].ID, rows[0].Version)
	require.NoError(t, err)
	claimed, err := repository.Claim(ctx, wake, "worker-a", time.Millisecond)
	require.NoError(t, err)
	_, err = repository.BeginProvider(ctx, claimed.ID, claimed.LeaseToken)
	require.NoError(t, err)
	require.NoError(t, db.Model(&WorkItem{}).Where("id = ?", claimed.ID).
		Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error)
	unknown, err := repository.RecoverExpiredLeases(ctx, 10)
	require.NoError(t, err)
	require.Len(t, unknown, 1)
	var item WorkItem
	require.NoError(t, db.First(&item, "id = ?", claimed.ID).Error)
	require.Equal(t, StateProviderUnknown, item.State)
}

func TestFinalizerPublishesWithoutModelRouteAndWaitsForTerminalSiblings(t *testing.T) {
	repository, db := derivativeRepositoryForTest(t)
	ctx := context.Background()
	require.NoError(t, db.Create(&derivativeTestKnowledge{
		ID: "knowledge-1", TenantID: 7, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusFinalizing,
	}).Error)

	siblings, err := repository.UpsertPlan(ctx, []PlanItem{
		derivativePlan(),
		{
			TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
			ProcessingGeneration: "generation-1", ProcessingAttempt: 1,
			ItemID: "graph_chunk[0]", WorkKind: WorkGraph,
			Payload: types.JSON(`{"chunk_ids":["chunk-1"]}`), ResourcePoolID: "pool-b",
		},
	})
	require.NoError(t, err)
	require.Len(t, siblings, 2)

	enqueuer := &derivativeWakeCapture{}
	finalizers, err := repository.PublishPlan(ctx, enqueuer, []PlanItem{{
		TenantID: 7, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1", ProcessingAttempt: 1,
		ItemID: "finalizer", WorkKind: WorkFinalizer, Payload: types.JSON(`{}`),
	}})
	require.NoError(t, err)
	require.Len(t, finalizers, 1)
	require.Empty(t, finalizers[0].ModelID)
	require.Empty(t, finalizers[0].ResourcePoolID)

	rows, ready, err := repository.FinalizerSiblings(ctx, finalizers[0])
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.False(t, ready)

	require.NoError(t, db.Model(&WorkItem{}).
		Where("id = ?", siblings[0].ID).
		Updates(map[string]any{"state": StateCompleted, "completed_at": time.Now().UTC()}).Error)
	require.NoError(t, db.Model(&WorkItem{}).
		Where("id = ?", siblings[1].ID).
		Updates(map[string]any{
			"state": StateFailed, "completed_at": time.Now().UTC(),
			"last_error_message": "provider failed",
		}).Error)

	rows, ready, err = repository.FinalizerSiblings(ctx, finalizers[0])
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, []string{StateFailed, StateCompleted}, []string{rows[0].State, rows[1].State})
}
