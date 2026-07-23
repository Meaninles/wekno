package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	chatpipeline "github.com/Tencent/WeKnora/internal/application/service/chat_pipeline"
	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const (
	// tableDescriptionPromptTemplate is the prompt template for generating table descriptions
	tableDescriptionPromptTemplate = `You are a data analysis expert. Based on the following table structure information and data samples, generate a concise table metadata description (200-300 words).

Table name: %s

%s

%s

Please describe the table from the following dimensions:
1. **Data Subject**: What type of data does this table record? (e.g., user information, sales records, log data, etc.)
2. **Core Fields**: List 3-5 most important fields and their meanings
3. **Data Scale**: Total number of rows and columns
4. **Business Scenarios**: What business analysis or application scenarios might this table be used for?
5. **Key Characteristics**: What notable features does the data have? (e.g., contains geographic locations, has category labels, has hierarchical relationships, etc.)

**Important Notes**:
- Do not output specific data values or sample content
- Use general descriptions so users can quickly determine if this table contains the information they need
- Use concise and professional language for easy retrieval and understanding
- Write the description in the same language as the data content`

	// columnDescriptionsPromptTemplate is the prompt template for generating column descriptions
	columnDescriptionsPromptTemplate = `You are a data analysis expert. Based on the following table structure information and data samples, generate structured description information for each column.

Table name: %s

%s

%s

Please generate a detailed description for each column, including the following information:
1. **Field Meaning**: What information does this column store? (e.g., user ID, order amount, creation time, etc.)
2. **Data Type**: The type and format of the data (e.g., integer, string, datetime, boolean, etc.)
3. **Business Purpose**: The role of this field in business (e.g., for user identification, amount calculation, time sorting, etc.)
4. **Data Characteristics**: Notable features of the data (e.g., unique identifier, nullable, has enum values, has units, etc.)

Please output in the following format (one paragraph per column):

**Column1** (data type)
- Field Meaning: xxx
- Business Purpose: xxx
- Data Characteristics: xxx

**Column2** (data type)
- Field Meaning: xxx
- Business Purpose: xxx
- Data Characteristics: xxx

**Important Notes**:
- Do not output specific data values, only describe the field metadata
- Use clear business terms for easy user understanding and search
- If enum value ranges can be inferred from sample data, provide a summary (e.g., status field contains pending/in-progress/completed states)
- Write descriptions in the same language as the data content`
)

// NewChunkExtractTask creates a new chunk extract task. It returns
// (enqueued, err): enqueued is true only when a task was actually placed on
// the queue. When NEO4J is disabled the call is a no-op and returns
// (false, nil) — callers that seeded a pending-subtask counter for this chunk
// MUST release that slot, otherwise the parent knowledge stays stuck in
// "finalizing" forever (the graph subtask it's waiting on was never enqueued).
func NewChunkExtractTask(
	ctx context.Context,
	client interfaces.TaskEnqueuer,
	tenantID uint64,
	chunkID string,
	modelID string,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
	attempt int,
	chunkIndex int,
) (bool, error) {
	if strings.ToLower(os.Getenv("NEO4J_ENABLE")) != "true" {
		logger.Warn(ctx, "NEO4J is not enabled, skip chunk extract task")
		return false, nil
	}
	taskPayload := types.ExtractChunkPayload{
		TenantID:             tenantID,
		ChunkID:              chunkID,
		ModelID:              modelID,
		KnowledgeID:          knowledgeID,
		KnowledgeBaseID:      knowledgeBaseID,
		ProcessingGeneration: processingGeneration,
		Attempt:              attempt,
		ChunkIndex:           chunkIndex,
	}
	langfuse.InjectTracing(ctx, &taskPayload)
	payload, err := json.Marshal(taskPayload)
	if err != nil {
		return false, err
	}
	task := asynq.NewTask(types.TypeChunkExtract, payload)
	info, err := client.Enqueue(
		task,
		asynq.Queue(types.QueueGraph),
		asynq.MaxRetry(3),
		asynq.Retention(processownership.GenerationTaskRetention),
		asynq.TaskID(processownership.ExtractTaskID(knowledgeID, processingGeneration, chunkIndex)),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		logger.Infof(ctx, "graph extract task already exists: knowledge=%s generation=%s chunk=%s",
			knowledgeID, processingGeneration, chunkID)
		return true, nil
	}
	if err != nil {
		logger.Errorf(ctx, "failed to enqueue task: %v", err)
		return false, fmt.Errorf("failed to enqueue task: %v", err)
	}
	logger.Infof(ctx, "enqueued task: id=%s queue=%s chunk=%s", info.ID, info.Queue, chunkID)
	return true, nil
}

// NewTableExtractTask creates a new table extract task
func NewDataTableSummaryTask(
	ctx context.Context,
	client interfaces.TaskEnqueuer,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
	summaryModel string,
	embeddingModel string,
) error {
	taskPayload := types.DataTableSummaryPayload{
		TenantID:             tenantID,
		KnowledgeID:          knowledgeID,
		KnowledgeBaseID:      knowledgeBaseID,
		ProcessingGeneration: processingGeneration,
		SummaryModel:         summaryModel,
		EmbeddingModel:       embeddingModel,
	}
	langfuse.InjectTracing(ctx, &taskPayload)
	payload, err := json.Marshal(taskPayload)
	if err != nil {
		return err
	}
	task := asynq.NewTask(types.TypeDataTableSummary, payload)
	info, err := client.Enqueue(
		task,
		asynq.Queue(types.QueueDefault),
		asynq.MaxRetry(3),
		asynq.Retention(processownership.GenerationTaskRetention),
		asynq.TaskID(processownership.DataTableSummaryTaskID(knowledgeID, processingGeneration)),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		logger.Infof(ctx, "data-table summary task already exists for knowledge %s generation %s",
			knowledgeID, processingGeneration)
		return nil
	}
	if err != nil {
		logger.Errorf(ctx, "failed to enqueue data table summary task: %v", err)
		return fmt.Errorf("failed to enqueue data table summary task: %v", err)
	}
	logger.Infof(ctx, "enqueued data table summary task: id=%s queue=%s knowledge=%s",
		info.ID, info.Queue, knowledgeID)
	return nil
}

// ChunkExtractService is a service for extracting chunks
type ChunkExtractService struct {
	template          *types.PromptTemplateStructured
	modelService      interfaces.ModelService
	knowledgeBaseRepo interfaces.KnowledgeBaseRepository
	knowledgeRepo     interfaces.KnowledgeRepository
	chunkRepo         interfaces.ChunkRepository
	graphEngine       interfaces.RetrieveGraphRepository
	// spanTracker records this graph-extract task's subspan under the
	// parent attempt's postprocess stage so the trace viewer shows real
	// per-chunk graph extraction time rather than the upstream's enqueue.
	spanTracker SpanTracker
}

// NewChunkExtractService creates a new chunk extract service
func NewChunkExtractService(
	config *config.Config,
	modelService interfaces.ModelService,
	knowledgeBaseRepo interfaces.KnowledgeBaseRepository,
	knowledgeRepo interfaces.KnowledgeRepository,
	chunkRepo interfaces.ChunkRepository,
	graphEngine interfaces.RetrieveGraphRepository,
	spanTracker SpanTracker,
) interfaces.TaskHandler {
	return &ChunkExtractService{
		template:          config.ExtractManager.ExtractGraph,
		modelService:      modelService,
		knowledgeBaseRepo: knowledgeBaseRepo,
		knowledgeRepo:     knowledgeRepo,
		chunkRepo:         chunkRepo,
		graphEngine:       graphEngine,
		spanTracker:       spanTracker,
	}
}

func (s *ChunkExtractService) tracker() SpanTracker {
	if s.spanTracker == nil {
		return noopSpanTracker{}
	}
	return s.spanTracker
}

// Handle handles the chunk extraction task
func (s *ChunkExtractService) Handle(ctx context.Context, t *asynq.Task) (retErr error) {
	var p types.ExtractChunkPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		logger.Errorf(ctx, "failed to unmarshal task payload: %v", err)
		return err
	}
	ctx = logger.WithRequestID(ctx, uuid.New().String())
	ctx = logger.WithField(ctx, "extract", p.ChunkID)
	ctx = context.WithValue(ctx, types.TenantIDContextKey, p.TenantID)
	if p.TenantID == 0 || strings.TrimSpace(p.ChunkID) == "" {
		return errors.New("graph extract: complete tenant and chunk identity is required")
	}
	// Pre-ownership graph payloads carried only chunk_id. Resolve their parent
	// identity read-only so the active generation-less row can be repaired
	// instead of silently acknowledged and left finalizing forever.
	if strings.TrimSpace(p.KnowledgeID) == "" || strings.TrimSpace(p.KnowledgeBaseID) == "" {
		chunk, err := s.chunkRepo.GetChunkByID(ctx, p.TenantID, p.ChunkID)
		if err != nil {
			return fmt.Errorf("graph extract: resolve legacy chunk identity: %w", err)
		}
		if chunk == nil || chunk.KnowledgeID == "" || chunk.KnowledgeBaseID == "" {
			return errors.New("graph extract: legacy chunk has no parent knowledge identity")
		}
		p.KnowledgeID = chunk.KnowledgeID
		p.KnowledgeBaseID = chunk.KnowledgeBaseID
	}
	if strings.TrimSpace(p.ProcessingGeneration) == "" {
		return processownership.RepairLegacyTask(
			ctx, s.knowledgeRepo, p.TenantID, p.KnowledgeID,
			p.KnowledgeBaseID, "graph extraction",
		)
	}

	// A newer attempt (re-upload / edit / reparse) has superseded this one:
	// skip before opening the span or registering the FinalizeSubtask defer.
	// The chunk this task references was deleted by the new attempt's cleanup,
	// and decrementing here would drain the new attempt's counter.
	if attemptSuperseded(ctx, s.tracker(), p.KnowledgeID, p.Attempt) {
		logger.Infof(ctx, "graph extract: attempt %d superseded for %s, skipping stale enrichment",
			p.Attempt, p.KnowledgeID)
		return nil
	}
	currentGeneration, err := validateEnrichmentGeneration(
		ctx, s.knowledgeRepo, p.TenantID, p.KnowledgeID,
		p.KnowledgeBaseID, p.ProcessingGeneration,
	)
	if err != nil {
		return fmt.Errorf("validate graph extraction generation: %w", err)
	}
	if !currentGeneration {
		logger.Infof(ctx, "graph extract: stale or incomplete processing generation for %s, skipping", p.KnowledgeID)
		return nil
	}

	// Open a postprocess subspan keyed by chunk ordinal so the trace
	// shows real per-chunk graph extraction time. Skipped silently when
	// upstream didn't pass the parent attempt (legacy in-flight tasks)
	// or when the postprocess stage span isn't found.
	var gSpan *Span
	if p.KnowledgeID != "" && p.Attempt > 0 {
		parent := s.tracker().LookupStage(ctx, p.KnowledgeID, p.Attempt, types.StagePostProcess)
		if parent != nil {
			gSpan = s.tracker().BeginSubSpan(ctx, parent,
				fmt.Sprintf("postprocess.graph.chunk[%d]", p.ChunkIndex),
				types.SpanKindSubSpan,
				types.JSONMap{
					"chunk_id":    p.ChunkID,
					"chunk_index": p.ChunkIndex,
					"model_id":    p.ModelID,
				})
		}
	}
	var handleErr error
	graphOut := types.JSONMap{}
	defer func() {
		// Decrement the parent's enrichment counter on terminal exit so a
		// completed (or terminally-failed) per-chunk extract releases its
		// slot in pending_subtasks_count. KnowledgeID is the new (post-#? )
		// payload field; legacy in-flight tasks without it are skipped.
		if finalizeErr := finalizeSubtaskDetached(ctx, s.knowledgeRepo, p.TenantID, p.KnowledgeID,
			p.KnowledgeBaseID, p.ProcessingGeneration,
			fmt.Sprintf("graph_chunk[%d]", p.ChunkIndex),
			retErr, false, isFinalAsynqAttempt(ctx)); finalizeErr != nil {
			retErr = errors.Join(retErr, finalizeErr)
			handleErr = errors.Join(handleErr, finalizeErr)
		}
		if gSpan == nil {
			return
		}
		if handleErr != nil {
			s.tracker().FailSpan(ctx, gSpan, "GRAPH_EXTRACT_FAILED", handleErr.Error(), handleErr)
		} else {
			s.tracker().EndSpan(ctx, gSpan, graphOut)
		}
	}()

	// Short-circuit when the parent knowledge has been cancelled / deleted.
	// Each graph extract is per-chunk and runs one LLM call — the most
	// expensive enrichment fan-out in the pipeline. Skipping on cancel
	// is the whole point of the finalizing-state machinery above.
	if p.KnowledgeID != "" && s.knowledgeRepo != nil {
		k, kerr := s.knowledgeRepo.GetKnowledgeByID(ctx, p.TenantID, p.KnowledgeID)
		if kerr != nil {
			if errors.Is(kerr, apprepo.ErrKnowledgeNotFound) {
				graphOut["skipped"] = "knowledge_not_found"
				return nil
			}
			handleErr = kerr
			return fmt.Errorf("reload graph extraction knowledge: %w", kerr)
		}
		if k == nil {
			graphOut["skipped"] = "knowledge_not_found"
			return nil
		}
		switch k.ParseStatus {
		case types.ParseStatusCancelling, types.ParseStatusCancelled, types.ParseStatusDeleting:
			logger.Infof(ctx, "graph extract: knowledge %s aborted (%s), skipping chunk %s",
				p.KnowledgeID, k.ParseStatus, p.ChunkID)
			graphOut["skipped"] = "knowledge_" + k.ParseStatus
			return nil
		}
	}

	chunk, err := s.chunkRepo.GetChunkByID(ctx, p.TenantID, p.ChunkID)
	if err != nil {
		if errors.Is(err, apprepo.ErrChunkNotFound) {
			graphOut["skipped"] = "chunk_not_found"
			return nil
		}
		logger.Errorf(ctx, "failed to get chunk: %v", err)
		handleErr = err
		return fmt.Errorf("get graph extraction chunk: %w", err)
	}
	// Capture chunk content shape on output — lets traces answer "WHAT
	// did the LLM call see?" without joining back to the chunk store.
	// Preview is truncated to keep span rows reasonable.
	if gSpan != nil {
		graphOut["chunk_chars"] = len([]rune(chunk.Content))
		graphOut["chunk_preview"] = previewText(chunk.Content, 200)
	}
	kb, err := s.knowledgeBaseRepo.GetKnowledgeBaseByID(ctx, chunk.KnowledgeBaseID)
	if err != nil {
		logger.Errorf(ctx, "failed to get knowledge base: %v", err)
		handleErr = err
		return err
	}

	var processOverrides *types.KnowledgeProcessOverrides
	knowledgeID := p.KnowledgeID
	if knowledgeID == "" {
		knowledgeID = chunk.KnowledgeID
	}
	if knowledgeID != "" && s.knowledgeRepo != nil {
		k, kerr := s.knowledgeRepo.GetKnowledgeByID(ctx, p.TenantID, knowledgeID)
		if kerr != nil {
			if errors.Is(kerr, apprepo.ErrKnowledgeNotFound) {
				graphOut["skipped"] = "knowledge_not_found"
				return nil
			}
			handleErr = kerr
			return fmt.Errorf("load graph extraction process overrides: %w", kerr)
		}
		if k == nil {
			graphOut["skipped"] = "knowledge_not_found"
			return nil
		}
		processOverrides, kerr = k.ProcessOverrides()
		if kerr != nil {
			handleErr = kerr
			return fmt.Errorf("decode graph extraction process overrides: %w", kerr)
		}
	}
	extractCfg := ResolveProcessConfig(kb, processOverrides).ExtractConfig
	if !extractCfg.Enabled {
		logger.Warnf(ctx, "extract config not enabled")
		graphOut["skipped"] = "extract_disabled"
		return nil
	}

	chatModel, err := s.modelService.GetChatModel(ctx, p.ModelID)
	if err != nil {
		logger.Errorf(ctx, "failed to get chat model: %v", err)
		handleErr = err
		return err
	}

	template := &types.PromptTemplateStructured{
		Description: s.template.Description,
		Tags:        extractCfg.Tags,
		Examples: []types.GraphData{
			{
				Text:     extractCfg.Text,
				Node:     extractCfg.Nodes,
				Relation: extractCfg.Relations,
			},
		},
	}
	extractor := chatpipeline.NewExtractor(chatModel, template)
	graph, err := extractor.Extract(ctx, chunk.Content)
	if err != nil {
		handleErr = err
		return err
	}

	chunk, err = s.chunkRepo.GetChunkByID(ctx, p.TenantID, p.ChunkID)
	if err != nil {
		if errors.Is(err, apprepo.ErrChunkNotFound) {
			logger.Infof(ctx, "graph ignore disappeared chunk %s", p.ChunkID)
			graphOut["skipped"] = "chunk_disappeared"
			return nil
		}
		handleErr = err
		return fmt.Errorf("reload graph extraction chunk %s: %w", p.ChunkID, err)
	}

	for _, node := range graph.Node {
		node.Chunks = []string{chunk.ID}
	}
	currentGeneration, err = validateEnrichmentGeneration(
		ctx, s.knowledgeRepo, p.TenantID, p.KnowledgeID,
		p.KnowledgeBaseID, p.ProcessingGeneration,
	)
	if err != nil {
		handleErr = err
		return fmt.Errorf("fence graph write generation: %w", err)
	}
	if !currentGeneration {
		graphOut["skipped"] = "generation_changed"
		return nil
	}
	if err = s.graphEngine.AddGraph(ctx,
		types.NameSpace{KnowledgeBase: chunk.KnowledgeBaseID, Knowledge: chunk.KnowledgeID},
		[]*types.GraphData{graph},
	); err != nil {
		logger.Errorf(ctx, "failed to add graph: %v", err)
		handleErr = err
		return err
	}
	graphOut["nodes_added"] = len(graph.Node)
	graphOut["relations_added"] = len(graph.Relation)
	// Capture a couple of sample nodes/relations so the trace viewer can
	// answer "what did the LLM actually extract?" without round-tripping
	// to the graph store. Cap to two each — anything more bloats span
	// rows and the full graph is queryable elsewhere.
	if len(graph.Node) > 0 {
		samples := graph.Node
		if len(samples) > 2 {
			samples = samples[:2]
		}
		names := make([]string, 0, len(samples))
		for _, n := range samples {
			names = append(names, n.Name)
		}
		graphOut["sample_nodes"] = names
	}
	if len(graph.Relation) > 0 {
		samples := graph.Relation
		if len(samples) > 2 {
			samples = samples[:2]
		}
		out := make([]string, 0, len(samples))
		for _, r := range samples {
			out = append(out, fmt.Sprintf("%s --[%s]--> %s", r.Node1, r.Type, r.Node2))
		}
		graphOut["sample_relations"] = out
	}
	return nil
}

// DataTableExtractPayload represents the table extract task payload
type DataTableSummaryPayload = types.DataTableSummaryPayload

// DataTableSummaryService is a service for extracting tables
type DataTableSummaryService struct {
	modelService         interfaces.ModelService
	knowledgeBaseService interfaces.KnowledgeBaseService
	knowledgeService     interfaces.KnowledgeService
	knowledgeRepo        interfaces.KnowledgeRepository
	fileService          interfaces.FileService
	chunkService         interfaces.ChunkService
	tenantService        interfaces.TenantService
	retrieveEngine       interfaces.RetrieveEngineRegistry
	ownership            retriever.TenantStoreOwnership
	sqlDB                *sql.DB
	taskEnqueuer         interfaces.TaskEnqueuer
	redisClient          *redis.Client
}

// NewDataTableSummaryService creates a new DataTableSummaryService
func NewDataTableSummaryService(
	modelService interfaces.ModelService,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	knowledgeRepo interfaces.KnowledgeRepository,
	fileService interfaces.FileService,
	chunkService interfaces.ChunkService,
	tenantService interfaces.TenantService,
	retrieveEngine interfaces.RetrieveEngineRegistry,
	ownership retriever.TenantStoreOwnership,
	sqlDB *sql.DB,
	taskEnqueuer interfaces.TaskEnqueuer,
	redisClient *redis.Client,
) interfaces.TaskHandler {
	return &DataTableSummaryService{
		modelService:         modelService,
		knowledgeBaseService: knowledgeBaseService,
		knowledgeService:     knowledgeService,
		knowledgeRepo:        knowledgeRepo,
		fileService:          fileService,
		chunkService:         chunkService,
		tenantService:        tenantService,
		retrieveEngine:       retrieveEngine,
		ownership:            ownership,
		sqlDB:                sqlDB,
		taskEnqueuer:         taskEnqueuer,
		redisClient:          redisClient,
	}
}

// Handle implements the TaskHandler interface for table extraction
// 整体流程：初始化 -> 准备资源 -> 加载数据 -> 生成摘要 -> 创建索引
func (s *DataTableSummaryService) Handle(ctx context.Context, t *asynq.Task) (retErr error) {
	// 1. 解析任务并初始化上下文
	var payload DataTableSummaryPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "failed to unmarshal table extract task payload: %v", err)
		return err
	}

	ctx = logger.WithRequestID(ctx, uuid.New().String())
	ctx = logger.WithField(ctx, "knowledge", payload.KnowledgeID)
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.TenantID == 0 || payload.KnowledgeID == "" || payload.KnowledgeBaseID == "" {
		return errors.New("data-table summary: complete tenant, KB and knowledge identity is required")
	}
	if payload.ProcessingGeneration == "" {
		return processownership.RepairLegacyTask(
			ctx, s.knowledgeRepo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, "data-table summary",
		)
	}
	knowledge, current, err := s.currentDataTableGeneration(ctx, payload)
	if err != nil {
		return fmt.Errorf("validate data-table generation: %w", err)
	}
	if !current {
		logger.Infof(ctx, "data-table summary: stale generation for %s, skipping", payload.KnowledgeID)
		return nil
	}
	plan, err := processownership.ParseFanoutPlan(knowledge.ProcessingFanout)
	if err != nil {
		return fmt.Errorf("load data-table durable fanout plan: %w", err)
	}
	completionStore, ok := s.knowledgeRepo.(processownership.DurableFanoutCompletionStore)
	if !ok || completionStore == nil {
		return errors.New("data-table summary: durable fanout completion repository is unavailable")
	}
	item := processownership.DataTableFanoutItem()
	done, _, err := processownership.DurableFanoutItemCompleted(
		ctx, completionStore, s.redisClient, plan, item,
	)
	if err != nil {
		return fmt.Errorf("read durable data-table fan-in completion: %w", err)
	}
	if done {
		return s.replayDataTableFanIn(ctx, payload, plan, completionStore)
	}
	skipFanIn := false
	defer func() {
		if skipFanIn || (retErr != nil && !isFinalAsynqAttempt(ctx)) {
			return
		}
		if err := s.completeDataTableFanIn(ctx, payload, plan, completionStore); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	logger.Infof(ctx, "Processing table extraction for knowledge: %s", payload.KnowledgeID)

	// 2. 准备所有必需的资源（知识、模型、引擎等）
	resources, err := s.prepareResources(ctx, payload, knowledge)
	if err != nil {
		return err
	}

	// 3. 加载表格数据并生成摘要
	chunks, err := s.processTableData(ctx, resources)
	if err != nil {
		return err
	}
	_, current, err = s.currentDataTableGeneration(ctx, payload)
	if err != nil {
		return fmt.Errorf("revalidate data-table generation: %w", err)
	}
	if !current {
		skipFanIn = true
		logger.Infof(ctx, "data-table summary: generation changed before chunk write for %s, skipping", payload.KnowledgeID)
		return nil
	}

	// 4. 索引到向量数据库
	if err := s.indexToVectorDB(ctx, chunks, resources.retrieveEngine, resources.embeddingModel); err != nil {
		s.cleanupOnFailure(ctx, resources, chunks, err)
		return err
	}

	logger.Infof(ctx, "Table extraction completed for knowledge: %s", payload.KnowledgeID)
	return nil
}

func (s *DataTableSummaryService) currentDataTableGeneration(
	ctx context.Context,
	payload types.DataTableSummaryPayload,
) (*types.Knowledge, bool, error) {
	if s.knowledgeService == nil {
		return nil, false, errors.New("knowledge service is unavailable")
	}
	knowledge, err := s.knowledgeService.GetKnowledgeByID(ctx, payload.KnowledgeID)
	if err != nil {
		return nil, false, err
	}
	current := knowledge != nil &&
		knowledge.TenantID == payload.TenantID &&
		knowledge.KnowledgeBaseID == payload.KnowledgeBaseID &&
		knowledge.ProcessingGeneration == payload.ProcessingGeneration &&
		(knowledge.ParseStatus == types.ParseStatusPending || knowledge.ParseStatus == types.ParseStatusProcessing)
	return knowledge, current, nil
}

func (s *DataTableSummaryService) replayDataTableFanIn(
	ctx context.Context,
	payload types.DataTableSummaryPayload,
	plan processownership.FanoutPlan,
	completionStore processownership.DurableFanoutCompletionStore,
) error {
	remaining, err := processownership.DurableFanoutRemaining(
		ctx, completionStore, s.redisClient, plan,
	)
	if err != nil {
		return fmt.Errorf("replay data-table fan-in: %w", err)
	}
	if remaining > 0 {
		return nil
	}
	if err := s.enqueueDataTablePostProcess(payload); err != nil {
		return fmt.Errorf("replay data-table postprocess enqueue: %w", err)
	}
	return processownership.ClearFanIn(
		ctx, s.redisClient, payload.KnowledgeID, payload.ProcessingGeneration,
	)
}

func (s *DataTableSummaryService) completeDataTableFanIn(
	ctx context.Context,
	payload types.DataTableSummaryPayload,
	plan processownership.FanoutPlan,
	completionStore processownership.DurableFanoutCompletionStore,
) error {
	completionCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), finalizeSubtaskDetachedTimeout,
	)
	defer cancel()
	remaining, _, err := processownership.CompleteDurableFanoutItem(
		completionCtx,
		completionStore,
		s.redisClient,
		plan,
		processownership.DataTableFanoutItem(),
	)
	if err != nil {
		return fmt.Errorf("complete data-table fan-in: %w", err)
	}
	if remaining > 0 {
		return nil
	}
	if err := s.enqueueDataTablePostProcess(payload); err != nil {
		return fmt.Errorf("enqueue postprocess after data-table fan-in: %w", err)
	}
	if err := processownership.ClearFanIn(
		completionCtx, s.redisClient, payload.KnowledgeID, payload.ProcessingGeneration,
	); err != nil {
		logger.Warnf(ctx, "failed to clear completed data-table fan-in keys for %s: %v",
			payload.KnowledgeID, err)
	}
	return nil
}

func (s *DataTableSummaryService) enqueueDataTablePostProcess(
	payload types.DataTableSummaryPayload,
) error {
	return processownership.EnqueuePostProcess(s.taskEnqueuer, types.KnowledgePostProcessPayload{
		TracingContext:       payload.TracingContext,
		TenantID:             payload.TenantID,
		KnowledgeID:          payload.KnowledgeID,
		KnowledgeBaseID:      payload.KnowledgeBaseID,
		ProcessingGeneration: payload.ProcessingGeneration,
		Language:             payload.Language,
		Attempt:              payload.Attempt,
	})
}

// extractionResources 封装提取过程所需的所有资源
type extractionResources struct {
	knowledge      *types.Knowledge
	tenant         *types.Tenant
	chatModel      chat.Chat
	embeddingModel embedding.Embedder
	retrieveEngine *retriever.CompositeRetrieveEngine
}

// prepareResources 准备提取所需的所有资源
// 思路：集中加载所有依赖，统一错误处理，避免分散的资源获取逻辑
func (s *DataTableSummaryService) prepareResources(
	ctx context.Context,
	payload DataTableSummaryPayload,
	knowledge *types.Knowledge,
) (*extractionResources, error) {
	// 验证文件类型
	fileType := strings.ToLower(knowledge.FileType)
	if fileType != "csv" && fileType != "xlsx" && fileType != "xls" {
		logger.Warnf(ctx, "knowledge %s is not a CSV or Excel file, skipping table summary", payload.KnowledgeID)
		return nil, fmt.Errorf("unsupported file type: %s", fileType)
	}

	// 获取租户信息
	tenantInfo, err := s.tenantService.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		logger.Errorf(ctx, "failed to get tenant: %v", err)
		return nil, err
	}

	// 获取聊天模型（用于生成摘要）
	chatModel, err := s.modelService.GetChatModel(ctx, payload.SummaryModel)
	if err != nil {
		logger.Errorf(ctx, "failed to get chat model: %v", err)
		return nil, err
	}

	// 获取嵌入模型（用于向量化）
	embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, payload.EmbeddingModel)
	if err != nil {
		logger.Errorf(ctx, "failed to get embedding model: %v", err)
		return nil, err
	}

	// Load the KB to discover its VectorStoreID binding so the factory can
	// route to the bound store (or fall back to tenant engines if unbound).
	kb, err := s.knowledgeBaseService.GetKnowledgeBaseByID(ctx, knowledge.KnowledgeBaseID)
	if err != nil {
		logger.Errorf(ctx, "failed to get knowledge base for vector store lookup: %v", err)
		return nil, err
	}
	var vectorStoreID *string
	if kb != nil {
		vectorStoreID = kb.VectorStoreID
	}

	// The factory's unbound path reads TenantInfo from ctx.
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	// Resolve the engine via the factory using the KB's VectorStore binding
	// (nil -> tenant effective engines fallback; verified tenant ownership otherwise).
	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, payload.TenantID, vectorStoreID)
	if err != nil {
		logger.Errorf(ctx, "failed to get retrieve engine: %v", err)
		return nil, err
	}

	return &extractionResources{
		knowledge:      knowledge,
		tenant:         tenantInfo,
		chatModel:      chatModel,
		embeddingModel: embeddingModel,
		retrieveEngine: retrieveEngine,
	}, nil
}

// resolveFileServiceForKnowledge resolves a provider-specific file service for the current knowledge file.
// It falls back to the global service when tenant storage config is unavailable.
func (s *DataTableSummaryService) resolveFileServiceForKnowledge(ctx context.Context, resources *extractionResources) interfaces.FileService {
	if resources == nil || resources.knowledge == nil {
		return s.fileService
	}
	if resources.tenant == nil || resources.tenant.StorageEngineConfig == nil {
		return s.fileService
	}

	provider := types.InferStorageFromFilePath(resources.knowledge.FilePath)
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(resources.tenant.StorageEngineConfig.DefaultProvider))
	}
	if provider == "" {
		return s.fileService
	}

	baseDir := strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR"))
	resolvedSvc, resolvedProvider, err := filesvc.NewFileServiceFromStorageConfig(
		provider,
		resources.tenant.StorageEngineConfig,
		baseDir,
	)
	if err != nil {
		logger.Warnf(ctx, "[TableSummary] Failed to resolve file service for provider=%s, fallback to default: %v", provider, err)
		return s.fileService
	}
	logger.Infof(ctx, "[TableSummary] Resolved file service for knowledge=%s provider=%s", resources.knowledge.ID, resolvedProvider)
	return resolvedSvc
}

// processTableData 处理表格数据：加载 -> 分析 -> 生成摘要 -> 创建chunks
// 思路：将数据处理的核心流程集中在一起，保持逻辑连贯性
func (s *DataTableSummaryService) processTableData(ctx context.Context, resources *extractionResources) ([]*types.Chunk, error) {
	// 创建DuckDB会话并加载数据
	sessionID := fmt.Sprintf("table_summary_%s", resources.knowledge.ID)
	fileSvc := s.resolveFileServiceForKnowledge(ctx, resources)
	duckdbTool := tools.NewDataAnalysisTool(s.knowledgeBaseService, s.knowledgeService, s.tenantService, fileSvc, s.sqlDB, sessionID)
	defer duckdbTool.Cleanup(ctx)

	// 使用knowledge.ID作为表名，根据文件类型自动加载数据
	tableSchema, err := duckdbTool.LoadFromKnowledge(ctx, resources.knowledge)
	if err != nil {
		logger.Errorf(ctx, "failed to load data into DuckDB: %v", err)
		return nil, err
	}

	logger.Infof(ctx, "Loaded table %s with %d columns and %d rows", tableSchema.TableName, len(tableSchema.Columns), tableSchema.RowCount)

	// 获取样本数据用于生成摘要
	input := tools.DataAnalysisInput{
		KnowledgeID: resources.knowledge.ID,
		Sql:         fmt.Sprintf("SELECT * FROM \"%s\" LIMIT 10", tableSchema.TableName),
	}
	jsonData, err := json.Marshal(input)
	if err != nil {
		logger.Errorf(ctx, "failed to marshal input: %v", err)
		return nil, err
	}
	sampleResult, err := duckdbTool.Execute(ctx, jsonData)
	if err != nil {
		logger.Errorf(ctx, "failed to get sample data: %v", err)
		return nil, err
	}

	// 构建共用的schema和样本数据描述
	schemaDesc := tableSchema.Description()
	sampleDesc := s.buildSampleDataDescription(sampleResult, 10)

	// 使用AI生成表格摘要和列描述
	tableDescription, err := s.generateTableDescription(ctx, resources.chatModel, tableSchema.TableName, schemaDesc, sampleDesc)
	if err != nil {
		logger.Errorf(ctx, "failed to generate table description: %v", err)
		return nil, err
	}
	logger.Debugf(ctx, "table describe of knowledge %s: %s", resources.knowledge.ID, tableDescription)

	columnDescription, err := s.generateColumnDescriptions(ctx, resources.chatModel, tableSchema.TableName, schemaDesc, sampleDesc)
	if err != nil {
		logger.Errorf(ctx, "failed to generate column descriptions: %v", err)
		return nil, err
	}
	logger.Debugf(ctx, "column describe of knowledge %s: %s", resources.knowledge.ID, columnDescription)

	// 构建chunks：一个表格摘要chunk + 多个列描述chunks
	chunks := s.buildChunks(resources, tableDescription, columnDescription)
	return chunks, nil
}

// buildChunks 构建chunk对象
// tableDescription和columnDescriptions分别生成一个chunk
func (s *DataTableSummaryService) buildChunks(resources *extractionResources, tableDescription string, columnDescription string) []*types.Chunk {
	chunks := make([]*types.Chunk, 0, 2)

	// 表格摘要chunk
	summaryChunk := &types.Chunk{
		ID:              stableDataTableChunkID(resources.knowledge, "summary"),
		TenantID:        resources.knowledge.TenantID,
		KnowledgeID:     resources.knowledge.ID,
		KnowledgeBaseID: resources.knowledge.KnowledgeBaseID,
		Content:         tableDescription,
		ChunkIndex:      0,
		IsEnabled:       true,
		ChunkType:       types.ChunkTypeTableSummary,
		Status:          int(types.ChunkStatusStored),
	}
	chunks = append(chunks, summaryChunk)

	// 列描述chunk（所有列的描述合并为一个chunk）
	columnChunk := &types.Chunk{
		ID:              stableDataTableChunkID(resources.knowledge, "columns"),
		TenantID:        resources.knowledge.TenantID,
		KnowledgeID:     resources.knowledge.ID,
		KnowledgeBaseID: resources.knowledge.KnowledgeBaseID,
		Content:         columnDescription,
		ChunkIndex:      1,
		IsEnabled:       true,
		ChunkType:       types.ChunkTypeTableColumn,
		ParentChunkID:   summaryChunk.ID,
		Status:          int(types.ChunkStatusStored),
	}
	chunks = append(chunks, columnChunk)

	summaryChunk.NextChunkID = columnChunk.ID
	columnChunk.PreChunkID = summaryChunk.ID

	return chunks
}

func stableDataTableChunkID(knowledge *types.Knowledge, kind string) string {
	name := fmt.Sprintf(
		"%d:%s:%s:%s:datatable:%s",
		knowledge.TenantID,
		knowledge.KnowledgeBaseID,
		knowledge.ID,
		knowledge.ProcessingGeneration,
		kind,
	)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// indexToVectorDB 将chunks索引到向量数据库
// 思路：批量构建索引信息，统一索引，更新状态
func (s *DataTableSummaryService) indexToVectorDB(
	ctx context.Context,
	chunks []*types.Chunk,
	engine *retriever.CompositeRetrieveEngine,
	embedder embedding.Embedder,
) error {
	// 构建索引信息列表
	indexInfoList := make([]*types.IndexInfo, 0, len(chunks))
	for _, chunk := range chunks {
		indexInfoList = append(indexInfoList, &types.IndexInfo{
			Content:         chunk.Content,
			SourceID:        chunk.ID,
			SourceType:      types.ChunkSourceType,
			ChunkID:         chunk.ID,
			KnowledgeID:     chunk.KnowledgeID,
			KnowledgeBaseID: chunk.KnowledgeBaseID,
			IsEnabled:       true,
		})
	}

	// 保存到数据库
	if err := upsertGenerationChunks(ctx, s.chunkService, chunks); err != nil {
		logger.Errorf(ctx, "failed to upsert deterministic data-table chunks: %v", err)
		return err
	}
	logger.Infof(ctx, "Created %d chunks for data table", len(chunks))

	// 批量索引
	if err := engine.BatchIndex(ctx, embedder, indexInfoList); err != nil {
		logger.Errorf(ctx, "failed to index chunks: %v", err)
		return err
	}

	// 更新chunk状态为已索引
	for _, chunk := range chunks {
		chunk.Status = int(types.ChunkStatusIndexed)
	}
	if err := s.chunkService.UpdateChunks(ctx, chunks); err != nil {
		logger.Errorf(ctx, "failed to update chunk status: %v", err)
		return err
	}

	return nil
}

// cleanupOnFailure 索引失败时的清理工作
// 思路：删除已创建的chunk和对应的向量索引，避免脏数据残留
func (s *DataTableSummaryService) cleanupOnFailure(ctx context.Context, resources *extractionResources, chunks []*types.Chunk, indexErr error) {
	logger.Warnf(ctx, "Starting cleanup due to failure: %v", indexErr)

	// 提取chunk IDs
	chunkIDs := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		chunkIDs = append(chunkIDs, chunk.ID)
	}

	// 删除已创建的chunks
	if len(chunkIDs) > 0 {
		if err := s.chunkService.DeleteChunks(ctx, chunkIDs); err != nil {
			logger.Errorf(ctx, "Failed to delete chunks: %v", err)
		} else {
			logger.Infof(ctx, "Deleted %d chunks", len(chunkIDs))
		}
	}

	// 删除对应的向量索引
	if len(chunkIDs) > 0 {
		if err := resources.retrieveEngine.DeleteBySourceIDList(
			ctx, chunkIDs, resources.embeddingModel.GetDimensions(), types.KnowledgeBaseTypeDocument,
		); err != nil {
			logger.Errorf(ctx, "Failed to delete vector index: %v", err)
		} else {
			logger.Infof(ctx, "Deleted vector index for %d chunks", len(chunkIDs))
		}
	}

	logger.Infof(ctx, "Cleanup completed")
}

// generateTableDescription generates a summary description for the entire table
func (s *DataTableSummaryService) generateTableDescription(ctx context.Context, chatModel chat.Chat, tableName, schemaDesc, sampleDesc string) (string, error) {
	prompt := fmt.Sprintf(tableDescriptionPromptTemplate, tableName, schemaDesc, sampleDesc)
	// logger.Debugf(ctx, "generateTableDescription prompt: %s", prompt)

	thinking := false
	response, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "user", Content: prompt},
	}, &chat.ChatOptions{
		Temperature: 0.3,
		MaxTokens:   512,
		Thinking:    &thinking,
	})
	if err != nil {
		return "", fmt.Errorf("failed to generate table description: %w", err)
	}

	return fmt.Sprintf("# Table Summary\n\nTable name: %s\n\n%s", tableName, response.Content), nil
}

// generateColumnDescriptions generates descriptions for each column in batch
func (s *DataTableSummaryService) generateColumnDescriptions(ctx context.Context, chatModel chat.Chat, tableName, schemaDesc, sampleDesc string) (string, error) {
	// Build batch prompt for all columns
	prompt := fmt.Sprintf(columnDescriptionsPromptTemplate, tableName, schemaDesc, sampleDesc)
	// logger.Debugf(ctx, "generateColumnDescriptions prompt: %s", prompt)

	// Call LLM once for all columns
	thinking := false
	response, err := chatModel.Chat(ctx, []chat.Message{
		{Role: "user", Content: prompt},
	}, &chat.ChatOptions{
		Temperature: 0.3,
		MaxTokens:   2048,
		Thinking:    &thinking,
	})
	if err != nil {
		return "", fmt.Errorf("failed to generate column descriptions: %w", err)
	}

	return fmt.Sprintf("# Table Column Information\n\nTable name: %s\n\n%s", tableName, response.Content), nil
}

// buildSampleDataDescription builds a formatted sample data description
func (s *DataTableSummaryService) buildSampleDataDescription(sampleData *types.ToolResult, maxRows int) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Sample data (first %d rows):\n", maxRows))

	rows, ok := sampleData.Data["rows"].([]map[string]interface{})
	if !ok {
		return builder.String()
	}

	for i, row := range rows {
		if i >= maxRows {
			break
		}
		jsonBytes, err := json.Marshal(row)
		if err != nil {
			continue
		}
		builder.WriteString(string(jsonBytes))
		builder.WriteString("\n")
	}

	return builder.String()
}
