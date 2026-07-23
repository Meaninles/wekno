package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
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
}

// enrichmentPlan describes the asynchronous work spawned after the primary
// document parse has produced its chunks and search indexes.
//
// Wiki is intentionally present in the plan but absent from
// finalizationSubtaskCount. Wiki ingestion is a KB-scoped, durable background
// pipeline with its own pending/active status; it must never keep an otherwise
// usable document in parse_status=finalizing. This restores the lifecycle
// contract introduced by migration 000056: pending_subtasks_count owns only
// per-knowledge enrichment tasks that directly participate in document
// finalization (summary, question generation and graph extraction).
type enrichmentPlan struct {
	spawnSummary       bool
	questionBatchCount int
	spawnWiki          bool
	graphChunkCount    int
}

const durableEnrichmentPlanStage = "enrichment"

type durableQuestionBatch struct {
	ChunkIDs    []string `json:"chunk_ids"`
	BatchIndex  int      `json:"batch_index"`
	PrevChunkID string   `json:"prev_chunk_id,omitempty"`
	NextChunkID string   `json:"next_chunk_id,omitempty"`
}

type durableGraphTask struct {
	ChunkID    string `json:"chunk_id"`
	ChunkIndex int    `json:"chunk_index"`
	ModelID    string `json:"model_id"`
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
}

func (p durableEnrichmentFanout) validate(payload types.KnowledgePostProcessPayload) error {
	if p.Stage != durableEnrichmentPlanStage || p.Version != 1 ||
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
		if graphTask.ChunkID == "" || graphTask.ModelID == "" || graphTask.ChunkIndex < 0 {
			return errors.New("durable enrichment fanout has invalid graph task")
		}
	}
	return nil
}

func (p durableEnrichmentFanout) subtaskCount() int {
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

func finalizePostProcessGenerationSlot(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	payload types.KnowledgePostProcessPayload,
	itemID string,
) error {
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
) interfaces.TaskHandler {
	return &KnowledgePostProcessService{
		knowledgeRepo: knowledgeRepo,
		kbService:     kbService,
		chunkService:  chunkService,
		taskEnqueuer:  taskEnqueuer,
		pendingRepo:   pendingRepo,
		redisClient:   redisClient,
		spanTracker:   spanTracker,
	}
}

func (s *KnowledgePostProcessService) tracker() SpanTracker {
	if s.spanTracker == nil {
		return noopSpanTracker{}
	}
	return s.spanTracker
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
		s.tracker().EndSpan(ctx, mm, nil)
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
			for start, batchIndex := 0, 0; start < len(questionChunks); start, batchIndex = start+questionGenChunkBatchSize, batchIndex+1 {
				end := start + questionGenChunkBatchSize
				if end > len(questionChunks) {
					end = len(questionChunks)
				}
				batch := durableQuestionBatch{BatchIndex: batchIndex}
				for _, chunk := range questionChunks[start:end] {
					batch.ChunkIDs = append(batch.ChunkIDs, chunk.ID)
				}
				if start > 0 {
					batch.PrevChunkID = questionChunks[start-1].ID
				}
				if end < len(questionChunks) {
					batch.NextChunkID = questionChunks[end].ID
				}
				durablePlan.QuestionBatches = append(durablePlan.QuestionBatches, batch)
			}
		}
		if eff.GraphEnabled {
			for index, chunk := range textChunks {
				durablePlan.GraphTasks = append(durablePlan.GraphTasks, durableGraphTask{
					ChunkID: chunk.ID, ChunkIndex: index, ModelID: kb.SummaryModelID,
				})
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
	expectedSubtasks := durablePlan.subtaskCount()

	enqueuedWiki := false
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
		// the same CAS that clears its durable core fan-out plan.
		now := time.Now()
		updates := map[string]interface{}{
			"parse_status":           types.ParseStatusCompleted,
			"pending_subtasks_count": 0,
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
				"summary_status":         summaryStatus,
				"processing_owner":       "",
				"processing_fanout":      types.JSON(durablePlanBytes),
				"updated_at":             time.Now(),
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
		enqueuedWiki = true
		if wikiErr != nil {
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
			enqueuedQuestionCount, err = s.enqueueQuestionGenerationTasks(
				ctx, payload, durablePlan.QuestionCount, attempt, durablePlan.QuestionBatches,
			)
			if err != nil {
				return fmt.Errorf("enqueue durable question fanout: %w", err)
			}
		}
	}

	// 5. Spawn Graph RAG Tasks — only when graph indexing is enabled in IndexingStrategy
	enqueuedGraphCount := 0
	if graphChunkCount > 0 {
		logger.Infof(ctx, "[KnowledgePostProcess] Replaying Graph RAG task plan with %d chunks", graphChunkCount)
		for _, graphTask := range durablePlan.GraphTasks {
			ok, err := NewChunkExtractTask(
				ctx, s.taskEnqueuer, payload.TenantID, graphTask.ChunkID, graphTask.ModelID,
				payload.KnowledgeID, payload.KnowledgeBaseID, payload.ProcessingGeneration,
				attempt, graphTask.ChunkIndex,
			)
			if err != nil {
				logger.Errorf(ctx, "[KnowledgePostProcess] Failed to create chunk extract task for %s: %v", graphTask.ChunkID, err)
				return fmt.Errorf("enqueue durable graph fanout chunk %d: %w", graphTask.ChunkIndex, err)
			}
			if ok {
				enqueuedGraphCount++
			} else {
				unownedItems = append(unownedItems, fmt.Sprintf("graph_chunk[%d]", graphTask.ChunkIndex))
			}
		}
	}

	// Reconcile the seeded counter against what was actually enqueued.
	// summary/question/graph each own a counted slot that ONLY their own
	// task drains; a slot whose task was never enqueued (graph with NEO4J
	// off, a transient enqueue/marshal failure, a nil enqueuer) has no owner
	// and would otherwise strand the row in "finalizing". Release exactly the
	// shortfall — each release is a clamped decrement that promotes the row to
	// "completed" if it brings the counter to zero. Wiki is not a document
	// finalization owner and therefore never enters either tally. Safe against fast workers: shortfall slots have no draining
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
				err := finalizePostProcessGenerationSlot(rctx, s.knowledgeRepo, payload, itemID)
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
				rctx, s.knowledgeRepo, payload, "postprocess_zero",
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
		"chunks_total":            durablePlan.TextChunkCount,
		"enqueued_summary":        enqueuedSummary,
		"enqueued_question":       enqueuedQuestionCount > 0,
		"enqueued_question_count": enqueuedQuestionCount,
		"enqueued_wiki":           enqueuedWiki,
		"enqueued_graph":          enqueuedGraphCount > 0,
		"enqueued_graph_count":    enqueuedGraphCount,
	}
	if wikiEnqueueError != "" {
		postOutput["wiki_enqueue_error"] = wikiEnqueueError
	}
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
	if _, err := s.taskEnqueuer.Enqueue(
		task,
		asynq.Queue("low"),
		asynq.MaxRetry(3),
		asynq.Retention(processownership.GenerationTaskRetention),
		asynq.TaskID(processownership.SummaryTaskID(payload.KnowledgeID, payload.ProcessingGeneration)),
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
		}

		langfuse.InjectTracing(ctx, &taskPayload)
		payloadBytes, err := json.Marshal(taskPayload)
		if err != nil {
			logger.Warnf(ctx, "[KnowledgePostProcess] Failed to marshal question generation payload for batch %d: %v", currentBatchIndex, err)
			return enqueued, err
		}

		task := asynq.NewTask(types.TypeQuestionGeneration, payloadBytes)
		if _, err := s.taskEnqueuer.Enqueue(
			task,
			asynq.Queue(types.QueueQuestion),
			asynq.MaxRetry(3),
			asynq.Retention(processownership.GenerationTaskRetention),
			asynq.TaskID(processownership.QuestionTaskID(
				payload.KnowledgeID, payload.ProcessingGeneration, currentBatchIndex,
			)),
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
