package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/custom/modules/workloadbudget"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// KnowledgePostProcessService acts as an orchestrator for all post-processing tasks
// after a document has been parsed and split into chunks (including multimodal OCR/Caption).
type KnowledgePostProcessService struct {
	knowledgeRepo interfaces.KnowledgeRepository
	kbService     interfaces.KnowledgeBaseService
	chunkService  interfaces.ChunkService
	taskEnqueuer  interfaces.TaskEnqueuer
	pendingRepo   interfaces.TaskPendingOpsRepository
	redisClient   *redis.Client
	spanTracker   SpanTracker
	splitManager  *documentsplit.Manager
}

// enrichmentPlan describes the asynchronous work spawned after the primary
// document parse has produced its chunks and search indexes.
//
// Wiki is intentionally present in the plan but absent from
// finalizationSubtaskCount. Wiki ingestion is a KB-scoped, durable background
// pipeline with its own pending/active status, so it does not consume a
// per-document counter slot. The final promotion nevertheless requires both
// the counter to reach zero and WikiStatus to become terminal, ensuring the
// end-to-end document is never reported completed while Wiki still runs.
type enrichmentPlan struct {
	spawnSummary       bool
	questionBatchCount int
	spawnWiki          bool
	graphChunkCount    int
}

const durableEnrichmentPlanStage = "enrichment"

type durableQuestionBatch struct {
	ChunkIDs     []string `json:"chunk_ids"`
	BatchIndex   int      `json:"batch_index"`
	PrevChunkID  string   `json:"prev_chunk_id,omitempty"`
	NextChunkID  string   `json:"next_chunk_id,omitempty"`
	SparseSample bool     `json:"sparse_sample,omitempty"`
}

type durableGraphTask struct {
	ChunkID    string   `json:"chunk_id,omitempty"`
	ChunkIDs   []string `json:"chunk_ids,omitempty"`
	ChunkIndex int      `json:"chunk_index"`
	ModelID    string   `json:"model_id"`
}

func (t durableGraphTask) chunkIDs() []string {
	if len(t.ChunkIDs) > 0 {
		return t.ChunkIDs
	}
	if strings.TrimSpace(t.ChunkID) != "" {
		return []string{t.ChunkID}
	}
	return nil
}

func graphChunkIDBatches(chunks []*types.Chunk, batchSize int) [][]string {
	if batchSize <= 0 {
		batchSize = 1
	}
	result := make([][]string, 0, (len(chunks)+batchSize-1)/batchSize)
	for start := 0; start < len(chunks); start += batchSize {
		end := min(len(chunks), start+batchSize)
		ids := make([]string, 0, end-start)
		for _, chunk := range chunks[start:end] {
			if chunk != nil && strings.TrimSpace(chunk.ID) != "" {
				ids = append(ids, chunk.ID)
			}
		}
		if len(ids) > 0 {
			result = append(result, ids)
		}
	}
	return result
}

// durableEnrichmentFanout is the exact task set whose size seeds
// pending_subtasks_count. It replaces the core fan-out plan in the same
// processing->finalizing CAS, so a retry never recomputes children from a KB
// config or chunk set that may have changed since the counter was seeded.
type durableEnrichmentFanout struct {
	Stage                string                 `json:"stage"`
	Version              int                    `json:"version"`
	TenantID             uint64                 `json:"tenant_id"`
	KnowledgeID          string                 `json:"knowledge_id"`
	KnowledgeBaseID      string                 `json:"knowledge_base_id"`
	ProcessingGeneration string                 `json:"processing_generation"`
	Language             string                 `json:"language,omitempty"`
	Attempt              int                    `json:"attempt,omitempty"`
	Tracing              types.TracingContext   `json:"tracing,omitempty"`
	TextChunkCount       int                    `json:"text_chunk_count"`
	SpawnSummary         bool                   `json:"spawn_summary"`
	SpawnWiki            bool                   `json:"spawn_wiki"`
	QuestionCount        int                    `json:"question_count,omitempty"`
	QuestionBatches      []durableQuestionBatch `json:"question_batches,omitempty"`
	GraphTasks           []durableGraphTask     `json:"graph_tasks,omitempty"`
	QuestionChunkCount   int                    `json:"question_chunk_count,omitempty"`
	QuestionBatchCount   int                    `json:"question_batch_count,omitempty"`
	GraphChunkCount      int                    `json:"graph_chunk_count,omitempty"`
	GraphBatchCount      int                    `json:"graph_batch_count,omitempty"`
	GraphBatchSize       int                    `json:"graph_batch_size,omitempty"`
	GraphModelID         string                 `json:"graph_model_id,omitempty"`
}

func (p durableEnrichmentFanout) validate(payload types.KnowledgePostProcessPayload) error {
	if p.Stage != durableEnrichmentPlanStage ||
		(p.Version != 1 && p.Version != 2 && p.Version != 3) ||
		p.TenantID != payload.TenantID || p.KnowledgeID != payload.KnowledgeID ||
		p.KnowledgeBaseID != payload.KnowledgeBaseID ||
		p.ProcessingGeneration != payload.ProcessingGeneration {
		return errors.New("durable enrichment fanout identity mismatch")
	}
	for _, batch := range p.QuestionBatches {
		if len(batch.ChunkIDs) == 0 || batch.BatchIndex < 0 {
			return errors.New("durable enrichment fanout has invalid question batch")
		}
	}
	for _, graphTask := range p.GraphTasks {
		if len(graphTask.chunkIDs()) == 0 || graphTask.ModelID == "" || graphTask.ChunkIndex < 0 {
			return errors.New("durable enrichment fanout has invalid graph task")
		}
	}
	if p.Version == 2 || p.Version == 3 {
		if p.QuestionChunkCount < 0 || p.QuestionBatchCount < 0 ||
			p.GraphChunkCount < 0 ||
			(p.QuestionBatchCount > 0 && p.QuestionChunkCount == 0) ||
			(p.GraphChunkCount > 0 && strings.TrimSpace(p.GraphModelID) == "") {
			return errors.New("durable enrichment fanout has invalid paged counts")
		}
	}
	if p.Version == 3 &&
		(p.GraphBatchCount < 0 ||
			(p.GraphChunkCount > 0 && p.GraphBatchCount == 0) ||
			(p.GraphChunkCount == 0 && p.GraphBatchCount != 0) ||
			(p.GraphChunkCount > 0 && p.GraphBatchSize <= 0) ||
			(p.GraphChunkCount > 0 && p.GraphBatchCount !=
				(p.GraphChunkCount+p.GraphBatchSize-1)/max(1, p.GraphBatchSize))) {
		return errors.New("durable enrichment fanout has invalid graph batch counts")
	}
	return nil
}

func (p durableEnrichmentFanout) subtaskCount() int {
	if p.Version == 3 {
		count := p.QuestionBatchCount + p.GraphBatchCount
		if p.SpawnSummary {
			count++
		}
		return count
	}
	if p.Version == 2 {
		count := p.QuestionBatchCount + p.GraphChunkCount
		if p.SpawnSummary {
			count++
		}
		return count
	}
	count := len(p.QuestionBatches) + len(p.GraphTasks)
	if p.SpawnSummary {
		count++
	}
	return count
}

type postProcessGenerationFinalizer interface {
	FinalizeSubtaskGenerationItem(
		context.Context, uint64, string, string, string, string,
	) (int, bool, error)
}

type postProcessGenerationOutcomeFinalizer interface {
	FinalizeSubtaskGenerationItemOutcome(
		context.Context, uint64, string, string, string, string, string, string,
	) (int, bool, error)
}

func finalizePostProcessGenerationSlot(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	payload types.KnowledgePostProcessPayload,
	itemID string,
	outcomeStatus string,
	outcomeDetail string,
) error {
	if outcomeStatus != "" {
		finalizer, ok := repo.(postProcessGenerationOutcomeFinalizer)
		if !ok || finalizer == nil {
			return errors.New("postprocess generation outcome finalizer is unavailable")
		}
		_, _, err := finalizer.FinalizeSubtaskGenerationItemOutcome(
			ctx,
			payload.TenantID,
			payload.KnowledgeID,
			payload.KnowledgeBaseID,
			payload.ProcessingGeneration,
			itemID,
			outcomeStatus,
			outcomeDetail,
		)
		return err
	}
	finalizer, ok := repo.(postProcessGenerationFinalizer)
	if !ok || finalizer == nil {
		return errors.New("postprocess generation finalizer is unavailable")
	}
	_, _, err := finalizer.FinalizeSubtaskGenerationItem(
		ctx,
		payload.TenantID,
		payload.KnowledgeID,
		payload.KnowledgeBaseID,
		payload.ProcessingGeneration,
		itemID,
	)
	return err
}

func (p enrichmentPlan) finalizationSubtaskCount() int {
	count := p.questionBatchCount + p.graphChunkCount
	if p.spawnSummary {
		count++
	}
	return count
}

func NewKnowledgePostProcessService(
	knowledgeRepo interfaces.KnowledgeRepository,
	kbService interfaces.KnowledgeBaseService,
	chunkService interfaces.ChunkService,
	taskEnqueuer interfaces.TaskEnqueuer,
	pendingRepo interfaces.TaskPendingOpsRepository,
	redisClient *redis.Client,
	spanTracker SpanTracker,
	splitManager *documentsplit.Manager,
) interfaces.TaskHandler {
	return &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		kbService:     kbService,
		chunkService:  chunkService,
		taskEnqueuer:  taskEnqueuer,
		pendingRepo:   pendingRepo,
		redisClient:   redisClient,
		spanTracker:   spanTracker,
		splitManager:  splitManager,
	}
}

func (s *KnowledgePostProcessService) tracker() SpanTracker {
	if s.spanTracker == nil {
		return noopSpanTracker{}
	}
	return s.spanTracker
}

func splitEnrichmentStrataCaps(
	plan *documentsplit.Plan, cfg documentsplit.Config,
) (question, graph int64) {
	question = int64(cfg.QuestionStrata)
	graph = int64(cfg.GraphStrata)
	if plan == nil {
		return question, graph
	}
	switch strings.ToLower(strings.TrimPrefix(
		strings.TrimSpace(plan.SourceType), ".",
	)) {
	case "csv", "xls", "xlsx":
		question = int64(cfg.TableQuestionStrata)
		graph = int64(cfg.TableGraphStrata)
	}
	return question, graph
}

func (s *KnowledgePostProcessService) buildPagedSplitEnrichmentPlan(
	ctx context.Context,
	payload types.KnowledgePostProcessPayload,
	kb *types.KnowledgeBase,
	eff types.EffectiveProcessConfig,
	attempt int,
) (durableEnrichmentFanout, bool, error) {
	var empty durableEnrichmentFanout
	if s.splitManager == nil {
		return empty, false, nil
	}
	splitPlan, err := s.splitManager.GetPlanForGeneration(
		ctx, payload.TenantID, payload.KnowledgeID, payload.ProcessingGeneration,
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Ordinary documents do not have a physical split plan, but their
		// chunks still carry the immutable processing generation. Keep them on
		// the same generation-scoped, paged path as physically split
		// documents. The legacy ListChunksByKnowledgeID path intentionally
		// returns only text chunks and would otherwise omit multimodal OCR and
		// caption children from graph extraction.
		splitPlan = nil
		err = nil
	}
	if err != nil {
		return empty, false, fmt.Errorf("load physical split enrichment plan: %w", err)
	}
	textTypes := []types.ChunkType{
		types.ChunkTypeText, types.ChunkTypeImageOCR, types.ChunkTypeImageCaption,
	}
	textCount, err := s.splitManager.CountGenerationChunks(
		ctx, payload.TenantID, payload.KnowledgeID,
		payload.ProcessingGeneration, textTypes,
	)
	if err != nil {
		return empty, true, err
	}
	plainCount, err := s.splitManager.CountGenerationChunks(
		ctx, payload.TenantID, payload.KnowledgeID,
		payload.ProcessingGeneration, []types.ChunkType{types.ChunkTypeText},
	)
	if err != nil {
		return empty, true, err
	}
	const maxSafePlanCount = int64(100_000_000)
	if textCount < 0 || plainCount < 0 ||
		textCount > maxSafePlanCount || plainCount > maxSafePlanCount {
		return empty, true, errors.New("physical split enrichment chunk count is outside safety bounds")
	}
	plan := durableEnrichmentFanout{
		Stage: durableEnrichmentPlanStage, Version: 3,
		TenantID: payload.TenantID, KnowledgeID: payload.KnowledgeID,
		KnowledgeBaseID:      payload.KnowledgeBaseID,
		ProcessingGeneration: payload.ProcessingGeneration,
		Language:             payload.Language, Attempt: attempt, Tracing: payload.TracingContext,
		TextChunkCount: int(textCount), SpawnSummary: textCount > 0,
		SpawnWiki: kb.IndexingStrategy.WikiEnabled && textCount > 0,
	}
	budget := workloadbudget.FromEnv()
	if plan.SpawnSummary && kb.NeedsEmbeddingModel() && eff.QuestionGenerationConfig.Enabled {
		plan.QuestionCount = eff.QuestionGenerationConfig.QuestionCount
		if plan.QuestionCount <= 0 {
			plan.QuestionCount = 3
		}
		if plan.QuestionCount > 10 {
			plan.QuestionCount = 10
		}
		questionCap := plainCount
		if splitPlan != nil {
			questionCap, _ = splitEnrichmentStrataCaps(
				splitPlan, s.splitManager.Config(),
			)
		}
		questionCap = min(questionCap, int64(budget.QuestionChunkCap(plan.QuestionCount)))
		if budget.MaxDownstreamTasks > 0 {
			maxQuestionTasks := budget.MaxDownstreamTasks
			if plan.SpawnSummary {
				maxQuestionTasks--
			}
			if maxQuestionTasks < 0 {
				maxQuestionTasks = 0
			}
			questionCap = min(
				questionCap,
				int64(maxQuestionTasks*questionGenChunkBatchSize),
			)
		}
		plan.QuestionChunkCount = int(min(plainCount, questionCap))
		plan.QuestionBatchCount =
			(plan.QuestionChunkCount + questionGenChunkBatchSize - 1) /
				questionGenChunkBatchSize
	}
	if eff.GraphEnabled && textCount > 0 {
		if strings.TrimSpace(kb.SummaryModelID) == "" {
			return empty, true, errors.New("graph extraction model is not configured")
		}
		graphCap := textCount
		if splitPlan != nil {
			_, graphCap = splitEnrichmentStrataCaps(
				splitPlan, s.splitManager.Config(),
			)
		}
		summaryTasks := 0
		if plan.SpawnSummary {
			summaryTasks = 1
		}
		graphCap = min(
			graphCap,
			int64(budget.GraphTaskCap(summaryTasks, plan.QuestionBatchCount)),
		)
		plan.GraphChunkCount = int(min(textCount, graphCap))
		plan.GraphBatchSize = budget.GraphBatchSize
		if plan.GraphBatchSize <= 0 {
			plan.GraphBatchSize = 1
		}
		plan.GraphBatchCount = budget.GraphTaskCount(plan.GraphChunkCount)
		plan.GraphModelID = kb.SummaryModelID
	}
	if err := plan.validate(payload); err != nil {
		return empty, true, err
	}
	return plan, true, nil
}

func (s *KnowledgePostProcessService) enqueuePagedSplitQuestionTasks(
	ctx context.Context,
	payload types.KnowledgePostProcessPayload,
	questionCount int,
	attempt int,
	expectedChunks int,
	expectedBatches int,
) (int, error) {
	if expectedChunks == 0 && expectedBatches == 0 {
		return 0, nil
	}
	if expectedChunks <= 0 || expectedBatches <= 0 {
		return 0, errors.New("paged split question plan has inconsistent counts")
	}
	if s.splitManager == nil {
		return 0, errors.New("paged split question planner is unavailable")
	}
	selected, logicalTotal, err := loadGenerationChunkStrata(
		ctx, s.splitManager, payload.TenantID, payload.KnowledgeID,
		payload.ProcessingGeneration,
		[]types.ChunkType{types.ChunkTypeText}, int64(expectedChunks),
	)
	if err != nil {
		return 0, err
	}
	if len(selected) != expectedChunks {
		return 0, fmt.Errorf(
			"paged split question plan changed: expected %d sampled chunks, found %d",
			expectedChunks, len(selected),
		)
	}
	sparse := logicalTotal > int64(len(selected))
	enqueued := 0
	batchIndex := 0
	for start := 0; start < len(selected); start += questionGenChunkBatchSize {
		end := min(len(selected), start+questionGenChunkBatchSize)
		batch := durableQuestionBatch{
			BatchIndex:   batchIndex,
			ChunkIDs:     make([]string, 0, end-start),
			SparseSample: sparse,
		}
		for _, chunk := range selected[start:end] {
			batch.ChunkIDs = append(batch.ChunkIDs, chunk.ID)
		}
		// Only contiguous/full selections have real in-document neighbors.
		// Sparse strata deliberately carry no false adjacency between distant
		// rows/pages; each sampled chunk still includes its precise locator.
		if !sparse {
			if start > 0 {
				batch.PrevChunkID = selected[start-1].ID
			}
			if end < len(selected) {
				batch.NextChunkID = selected[end].ID
			}
		}
		accepted, enqueueErr := s.enqueueQuestionGenerationTasks(
			ctx, payload, questionCount, attempt, []durableQuestionBatch{batch},
		)
		enqueued += accepted
		if enqueueErr != nil {
			return enqueued, enqueueErr
		}
		batchIndex++
	}
	if batchIndex != expectedBatches {
		return enqueued, fmt.Errorf(
			"paged split question plan changed: expected %d batches, found %d",
			expectedBatches, batchIndex,
		)
	}
	return enqueued, nil
}

func (s *KnowledgePostProcessService) enqueuePagedSplitGraphTasks(
	ctx context.Context,
	payload types.KnowledgePostProcessPayload,
	attempt int,
	modelID string,
	expectedChunks int,
) (int, []string, error) {
	if expectedChunks == 0 {
		return 0, nil, nil
	}
	if s.splitManager == nil {
		return 0, nil, errors.New("paged split graph planner is unavailable")
	}
	chunkTypes := []types.ChunkType{
		types.ChunkTypeText, types.ChunkTypeImageOCR, types.ChunkTypeImageCaption,
	}
	selected, _, err := loadGenerationChunkStrata(
		ctx, s.splitManager, payload.TenantID, payload.KnowledgeID,
		payload.ProcessingGeneration, chunkTypes, int64(expectedChunks),
	)
	if err != nil {
		return 0, nil, err
	}
	if len(selected) != expectedChunks {
		return 0, nil, fmt.Errorf(
			"paged split graph plan changed: expected %d chunks, found %d",
			expectedChunks, len(selected),
		)
	}
	enqueued := 0
	var unowned []string
	for position, chunk := range selected {
		ok, enqueueErr := NewChunkExtractTask(
			ctx, s.taskEnqueuer, payload.TenantID, chunk.ID, modelID,
			payload.KnowledgeID, payload.KnowledgeBaseID,
			payload.ProcessingGeneration, attempt, position,
		)
		if enqueueErr != nil {
			return enqueued, unowned, fmt.Errorf(
				"enqueue paged graph fanout chunk %d: %w", position, enqueueErr,
			)
		}
		if ok {
			enqueued++
		} else {
			unowned = append(unowned, fmt.Sprintf("graph_chunk[%d]", position))
		}
	}
	return enqueued, unowned, nil
}

func (s *KnowledgePostProcessService) enqueuePagedSplitGraphBatchTasks(
	ctx context.Context,
	payload types.KnowledgePostProcessPayload,
	attempt int,
	modelID string,
	expectedChunks int,
	expectedBatches int,
	batchSize int,
) (int, []string, error) {
	if expectedChunks == 0 && expectedBatches == 0 {
		return 0, nil, nil
	}
	if expectedChunks <= 0 || expectedBatches <= 0 || batchSize <= 0 {
		return 0, nil, errors.New("paged split graph batch plan has inconsistent counts")
	}
	if s.splitManager == nil {
		return 0, nil, errors.New("paged split graph batch planner is unavailable")
	}
	selected, _, err := loadGenerationChunkStrata(
		ctx, s.splitManager, payload.TenantID, payload.KnowledgeID,
		payload.ProcessingGeneration,
		[]types.ChunkType{
			types.ChunkTypeText,
			types.ChunkTypeImageOCR,
			types.ChunkTypeImageCaption,
		},
		int64(expectedChunks),
	)
	if err != nil {
		return 0, nil, err
	}
	if len(selected) != expectedChunks {
		return 0, nil, fmt.Errorf(
			"paged split graph batch plan changed: expected %d sampled chunks, found %d",
			expectedChunks,
			len(selected),
		)
	}
	enqueued := 0
	var unowned []string
	batches := graphChunkIDBatches(selected, batchSize)
	for batchIndex, chunkIDs := range batches {
		ok, enqueueErr := NewChunkExtractBatchTask(
			ctx,
			s.taskEnqueuer,
			payload.TenantID,
			chunkIDs,
			modelID,
			payload.KnowledgeID,
			payload.KnowledgeBaseID,
			payload.ProcessingGeneration,
			attempt,
			batchIndex,
		)
		if enqueueErr != nil {
			return enqueued, unowned, fmt.Errorf(
				"enqueue paged graph batch %d: %w",
				batchIndex,
				enqueueErr,
			)
		}
		if ok {
			enqueued++
		} else {
			unowned = append(unowned, fmt.Sprintf("graph_chunk[%d]", batchIndex))
		}
	}
	if len(batches) != expectedBatches {
		return enqueued, unowned, fmt.Errorf(
			"paged split graph batch plan changed: expected %d batches, found %d",
			expectedBatches,
			len(batches),
		)
	}
	return enqueued, unowned, nil
}

// Handle implements asynq handler for TypeKnowledgePostProcess.
func (s *KnowledgePostProcessService) Handle(ctx context.Context, task *asynq.Task) error {
	var payload types.KnowledgePostProcessPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal knowledge post process payload: %w", err)
	}
	if payload.TenantID == 0 || payload.KnowledgeID == "" || payload.KnowledgeBaseID == "" {
		return errors.New("knowledge post process: complete tenant, knowledge base and knowledge identity is required")
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.ProcessingGeneration == "" {
		if err := processownership.RepairLegacyTask(
			ctx, s.knowledgeRepo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, "knowledge post process",
		); err != nil {
			return fmt.Errorf("knowledge post process: processing generation identity is required: %w", err)
		}
		return nil
	}

	logger.Infof(ctx, "[KnowledgePostProcess] Orchestrating post processing for knowledge: %s", payload.KnowledgeID)

	if payload.Language != "" {
		ctx = context.WithValue(ctx, types.LanguageContextKey, payload.Language)
	}

	// Load through the tenant-scoped path and reject stale generations before
	// touching spans, chunks, Wiki state, or any lifecycle column. Finalizing
	// is accepted only as a deterministic replay of the same generation after
	// the initial processing->finalizing CAS succeeded but fan-out was cut
	// short by a worker crash.
	knowledge, err := s.knowledgeRepo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("get knowledge %s: %w", payload.KnowledgeID, err)
	}
	if knowledge == nil || knowledge.KnowledgeBaseID != payload.KnowledgeBaseID ||
		knowledge.ProcessingGeneration != payload.ProcessingGeneration {
		logger.Infof(ctx, "[KnowledgePostProcess] Stale tenant/KB/generation for %s, skipping", payload.KnowledgeID)
		return nil
	}
	if knowledge.ParseStatus != types.ParseStatusProcessing && knowledge.ParseStatus != types.ParseStatusFinalizing {
		logger.Infof(ctx, "[KnowledgePostProcess] Knowledge %s is in %s, skipping generation fan-out.",
			payload.KnowledgeID, knowledge.ParseStatus)
		return nil
	}
	replayingFinalizing := knowledge.ParseStatus == types.ParseStatusFinalizing

	// Resolve attempt: payload carries it from the upstream stage, but
	// fall back to the latest known attempt for compatibility with
	// in-flight tasks queued before this code shipped.
	attempt := payload.Attempt
	if attempt <= 0 {
		attempt = s.tracker().LatestAttempt(ctx, payload.KnowledgeID)
	}

	// Core fan-out tasks finish before this handoff. Their terminal failures
	// are generation-scoped PostgreSQL outcomes, not Redis counters, so they
	// survive restarts and participate in the same final aggregate as
	// summary/question/graph work.
	var generationOutcomes enrichmentoutcome.Aggregate
	if outcomeStore, ok := s.knowledgeRepo.(enrichmentoutcome.GenerationStore); ok {
		generationOutcomes, err = outcomeStore.GetGenerationOutcomeAggregate(
			ctx,
			payload.TenantID,
			payload.KnowledgeID,
			payload.KnowledgeBaseID,
			payload.ProcessingGeneration,
		)
		if err != nil {
			return fmt.Errorf("load core fanout outcomes for %s: %w", payload.KnowledgeID, err)
		}
	}

	// Close the multimodal stage span (parent enqueued it as "running"
	// and we never see the per-image fan-in here other than by reaching
	// post-process). If the parent skipped multimodal entirely, the
	// stage row will already be in "skipped" state and EndSpan is a
	// no-op for missing rows. Per-image success/failure counts are NOT
	// aggregated here — the frontend already walks the children when
	// rendering the multimodal stage detail and counts them itself,
	// avoiding an extra query path.
	if mm := s.tracker().LookupStage(ctx, payload.KnowledgeID, attempt, types.StageMultimodal); mm != nil &&
		mm.Kind == types.SpanKindStage {
		s.tracker().EndSpan(ctx, mm, types.JSONMap{
			"terminal_outcomes": generationOutcomes.Total,
			"failed_images":     generationOutcomes.Failed,
			"degraded_images":   generationOutcomes.Degraded,
		})
	}

	postSpan := s.tracker().BeginStage(ctx, payload.KnowledgeID, attempt, types.StagePostProcess, nil)

	var durablePlan durableEnrichmentFanout
	var durablePlanBytes []byte
	if replayingFinalizing {
		if err := json.Unmarshal(knowledge.ProcessingFanout, &durablePlan); err != nil {
			return fmt.Errorf("decode durable enrichment fanout: %w", err)
		}
		if err := durablePlan.validate(payload); err != nil {
			return err
		}
	} else {
		// The first processing pass snapshots the exact KB configuration and
		// chunk IDs. The resulting task set is persisted by the lifecycle CAS
		// and is the only source used by finalizing replays.
		kb, err := s.kbService.GetKnowledgeBaseByIDOnly(ctx, payload.KnowledgeBaseID)
		if err != nil {
			return fmt.Errorf("get knowledge base %s: %w", payload.KnowledgeBaseID, err)
		}
		if kb == nil {
			return fmt.Errorf("get knowledge base %s: service returned nil without error", payload.KnowledgeBaseID)
		}
		if kb.ID != payload.KnowledgeBaseID || kb.TenantID != payload.TenantID {
			return fmt.Errorf("get knowledge base %s: tenant or identity mismatch", payload.KnowledgeBaseID)
		}
		processOverrides, _ := knowledge.ProcessOverrides()
		eff := ResolveProcessConfig(kb, processOverrides)
		var pagedSplit bool
		durablePlan, pagedSplit, err = s.buildPagedSplitEnrichmentPlan(
			ctx, payload, kb, eff, attempt,
		)
		if err != nil {
			return err
		}
		if !pagedSplit {
			chunks, err := s.chunkService.ListChunksByKnowledgeID(ctx, payload.KnowledgeID)
			if err != nil {
				return fmt.Errorf("list chunks for knowledge %s: %w", payload.KnowledgeID, err)
			}
			var textChunks []*types.Chunk
			for _, chunk := range chunks {
				if chunk.ChunkType == types.ChunkTypeText || chunk.ChunkType == types.ChunkTypeImageOCR ||
					chunk.ChunkType == types.ChunkTypeImageCaption {
					textChunks = append(textChunks, chunk)
				}
			}
			durablePlan = durableEnrichmentFanout{
				Stage:                durableEnrichmentPlanStage,
				Version:              1,
				TenantID:             payload.TenantID,
				KnowledgeID:          payload.KnowledgeID,
				KnowledgeBaseID:      payload.KnowledgeBaseID,
				ProcessingGeneration: payload.ProcessingGeneration,
				Language:             payload.Language,
				Attempt:              attempt,
				Tracing:              payload.TracingContext,
				TextChunkCount:       len(textChunks),
				SpawnSummary:         len(textChunks) > 0,
				SpawnWiki:            kb.IndexingStrategy.WikiEnabled && len(textChunks) > 0,
			}
			budget := workloadbudget.FromEnv()
			if durablePlan.SpawnSummary && kb.NeedsEmbeddingModel() && eff.QuestionGenerationConfig.Enabled {
				questionCount := eff.QuestionGenerationConfig.QuestionCount
				if questionCount <= 0 {
					questionCount = 3
				}
				if questionCount > 10 {
					questionCount = 10
				}
				durablePlan.QuestionCount = questionCount
				var questionChunks []*types.Chunk
				for _, chunk := range textChunks {
					if chunk.ChunkType == types.ChunkTypeText {
						questionChunks = append(questionChunks, chunk)
					}
				}
				sort.Slice(questionChunks, func(i, j int) bool {
					return questionChunks[i].StartAt < questionChunks[j].StartAt
				})
				questionChunkCap := budget.QuestionChunkCap(questionCount)
				if budget.MaxDownstreamTasks > 0 {
					maxQuestionTasks := budget.MaxDownstreamTasks
					if durablePlan.SpawnSummary {
						maxQuestionTasks--
					}
					if maxQuestionTasks < 0 {
						maxQuestionTasks = 0
					}
					questionChunkCap = min(
						questionChunkCap,
						maxQuestionTasks*questionGenChunkBatchSize,
					)
				}
				questionChunks = workloadbudget.Stratified(questionChunks, questionChunkCap)
				sparseQuestions := len(questionChunks) < len(textChunks)
				durablePlan.QuestionChunkCount = len(questionChunks)
				for start, batchIndex := 0, 0; start < len(questionChunks); start, batchIndex = start+questionGenChunkBatchSize, batchIndex+1 {
					end := start + questionGenChunkBatchSize
					if end > len(questionChunks) {
						end = len(questionChunks)
					}
					batch := durableQuestionBatch{
						BatchIndex:   batchIndex,
						SparseSample: sparseQuestions,
					}
					for _, chunk := range questionChunks[start:end] {
						batch.ChunkIDs = append(batch.ChunkIDs, chunk.ID)
					}
					if !sparseQuestions && start > 0 {
						batch.PrevChunkID = questionChunks[start-1].ID
					}
					if !sparseQuestions && end < len(questionChunks) {
						batch.NextChunkID = questionChunks[end].ID
					}
					durablePlan.QuestionBatches = append(durablePlan.QuestionBatches, batch)
				}
				durablePlan.QuestionBatchCount = len(durablePlan.QuestionBatches)
			}
			if eff.GraphEnabled {
				if strings.TrimSpace(kb.SummaryModelID) == "" {
					return errors.New("graph extraction model is not configured")
				}
				summaryTasks := 0
				if durablePlan.SpawnSummary {
					summaryTasks = 1
				}
				graphChunks := workloadbudget.Stratified(
					textChunks,
					budget.GraphTaskCap(summaryTasks, len(durablePlan.QuestionBatches)),
				)
				durablePlan.GraphChunkCount = len(graphChunks)
				durablePlan.GraphBatchSize = budget.GraphBatchSize
				if durablePlan.GraphBatchSize <= 0 {
					durablePlan.GraphBatchSize = 1
				}
				for batchIndex, chunkIDs := range graphChunkIDBatches(
					graphChunks,
					durablePlan.GraphBatchSize,
				) {
					durablePlan.GraphTasks = append(durablePlan.GraphTasks, durableGraphTask{
						ChunkIDs: chunkIDs, ChunkIndex: batchIndex, ModelID: kb.SummaryModelID,
					})
				}
				durablePlan.GraphBatchCount = len(durablePlan.GraphTasks)
			}
		}
		if err := durablePlan.validate(payload); err != nil {
			return err
		}
		durablePlanBytes, err = json.Marshal(durablePlan)
		if err != nil {
			return fmt.Errorf("encode durable enrichment fanout: %w", err)
		}
	}

	willSpawnSummary := durablePlan.SpawnSummary
	willSpawnWiki := durablePlan.SpawnWiki
	questionBatchCount := len(durablePlan.QuestionBatches)
	graphChunkCount := len(durablePlan.GraphTasks)
	if durablePlan.Version == 2 {
		questionBatchCount = durablePlan.QuestionBatchCount
		graphChunkCount = durablePlan.GraphChunkCount
	} else if durablePlan.Version == 3 {
		questionBatchCount = durablePlan.QuestionBatchCount
		graphChunkCount = durablePlan.GraphBatchCount
	}
	expectedSubtasks := durablePlan.subtaskCount()

	enqueuedWiki := false
	wikiAlreadySettled := false
	wikiEnqueueError := ""

	// enteredFinalizing covers both the first exact generation transition and
	// a replay of that same finalizing generation. Replays deterministically
	// fill a fan-out interrupted between lifecycle commit and task enqueue.
	enteredFinalizing := false

	switch {
	case replayingFinalizing:
		enteredFinalizing = true
		logger.Infof(ctx,
			"[KnowledgePostProcess] Replaying stable enrichment fan-out for %s generation %s.",
			payload.KnowledgeID, payload.ProcessingGeneration)
	case expectedSubtasks == 0 && !willSpawnWiki:
		// Nothing to enrich. The exact processing generation is consumed by
		// the same CAS that clears its durable core fan-out plan. A terminal
		// core fan-out failure must remain externally visible even though
		// there are no later summary/question/graph slots to aggregate it.
		now := time.Now()
		enrichmentStatus := types.EnrichmentStatusNone
		switch generationOutcomes.Status() {
		case enrichmentoutcome.StatusFailed:
			enrichmentStatus = types.EnrichmentStatusFailed
		case enrichmentoutcome.StatusDegraded:
			enrichmentStatus = types.EnrichmentStatusDegraded
		case enrichmentoutcome.StatusCompleted:
			enrichmentStatus = types.EnrichmentStatusCompleted
		}
		updates := map[string]interface{}{
			"parse_status":           types.ParseStatusCompleted,
			"pending_subtasks_count": 0,
			"enrichment_status":      enrichmentStatus,
			"wiki_status":            types.WikiStatusNone,
			"wiki_error_message":     "",
			"processing_owner":       "",
			"processing_fanout":      nil,
			"processed_at":           now,
			"updated_at":             now,
		}
		if durablePlan.TextChunkCount > 0 {
			updates["summary_status"] = types.SummaryStatusNone
		}
		completed, err := compareAndSwapProcessingGeneration(
			ctx, s.knowledgeRepo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
			[]string{types.ParseStatusProcessing}, updates,
		)
		if err != nil {
			wrapped := fmt.Errorf("mark knowledge %s completed with no enrichment subtasks: %w", payload.KnowledgeID, err)
			s.tracker().FailSpan(ctx, postSpan, "POSTPROCESS_COMPLETE_FAILED", wrapped.Error(), wrapped)
			s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
				types.SpanStatusFailed, nil, "POSTPROCESS_COMPLETE_FAILED", wrapped.Error())
			return wrapped
		}
		if !completed {
			logger.Infof(ctx, "[KnowledgePostProcess] Completion CAS lost for %s generation %s, skipping fan-out.",
				payload.KnowledgeID, payload.ProcessingGeneration)
			return nil
		}
		logger.Infof(ctx, "[KnowledgePostProcess] Knowledge %s marked completed (no enrichment subtasks).",
			payload.KnowledgeID)
	default:
		// Flip the exact processing generation to finalizing and seed the
		// counter in one statement. summary_status and processing_fanout are
		// part of this CAS so an old postprocess task cannot mutate a new run.
		summaryStatus := types.SummaryStatusNone
		if willSpawnSummary {
			summaryStatus = types.SummaryStatusPending
		}
		promoted, err := compareAndSwapProcessingGeneration(
			ctx, s.knowledgeRepo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
			[]string{types.ParseStatusProcessing}, map[string]interface{}{
				"parse_status":           types.ParseStatusFinalizing,
				"pending_subtasks_count": expectedSubtasks,
				"enrichment_status": func() string {
					if expectedSubtasks > 0 {
						return types.EnrichmentStatusPending
					}
					return types.EnrichmentStatusNone
				}(),
				"wiki_status": func() string {
					if willSpawnWiki {
						return types.WikiStatusPending
					}
					return types.WikiStatusNone
				}(),
				"wiki_error_message": "",
				"summary_status":     summaryStatus,
				"processing_owner":   "",
				"processing_fanout":  types.JSON(durablePlanBytes),
				"updated_at":         time.Now(),
			},
		)
		if err != nil {
			wrapped := fmt.Errorf("set knowledge %s finalizing: %w", payload.KnowledgeID, err)
			s.tracker().FailSpan(ctx, postSpan, "SET_FINALIZING_FAILED", wrapped.Error(), wrapped)
			s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
				types.SpanStatusFailed, nil, "SET_FINALIZING_FAILED", wrapped.Error())
			return wrapped
		}
		if promoted {
			enteredFinalizing = true
			logger.Infof(ctx,
				"[KnowledgePostProcess] Knowledge %s entered finalizing (pending_subtasks=%d).",
				payload.KnowledgeID, expectedSubtasks)
		} else {
			// Row was no longer 'processing' (cancel / delete won the race).
			// Skip enrichment entirely so we don't waste LLM quota on a row
			// the user already abandoned.
			logger.Infof(ctx,
				"[KnowledgePostProcess] Knowledge %s no longer in processing, skipping enrichment fan-out.",
				payload.KnowledgeID)
			s.tracker().EndSpan(ctx, postSpan, types.JSONMap{
				"skipped": "knowledge_no_longer_processing",
			})
			s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
				types.SpanStatusDone, types.JSONMap{
					"skipped": "knowledge_no_longer_processing",
				}, "", "")
			return nil
		}
	}

	// Wiki persistence happens only after the exact generation CAS. For a
	// Wiki-only plan we deliberately use finalizing with a zero counter until
	// the durable pending row exists; a failed persistence retry can then
	// replay the stored plan instead of losing Wiki work behind a completed row.
	if willSpawnWiki {
		wikiResult, wikiErr := EnqueueWikiIngest(
			ctx,
			s.taskEnqueuer,
			s.pendingRepo,
			payload.TenantID,
			payload.KnowledgeBaseID,
			payload.KnowledgeID,
			payload.ProcessingGeneration,
		)
		if !wikiResult.PendingPersisted {
			if wikiErr == nil {
				wikiErr = fmt.Errorf("wiki ingest pending row was not persisted")
			}
			wrapped := fmt.Errorf("persist wiki ingest after generation claim: %w", wikiErr)
			s.tracker().FailSpan(ctx, postSpan, "WIKI_QUEUE_PERSIST_FAILED", wrapped.Error(), wrapped)
			s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
				types.SpanStatusFailed, nil, "WIKI_QUEUE_PERSIST_FAILED", wrapped.Error())
			return wrapped
		}
		enqueuedWiki = !wikiResult.AlreadySettled
		wikiAlreadySettled = wikiResult.AlreadySettled
		if wikiResult.AlreadySettled {
			logger.Infof(ctx,
				"[KnowledgePostProcess] Wiki generation already settled for %s; no queue row or trigger created",
				payload.KnowledgeID)
		} else if wikiErr != nil {
			wikiEnqueueError = wikiErr.Error()
			logger.Warnf(ctx,
				"[KnowledgePostProcess] Wiki row persisted but trigger enqueue degraded for %s: %v",
				payload.KnowledgeID, wikiErr)
		} else {
			logger.Infof(ctx, "[KnowledgePostProcess] Persisted and triggered wiki ingest for %s", payload.KnowledgeID)
		}
	}

	// 4. Spawn Summary and Question Tasks
	enqueuedSummary := false
	enqueuedQuestionCount := 0
	unownedItems := make([]string, 0)
	if willSpawnSummary {
		enqueuedSummary, err = s.enqueueSummaryGenerationTask(ctx, payload, attempt)
		if err != nil {
			return fmt.Errorf("enqueue durable summary fanout: %w", err)
		}
	}
	if questionBatchCount > 0 {
		// Create the postprocess.question grouping span up front so the
		// per-batch subspans (enqueued just below, run later in their own
		// workers) have a parent to nest under. It's begun and ended right
		// here as a structural container — the batches extend past it,
		// which the timeline renders with the wrapping outline bar.
		if grp := s.tracker().BeginSubSpan(ctx, postSpan, postprocessQuestionGroupSpanName,
			types.SpanKindSubSpan, types.JSONMap{
				"batch_count": questionBatchCount,
				"chunk_count": durablePlan.TextChunkCount,
				"batch_size":  questionGenChunkBatchSize,
			}); grp != nil {
			s.tracker().EndSpan(ctx, grp, types.JSONMap{
				"batch_count": questionBatchCount,
				"chunk_count": durablePlan.TextChunkCount,
			})
		}
		if durablePlan.Version == 2 || durablePlan.Version == 3 {
			enqueuedQuestionCount, err = s.enqueuePagedSplitQuestionTasks(
				ctx, payload, durablePlan.QuestionCount, attempt,
				durablePlan.QuestionChunkCount,
				durablePlan.QuestionBatchCount,
			)
		} else {
			enqueuedQuestionCount, err = s.enqueueQuestionGenerationTasks(
				ctx, payload, durablePlan.QuestionCount, attempt, durablePlan.QuestionBatches,
			)
		}
		if err != nil {
			return fmt.Errorf("enqueue durable question fanout: %w", err)
		}
		// A persisted counter without the same number of durable owners strands
		// the document forever. Fail before recording the publication receipt;
		// the stable postprocess task can then be replayed safely.
		if enqueuedQuestionCount != questionBatchCount {
			return fmt.Errorf(
				"enqueue durable question fanout: published %d batches, planned %d",
				enqueuedQuestionCount, questionBatchCount,
			)
		}
	}

	// 5. Spawn Graph RAG Tasks — only when graph indexing is enabled in IndexingStrategy
	enqueuedGraphCount := 0
	if graphChunkCount > 0 {
		logger.Infof(ctx, "[KnowledgePostProcess] Replaying Graph RAG task plan with %d batches", graphChunkCount)
		if durablePlan.Version == 3 {
			var pagedUnowned []string
			enqueuedGraphCount, pagedUnowned, err = s.enqueuePagedSplitGraphBatchTasks(
				ctx,
				payload,
				attempt,
				durablePlan.GraphModelID,
				durablePlan.GraphChunkCount,
				durablePlan.GraphBatchCount,
				durablePlan.GraphBatchSize,
			)
			if err != nil {
				return err
			}
			unownedItems = append(unownedItems, pagedUnowned...)
		} else if durablePlan.Version == 2 {
			var pagedUnowned []string
			enqueuedGraphCount, pagedUnowned, err = s.enqueuePagedSplitGraphTasks(
				ctx, payload, attempt, durablePlan.GraphModelID, durablePlan.GraphChunkCount,
			)
			if err != nil {
				return err
			}
			unownedItems = append(unownedItems, pagedUnowned...)
		} else {
			for _, graphTask := range durablePlan.GraphTasks {
				ok, err := NewChunkExtractBatchTask(
					ctx, s.taskEnqueuer, payload.TenantID, graphTask.chunkIDs(), graphTask.ModelID,
					payload.KnowledgeID, payload.KnowledgeBaseID, payload.ProcessingGeneration,
					attempt, graphTask.ChunkIndex,
				)
				if err != nil {
					logger.Errorf(ctx, "[KnowledgePostProcess] Failed to create graph batch task %d: %v", graphTask.ChunkIndex, err)
					return fmt.Errorf("enqueue durable graph fanout batch %d: %w", graphTask.ChunkIndex, err)
				}
				if ok {
					enqueuedGraphCount++
				} else {
					unownedItems = append(unownedItems, fmt.Sprintf("graph_chunk[%d]", graphTask.ChunkIndex))
				}
			}
		}
	}

	// Reconcile the seeded counter against what was actually enqueued.
	// summary/question/graph each own a counted slot that ONLY their own
	// task drains; a slot whose task was never enqueued (graph with NEO4J
	// off, a transient enqueue/marshal failure, a nil enqueuer) has no owner
	// and would otherwise strand the row in "finalizing". Release exactly the
	// shortfall — each release is a clamped decrement that can promote the row
	// once the counter is zero and Wiki is terminal. Wiki never enters either
	// tally because its own terminal status is the independent gate. Safe against fast workers: shortfall slots have no draining
	// task, so total drains == seeded count regardless of ordering.
	//
	// Detached ctx: the same reasoning that motivates finalizeSubtaskDetached
	// for terminal worker drains applies here. If the postprocess handler's
	// ctx is cancelled (graceful shutdown, preempted worker) between SetFinalizing
	// and this point, the seeded slots have NO other path to drain — every
	// owning task either failed to enqueue or was never created. Riding a
	// cancelled ctx would silently abort the releases and strand the row in
	// "finalizing". The bound is per-call (matches the helper) so a wedged
	// connection can't pin the goroutine for the whole serial loop.
	if enteredFinalizing {
		plannedOwned := expectedSubtasks
		actualOwned := plannedOwned - len(unownedItems)
		if len(unownedItems) > 0 {
			logger.Warnf(ctx,
				"[KnowledgePostProcess] Releasing %d un-enqueued subtask slot(s) for %s (planned=%d actual=%d)",
				len(unownedItems), payload.KnowledgeID, plannedOwned, actualOwned)
			for _, itemID := range unownedItems {
				rctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), finalizeSubtaskDetachedTimeout)
				err := finalizePostProcessGenerationSlot(
					rctx, s.knowledgeRepo, payload, itemID,
					enrichmentoutcome.StatusDegraded,
					"planned enrichment task was not durably enqueued",
				)
				cancel()
				if err != nil {
					wrapped := fmt.Errorf(
						"release unowned postprocess slot %s for %s: %w",
						itemID, payload.KnowledgeID, err,
					)
					s.tracker().FailSpan(ctx, postSpan, "POSTPROCESS_RECONCILE_FAILED", wrapped.Error(), wrapped)
					s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
						types.SpanStatusFailed, nil, "POSTPROCESS_RECONCILE_FAILED", wrapped.Error())
					return wrapped
				}
			}
		}
		// A prior decrement may have reached zero but crashed before its
		// guarded promotion statement. A replay with no shortfall still runs
		// the generation finalizer once; its decrement is clamped and its
		// independent zero-count promotion repairs that state.
		if expectedSubtasks == 0 || (replayingFinalizing && knowledge.PendingSubtasksCount <= 0) {
			rctx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), finalizeSubtaskDetachedTimeout)
			if err := finalizePostProcessGenerationSlot(
				rctx, s.knowledgeRepo, payload, "postprocess_zero", "", "",
			); err != nil {
				cancel()
				wrapped := fmt.Errorf("repair zero-count finalizing row %s: %w", payload.KnowledgeID, err)
				s.tracker().FailSpan(ctx, postSpan, "POSTPROCESS_RECONCILE_FAILED", wrapped.Error(), wrapped)
				s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
					types.SpanStatusFailed, nil, "POSTPROCESS_RECONCILE_FAILED", wrapped.Error())
				return wrapped
			}
			cancel()
		}
	}

	postOutput := types.JSONMap{
		"chunks_total":             durablePlan.TextChunkCount,
		"enqueued_summary":         enqueuedSummary,
		"enqueued_question":        enqueuedQuestionCount > 0,
		"enqueued_question_count":  enqueuedQuestionCount,
		"enqueued_wiki":            enqueuedWiki,
		"wiki_already_settled":     wikiAlreadySettled,
		"enqueued_graph":           enqueuedGraphCount > 0,
		"enqueued_graph_count":     enqueuedGraphCount,
		"graph_source_chunk_count": durablePlan.GraphChunkCount,
		"graph_batch_count":        graphChunkCount,
		"core_outcomes_total":      generationOutcomes.Total,
		"core_outcomes_failed":     generationOutcomes.Failed,
		"core_outcomes_degraded":   generationOutcomes.Degraded,
	}
	if durablePlan.Version == 2 {
		postOutput["question_sampled_chunks"] = durablePlan.QuestionChunkCount
		postOutput["graph_sampled_chunks"] = durablePlan.GraphChunkCount
	} else {
		postOutput["question_sampled_chunks"] = durablePlan.QuestionChunkCount
		postOutput["graph_sampled_chunks"] = durablePlan.GraphChunkCount
	}
	if wikiEnqueueError != "" {
		postOutput["wiki_enqueue_error"] = wikiEnqueueError
	}
	// The Asynq task may remain retained as "completed" for a week. Persist a
	// generation-scoped receipt only after every child task and Wiki intent has
	// been durably published. Recovery can then distinguish successful
	// orchestration from a crash in the middle of fan-out and will not replay
	// this whole plan on every document-worker restart.
	completionStore, ok := s.knowledgeRepo.(processownership.DurableFanoutCompletionStore)
	if !ok || completionStore == nil {
		wrapped := errors.New("record post-process completion: durable completion store is unavailable")
		s.tracker().FailSpan(ctx, postSpan, "POSTPROCESS_RECEIPT_FAILED", wrapped.Error(), wrapped)
		s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
			types.SpanStatusFailed, nil, "POSTPROCESS_RECEIPT_FAILED", wrapped.Error())
		return wrapped
	}
	receiptCtx, cancelReceipt := context.WithTimeout(
		context.WithoutCancel(ctx), finalizeSubtaskDetachedTimeout,
	)
	_, receiptErr := completionStore.RecordKnowledgeFanoutCompletion(
		receiptCtx,
		payload.TenantID,
		payload.KnowledgeID,
		payload.KnowledgeBaseID,
		payload.ProcessingGeneration,
		processownership.PostProcessCompletionItem,
	)
	cancelReceipt()
	if receiptErr != nil {
		wrapped := fmt.Errorf("record post-process completion: %w", receiptErr)
		s.tracker().FailSpan(ctx, postSpan, "POSTPROCESS_RECEIPT_FAILED", wrapped.Error(), wrapped)
		s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
			types.SpanStatusFailed, nil, "POSTPROCESS_RECEIPT_FAILED", wrapped.Error())
		return wrapped
	}
	postOutput["fanout_receipt"] = true
	s.tracker().EndSpan(ctx, postSpan, postOutput)
	// Close the root span — the parse pipeline is done. Async
	// downstream stages (summary/question/wiki/graph) record their
	// own spans independently; their finishing extends the trace's
	// end-time but does not reopen the root. A late failure in one
	// of those stages does not poison the parse result.
	s.tracker().FinalizeAttempt(ctx, payload.KnowledgeID, attempt,
		types.SpanStatusDone, postOutput, "", "")
	return nil
}

// enqueueSummaryGenerationTask enqueues the summary task. Returns true only
// when a task was actually placed on the queue, so the caller can release the
// seeded pending-subtask slot when enqueue is skipped or fails.
func (s *KnowledgePostProcessService) enqueueSummaryGenerationTask(
	ctx context.Context,
	payload types.KnowledgePostProcessPayload,
	attempt int,
) (bool, error) {
	if s.taskEnqueuer == nil {
		return false, errors.New("summary task enqueuer is unavailable")
	}

	taskPayload := types.SummaryGenerationPayload{
		TenantID:             payload.TenantID,
		KnowledgeBaseID:      payload.KnowledgeBaseID,
		KnowledgeID:          payload.KnowledgeID,
		ProcessingGeneration: payload.ProcessingGeneration,
		Language:             payload.Language,
		Attempt:              attempt,
	}
	langfuse.InjectTracing(ctx, &taskPayload)
	payloadBytes, err := json.Marshal(taskPayload)
	if err != nil {
		logger.Warnf(ctx, "[KnowledgePostProcess] Failed to marshal summary generation payload: %v", err)
		return false, err
	}

	task := asynq.NewTask(types.TypeSummaryGeneration, payloadBytes)
	if _, err := processownership.EnqueueStableTask(
		ctx,
		s.taskEnqueuer,
		task,
		types.QueueLow,
		processownership.SummaryTaskID(payload.KnowledgeID, payload.ProcessingGeneration),
		asynq.MaxRetry(3),
		asynq.Timeout(processownership.GenerationTaskTimeout),
		asynq.Retention(processownership.GenerationTaskRetention),
	); errors.Is(err, asynq.ErrTaskIDConflict) {
		logger.Infof(ctx, "[KnowledgePostProcess] Summary task already exists for %s generation %s",
			payload.KnowledgeID, payload.ProcessingGeneration)
		return true, nil
	} else if err != nil {
		logger.Warnf(ctx, "[KnowledgePostProcess] Failed to enqueue summary generation for %s: %v", payload.KnowledgeID, err)
		return false, err
	}
	logger.Infof(ctx, "[KnowledgePostProcess] Enqueued summary generation task for %s", payload.KnowledgeID)
	return true, nil
}

// questionGenChunkBatchSize is the number of text chunks handled by a single
// question-generation task. Batching keeps the task count bounded for very
// large documents (a 5k-chunk doc becomes ~250 tasks instead of 5k) while
// preserving per-batch retry / cancellation granularity and letting each task
// do one embedding BatchIndex over the whole batch.
const questionGenChunkBatchSize = 20

// postprocessQuestionGroupSpanName is the grouping span the per-batch
// question subspans (postprocess.question.batch[i]) nest under, so the trace
// viewer shows one "postprocess.question" node instead of dozens of siblings
// directly beneath the postprocess stage.
const postprocessQuestionGroupSpanName = "postprocess.question"

// enqueueQuestionGenerationTasks fans out one TypeQuestionGeneration task per
// batch of questionGenChunkBatchSize text chunks. Each task carries only chunk
// ids (+ the adjacent boundary ids for context) — never the chunk content — so
// the payload stays small and the worker reads fresh content at run time,
// matching the ExtractChunkPayload precedent.
//
// Returns the number of batch tasks successfully enqueued. Any real
// marshal/enqueue failure is returned to Asynq: the persisted plan and stable
// task IDs let the retry observe already-enqueued batches as conflicts and
// continue with the first missing batch without dropping enrichment work.
func (s *KnowledgePostProcessService) enqueueQuestionGenerationTasks(
	ctx context.Context,
	payload types.KnowledgePostProcessPayload,
	questionCount int,
	attempt int,
	batches []durableQuestionBatch,
) (int, error) {
	if s.taskEnqueuer == nil || len(batches) == 0 {
		if len(batches) == 0 {
			return 0, nil
		}
		return 0, errors.New("question task enqueuer is unavailable")
	}
	if questionCount <= 0 {
		questionCount = 3
	}
	if questionCount > 10 {
		questionCount = 10
	}

	totalChunks := 0
	enqueued := 0
	for _, batch := range batches {
		totalChunks += len(batch.ChunkIDs)
		currentBatchIndex := batch.BatchIndex
		taskPayload := types.QuestionGenerationPayload{
			TenantID:             payload.TenantID,
			KnowledgeBaseID:      payload.KnowledgeBaseID,
			KnowledgeID:          payload.KnowledgeID,
			ProcessingGeneration: payload.ProcessingGeneration,
			QuestionCount:        questionCount,
			Language:             payload.Language,
			Attempt:              attempt,
			ChunkIDs:             batch.ChunkIDs,
			BatchIndex:           currentBatchIndex,
			PrevChunkID:          batch.PrevChunkID,
			NextChunkID:          batch.NextChunkID,
			SparseSample:         batch.SparseSample,
		}

		langfuse.InjectTracing(ctx, &taskPayload)
		payloadBytes, err := json.Marshal(taskPayload)
		if err != nil {
			logger.Warnf(ctx, "[KnowledgePostProcess] Failed to marshal question generation payload for batch %d: %v", currentBatchIndex, err)
			return enqueued, err
		}

		task := asynq.NewTask(types.TypeQuestionGeneration, payloadBytes)
		if _, err := processownership.EnqueueStableTask(
			ctx,
			s.taskEnqueuer,
			task,
			types.QueueQuestion,
			processownership.QuestionTaskID(
				payload.KnowledgeID, payload.ProcessingGeneration, currentBatchIndex,
			),
			asynq.MaxRetry(3),
			asynq.Timeout(processownership.GenerationTaskTimeout),
			asynq.Retention(processownership.GenerationTaskRetention),
		); errors.Is(err, asynq.ErrTaskIDConflict) {
			logger.Infof(ctx, "[KnowledgePostProcess] Question batch %d already exists for %s generation %s",
				currentBatchIndex, payload.KnowledgeID, payload.ProcessingGeneration)
			enqueued++
			continue
		} else if err != nil {
			logger.Warnf(ctx, "[KnowledgePostProcess] Failed to enqueue question generation batch %d for %s: %v", currentBatchIndex, payload.KnowledgeID, err)
			return enqueued, err
		}
		enqueued++
	}
	logger.Infof(ctx, "[KnowledgePostProcess] Enqueued %d question generation batch tasks (%d chunks, batch_size=%d) for %s (count=%d)",
		enqueued, totalChunks, questionGenChunkBatchSize, payload.KnowledgeID, questionCount)
	return enqueued, nil
}
