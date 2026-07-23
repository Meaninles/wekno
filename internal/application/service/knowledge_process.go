package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/custom/modules/corefanout"
	"github.com/Tencent/WeKnora/internal/custom/modules/fileguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/infrastructure/chunker"
	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

func (s *knowledgeService) cloneKnowledge(
	ctx context.Context,
	src *types.Knowledge,
	targetKB *types.KnowledgeBase,
) (err error) {
	if src.ParseStatus != "completed" {
		logger.GetLogger(ctx).WithField("knowledge_id", src.ID).Errorf("MoveKnowledge parse status is not completed")
		return nil
	}
	processingGeneration := uuid.NewString()
	dst := &types.Knowledge{
		ID:                   uuid.New().String(),
		TenantID:             targetKB.TenantID,
		KnowledgeBaseID:      targetKB.ID,
		Type:                 src.Type,
		Channel:              src.Channel,
		Title:                src.Title,
		Description:          src.Description,
		Source:               src.Source,
		ParseStatus:          types.ParseStatusProcessing,
		EnableStatus:         "disabled",
		EmbeddingModelID:     targetKB.EmbeddingModelID,
		FileName:             src.FileName,
		FileType:             src.FileType,
		FileSize:             src.FileSize,
		FileHash:             src.FileHash,
		FilePath:             src.FilePath,
		StorageSize:          0,
		Metadata:             src.Metadata,
		ProcessingGeneration: processingGeneration,
	}
	dst.ProcessingOwner = processownership.DocumentOwner(dst.ID, processingGeneration)

	// Deep-copy the source document into a persistent object owned by the
	// destination. The planned ownership row is committed before provider I/O
	// and atomically adopted by the destination knowledge row.
	if src.FilePath != "" {
		srcKB, kbErr := s.kbService.GetKnowledgeBaseByID(ctx, src.KnowledgeBaseID)
		if kbErr != nil {
			return fmt.Errorf("clone knowledge: failed to load source knowledge base: %w", kbErr)
		}
		sourceService, routeErr := s.auxiliaryFileServiceForPath(
			ctx, srcKB, src.KnowledgeBaseID, src.ID, src.FilePath,
		)
		if routeErr != nil {
			return fmt.Errorf("clone knowledge: resolve source file provider: %w", routeErr)
		}
		if copyErr := s.reserveCommitAndAdoptSourceCopy(
			ctx, targetKB, dst, sourceService, src.FilePath, src.FileSize,
		); copyErr != nil {
			return fmt.Errorf("clone knowledge file copy failed: %w", copyErr)
		}
	} else {
		if err = s.repo.CreateKnowledge(ctx, dst); err != nil {
			logger.GetLogger(ctx).WithField("error", err).Errorf("MoveKnowledge create knowledge failed")
			return
		}
	}
	if err = s.CloneChunk(ctx, src, dst); err != nil {
		logger.GetLogger(ctx).WithField("knowledge_id", dst.ID).
			WithField("error", err).Errorf("MoveKnowledge move chunks failed")
		// Publish failed only after every cloned artifact has been removed while
		// this exact generation still owns the row. If either the ownership probe
		// or cleanup fails, leave processing intact so housekeeping/retry can
		// recover it; a terminal failed row must never advertise leaked vectors.
		if fenceErr := s.heartbeatActiveProcessing(ctx, dst, "fence failed clone cleanup"); fenceErr != nil {
			err = errors.Join(err, fenceErr)
			return
		}
		if cleanupErr := s.cleanupKnowledgeResources(ctx, dst); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup failed clone artifacts: %w", cleanupErr))
			return
		}
		if markErr := s.markActiveProcessingFailed(ctx, dst, err.Error(), "mark failed clone generation"); markErr != nil {
			err = errors.Join(err, markErr)
		}
		return
	}

	completedAt := time.Now()
	dst.ParseStatus = types.ParseStatusCompleted
	dst.EnableStatus = "enabled"
	dst.StorageSize = src.StorageSize
	dst.ProcessedAt = &completedAt
	dst.UpdatedAt = completedAt
	dst.ProcessingOwner = ""
	dst.ProcessingFanout = nil
	finalized, finalizeErr := finalizeKnowledgeWithStorageOwned(
		ctx,
		s.repo,
		dst,
		types.ParseStatusProcessing,
		processingGeneration,
		processownership.DocumentOwner(dst.ID, processingGeneration),
		src.StorageSize,
	)
	if finalizeErr != nil {
		// Commit outcome can be uncertain. Never delete cloned artifacts after a
		// finalization error: the transaction may have committed before the
		// connection failed and a destructive rollback would corrupt a live row.
		return fmt.Errorf("atomically finalize cloned knowledge: %w", finalizeErr)
	}
	if !finalized {
		// Another lifecycle owner won. Its row and artifacts are authoritative.
		return fmt.Errorf("atomically finalize cloned knowledge: %w", errKnowledgeStateFenceConflict)
	}
	if tenantInfo, ok := ctx.Value(types.TenantInfoContextKey).(*types.Tenant); ok &&
		tenantInfo != nil && tenantInfo.ID == dst.TenantID {
		tenantInfo.StorageUsed += src.StorageSize
	}
	logger.GetLogger(ctx).WithField("knowledge_id", dst.ID).Infof("MoveKnowledge move knowledge successfully")
	return
}

// processDocumentFromPassage handles asynchronous processing of text passages
func (s *knowledgeService) processDocumentFromPassage(ctx context.Context,
	kb *types.KnowledgeBase, knowledge *types.Knowledge, passage []string,
) error {
	// Update status to processing
	claimTime := time.Now()
	if err := s.requireDocumentProcessingIdentitySwap(
		ctx,
		knowledge,
		types.ParseStatusPending,
		knowledge.ProcessingGeneration,
		knowledge.ProcessingOwner,
		map[string]interface{}{
			"parse_status": types.ParseStatusProcessing,
			"updated_at":   claimTime,
		},
		"claim synchronous passage processing",
	); err != nil {
		return err
	}
	knowledge.ParseStatus = types.ParseStatusProcessing
	knowledge.UpdatedAt = claimTime

	// Convert passages to chunks
	chunks := make([]types.ParsedChunk, 0, len(passage))
	start, end := 0, 0
	for i, p := range passage {
		if p == "" {
			continue
		}
		end += len([]rune(p))
		chunks = append(chunks, types.ParsedChunk{
			Content: p,
			Seq:     i,
			Start:   start,
			End:     end,
		})
		start = end
	}
	// Process and store chunks
	var opts ProcessChunksOptions
	if kb.QuestionGenerationConfig != nil && kb.QuestionGenerationConfig.Enabled {
		opts.EnableQuestionGeneration = true
		opts.QuestionCount = kb.QuestionGenerationConfig.QuestionCount
		if opts.QuestionCount <= 0 {
			opts.QuestionCount = 3
		}
	}
	return s.processChunks(ctx, kb, knowledge, chunks, opts)
}

// ProcessChunksOptions contains options for processing chunks
type ProcessChunksOptions struct {
	EnableQuestionGeneration bool
	QuestionCount            int
	EnableMultimodel         bool
	StoredImages             []docparser.StoredImage
	// ParentChunks holds parent chunk data when parent-child chunking is enabled.
	// When set, the chunks passed to processChunks are child chunks, and each
	// child's ParentIndex references an entry in this slice.
	ParentChunks []types.ParsedParentChunk
	Metadata     map[string]string
}

func buildDocumentFanoutPlan(
	ctx context.Context,
	knowledge *types.Knowledge,
	kb *types.KnowledgeBase,
	options ProcessChunksOptions,
	chunks []types.ParsedChunk,
) (processownership.FanoutPlan, error) {
	lang, _ := types.LanguageFromContext(ctx)
	traceCarrier := types.KnowledgePostProcessPayload{}
	langfuse.InjectTracing(ctx, &traceCarrier)
	plan := processownership.FanoutPlan{
		Version:              processownership.FanoutPlanVersion,
		TenantID:             knowledge.TenantID,
		KnowledgeID:          knowledge.ID,
		KnowledgeBaseID:      kb.ID,
		ProcessingGeneration: knowledge.ProcessingGeneration,
		Language:             lang,
		Attempt:              attemptFromCtx(ctx),
		Tracing:              traceCarrier.TracingContext,
	}
	switch strings.ToLower(getFileType(knowledge.FileName)) {
	case "csv", "xlsx", "xls":
		// Table enrichment is optional. A KB without a summary model must
		// still be able to parse, chunk and vectorize CSV/Excel documents.
		// Persisting a half-configured durable fanout would make every retry
		// fail after those core artifacts have already committed.
		if strings.TrimSpace(kb.SummaryModelID) != "" && strings.TrimSpace(kb.EmbeddingModelID) != "" {
			plan.DataTable = &processownership.DataTableFanout{
				SummaryModel:   kb.SummaryModelID,
				EmbeddingModel: kb.EmbeddingModelID,
			}
		} else {
			logger.Warnf(ctx,
				"Skipping optional data-table enrichment for knowledge %s: summary/embedding model is incomplete",
				knowledge.ID,
			)
		}
	}
	if options.EnableMultimodel {
		for idx, image := range options.StoredImages {
			chunkID := ""
			for _, chunk := range chunks {
				if strings.Contains(chunk.Content, image.ServingURL) {
					chunkID = chunk.ChunkID
					break
				}
			}
			if chunkID == "" && len(chunks) > 0 {
				chunkID = chunks[0].ChunkID
			}
			plan.Images = append(plan.Images, processownership.ImageFanout{
				ChunkID:         chunkID,
				ImageURL:        image.ServingURL,
				ImageSourceType: options.Metadata["image_source_type"],
				Index:           idx,
			})
		}
	}
	if err := plan.Validate(); err != nil {
		return processownership.FanoutPlan{}, err
	}
	return plan, nil
}

// finalizeIndexedKnowledgeState makes a document retrievable as soon as chunks
// and indexes are persisted (enable_status=enabled), but it deliberately does
// NOT mark the row completed when enrichment is still expected. Whenever the
// document still has work to fan out — pending multimodal image tasks, or text
// chunks that feed summary/question/graph generation — parse_status stays
// "processing" so KnowledgePostProcess remains the single authority that drives
// processing → finalizing → completed. Marking the row completed here would make
// post-process hit its "non-processing status" guard and skip the summary
// fan-out, stranding summary_status on "pending" forever.
func finalizeIndexedKnowledgeState(
	knowledge *types.Knowledge,
	totalStorageSize int64,
	textChunkCount int,
	hasPendingMultimodal bool,
	now time.Time,
) {
	if hasPendingMultimodal || textChunkCount > 0 {
		knowledge.ParseStatus = types.ParseStatusProcessing
		knowledge.SummaryStatus = types.SummaryStatusNone
	} else {
		// No text chunks and no pending multimodal work: there is nothing for
		// post-process to enrich, so complete immediately.
		knowledge.ParseStatus = types.ParseStatusCompleted
		knowledge.SummaryStatus = types.SummaryStatusNone
	}

	knowledge.EnableStatus = "enabled"
	knowledge.StorageSize = totalStorageSize
	knowledge.ProcessedAt = &now
	knowledge.UpdatedAt = now
}

// buildSplitterConfig creates a SplitterConfig with fallbacks from a KnowledgeBase.
// Defaults mirror chunker.DefaultChunkSize / DefaultChunkOverlap so behavior is
// identical whether callers come through this path or invoke the chunker
// directly with a zero-value config.
func buildSplitterConfig(kb *types.KnowledgeBase) chunker.SplitterConfig {
	return buildSplitterConfigFromChunking(kb.ChunkingConfig)
}

func buildSplitterConfigFromChunking(cc types.ChunkingConfig) chunker.SplitterConfig {
	chunkCfg := chunker.SplitterConfig{
		ChunkSize:    cc.ChunkSize,
		ChunkOverlap: cc.ChunkOverlap,
		Separators:   cc.Separators,
		Strategy:     cc.Strategy,
		TokenLimit:   cc.TokenLimit,
		Languages:    cc.Languages,
	}
	if chunkCfg.ChunkSize <= 0 {
		chunkCfg.ChunkSize = chunker.DefaultChunkSize
	}
	if chunkCfg.ChunkOverlap <= 0 {
		chunkCfg.ChunkOverlap = chunker.DefaultChunkOverlap
	}
	if len(chunkCfg.Separators) == 0 {
		chunkCfg.Separators = []string{"\n\n", "\n", "。"}
	}
	return chunkCfg
}

// buildParentChildConfigs derives parent and child SplitterConfig from ChunkingConfig.
// The base config (already validated with defaults) is used for separators.
func buildParentChildConfigs(cc types.ChunkingConfig, base chunker.SplitterConfig) (parent, child chunker.SplitterConfig) {
	parentSize := cc.ParentChunkSize
	if parentSize <= 0 {
		parentSize = 4096
	}
	childSize := cc.ChildChunkSize
	if childSize <= 0 {
		childSize = 384
	}
	parent = chunker.SplitterConfig{
		ChunkSize:    parentSize,
		ChunkOverlap: base.ChunkOverlap, // reuse configured overlap for parents
		Separators:   base.Separators,
	}
	child = chunker.SplitterConfig{
		ChunkSize:    childSize,
		ChunkOverlap: childSize / 5, // ~20% overlap for child chunks
		Separators:   base.Separators,
	}
	return
}

// processChunks processes chunks and creates embeddings for knowledge content
func (s *knowledgeService) processChunks(ctx context.Context,
	kb *types.KnowledgeBase, knowledge *types.Knowledge, chunks []types.ParsedChunk,
	opts ...ProcessChunksOptions,
) error {
	// Get options
	var options ProcessChunksOptions
	if len(opts) > 0 {
		options = opts[0]
	}

	// Check if knowledge is being deleted/cancelled before processing.
	// Both statuses short-circuit identically here — there's nothing to clean
	// up yet so the branch is purely "stop early".
	if aborted, status, abortErr := s.isKnowledgeAborted(ctx, knowledge.TenantID, knowledge.ID); abortErr != nil {
		return abortErr
	} else if aborted {
		logger.Infof(ctx, "Knowledge aborted (%s), skipping chunk processing: %s", status, knowledge.ID)
		return nil
	}
	if strings.TrimSpace(knowledge.ProcessingGeneration) == "" || strings.TrimSpace(knowledge.ProcessingOwner) == "" {
		return errors.New("process chunks: processing generation and owner are required")
	}

	// Get embedding model for vectorization — only needed when vector/keyword indexing is enabled
	var embeddingModel embedding.Embedder
	if kb.NeedsEmbeddingModel() {
		var err error
		embeddingModel, err = s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
		if err != nil {
			logger.GetLogger(ctx).WithField("error", err).Errorf("processChunks get embedding model failed")
			return err
		}
	} else {
		logger.Infof(ctx, "Vector/keyword indexing disabled for KB %s, skipping embedding model", kb.ID)
	}

	// 幂等性处理：清理旧的chunks和索引数据，避免重复数据
	logger.Infof(ctx, "Cleaning up existing chunks and index data for knowledge: %s", knowledge.ID)
	if err := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before deleting old chunks"); err != nil {
		return err
	}

	// 删除旧的chunks
	if err := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); err != nil {
		return fmt.Errorf("delete existing chunks before processing: %w", err)
	}

	// 删除旧的索引数据 — only when vector/keyword indexing is enabled
	tenantInfo := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, tenantInfo.ID, kb.VectorStoreID)
	if err != nil {
		return fmt.Errorf("resolve retrieve engine before processing: %w", err)
	}
	if embeddingModel != nil {
		if err := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before deleting old index"); err != nil {
			return err
		}
		if err := retrieveEngine.DeleteByKnowledgeIDList(ctx, []string{knowledge.ID}, embeddingModel.GetDimensions(), knowledge.Type); err != nil {
			return fmt.Errorf("delete existing index before processing: %w", err)
		} else {
			logger.Infof(ctx, "Successfully deleted existing index data for knowledge: %s", knowledge.ID)
		}
	}

	// 删除知识图谱数据（如果存在）
	if err := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before deleting old graph"); err != nil {
		return err
	}
	namespace := types.NameSpace{KnowledgeBase: knowledge.KnowledgeBaseID, Knowledge: knowledge.ID}
	if err := s.graphEngine.DelGraph(ctx, []types.NameSpace{namespace}); err != nil {
		return fmt.Errorf("delete existing graph before processing: %w", err)
	}

	logger.Infof(ctx, "Cleanup completed, starting to process new chunks")

	// ========== DocReader 解析结果日志 ==========
	logger.Infof(ctx, "[DocReader] ========== 解析结果概览 ==========")
	logger.Infof(ctx, "[DocReader] 知识ID: %s, 知识库ID: %s", knowledge.ID, knowledge.KnowledgeBaseID)
	logger.Infof(ctx, "[DocReader] 总Chunk数量: %d", len(chunks))

	// 统计图片信息
	totalImages := 0
	chunksWithImages := 0
	for _, chunkData := range chunks {
		if len(chunkData.Images) > 0 {
			chunksWithImages++
			totalImages += len(chunkData.Images)
		}
	}
	logger.Infof(ctx, "[DocReader] 包含图片的Chunk数: %d, 总图片数: %d", chunksWithImages, totalImages)

	// 打印每个Chunk的详细信息
	for idx, chunkData := range chunks {
		contentPreview := chunkData.Content
		if len(contentPreview) > 200 {
			contentPreview = contentPreview[:200] + "..."
		}
		logger.Infof(ctx, "[DocReader] Chunk #%d (seq=%d): 内容长度=%d, 图片数=%d, 范围=[%d-%d]",
			idx, chunkData.Seq, len(chunkData.Content), len(chunkData.Images), chunkData.Start, chunkData.End)
		logger.Debugf(ctx, "[DocReader] Chunk #%d 内容预览: %s", idx, contentPreview)

		// 打印图片详细信息
		for imgIdx, img := range chunkData.Images {
			logger.Infof(ctx, "[DocReader]   图片 #%d: URL=%s", imgIdx, img.URL)
			logger.Infof(ctx, "[DocReader]   图片 #%d: OriginalURL=%s", imgIdx, img.OriginalURL)
			if img.Caption != "" {
				captionPreview := img.Caption
				if len(captionPreview) > 100 {
					captionPreview = captionPreview[:100] + "..."
				}
				logger.Infof(ctx, "[DocReader]   图片 #%d: Caption=%s", imgIdx, captionPreview)
			}
			if img.OCRText != "" {
				ocrPreview := img.OCRText
				if len(ocrPreview) > 100 {
					ocrPreview = ocrPreview[:100] + "..."
				}
				logger.Infof(ctx, "[DocReader]   图片 #%d: OCRText=%s", imgIdx, ocrPreview)
			}
			logger.Infof(ctx, "[DocReader]   图片 #%d: 位置=[%d-%d]", imgIdx, img.Start, img.End)
		}
	}
	logger.Infof(ctx, "[DocReader] ========== 解析结果概览结束 ==========")

	// Create chunk objects from proto chunks
	maxSeq := 0

	// 统计图片相关的子Chunk数量，用于扩展insertChunks的容量
	imageChunkCount := 0
	for _, chunkData := range chunks {
		if len(chunkData.Images) > 0 {
			// 为每个图片的OCR和Caption分别创建一个Chunk
			imageChunkCount += len(chunkData.Images) * 2
		}
		if int(chunkData.Seq) > maxSeq {
			maxSeq = int(chunkData.Seq)
		}
	}

	// === Parent-Child Chunking: create parent chunks first ===
	hasParentChild := len(options.ParentChunks) > 0
	var parentDBChunks []*types.Chunk // indexed by ParsedParentChunk position
	if hasParentChild {
		parentDBChunks = make([]*types.Chunk, len(options.ParentChunks))
		for i, pc := range options.ParentChunks {
			parentDBChunks[i] = &types.Chunk{
				ID:              uuid.New().String(),
				TenantID:        knowledge.TenantID,
				KnowledgeID:     knowledge.ID,
				KnowledgeBaseID: knowledge.KnowledgeBaseID,
				Content:         pc.Content,
				ChunkIndex:      pc.Seq,
				IsEnabled:       true,
				CreatedAt:       time.Now(),
				UpdatedAt:       time.Now(),
				StartAt:         pc.Start,
				EndAt:           pc.End,
				ChunkType:       types.ChunkTypeParentText,
			}
		}
		// Set prev/next links for parent chunks
		for i := range parentDBChunks {
			if i > 0 {
				parentDBChunks[i-1].NextChunkID = parentDBChunks[i].ID
				parentDBChunks[i].PreChunkID = parentDBChunks[i-1].ID
			}
		}
		logger.Infof(ctx, "Created %d parent chunks for parent-child strategy", len(parentDBChunks))
	}

	// 重新分配容量，考虑图片相关的Chunk + parent chunks
	parentCount := len(options.ParentChunks)
	insertChunks := make([]*types.Chunk, 0, len(chunks)+imageChunkCount+parentCount)
	// Add parent chunks first (they go into DB but NOT into the vector index)
	if hasParentChild {
		insertChunks = append(insertChunks, parentDBChunks...)
	}

	for idx, chunkData := range chunks {
		if strings.TrimSpace(chunkData.Content) == "" {
			continue
		}

		// 创建主文本Chunk
		textChunk := &types.Chunk{
			ID:              uuid.New().String(),
			TenantID:        knowledge.TenantID,
			KnowledgeID:     knowledge.ID,
			KnowledgeBaseID: knowledge.KnowledgeBaseID,
			Content:         chunkData.Content,
			ContextHeader:   chunkData.ContextHeader,
			ChunkIndex:      int(chunkData.Seq),
			IsEnabled:       true,
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
			StartAt:         int(chunkData.Start),
			EndAt:           int(chunkData.End),
			ChunkType:       types.ChunkTypeText,
		}

		// Wire up ParentChunkID for child chunks
		if hasParentChild && chunkData.ParentIndex >= 0 && chunkData.ParentIndex < len(parentDBChunks) {
			textChunk.ParentChunkID = parentDBChunks[chunkData.ParentIndex].ID
		}

		chunks[idx].ChunkID = textChunk.ID
		insertChunks = append(insertChunks, textChunk)
	}

	// Sort chunks by index for proper ordering
	sort.Slice(insertChunks, func(i, j int) bool {
		return insertChunks[i].ChunkIndex < insertChunks[j].ChunkIndex
	})

	// 仅为文本类型的Chunk设置前后关系（child chunks only, parents already linked above）
	textChunks := make([]*types.Chunk, 0, len(chunks))
	for _, chunk := range insertChunks {
		if chunk.ChunkType == types.ChunkTypeText && chunk.ParentChunkID != "" {
			// This is a child chunk in parent-child mode
			textChunks = append(textChunks, chunk)
		} else if chunk.ChunkType == types.ChunkTypeText && !hasParentChild {
			// Normal flat chunk (no parent-child mode)
			textChunks = append(textChunks, chunk)
		}
	}

	// 设置文本Chunk之间的前后关系 (skip if parent-child, children don't need prev/next links)
	if !hasParentChild {
		for i, chunk := range textChunks {
			if i > 0 {
				textChunks[i-1].NextChunkID = chunk.ID
			}
			if i < len(textChunks)-1 {
				textChunks[i+1].PreChunkID = chunk.ID
			}
		}
	}

	// Check if knowledge is being deleted/cancelled before writing chunks.
	// Nothing has been persisted yet, so both branches just bail.
	if aborted, status, abortErr := s.isKnowledgeAborted(ctx, knowledge.TenantID, knowledge.ID); abortErr != nil {
		return abortErr
	} else if aborted {
		logger.Infof(ctx, "Knowledge aborted (%s), skipping chunk write: %s", status, knowledge.ID)
		return nil
	}

	// Save chunks to database — ALWAYS, regardless of indexing strategy.
	// Chunks are needed for wiki generation, graph extraction, and summary generation
	// even when vector/keyword indexing is disabled.
	s.beginStage(ctx, knowledge.ID, types.StageChunking, types.JSONMap{
		"chunks_planned": len(insertChunks),
	})
	if err := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before creating chunks"); err != nil {
		return err
	}
	if err := s.chunkService.CreateChunks(ctx, insertChunks); err != nil {
		if checkpointErr := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before failed chunk-write cleanup"); checkpointErr != nil {
			return errors.Join(err, checkpointErr)
		}
		if sequenceErr := cleanupArtifactsBeforeFailureTransition(
			func() error {
				if cleanupErr := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); cleanupErr != nil {
					// CreateChunks can be partially committed by a batched repository.
					// Never publish Failed until that possible partial write is gone.
					return errors.Join(err, fmt.Errorf("cleanup partial chunk write: %w", cleanupErr))
				}
				return nil
			},
			func() error {
				if markErr := s.markActiveProcessingFailed(ctx, knowledge, err.Error(), "mark chunk creation failed"); markErr != nil {
					return errors.Join(err, markErr)
				}
				return nil
			},
		); sequenceErr != nil {
			return sequenceErr
		}
		s.failStage(ctx, knowledge.ID, types.StageChunking,
			werrors.ErrCodeChunkingFailed, "create chunks failed", err)
		return nil
	}
	totalChunkChars := 0
	for _, c := range insertChunks {
		totalChunkChars += len(c.Content)
	}
	s.endStage(ctx, knowledge.ID, types.StageChunking, types.JSONMap{
		"chunks_written":   len(insertChunks),
		"total_text_chars": totalChunkChars,
	})

	// Create index information and perform vector indexing — only when vector/keyword is enabled.
	// Chunks are ALWAYS saved to DB (above) because wiki and graph need them even without vector indexing.
	var totalStorageSize int64
	if kb.NeedsEmbeddingModel() && embeddingModel != nil {
		embedInput := types.JSONMap{
			"chunks_to_embed": len(textChunks),
			"model_id":        kb.EmbeddingModelID,
		}
		if dim := embeddingModel.GetDimensions(); dim > 0 {
			embedInput["dim"] = dim
		}
		s.beginStage(ctx, knowledge.ID, types.StageEmbedding, embedInput)
		// Create index information — only for child/flat chunks, NOT parent chunks.
		// Parent chunks are stored for context retrieval but do not need vector embeddings.
		// Prepend the document title to improve semantic alignment between
		// question-style queries and statement-style chunk content.
		indexInfoList := make([]*types.IndexInfo, 0, len(textChunks))
		titlePrefix := ""
		if t := strings.TrimSpace(knowledge.Title); t != "" {
			titlePrefix = t + "\n"
		}
		for _, chunk := range textChunks {
			// chunk.EmbeddingContent prepends ContextHeader (heading breadcrumb)
			// when the chunker populated it during Tier-1 splitting; falls back
			// to plain Content otherwise. Title prefix sits outermost.
			indexContent := titlePrefix + chunk.EmbeddingContent()
			indexInfoList = append(indexInfoList, &types.IndexInfo{
				Content:         indexContent,
				SourceID:        chunk.ID,
				SourceType:      types.ChunkSourceType,
				ChunkID:         chunk.ID,
				KnowledgeID:     knowledge.ID,
				KnowledgeBaseID: knowledge.KnowledgeBaseID,
				IsEnabled:       true,
			})
		}

		// Calculate storage size required for embeddings
		totalStorageSize = retrieveEngine.EstimateStorageSize(ctx, embeddingModel, indexInfoList)
		if tenantInfo.StorageQuota > 0 {
			// Re-fetch tenant storage information
			tenantInfo, err = s.tenantRepo.GetTenantByID(ctx, tenantInfo.ID)
			if err != nil {
				if checkpointErr := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before tenant-refresh cleanup"); checkpointErr != nil {
					return errors.Join(err, checkpointErr)
				}
				if cleanupErr := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); cleanupErr != nil {
					return errors.Join(err, fmt.Errorf("cleanup chunks after tenant refresh failure: %w", cleanupErr))
				}
				return fmt.Errorf("refresh tenant storage before indexing: %w", err)
			}
			// Check if there's enough storage quota available
			if tenantInfo.StorageUsed+totalStorageSize > tenantInfo.StorageQuota {
				quotaErr := errors.New("存储空间不足")
				if checkpointErr := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before quota-failure cleanup"); checkpointErr != nil {
					return errors.Join(quotaErr, checkpointErr)
				}
				if sequenceErr := cleanupArtifactsBeforeFailureTransition(
					func() error {
						if cleanupErr := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); cleanupErr != nil {
							return errors.Join(quotaErr, fmt.Errorf("cleanup chunks after storage quota failure: %w", cleanupErr))
						}
						return nil
					},
					func() error {
						if markErr := s.markActiveProcessingFailed(ctx, knowledge, "存储空间不足", "mark storage quota failure"); markErr != nil {
							return errors.Join(quotaErr, markErr)
						}
						return nil
					},
				); sequenceErr != nil {
					return sequenceErr
				}
				return nil
			}
		}

		// Check again before batch indexing (heavy operation).
		// deleting → row is going away anyway, drop the chunks we just wrote.
		// cancelled → user wants to keep what was already persisted, just stop.
		if aborted, status, abortErr := s.isKnowledgeAborted(ctx, knowledge.TenantID, knowledge.ID); abortErr != nil {
			return abortErr
		} else if aborted {
			logger.Infof(ctx, "Knowledge aborted (%s) before indexing: %s", status, knowledge.ID)
			if status == types.ParseStatusDeleting {
				if err := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); err != nil {
					logger.Warnf(ctx, "Failed to cleanup chunks after deletion detected: %v", err)
				}
			}
			return nil
		}

		if err := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before batch indexing"); err != nil {
			return err
		}
		err = retrieveEngine.BatchIndex(ctx, embeddingModel, indexInfoList)
		if err != nil {
			// delete failed chunks
			if checkpointErr := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint before vector failure cleanup"); checkpointErr != nil {
				return errors.Join(err, checkpointErr)
			}
			if sequenceErr := cleanupArtifactsBeforeFailureTransition(
				func() error {
					var cleanupErr error
					if deleteErr := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); deleteErr != nil {
						cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete chunks after vector failure: %w", deleteErr))
					}
					if deleteErr := retrieveEngine.DeleteByKnowledgeIDList(
						ctx, []string{knowledge.ID}, embeddingModel.GetDimensions(), kb.Type,
					); deleteErr != nil {
						cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete index after vector failure: %w", deleteErr))
					}
					if cleanupErr != nil {
						return errors.Join(err, cleanupErr)
					}
					return nil
				},
				func() error {
					if markErr := s.markActiveProcessingFailed(ctx, knowledge, err.Error(), "mark vector indexing failed"); markErr != nil {
						return errors.Join(err, markErr)
					}
					return nil
				},
			); sequenceErr != nil {
				return sequenceErr
			}
			// Map vector store / embedding rate-limit errors to a
			// stable code so the UI can offer "retry later" hints.
			code := werrors.ErrCodeVectorStoreWriteFailed
			if isLikelyRateLimitError(err) {
				code = werrors.ErrCodeEmbeddingRateLimit
			}
			s.failStage(ctx, knowledge.ID, types.StageEmbedding,
				code, "batch index failed", err)
			return nil
		}
		logger.GetLogger(ctx).Infof("processChunks batch index successfully, with %d index", len(indexInfoList))
		s.endStage(ctx, knowledge.ID, types.StageEmbedding, types.JSONMap{
			"vectors_written": len(indexInfoList),
			"storage_bytes":   totalStorageSize,
		})

		// Final check before marking as completed.
		// deleting → drop chunks+index we just wrote.
		// cancelled → keep persisted data; the row stays in cancelled status
		// and downstream stages skip via the entry guards.
		if aborted, status, abortErr := s.isKnowledgeAborted(ctx, knowledge.TenantID, knowledge.ID); abortErr != nil {
			return abortErr
		} else if aborted {
			logger.Infof(ctx, "Knowledge aborted (%s) after indexing: %s", status, knowledge.ID)
			if status == types.ParseStatusDeleting {
				if err := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); err != nil {
					logger.Warnf(ctx, "Failed to cleanup chunks after deletion detected: %v", err)
				}
				if err := retrieveEngine.DeleteByKnowledgeIDList(ctx, []string{knowledge.ID}, embeddingModel.GetDimensions(), kb.Type); err != nil {
					logger.Warnf(ctx, "Failed to cleanup index after deletion detected: %v", err)
				}
			}
			return nil
		}
	} else {
		logger.Infof(ctx, "Vector/keyword indexing disabled for KB %s, skipping BatchIndex", kb.ID)
		s.skipStage(ctx, knowledge.ID, types.StageEmbedding, "skipped")
	}

	// Check if this document has extracted images that will be processed asynchronously
	isImage := IsImageType(knowledge.FileType)
	isVideo := IsVideoType(knowledge.FileType)
	pendingMultimodal := isImage && options.EnableMultimodel && len(options.StoredImages) > 0
	pendingPDFMultimodal := !isImage && !isVideo && options.EnableMultimodel && len(options.StoredImages) > 0

	expectedFinalizeStatus := knowledge.ParseStatus
	expectedGeneration := knowledge.ProcessingGeneration
	expectedOwner := knowledge.ProcessingOwner
	now := time.Now()
	finalizeIndexedKnowledgeState(
		knowledge,
		totalStorageSize,
		len(textChunks),
		pendingMultimodal || pendingPDFMultimodal,
		now,
	)

	var fanoutPlan processownership.FanoutPlan
	if knowledge.ParseStatus == types.ParseStatusProcessing {
		var planErr error
		fanoutPlan, planErr = buildDocumentFanoutPlan(ctx, knowledge, kb, options, chunks)
		if planErr != nil {
			return fmt.Errorf("build durable document fanout: %w", planErr)
		}
		planBytes, planErr := processownership.MarshalFanoutPlan(fanoutPlan)
		if planErr != nil {
			return fmt.Errorf("marshal durable document fanout: %w", planErr)
		}
		knowledge.ProcessingFanout = types.JSON(planBytes)
	} else {
		knowledge.ProcessingFanout = nil
	}
	// Consuming the owner in the same transaction is what makes a retry enter
	// fan-out recovery instead of deleting and rebuilding committed artifacts.
	knowledge.ProcessingOwner = ""
	finalized, finalizeErr := finalizeKnowledgeWithStorageOwned(
		ctx,
		s.repo,
		knowledge,
		expectedFinalizeStatus,
		expectedGeneration,
		expectedOwner,
		totalStorageSize,
	)
	if finalizeErr != nil || !finalized {
		if finalizeErr == nil {
			finalizeErr = errKnowledgeStateFenceConflict
		}
		logger.GetLogger(ctx).WithField("error", finalizeErr).
			Errorf("processChunks atomic knowledge/storage finalization failed")
		// Clean only if an exact owner heartbeat proves this generation is still
		// active. A committed-but-uncertain response consumes owner; a newer
		// generation or delete/cancel also makes this checkpoint fail closed.
		knowledge.ProcessingOwner = expectedOwner
		if checkpointErr := s.heartbeatActiveProcessing(ctx, knowledge, "checkpoint after finalization conflict"); checkpointErr == nil {
			if err := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); err != nil {
				finalizeErr = errors.Join(finalizeErr, fmt.Errorf("cleanup chunks after finalization failure: %w", err))
			}
			if kb.NeedsEmbeddingModel() && retrieveEngine != nil && embeddingModel != nil {
				if err := retrieveEngine.DeleteByKnowledgeIDList(
					ctx, []string{knowledge.ID}, embeddingModel.GetDimensions(), kb.Type,
				); err != nil {
					finalizeErr = errors.Join(finalizeErr, fmt.Errorf("cleanup index after finalization failure: %w", err))
				}
			}
		} else {
			logger.Infof(ctx, "Skipping finalization-conflict cleanup for %s because exact generation ownership was lost: %v",
				knowledge.ID, checkpointErr)
		}
		s.failStage(ctx, knowledge.ID, types.StageEmbedding,
			werrors.ErrCodeVectorStoreWriteFailed, "atomic finalization failed", finalizeErr)
		return finalizeErr
	}
	tenantInfo.StorageUsed += totalStorageSize

	if knowledge.ParseStatus == types.ParseStatusCompleted {
		s.skipStage(ctx, knowledge.ID, types.StageMultimodal, "skipped")
		logger.GetLogger(ctx).Infof("processChunks successfully")
		return nil
	}

	// Dispatch only from the durable plan written by the core commit. Any real
	// enqueue error propagates so Asynq retries and replays the same stable IDs.
	if len(fanoutPlan.Images) > 0 {
		s.beginStage(ctx, knowledge.ID, types.StageMultimodal, types.JSONMap{
			"image_count":    len(fanoutPlan.Images),
			"enable_ocr":     true,
			"enable_caption": true,
		})
	} else {
		s.skipStage(ctx, knowledge.ID, types.StageMultimodal, "skipped")
	}
	completionStore, ok := s.repo.(processownership.DurableFanoutCompletionStore)
	if !ok || completionStore == nil {
		return errors.New("dispatch durable document fanout: durable completion store is unavailable")
	}
	if err := processownership.DispatchFanout(ctx, s.task, s.redisClient, fanoutPlan, completionStore); err != nil {
		return fmt.Errorf("dispatch durable document fanout: %w", err)
	}

	logger.GetLogger(ctx).Infof("processChunks successfully")
	return nil
}

// defaultMaxInputChars is the default maximum characters used as input for summary generation.
const defaultMaxInputChars = 1024 * 24

func summaryGenerationChunkID(knowledgeID, generation string) string {
	return uuid.NewSHA1(
		uuid.NameSpaceOID,
		[]byte("weknora:summary:"+knowledgeID+":"+generation),
	).String()
}

func questionGenerationID(generation, chunkID string, index int) string {
	return "q" + uuid.NewSHA1(
		uuid.NameSpaceOID,
		[]byte(fmt.Sprintf("weknora:question:%s:%s:%d", generation, chunkID, index)),
	).String()
}

// questionVectorSourceID deliberately uses the generation-scoped question ID
// directly. The embeddings schema limits source_id to 64 characters; joining
// a UUID chunk ID and a UUID question ID produces 74 characters and makes
// every generated-question index write fail deterministically. Chunk identity
// is already stored in IndexInfo.ChunkID, while questionGenerationID includes
// generation, chunk and ordinal in its SHA-1 namespace, so the compact ID is
// both unambiguous and retry-stable.
func questionVectorSourceID(questionID string) string {
	return questionID
}

// imageDominatedTextThreshold is the rune count below which a document is
// considered "image-dominated" — i.e. the body text is so sparse that we
// should fall back to full image enrichment (caption + OCR) for the summary
// LLM call. Above this threshold the document has enough native text that
// caption-only enrichment is preferable (OCR text from incidental figures
// would otherwise add noise without contributing to the main topic).
const imageDominatedTextThreshold = 200

// errInsufficientSummaryContent signals that getSummary refused to call the
// LLM because the document had no usable text after image markup was stripped
// (typical for scanned PDFs where VLM OCR yielded nothing). Callers should
// mark the knowledge's summary as failed instead of falling back to the first
// chunk's raw content (which would just be a bare image reference).
var errInsufficientSummaryContent = errors.New("insufficient text content for summary generation")

// checkSufficientSummaryContent returns errInsufficientSummaryContent if the
// given content does not carry enough real text (after stripping image markup)
// for an LLM summary call, and logs a warning at the call site. Returns nil
// when the content passes the threshold.
//
// Extracted so the threshold gate can be unit-tested without standing up the
// full ProcessSummaryGeneration dependency graph.
func checkSufficientSummaryContent(ctx context.Context, knowledgeID, content string) error {
	realTextLen := realTextRuneCount(content)
	if realTextLen < minTextContentRunes {
		logger.GetLogger(ctx).Warnf(
			"summary content check: knowledge %s has insufficient text after stripping image markup (real_text_runes=%d, min=%d); skipping LLM call",
			knowledgeID, realTextLen, minTextContentRunes,
		)
		return errInsufficientSummaryContent
	}
	return nil
}

// getSummary generates a summary for knowledge content using an AI model
func (s *knowledgeService) getSummary(ctx context.Context,
	summaryModel chat.Chat, knowledge *types.Knowledge, chunks []*types.Chunk,
) (string, error) {
	// Get knowledge info from the first chunk
	if len(chunks) == 0 {
		return "", fmt.Errorf("no chunks provided for summary generation")
	}

	// Determine max input chars from config
	maxInputChars := defaultMaxInputChars
	if s.config.Conversation.Summary != nil && s.config.Conversation.Summary.MaxInputChars > 0 {
		maxInputChars = s.config.Conversation.Summary.MaxInputChars
	}

	// Sort chunks by logical order. Physical split generations use the global
	// chunk index because StartAt is a synthetic sparse coordinate; ordinary
	// one-shot documents preserve the historical StartAt reconstruction.
	sortedChunks := make([]*types.Chunk, len(chunks))
	copy(sortedChunks, chunks)
	isPhysicalSplit := false
	for _, chunk := range sortedChunks {
		if chunk.ProcessingGeneration != "" && chunk.SplitPartIndex >= 0 {
			isPhysicalSplit = true
			break
		}
	}
	if isPhysicalSplit {
		sort.Slice(sortedChunks, func(i, j int) bool {
			return sortedChunks[i].ChunkIndex < sortedChunks[j].ChunkIndex
		})
	} else {
		sort.Slice(sortedChunks, func(i, j int) bool {
			return sortedChunks[i].StartAt < sortedChunks[j].StartAt
		})
	}

	// Concatenate original chunk contents by StartAt offset to reconstruct the
	// document, then enrich with image info in a second pass. Enrichment must
	// happen AFTER concatenation because StartAt is based on original document
	// offsets — enriched (longer) content would break the positioning.
	chunkContents := ""
	if isPhysicalSplit {
		chunkContents = buildPhysicalSplitSummarySample(sortedChunks, maxInputChars)
	} else {
		for _, chunk := range sortedChunks {
			runes := []rune(chunkContents)
			if chunk.StartAt <= len(runes) {
				chunkContents = string(runes[:chunk.StartAt]) + chunk.Content
			} else {
				chunkContents = chunkContents + chunk.Content
			}
		}
	}

	// Collect image_info from image_ocr/image_caption children and enrich
	chunkIDs := make([]string, len(sortedChunks))
	for i, c := range sortedChunks {
		chunkIDs[i] = c.ID
	}
	imageInfoMap := searchutil.CollectImageInfoByChunkIDs(ctx, s.chunkRepo, knowledge.TenantID, chunkIDs)
	mergedImageInfo := searchutil.MergeImageInfoJSON(imageInfoMap)
	if mergedImageInfo != "" {
		// For image-dominated documents (e.g. a docx whose only payload is a
		// single embedded picture, or a screenshot-only file), captions alone
		// often carry too little signal — the real content lives in OCR text.
		// Detect that case by measuring the document's real (non-image-markup)
		// text BEFORE enrichment, and switch to full enrichment (caption + OCR)
		// when the body is essentially empty. Text-heavy documents stay on the
		// caption-only path to avoid OCR noise (page headers/footers/watermarks
		// from many figures diluting the main topic).
		if realTextRuneCount(chunkContents) < imageDominatedTextThreshold {
			// Caption + OCR (no URL/original wrappers — those are pure noise
			// for the summary LLM and have been observed to trigger the
			// "image reference with no extracted text" refusal heuristic).
			chunkContents = searchutil.EnrichContentCaptionAndOCR(chunkContents, mergedImageInfo)
		} else {
			chunkContents = searchutil.EnrichContentCaptionOnly(chunkContents, mergedImageInfo)
		}
	}

	// Apply length limit: sample long content to fit within maxInputChars
	if !isPhysicalSplit {
		chunkContents = sampleLongContent(chunkContents, maxInputChars)
	}

	logger.GetLogger(ctx).Infof("getSummary: content length=%d chars (max=%d) for knowledge %s",
		len([]rune(chunkContents)), maxInputChars, knowledge.ID)

	// Bail out before the LLM call when there is not enough actual text to
	// summarise. We deliberately do not pass filename/file-type metadata to the
	// LLM: scanned PDFs frequently carry filenames like "MX5280.pdf" (the
	// scanner model), and feeding that to the model would invite it to
	// hallucinate a scanner manual instead of admitting the document had no
	// extractable text.
	if err := checkSufficientSummaryContent(ctx, knowledge.ID, chunkContents); err != nil {
		return "", err
	}

	// Pass the raw chunk text to the LLM with no filename / file-type framing.
	contentWithMetadata := chunkContents

	// Determine max output tokens from config
	maxTokens := 2048
	if s.config.Conversation.Summary != nil && s.config.Conversation.Summary.MaxCompletionTokens > 0 {
		maxTokens = s.config.Conversation.Summary.MaxCompletionTokens
	}

	// Generate summary using AI model
	summaryPrompt := types.RenderPromptPlaceholders(s.config.Conversation.GenerateSummaryPrompt, types.PlaceholderValues{
		"language": types.LanguageNameFromContext(ctx),
	})
	thinking := false
	summary, err := summaryModel.Chat(ctx, []chat.Message{
		{
			Role:    "system",
			Content: summaryPrompt,
		},
		{
			Role:    "user",
			Content: contentWithMetadata,
		},
	}, &chat.ChatOptions{
		Temperature: 0.3,
		MaxTokens:   maxTokens,
		Thinking:    &thinking,
	})
	if err != nil {
		logger.GetLogger(ctx).WithField("error", err).Errorf("GetSummary failed")
		return "", err
	}
	logger.GetLogger(ctx).WithField("summary", summary.Content).Infof("GetSummary success")
	return summary.Content, nil
}

func buildPhysicalSplitSummarySample(chunks []*types.Chunk, maxChars int) string {
	if len(chunks) == 0 || maxChars <= 0 {
		return ""
	}
	// The loader already chooses evenly distributed logical positions. Divide
	// the LLM budget across those strata so a huge workbook/book/audio file is
	// represented end-to-end rather than by only the first physical part.
	perChunk := maxChars / len(chunks)
	if perChunk < 160 {
		perChunk = 160
	}
	var builder strings.Builder
	for _, chunk := range chunks {
		header := ""
		var locator map[string]any
		if len(chunk.SourceLocator) > 0 && json.Unmarshal(chunk.SourceLocator, &locator) == nil {
			header = logicalLocatorHeader(locator)
		}
		available := perChunk
		if header != "" {
			header = "\n\n【" + header + "】\n"
			builder.WriteString(header)
			available -= len([]rune(header))
		}
		if available <= 0 {
			continue
		}
		runes := []rune(strings.TrimSpace(chunk.Content))
		if len(runes) > available {
			head := available * 2 / 3
			tail := available - head
			builder.WriteString(string(runes[:head]))
			builder.WriteString("\n[…]\n")
			builder.WriteString(string(runes[len(runes)-tail:]))
		} else {
			builder.WriteString(string(runes))
		}
	}
	result := []rune(builder.String())
	if len(result) > maxChars {
		return string(result[:maxChars])
	}
	return string(result)
}

// sampleLongContent returns content that fits within maxChars.
// For short content (≤ maxChars), it is returned as-is.
// For long content, it samples: head (60%), tail (20%), and evenly-spaced middle (20%),
// joined by "[...content omitted...]" markers so the LLM knows content was skipped.
func sampleLongContent(content string, maxChars int) string {
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content
	}

	const omitMarker = "\n\n[...content omitted...]\n\n"
	omitRunes := len([]rune(omitMarker))

	// Reserve space for two omit markers (head→middle, middle→tail)
	usable := maxChars - 2*omitRunes
	if usable < 100 {
		// Fallback: just truncate
		return string(runes[:maxChars])
	}

	headLen := usable * 60 / 100
	tailLen := usable * 20 / 100
	midLen := usable - headLen - tailLen

	head := string(runes[:headLen])
	tail := string(runes[len(runes)-tailLen:])

	// Sample middle portion: take a contiguous block from the center of the document
	midStart := len(runes)/2 - midLen/2
	if midStart < headLen {
		midStart = headLen
	}
	midEnd := midStart + midLen
	if midEnd > len(runes)-tailLen {
		midEnd = len(runes) - tailLen
		midStart = midEnd - midLen
		if midStart < headLen {
			midStart = headLen
		}
	}
	middle := string(runes[midStart:midEnd])

	return head + omitMarker + middle + omitMarker + tail
}

// ProcessSummaryGeneration handles async summary generation task
func (s *knowledgeService) ProcessSummaryGeneration(ctx context.Context, t *asynq.Task) (retErr error) {
	var payload types.SummaryGenerationPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "Failed to unmarshal summary generation payload: %v", err)
		return fmt.Errorf("unmarshal summary generation payload: %w", err)
	}

	logger.Infof(ctx, "Processing summary generation for knowledge: %s", payload.KnowledgeID)

	// Set tenant and language context
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.Language != "" {
		ctx = context.WithValue(ctx, types.LanguageContextKey, payload.Language)
	}
	if payload.TenantID == 0 || strings.TrimSpace(payload.KnowledgeID) == "" ||
		strings.TrimSpace(payload.KnowledgeBaseID) == "" {
		return errors.New("summary generation: complete tenant, KB and knowledge identity is required")
	}
	if strings.TrimSpace(payload.ProcessingGeneration) == "" {
		return processownership.RepairLegacyTask(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, "summary generation",
		)
	}
	// A newer attempt (re-upload / edit / reparse) has superseded this one:
	// skip before opening the span or registering the FinalizeSubtask defer
	// so we neither read stale chunks nor decrement the new attempt's counter.
	if attemptSuperseded(ctx, s.tracker(), payload.KnowledgeID, payload.Attempt) {
		logger.Infof(ctx, "summary: attempt %d superseded for %s, skipping stale enrichment",
			payload.Attempt, payload.KnowledgeID)
		return nil
	}
	currentGeneration, err := validateEnrichmentGeneration(
		ctx, s.repo, payload.TenantID, payload.KnowledgeID,
		payload.KnowledgeBaseID, payload.ProcessingGeneration,
	)
	if err != nil {
		return fmt.Errorf("validate summary processing generation: %w", err)
	}
	if !currentGeneration {
		logger.Infof(ctx, "summary: stale or incomplete processing generation for %s, skipping", payload.KnowledgeID)
		return nil
	}

	// Open a subspan under the parent attempt's postprocess stage so the
	// trace surface shows the real summary-generation duration (LLM call
	// + chunk write + index) instead of just the upstream's enqueue time.
	// Closes via the deferred handler below — every return path lands in
	// the defer, including the early returns ahead.
	span := s.beginPostprocessSubspan(ctx, payload.KnowledgeID, payload.Attempt, "postprocess.summary",
		types.JSONMap{
			"language": payload.Language,
		})
	var summaryErr error
	summaryOut := types.JSONMap{}
	defer func() {
		// Decrement the parent's enrichment counter on terminal exit.
		// "Terminal" is keyed on the value RETURNED to asynq, not on
		// summaryErr: several branches record a failure on the span
		// (summaryErr != nil) yet deliberately `return nil` so asynq does
		// NOT retry (e.g. insufficient text content, KB/knowledge fetch
		// failures). Those are terminal and must drain — keying on
		// summaryErr would skip them and leave the row stuck in
		// "finalizing". When we DO return an error asynq will retry, so
		// we only drain on the final attempt.
		if finalizeErr := finalizeSubtaskDetached(ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration, "summary",
			retErr, false, isFinalAsynqAttempt(ctx)); finalizeErr != nil {
			retErr = errors.Join(retErr, finalizeErr)
			summaryErr = errors.Join(summaryErr, finalizeErr)
		}
		if span == nil {
			return
		}
		if summaryErr != nil {
			s.failPostprocessSubspan(ctx, span, "SUMMARY_FAILED", summaryErr.Error(), summaryErr)
		} else {
			s.endPostprocessSubspan(ctx, span, summaryOut)
		}
	}()

	// Get knowledge base
	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge base: %v", err)
		summaryErr = err
		return fmt.Errorf("get summary knowledge base %s: %w", payload.KnowledgeBaseID, err)
	}
	// Capture the resolved model id on the span output the moment we
	// know it — debugging "summary stage took 60s" benefits hugely from
	// seeing WHICH chat model was actually used (kb config drift, fall-
	// throughs to a slow upstream, etc.).
	summaryOut["model_id"] = kb.SummaryModelID

	if kb.SummaryModelID == "" {
		logger.Warn(ctx, "Knowledge base summary model ID is empty, skipping summary generation")
		summaryOut["skipped"] = "no_summary_model"
		if err := s.updateCurrentEnrichmentColumns(
			ctx, payload.TenantID, payload.KnowledgeID, payload.KnowledgeBaseID,
			payload.ProcessingGeneration,
			map[string]interface{}{
				"summary_status": types.SummaryStatusNone,
				"updated_at":     time.Now(),
			},
			"clear summary status without configured model",
		); err != nil {
			if errors.Is(err, errKnowledgeStateFenceConflict) {
				return nil
			}
			summaryErr = err
			return fmt.Errorf("clear summary status without configured model: %w", err)
		}
		return nil
	}

	// Get knowledge
	knowledge, err := s.repo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge: %v", err)
		summaryErr = err
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("reload summary knowledge %s: %w", payload.KnowledgeID, err)
	}
	// Short-circuit when the user cancelled parsing or the row is being deleted.
	if knowledge != nil {
		switch knowledge.ParseStatus {
		case types.ParseStatusCancelling, types.ParseStatusCancelled, types.ParseStatusDeleting:
			logger.Infof(ctx, "Summary generation: knowledge aborted (%s), skipping: %s",
				knowledge.ParseStatus, payload.KnowledgeID)
			summaryOut["skipped"] = "knowledge_" + knowledge.ParseStatus
			return nil
		}
	}

	// Summary metadata belongs to this exact finalizing generation. A generic
	// full-row update here can otherwise overwrite a cancel/reparse transition
	// that wins while the LLM call is running.
	if err := s.updateCurrentEnrichmentColumns(
		ctx, payload.TenantID, payload.KnowledgeID, payload.KnowledgeBaseID,
		payload.ProcessingGeneration,
		map[string]interface{}{
			"summary_status": types.SummaryStatusProcessing,
			"updated_at":     time.Now(),
		},
		"mark summary processing",
	); err != nil {
		if errors.Is(err, errKnowledgeStateFenceConflict) {
			summaryOut["skipped"] = "generation_changed"
			return nil
		}
		summaryErr = err
		return fmt.Errorf("mark summary processing: %w", err)
	}

	// Helper function to mark summary as failed
	markSummaryFailed := func() {
		if err := s.updateCurrentEnrichmentColumns(
			ctx, payload.TenantID, payload.KnowledgeID, payload.KnowledgeBaseID,
			payload.ProcessingGeneration,
			map[string]interface{}{
				"summary_status": types.SummaryStatusFailed,
				"updated_at":     time.Now(),
			},
			"mark summary failed",
		); err != nil && !errors.Is(err, errKnowledgeStateFenceConflict) {
			logger.Warnf(ctx, "Failed to update summary status to failed: %v", err)
		}
	}

	textChunks, logicalTextCount, err := s.loadLogicalSummaryChunks(
		ctx, knowledge, payload.ProcessingGeneration,
	)
	if err != nil {
		logger.Errorf(ctx, "Failed to get summary chunks: %v", err)
		summaryErr = err
		return fmt.Errorf("list summary chunks: %w", err)
	}
	summaryOut["text_chunks"] = logicalTextCount
	summaryOut["sampled_chunks"] = len(textChunks)

	if len(textChunks) == 0 {
		logger.Infof(ctx, "No text chunks found for knowledge: %s", payload.KnowledgeID)
		// Mark as completed since there's nothing to summarize
		if err := s.updateCurrentEnrichmentColumns(
			ctx, payload.TenantID, payload.KnowledgeID, payload.KnowledgeBaseID,
			payload.ProcessingGeneration,
			map[string]interface{}{
				"summary_status": types.SummaryStatusCompleted,
				"updated_at":     time.Now(),
			},
			"mark empty summary completed",
		); err != nil {
			if errors.Is(err, errKnowledgeStateFenceConflict) {
				summaryOut["skipped"] = "generation_changed"
				return nil
			}
			summaryErr = err
			return fmt.Errorf("mark empty summary completed: %w", err)
		}
		summaryOut["skipped"] = "no_text_chunks"
		return nil
	}

	// Sort chunks by ChunkIndex for proper ordering
	sort.Slice(textChunks, func(i, j int) bool {
		return textChunks[i].ChunkIndex < textChunks[j].ChunkIndex
	})

	// Initialize chat model for summary
	chatModel, err := s.modelService.GetChatModel(ctx, kb.SummaryModelID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get chat model: %v", err)
		markSummaryFailed()
		summaryErr = err
		return fmt.Errorf("failed to get chat model: %w", err)
	}

	// Generate summary
	summary, err := s.getSummary(ctx, chatModel, knowledge, textChunks)
	if err != nil {
		logger.Errorf(ctx, "Failed to generate summary for knowledge %s: %v", payload.KnowledgeID, err)
		// Surface the underlying LLM/IO error on the span so the trace UI
		// can explain "why did this stage take 60s and then fall back?"
		// without forcing the operator to grep worker logs. We also capture
		// the error type to disambiguate timeouts from upstream HTTP errors
		// (deadline exceeded vs unexpected EOF vs 5xx, etc.).
		summaryOut["error"] = previewText(err.Error(), 500)
		summaryOut["error_type"] = fmt.Sprintf("%T", err)
		// For the insufficient-content case (scanned PDF without OCR, etc.)
		// we deliberately do NOT fall back to the first chunk's raw content,
		// since that chunk is typically just a bare markdown image reference
		// and surfacing it in the description is misleading.
		if errors.Is(err, errInsufficientSummaryContent) {
			updateErr := s.updateCurrentEnrichmentColumns(
				ctx, payload.TenantID, payload.KnowledgeID, payload.KnowledgeBaseID,
				payload.ProcessingGeneration,
				map[string]interface{}{
					"description":    "",
					"summary_status": types.SummaryStatusFailed,
					"updated_at":     time.Now(),
				},
				"mark insufficient summary content",
			)
			if updateErr != nil {
				if errors.Is(updateErr, errKnowledgeStateFenceConflict) {
					summaryOut["skipped"] = "generation_changed"
					return nil
				}
				logger.Errorf(ctx, "Failed to mark summary as failed: %v", updateErr)
				summaryErr = updateErr
				return fmt.Errorf("failed to update knowledge: %w", updateErr)
			}
			summaryOut["fallback"] = "insufficient_content"
			summaryErr = err
			return nil
		}
		// For other errors (LLM API issues etc.), fall back to the first chunk.
		if len(textChunks) > 0 {
			summary = textChunks[0].Content
			if len(summary) > 500 {
				runes := []rune(summary)
				if len(runes) > 500 {
					summary = string(runes[:500])
				}
			}
			summaryOut["fallback"] = "first_chunk"
		}
	}

	summaryOut["summary_chars"] = len([]rune(summary))
	// Preview the generated summary on the span output so the trace
	// viewer can show "this is what the LLM produced" at a glance,
	// without hopping to the knowledge-detail page. Capped to keep
	// span rows compact.
	summaryOut["summary_preview"] = previewText(summary, 240)
	// Create summary chunk and index it — only when RAG indexing is enabled.
	// Wiki-only KBs don't need summary chunks in the vector index.
	if strings.TrimSpace(summary) != "" && kb.NeedsEmbeddingModel() {
		// Get max chunk index
		maxChunkIndex := 0
		for _, chunk := range textChunks {
			if chunk.ChunkIndex > maxChunkIndex {
				maxChunkIndex = chunk.ChunkIndex
			}
		}

		// Embed only the LLM-generated summary in the indexed chunk.
		// We deliberately omit knowledge.FileName here: filenames are an
		// unreliable signal (e.g. "MX5280.pdf" for a scanned legal letter)
		// and surfacing them in retrieved RAG context can re-introduce the
		// hallucination vector this branch is meant to close.
		summaryChunk := &types.Chunk{
			ID:                   summaryGenerationChunkID(payload.KnowledgeID, payload.ProcessingGeneration),
			TenantID:             knowledge.TenantID,
			KnowledgeID:          knowledge.ID,
			KnowledgeBaseID:      knowledge.KnowledgeBaseID,
			Content:              fmt.Sprintf("# Summary\n%s", summary),
			ChunkIndex:           maxChunkIndex + 1,
			IsEnabled:            true,
			CreatedAt:            time.Now(),
			UpdatedAt:            time.Now(),
			StartAt:              0,
			EndAt:                0,
			ChunkType:            types.ChunkTypeSummary,
			ParentChunkID:        textChunks[0].ID,
			ProcessingGeneration: payload.ProcessingGeneration,
		}

		// A retry must reuse the same summary chunk. If the first delivery
		// committed the row but failed during vector indexing, overwrite that
		// exact row instead of creating a duplicate summary artifact.
		existingSummaryChunks, err := s.chunkRepo.ListChunksByID(
			ctx, payload.TenantID, []string{summaryChunk.ID},
		)
		if err != nil {
			summaryErr = err
			return fmt.Errorf("reload summary chunk: %w", err)
		}
		currentGeneration, err := validateEnrichmentGeneration(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
		)
		if err != nil {
			summaryErr = err
			return fmt.Errorf("fence summary chunk write: %w", err)
		}
		if !currentGeneration {
			summaryOut["skipped"] = "generation_changed"
			return nil
		}
		if len(existingSummaryChunks) > 0 {
			existing := existingSummaryChunks[0]
			existing.Content = summaryChunk.Content
			existing.ChunkIndex = summaryChunk.ChunkIndex
			existing.IsEnabled = summaryChunk.IsEnabled
			existing.UpdatedAt = summaryChunk.UpdatedAt
			existing.StartAt = summaryChunk.StartAt
			existing.EndAt = summaryChunk.EndAt
			existing.ChunkType = summaryChunk.ChunkType
			existing.ParentChunkID = summaryChunk.ParentChunkID
			existing.ProcessingGeneration = summaryChunk.ProcessingGeneration
			summaryChunk = existing
			err = s.chunkService.UpdateChunk(ctx, summaryChunk)
		} else {
			err = s.chunkService.CreateChunks(ctx, []*types.Chunk{summaryChunk})
		}
		if err != nil {
			logger.Errorf(ctx, "Failed to create summary chunk: %v", err)
			summaryErr = err
			return fmt.Errorf("persist summary chunk: %w", err)
		}

		// Index summary chunk
		tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
		if err != nil {
			logger.Errorf(ctx, "Failed to get tenant info: %v", err)
			summaryErr = err
			return fmt.Errorf("failed to get tenant info: %w", err)
		}
		ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

		retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
			ctx, s.retrieveEngine, s.ownership, tenantInfo.ID, kb.VectorStoreID)
		if err != nil {
			logger.Errorf(ctx, "Failed to init retrieve engine: %v", err)
			summaryErr = err
			return fmt.Errorf("failed to init retrieve engine: %w", err)
		}

		embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
		if err != nil {
			logger.Errorf(ctx, "Failed to get embedding model: %v", err)
			summaryErr = err
			return fmt.Errorf("failed to get embedding model: %w", err)
		}

		indexInfo := []*types.IndexInfo{{
			Content:         summaryChunk.Content,
			SourceID:        summaryChunk.ID,
			SourceType:      types.ChunkSourceType,
			ChunkID:         summaryChunk.ID,
			KnowledgeID:     knowledge.ID,
			KnowledgeBaseID: knowledge.KnowledgeBaseID,
			IsEnabled:       true,
		}}

		currentGeneration, err = validateEnrichmentGeneration(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
		)
		if err != nil {
			summaryErr = err
			return fmt.Errorf("fence summary vector write: %w", err)
		}
		if !currentGeneration {
			summaryOut["skipped"] = "generation_changed"
			return nil
		}
		if err := retrieveEngine.BatchIndex(ctx, embeddingModel, indexInfo); err != nil {
			logger.Errorf(ctx, "Failed to index summary chunk: %v", err)
			summaryErr = err
			return fmt.Errorf("failed to index summary chunk: %w", err)
		}

		logger.Infof(ctx, "Successfully created and indexed summary chunk for knowledge: %s", payload.KnowledgeID)
		summaryOut["summary_chunk_indexed"] = true
	}

	// Publish the visible summary only after its optional chunk and vector
	// artifacts have committed. This avoids reporting a completed summary
	// whose retrievable artifact failed halfway through.
	if err := s.updateCurrentEnrichmentColumns(
		ctx, payload.TenantID, payload.KnowledgeID, payload.KnowledgeBaseID,
		payload.ProcessingGeneration,
		map[string]interface{}{
			"description":    summary,
			"summary_status": types.SummaryStatusCompleted,
			"updated_at":     time.Now(),
		},
		"publish completed summary",
	); err != nil {
		if errors.Is(err, errKnowledgeStateFenceConflict) {
			summaryOut["skipped"] = "generation_changed"
			return nil
		}
		summaryErr = err
		return fmt.Errorf("publish completed summary: %w", err)
	}

	logger.Infof(ctx, "Successfully generated summary for knowledge: %s", payload.KnowledgeID)
	summaryOut["status"] = "completed"
	return nil
}

// ProcessQuestionGeneration handles async question generation task. It
// dispatches between the batched fan-out path (current: one task per window of
// text chunks, payload.ChunkIDs set) and the legacy whole-knowledge path (kept
// for tasks enqueued before fan-out shipped, no chunk ids). A lone ChunkID
// (from an interim per-chunk build) is treated as a one-element batch.
func (s *knowledgeService) ProcessQuestionGeneration(ctx context.Context, t *asynq.Task) (retErr error) {
	var payload types.QuestionGenerationPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "Failed to unmarshal question generation payload: %v", err)
		return fmt.Errorf("unmarshal question generation payload: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.TenantID == 0 || strings.TrimSpace(payload.KnowledgeID) == "" ||
		strings.TrimSpace(payload.KnowledgeBaseID) == "" {
		return errors.New("question generation: complete tenant, KB and knowledge identity is required")
	}
	if strings.TrimSpace(payload.ProcessingGeneration) == "" {
		return processownership.RepairLegacyTask(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, "question generation",
		)
	}
	currentGeneration, err := validateEnrichmentGeneration(
		ctx, s.repo, payload.TenantID, payload.KnowledgeID,
		payload.KnowledgeBaseID, payload.ProcessingGeneration,
	)
	if err != nil {
		return fmt.Errorf("validate question processing generation: %w", err)
	}
	if !currentGeneration {
		logger.Infof(ctx, "question: stale or incomplete processing generation for %s, skipping", payload.KnowledgeID)
		return nil
	}
	if len(payload.ChunkIDs) > 0 || payload.ChunkID != "" {
		return s.processQuestionGenerationForChunks(ctx, t, payload)
	}
	return s.processQuestionGenerationForKnowledge(ctx, t, payload)
}

// processQuestionGenerationForKnowledge is the legacy whole-knowledge handler:
// it iterates every text chunk of the knowledge in one task. Retained for
// in-flight tasks queued before per-chunk fan-out; new enqueues always set
// payload.ChunkID and take the per-chunk path instead.
func (s *knowledgeService) processQuestionGenerationForKnowledge(ctx context.Context, t *asynq.Task, payload types.QuestionGenerationPayload) (retErr error) {
	taskStartedAt := time.Now()
	retryCount, maxRetry, _ := taskRetryMetadata(ctx)

	exitStatus := "success"
	totalChunks := 0
	totalTextChunks := 0
	emptyContentChunks := 0
	llmCallAttempts := 0
	llmCallSuccess := 0
	llmCallFailed := 0
	llmCallEmpty := 0
	generatedQuestionsTotal := 0
	chunkMetadataSetFailed := 0
	chunkUpdateFailed := 0
	indexEntriesPrepared := 0
	indexBatchAttempted := false
	indexBatchSucceeded := false
	// Sample question + model id surfaced on the span output so the
	// trace viewer can answer "what did the LLM actually produce?" and
	// "which model did it run on?" without joining back to the chunk
	// store. Captured the first time we see a non-empty question batch.
	var sampleQuestion string
	var resolvedModelID string
	// Postprocess subspan for the trace viewer. Opened lazily after we
	// unmarshal the payload (so we have payload.Attempt) and closed in
	// the defer below alongside the stats log so the span output mirrors
	// what we already log to stdout.
	var qSpan *Span
	var qErr error
	// Set when a newer attempt supersedes this run; suppresses the
	// FinalizeSubtask decrement so a stale task can't drain the new
	// attempt's counter.
	superseded := false
	// Decrement enrichment counter on terminal exit. Keyed on the value
	// RETURNED to asynq (retErr), not qErr: some branches record a span
	// failure (qErr != nil) yet `return nil` so asynq won't retry (KB /
	// knowledge fetch failures); those are terminal and must drain.
	// Keying on qErr would skip them and strand the row in "finalizing".
	// When we return an error asynq retries, so we only drain on the
	// final attempt. Runs AFTER the stats-log defer below — defers
	// unwind LIFO, so this one declared first executes last.
	defer func() {
		if finalizeErr := finalizeSubtaskDetached(ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration, "question_legacy",
			retErr, superseded, isFinalAsynqAttempt(ctx)); finalizeErr != nil {
			retErr = errors.Join(retErr, finalizeErr)
			qErr = errors.Join(qErr, finalizeErr)
		}
	}()
	defer func() {
		logger.Infof(
			ctx,
			"Question generation stats: knowledge=%s kb=%s retry=%d/%d status=%s elapsed=%s chunks(total=%d,text=%d,empty_text=%d) llm(attempt=%d,success=%d,empty=%d,failed=%d) generated_questions=%d chunk_update_failed=%d metadata_set_failed=%d index(prepared=%d,attempted=%v,succeeded=%v)",
			payload.KnowledgeID,
			payload.KnowledgeBaseID,
			retryCount,
			maxRetry,
			exitStatus,
			time.Since(taskStartedAt).Round(time.Millisecond),
			totalChunks,
			totalTextChunks,
			emptyContentChunks,
			llmCallAttempts,
			llmCallSuccess,
			llmCallEmpty,
			llmCallFailed,
			generatedQuestionsTotal,
			chunkUpdateFailed,
			chunkMetadataSetFailed,
			indexEntriesPrepared,
			indexBatchAttempted,
			indexBatchSucceeded,
		)
		if qSpan != nil {
			out := types.JSONMap{
				"status":                 exitStatus,
				"total_chunks":           totalChunks,
				"text_chunks":            totalTextChunks,
				"empty_content_chunks":   emptyContentChunks,
				"llm_attempts":           llmCallAttempts,
				"llm_success":            llmCallSuccess,
				"llm_empty":              llmCallEmpty,
				"llm_failed":             llmCallFailed,
				"questions_generated":    generatedQuestionsTotal,
				"chunk_update_failed":    chunkUpdateFailed,
				"metadata_set_failed":    chunkMetadataSetFailed,
				"index_entries_prepared": indexEntriesPrepared,
				"index_batch_attempted":  indexBatchAttempted,
				"index_batch_succeeded":  indexBatchSucceeded,
				"retry":                  retryCount,
				"max_retry":              maxRetry,
			}
			// Surface the resolved model id and a sample question on the
			// span output. These help debugging "why is question generation
			// slow" — both questions ("which model was hit?") and ("what
			// did it produce?") are hard to answer from logs alone.
			if resolvedModelID != "" {
				out["model_id"] = resolvedModelID
			}
			if sampleQuestion != "" {
				out["sample_question"] = sampleQuestion
			}
			// Treat any non-success exitStatus as a failed run; the
			// existing stats-string already enumerates them. qErr stays
			// optional for callers that want to surface a Go error.
			if exitStatus != "success" || qErr != nil {
				msg := exitStatus
				var detailErr error = qErr
				if qErr != nil {
					msg = qErr.Error()
				}
				s.failPostprocessSubspan(ctx, qSpan, "QUESTION_FAILED", msg, detailErr)
			} else {
				s.endPostprocessSubspan(ctx, qSpan, out)
			}
		}
	}()

	logger.Infof(ctx, "Processing question generation for knowledge: %s", payload.KnowledgeID)

	// A newer attempt has superseded this one: skip before opening the span
	// so we don't read stale chunks. superseded suppresses the counter
	// decrement in the defer above; qSpan stays nil so the stats defer no-ops.
	if attemptSuperseded(ctx, s.tracker(), payload.KnowledgeID, payload.Attempt) {
		superseded = true
		exitStatus = "superseded"
		logger.Infof(ctx, "question: attempt %d superseded for %s, skipping stale enrichment",
			payload.Attempt, payload.KnowledgeID)
		return nil
	}

	// Open the postprocess.question subspan now that we have payload.Attempt.
	// Closes via the defer above.
	qSpan = s.beginPostprocessSubspan(ctx, payload.KnowledgeID, payload.Attempt, "postprocess.question",
		types.JSONMap{
			"question_count": payload.QuestionCount,
			"language":       payload.Language,
		})

	// Set tenant context
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.Language != "" {
		ctx = context.WithValue(ctx, types.LanguageContextKey, payload.Language)
	}

	if strings.TrimSpace(s.config.Conversation.GenerateQuestionsPrompt) == "" {
		exitStatus = "prompt_not_configured"
		logger.Errorf(ctx, "GenerateQuestionsPrompt is empty: configure conversation.generate_questions_prompt_id")
		qErr = fmt.Errorf("generate questions prompt not configured")
		return qErr
	}

	// Get knowledge base
	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID)
	if err != nil {
		exitStatus = "kb_not_found"
		logger.Errorf(ctx, "Failed to get knowledge base: %v", err)
		qErr = err
		return fmt.Errorf("get question knowledge base %s: %w", payload.KnowledgeBaseID, err)
	}

	// Get knowledge
	knowledge, err := s.repo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		exitStatus = "knowledge_not_found"
		logger.Errorf(ctx, "Failed to get knowledge: %v", err)
		qErr = err
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("reload question knowledge %s: %w", payload.KnowledgeID, err)
	}
	// Short-circuit when the user cancelled parsing or the row is being deleted.
	if knowledge != nil {
		switch knowledge.ParseStatus {
		case types.ParseStatusCancelling, types.ParseStatusCancelled, types.ParseStatusDeleting:
			exitStatus = "knowledge_" + knowledge.ParseStatus
			logger.Infof(ctx, "Question generation: knowledge aborted (%s), skipping: %s",
				knowledge.ParseStatus, payload.KnowledgeID)
			return nil
		}
	}

	// Get text chunks for this knowledge
	chunks, err := s.chunkService.ListChunksByKnowledgeID(ctx, payload.KnowledgeID)
	if err != nil {
		exitStatus = "list_chunks_failed"
		logger.Errorf(ctx, "Failed to get chunks: %v", err)
		return fmt.Errorf("list question chunks for %s: %w", payload.KnowledgeID, err)
	}
	totalChunks = len(chunks)

	// Filter text chunks only
	textChunks := make([]*types.Chunk, 0)
	for _, chunk := range chunks {
		if chunk.ChunkType == types.ChunkTypeText {
			textChunks = append(textChunks, chunk)
		}
	}
	totalTextChunks = len(textChunks)

	if len(textChunks) == 0 {
		exitStatus = "no_text_chunks"
		logger.Infof(ctx, "No text chunks found for knowledge: %s", payload.KnowledgeID)
		return nil
	}

	// Sort chunks by StartAt for context building
	sort.Slice(textChunks, func(i, j int) bool {
		return textChunks[i].StartAt < textChunks[j].StartAt
	})

	// Initialize chat model
	chatModel, err := s.modelService.GetChatModel(ctx, kb.SummaryModelID)
	if err != nil {
		exitStatus = "get_chat_model_failed"
		logger.Errorf(ctx, "Failed to get chat model: %v", err)
		return fmt.Errorf("failed to get chat model: %w", err)
	}
	resolvedModelID = kb.SummaryModelID

	// Initialize embedding model and retrieval engine
	embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		exitStatus = "get_embedding_model_failed"
		logger.Errorf(ctx, "Failed to get embedding model: %v", err)
		return fmt.Errorf("failed to get embedding model: %w", err)
	}

	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		exitStatus = "get_tenant_failed"
		logger.Errorf(ctx, "Failed to get tenant info: %v", err)
		return fmt.Errorf("failed to get tenant info: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, tenantInfo.ID, kb.VectorStoreID)
	if err != nil {
		exitStatus = "init_retrieve_engine_failed"
		logger.Errorf(ctx, "Failed to init retrieve engine: %v", err)
		return fmt.Errorf("failed to init retrieve engine: %w", err)
	}

	questionCount := payload.QuestionCount
	if questionCount <= 0 {
		questionCount = 3
	}
	if questionCount > 10 {
		questionCount = 10
	}

	// Collect image info for all text chunks so question generation can
	// see caption / OCR text instead of bare image links.
	textChunkIDs := make([]string, len(textChunks))
	for i, c := range textChunks {
		textChunkIDs[i] = c.ID
	}
	imageInfoMap := searchutil.CollectImageInfoByChunkIDs(ctx, s.chunkRepo, payload.TenantID, textChunkIDs)

	enrichContent := func(chunk *types.Chunk) string {
		content := chunk.Content
		if info, ok := imageInfoMap[chunk.ID]; ok && info != "" {
			content = searchutil.EnrichContentWithImageInfo(content, info)
		}
		return logicalChunkLLMContent(chunk, content)
	}

	// Generate questions for each chunk with context
	var indexInfoList []*types.IndexInfo
	for i, chunk := range textChunks {
		if strings.TrimSpace(chunk.Content) == "" {
			emptyContentChunks++
			continue
		}

		// Build context from adjacent chunks
		var prevContent, nextContent string
		if i > 0 {
			prevContent = enrichContent(textChunks[i-1])
		}
		if i < len(textChunks)-1 {
			nextContent = enrichContent(textChunks[i+1])
		}

		llmCallAttempts++
		questions, err := s.generateQuestionsWithContext(ctx, chatModel, enrichContent(chunk), prevContent, nextContent, knowledge.Title, questionCount)
		if err != nil {
			llmCallFailed++
			logger.Warnf(ctx, "Failed to generate questions for chunk %s: %v", chunk.ID, err)
			continue
		}

		if len(questions) == 0 {
			llmCallEmpty++
			continue
		}
		llmCallSuccess++
		generatedQuestionsTotal += len(questions)
		if sampleQuestion == "" && len(questions) > 0 {
			sampleQuestion = previewText(questions[0], 200)
		}

		// Update chunk metadata with unique IDs for each question
		generatedQuestions := make([]types.GeneratedQuestion, len(questions))
		for j, question := range questions {
			questionID := questionGenerationID(payload.ProcessingGeneration, chunk.ID, j)
			generatedQuestions[j] = types.GeneratedQuestion{
				ID:       questionID,
				Question: question,
			}
		}
		meta := &types.DocumentChunkMetadata{
			GeneratedQuestions: generatedQuestions,
		}
		if err := chunk.SetDocumentMetadata(meta); err != nil {
			chunkMetadataSetFailed++
			logger.Warnf(ctx, "Failed to set document metadata for chunk %s: %v", chunk.ID, err)
			continue
		}

		// The LLM call above may take minutes. Re-check the durable generation
		// immediately before the unscoped chunk write; cancel/delete then wait
		// for this task to quiesce before a new generation can begin.
		currentGeneration, fenceErr := validateEnrichmentGeneration(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
		)
		if fenceErr != nil {
			qErr = fenceErr
			exitStatus = "generation_fence_failed"
			return fmt.Errorf("fence question chunk write: %w", fenceErr)
		}
		if !currentGeneration {
			superseded = true
			exitStatus = "generation_changed"
			return nil
		}

		// Update chunk in database
		if err := s.chunkService.UpdateChunk(ctx, chunk); err != nil {
			chunkUpdateFailed++
			qErr = err
			exitStatus = "update_question_chunk_failed"
			logger.Errorf(ctx, "Failed to update chunk %s: %v", chunk.ID, err)
			return fmt.Errorf("update generated questions for chunk %s: %w", chunk.ID, err)
		}

		// Create index entries for generated questions
		for _, gq := range generatedQuestions {
			indexInfoList = append(indexInfoList, &types.IndexInfo{
				Content:         gq.Question,
				SourceID:        questionVectorSourceID(gq.ID),
				SourceType:      types.ChunkSourceType,
				ChunkID:         chunk.ID,
				KnowledgeID:     knowledge.ID,
				KnowledgeBaseID: knowledge.KnowledgeBaseID,
				IsEnabled:       true,
			})
		}
		logger.Debugf(ctx, "Generated %d questions for chunk %s", len(questions), chunk.ID)
	}
	indexEntriesPrepared = len(indexInfoList)

	// Index generated questions
	if len(indexInfoList) > 0 {
		indexBatchAttempted = true
		currentGeneration, fenceErr := validateEnrichmentGeneration(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
		)
		if fenceErr != nil {
			qErr = fenceErr
			exitStatus = "generation_fence_failed"
			return fmt.Errorf("fence question vector write: %w", fenceErr)
		}
		if !currentGeneration {
			superseded = true
			exitStatus = "generation_changed"
			return nil
		}
		if err := retrieveEngine.BatchIndex(ctx, embeddingModel, indexInfoList); err != nil {
			exitStatus = "index_questions_failed"
			logger.Errorf(ctx, "Failed to index generated questions: %v", err)
			return fmt.Errorf("failed to index questions: %w", err)
		}
		indexBatchSucceeded = true
		logger.Infof(ctx, "Successfully indexed %d generated questions for knowledge: %s", len(indexInfoList), payload.KnowledgeID)
	}

	return nil
}

// processQuestionGenerationForChunks generates questions for a batch (window)
// of text chunks. This is the batched fan-out path (one asynq task per
// questionGenChunkBatchSize chunks), aligned with the graph-extract
// TypeChunkExtract pattern: independent retry, per-batch cancellation, and a
// postprocess.question.batch[i] subspan. The payload carries only chunk ids
// (never content); content is read fresh here, and all questions for the batch
// are indexed in a single embedding BatchIndex call.
func (s *knowledgeService) processQuestionGenerationForChunks(ctx context.Context, t *asynq.Task, payload types.QuestionGenerationPayload) (retErr error) {
	taskStartedAt := time.Now()
	retryCount, maxRetry, _ := taskRetryMetadata(ctx)

	// Normalize the batch: prefer ChunkIDs, fall back to a lone ChunkID
	// (interim per-chunk build) so those in-flight tasks still run.
	batchIDs := payload.ChunkIDs
	if len(batchIDs) == 0 && payload.ChunkID != "" {
		batchIDs = []string{payload.ChunkID}
	}

	exitStatus := "success"
	chunksInBatch := len(batchIDs)
	chunksProcessed := 0
	emptyChunks := 0
	llmCallFailed := 0
	generatedQuestionsTotal := 0
	indexEntriesPrepared := 0
	indexBatchSucceeded := false
	var sampleQuestion string
	var resolvedModelID string
	var qSpan *Span
	var qErr error
	// Suppresses the FinalizeSubtask drain when a newer attempt superseded
	// this run, so a stale task can't decrement the new attempt's counter.
	superseded := false

	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.Language != "" {
		ctx = context.WithValue(ctx, types.LanguageContextKey, payload.Language)
	}

	// Drain the parent's enrichment counter on terminal exit. Keyed on the
	// value RETURNED to asynq (retErr), not qErr: some branches record a
	// span failure yet `return nil` (terminal, must drain). Declared first
	// so it runs LAST (after the stats/span defer below).
	defer func() {
		if finalizeErr := finalizeSubtaskDetached(ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
			fmt.Sprintf("question_batch[%d]", payload.BatchIndex),
			retErr, superseded, isFinalAsynqAttempt(ctx)); finalizeErr != nil {
			retErr = errors.Join(retErr, finalizeErr)
			qErr = errors.Join(qErr, finalizeErr)
		}
	}()
	defer func() {
		logger.Infof(ctx,
			"Question generation (batch) stats: knowledge=%s batch=%d chunks(in_batch=%d,processed=%d,empty=%d) llm_failed=%d retry=%d/%d status=%s elapsed=%s generated_questions=%d index(entries=%d,succeeded=%v)",
			payload.KnowledgeID, payload.BatchIndex, chunksInBatch, chunksProcessed, emptyChunks, llmCallFailed,
			retryCount, maxRetry, exitStatus, time.Since(taskStartedAt).Round(time.Millisecond),
			generatedQuestionsTotal, indexEntriesPrepared, indexBatchSucceeded,
		)
		if qSpan != nil {
			out := types.JSONMap{
				"status":                 exitStatus,
				"batch_index":            payload.BatchIndex,
				"chunks_in_batch":        chunksInBatch,
				"chunks_processed":       chunksProcessed,
				"empty_chunks":           emptyChunks,
				"llm_failed":             llmCallFailed,
				"questions_generated":    generatedQuestionsTotal,
				"index_entries_prepared": indexEntriesPrepared,
				"index_batch_succeeded":  indexBatchSucceeded,
				"retry":                  retryCount,
				"max_retry":              maxRetry,
			}
			if resolvedModelID != "" {
				out["model_id"] = resolvedModelID
			}
			if sampleQuestion != "" {
				out["sample_question"] = sampleQuestion
			}
			if exitStatus != "success" || qErr != nil {
				msg := exitStatus
				if qErr != nil {
					msg = qErr.Error()
				}
				s.failPostprocessSubspan(ctx, qSpan, "QUESTION_FAILED", msg, qErr)
			} else {
				s.endPostprocessSubspan(ctx, qSpan, out)
			}
		}
	}()

	logger.Infof(ctx, "Processing question generation for knowledge=%s batch=%d chunks=%d",
		payload.KnowledgeID, payload.BatchIndex, chunksInBatch)

	if chunksInBatch == 0 {
		exitStatus = "empty_batch"
		return nil
	}

	// A newer attempt has superseded this one: skip before opening the span
	// so we don't read stale chunks and don't drain the new attempt.
	if attemptSuperseded(ctx, s.tracker(), payload.KnowledgeID, payload.Attempt) {
		superseded = true
		exitStatus = "superseded"
		logger.Infof(ctx, "question: attempt %d superseded for %s, skipping stale enrichment",
			payload.Attempt, payload.KnowledgeID)
		return nil
	}

	qSpan = s.beginQuestionBatchSubspan(ctx, payload.KnowledgeID, payload.Attempt,
		fmt.Sprintf("postprocess.question.batch[%d]", payload.BatchIndex),
		types.JSONMap{
			"batch_index":    payload.BatchIndex,
			"chunks":         chunksInBatch,
			"question_count": payload.QuestionCount,
			"language":       payload.Language,
		})

	if strings.TrimSpace(s.config.Conversation.GenerateQuestionsPrompt) == "" {
		exitStatus = "prompt_not_configured"
		logger.Errorf(ctx, "GenerateQuestionsPrompt is empty: configure conversation.generate_questions_prompt_id")
		qErr = fmt.Errorf("generate questions prompt not configured")
		return qErr
	}

	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID)
	if err != nil {
		exitStatus = "kb_not_found"
		logger.Errorf(ctx, "Failed to get knowledge base: %v", err)
		qErr = err
		return fmt.Errorf("get question batch knowledge base %s: %w", payload.KnowledgeBaseID, err)
	}

	knowledge, err := s.repo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		exitStatus = "knowledge_not_found"
		logger.Errorf(ctx, "Failed to get knowledge: %v", err)
		qErr = err
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("reload question batch knowledge %s: %w", payload.KnowledgeID, err)
	}
	// Short-circuit when the user cancelled parsing or the row is being
	// deleted — batched fan-out means we get this check for free on every
	// batch, so a cancel stops burning LLM quota on the remaining batches.
	if knowledge != nil {
		switch knowledge.ParseStatus {
		case types.ParseStatusCancelling, types.ParseStatusCancelled, types.ParseStatusDeleting:
			exitStatus = "knowledge_" + knowledge.ParseStatus
			logger.Infof(ctx, "Question generation: knowledge aborted (%s), skipping batch %d",
				knowledge.ParseStatus, payload.BatchIndex)
			return nil
		}
	}

	chatModel, err := s.modelService.GetChatModel(ctx, kb.SummaryModelID)
	if err != nil {
		exitStatus = "get_chat_model_failed"
		logger.Errorf(ctx, "Failed to get chat model: %v", err)
		return fmt.Errorf("failed to get chat model: %w", err)
	}
	resolvedModelID = kb.SummaryModelID

	embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		exitStatus = "get_embedding_model_failed"
		logger.Errorf(ctx, "Failed to get embedding model: %v", err)
		return fmt.Errorf("failed to get embedding model: %w", err)
	}

	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		exitStatus = "get_tenant_failed"
		logger.Errorf(ctx, "Failed to get tenant info: %v", err)
		return fmt.Errorf("failed to get tenant info: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, tenantInfo.ID, kb.VectorStoreID)
	if err != nil {
		exitStatus = "init_retrieve_engine_failed"
		logger.Errorf(ctx, "Failed to init retrieve engine: %v", err)
		return fmt.Errorf("failed to init retrieve engine: %w", err)
	}

	questionCount := payload.QuestionCount
	if questionCount <= 0 {
		questionCount = 3
	}
	if questionCount > 10 {
		questionCount = 10
	}

	// Fetch the batch chunks (in payload order) plus the two boundary
	// neighbors so we can rebuild the same surrounding context the legacy
	// loop used, all enriched with image OCR / caption info. A vanished
	// chunk degrades gracefully (skipped / empty context).
	getChunk := func(id string) (*types.Chunk, error) {
		if id == "" {
			return nil, nil
		}
		c, gerr := s.chunkRepo.GetChunkByID(ctx, payload.TenantID, id)
		if gerr != nil {
			if errors.Is(gerr, apprepo.ErrChunkNotFound) {
				return nil, nil
			}
			return nil, gerr
		}
		return c, nil
	}
	batchChunks := make([]*types.Chunk, len(batchIDs))
	for i, id := range batchIDs {
		batchChunks[i], err = getChunk(id)
		if err != nil {
			qErr = err
			exitStatus = "get_batch_chunk_failed"
			return fmt.Errorf("get question batch chunk %s: %w", id, err)
		}
	}
	prevChunk, err := getChunk(payload.PrevChunkID)
	if err != nil {
		qErr = err
		exitStatus = "get_previous_chunk_failed"
		return fmt.Errorf("get previous question context chunk %s: %w", payload.PrevChunkID, err)
	}
	nextChunk, err := getChunk(payload.NextChunkID)
	if err != nil {
		qErr = err
		exitStatus = "get_next_chunk_failed"
		return fmt.Errorf("get next question context chunk %s: %w", payload.NextChunkID, err)
	}

	infoIDs := make([]string, 0, len(batchIDs)+2)
	infoIDs = append(infoIDs, batchIDs...)
	if payload.PrevChunkID != "" {
		infoIDs = append(infoIDs, payload.PrevChunkID)
	}
	if payload.NextChunkID != "" {
		infoIDs = append(infoIDs, payload.NextChunkID)
	}
	imageInfoMap := searchutil.CollectImageInfoByChunkIDs(ctx, s.chunkRepo, payload.TenantID, infoIDs)
	enrich := func(c *types.Chunk) string {
		if c == nil {
			return ""
		}
		content := c.Content
		if info, ok := imageInfoMap[c.ID]; ok && info != "" {
			content = searchutil.EnrichContentWithImageInfo(content, info)
		}
		return logicalChunkLLMContent(c, content)
	}

	// neighborContent returns the context content for position i within the
	// batch: the in-batch neighbor when present, else the boundary chunk.
	prevContentAt := func(i int) string {
		if payload.SparseSample {
			return ""
		}
		if i > 0 {
			return enrich(batchChunks[i-1])
		}
		return enrich(prevChunk)
	}
	nextContentAt := func(i int) string {
		if payload.SparseSample {
			return ""
		}
		if i < len(batchChunks)-1 {
			return enrich(batchChunks[i+1])
		}
		return enrich(nextChunk)
	}

	var indexInfoList []*types.IndexInfo
	for i, chunk := range batchChunks {
		if chunk == nil || strings.TrimSpace(chunk.Content) == "" {
			emptyChunks++
			continue
		}

		questions, gerr := s.generateQuestionsWithContext(
			ctx, chatModel, enrich(chunk), prevContentAt(i), nextContentAt(i), knowledge.Title, questionCount)
		if gerr != nil {
			llmCallFailed++
			logger.Warnf(ctx, "Failed to generate questions for chunk %s: %v", chunk.ID, gerr)
			continue
		}
		if len(questions) == 0 {
			continue
		}
		chunksProcessed++
		generatedQuestionsTotal += len(questions)
		if sampleQuestion == "" {
			sampleQuestion = previewText(questions[0], 200)
		}

		generatedQuestions := make([]types.GeneratedQuestion, len(questions))
		for j, question := range questions {
			generatedQuestions[j] = types.GeneratedQuestion{
				ID:       questionGenerationID(payload.ProcessingGeneration, chunk.ID, j),
				Question: question,
			}
		}
		meta := &types.DocumentChunkMetadata{GeneratedQuestions: generatedQuestions}
		if err := chunk.SetDocumentMetadata(meta); err != nil {
			logger.Warnf(ctx, "Failed to set document metadata for chunk %s: %v", chunk.ID, err)
			continue
		}
		currentGeneration, fenceErr := validateEnrichmentGeneration(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
		)
		if fenceErr != nil {
			qErr = fenceErr
			exitStatus = "generation_fence_failed"
			return fmt.Errorf("fence question batch chunk write: %w", fenceErr)
		}
		if !currentGeneration {
			superseded = true
			exitStatus = "generation_changed"
			return nil
		}
		if err := s.chunkService.UpdateChunk(ctx, chunk); err != nil {
			qErr = err
			exitStatus = "update_question_chunk_failed"
			logger.Errorf(ctx, "Failed to update chunk %s: %v", chunk.ID, err)
			return fmt.Errorf("update generated questions for chunk %s: %w", chunk.ID, err)
		}
		for _, gq := range generatedQuestions {
			indexInfoList = append(indexInfoList, &types.IndexInfo{
				Content:         gq.Question,
				SourceID:        questionVectorSourceID(gq.ID),
				SourceType:      types.ChunkSourceType,
				ChunkID:         chunk.ID,
				KnowledgeID:     knowledge.ID,
				KnowledgeBaseID: knowledge.KnowledgeBaseID,
				IsEnabled:       true,
			})
		}
	}

	indexEntriesPrepared = len(indexInfoList)
	if len(indexInfoList) > 0 {
		currentGeneration, fenceErr := validateEnrichmentGeneration(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, payload.ProcessingGeneration,
		)
		if fenceErr != nil {
			qErr = fenceErr
			exitStatus = "generation_fence_failed"
			return fmt.Errorf("fence question batch vector write: %w", fenceErr)
		}
		if !currentGeneration {
			superseded = true
			exitStatus = "generation_changed"
			return nil
		}
		if err := retrieveEngine.BatchIndex(ctx, embeddingModel, indexInfoList); err != nil {
			exitStatus = "index_questions_failed"
			qErr = err
			logger.Errorf(ctx, "Failed to index generated questions for batch %d: %v", payload.BatchIndex, err)
			return fmt.Errorf("failed to index questions: %w", err)
		}
		indexBatchSucceeded = true
		logger.Infof(ctx, "Indexed %d generated questions for knowledge=%s batch=%d",
			len(indexInfoList), payload.KnowledgeID, payload.BatchIndex)
	}
	return nil
}

// generateQuestionsWithContext generates questions for a chunk with surrounding context
func (s *knowledgeService) generateQuestionsWithContext(ctx context.Context,
	chatModel chat.Chat, content, prevContent, nextContent, docName string, questionCount int,
) ([]string, error) {
	if content == "" || questionCount <= 0 {
		return nil, nil
	}

	prompt := strings.TrimSpace(s.config.Conversation.GenerateQuestionsPrompt)
	if prompt == "" {
		return nil, fmt.Errorf("generate questions prompt not configured")
	}

	// Build context section
	var contextSection string
	if prevContent != "" || nextContent != "" {
		contextSection = "<surrounding_context>\n"
		if prevContent != "" {
			contextSection += fmt.Sprintf("<preceding_content>\n%s\n\n</preceding_content>\n\n", prevContent)
		}
		if nextContent != "" {
			contextSection += fmt.Sprintf("<following_content>\n%s\n\n</following_content>\n\n", nextContent)
		}
		contextSection += "</surrounding_context>\n\n"
	}

	langName := types.LanguageNameFromContext(ctx)
	prompt = types.RenderPromptPlaceholders(prompt, types.PlaceholderValues{
		"question_count": fmt.Sprintf("%d", questionCount),
		"content":        content,
		"context":        contextSection,
		"doc_name":       docName,
		"language":       langName,
	})

	thinking := false
	response, err := chatModel.Chat(ctx, []chat.Message{
		{
			Role:    "user",
			Content: prompt,
		},
	}, &chat.ChatOptions{
		Temperature: 0.7,
		MaxTokens:   512,
		Thinking:    &thinking,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate questions: %w", err)
	}

	// Parse response
	lines := strings.Split(response.Content, "\n")
	questions := make([]string, 0, questionCount)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "0123456789.-*) ")
		line = strings.TrimSpace(line)
		if line != "" && len(line) > 5 {
			questions = append(questions, line)
			if len(questions) >= questionCount {
				break
			}
		}
	}

	return questions, nil
}

// processingFailureValuesPreservingStorage intentionally does not write
// storage_size. If external cleanup failed, the old charge still belongs to
// this knowledge row and must remain recoverable by a later delete/reparse. If
// cleanup succeeded, ResetKnowledgeStorage already changed row + tenant in one
// transaction, so there is nothing for failure handling to do.
func processingFailureValuesPreservingStorage(message string, now time.Time) map[string]interface{} {
	return map[string]interface{}{
		"parse_status":           types.ParseStatusFailed,
		"error_message":          message,
		"pending_subtasks_count": 0,
		"updated_at":             now,
	}
}

const (
	batchReparsePreparing = "batch-reparse-preparing"
	batchReparseReady     = "batch-reparse-ready"
)

func batchReparseMarker(phase, generation string, attempt int) string {
	return fmt.Sprintf("%s:%s:%d", phase, generation, attempt)
}

func batchReparseMarkerAttempt(message, phase, generation string) (int, bool) {
	prefix := phase + ":" + generation + ":"
	if !strings.HasPrefix(message, prefix) {
		return 0, false
	}
	attempt, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(message, prefix)))
	if err != nil || attempt < 0 {
		return 0, false
	}
	return attempt, true
}

type batchReparseSnapshotCASRepository interface {
	CompareAndSwapBatchReparseSnapshot(
		ctx context.Context,
		tenantID uint64,
		id string,
		expectedKnowledgeBaseID string,
		expectedParseStatus string,
		expectedGeneration string,
		expectedOwner string,
		expectedUpdatedAt time.Time,
		values map[string]interface{},
	) (bool, error)
}

type batchReparseResolution int

const (
	batchReparseClaimExpected batchReparseResolution = iota
	batchReparseResumePreparation
	batchReparseResumeSubmission
	batchReparseReclaimFailed
	batchReparseStale
)

func resolveBatchReparseChild(
	knowledge *types.Knowledge,
	requestedGeneration string,
	requestedOwner string,
	expected *types.KnowledgeReparseExpectedSnapshot,
) (batchReparseResolution, int, error) {
	if knowledge == nil || strings.TrimSpace(requestedGeneration) == "" || strings.TrimSpace(requestedOwner) == "" {
		return batchReparseStale, 0, errors.New("batch reparse child requires a complete processing identity")
	}
	if requestedOwner != processownership.DocumentOwner(knowledge.ID, requestedGeneration) {
		return batchReparseStale, 0, errors.New("batch reparse child processing owner does not match its generation")
	}
	if knowledge.ProcessingGeneration == requestedGeneration {
		if knowledge.ProcessingOwner != requestedOwner {
			return batchReparseStale, 0, nil
		}
		switch knowledge.ParseStatus {
		case types.ParseStatusPending:
			attempt, _ := batchReparseMarkerAttempt(
				knowledge.ErrorMessage, batchReparseReady, requestedGeneration,
			)
			return batchReparseResumeSubmission, attempt, nil
		case types.ParseStatusProcessing:
			if attempt, preparing := batchReparseMarkerAttempt(
				knowledge.ErrorMessage, batchReparsePreparing, requestedGeneration,
			); preparing {
				return batchReparseResumePreparation, attempt, nil
			}
			return batchReparseResumeSubmission, 0, nil
		case types.ParseStatusFailed:
			return batchReparseReclaimFailed, 0, nil
		default:
			return batchReparseStale, 0, nil
		}
	}
	if expected == nil {
		return batchReparseStale, 0, errors.New("batch reparse child is missing its durable expected snapshot")
	}
	if err := processownership.ValidateBatchReparseSnapshot(*expected); err != nil {
		return batchReparseStale, 0, err
	}
	if !processownership.BatchReparseSnapshotMatches(knowledge, *expected) {
		return batchReparseStale, 0, nil
	}
	return batchReparseClaimExpected, 0, nil
}

func claimBatchReparseExpectedSnapshot(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	snapshot types.KnowledgeReparseExpectedSnapshot,
	values map[string]interface{},
) (bool, error) {
	claimRepo, ok := repo.(batchReparseSnapshotCASRepository)
	if !ok || claimRepo == nil {
		return false, errors.New("claim batch reparse snapshot: exact snapshot CAS is unavailable")
	}
	if err := processownership.ValidateBatchReparseSnapshot(snapshot); err != nil {
		return false, err
	}
	return claimRepo.CompareAndSwapBatchReparseSnapshot(
		ctx,
		snapshot.TenantID,
		snapshot.KnowledgeID,
		snapshot.KnowledgeBaseID,
		snapshot.ParseStatus,
		snapshot.ProcessingGeneration,
		snapshot.ProcessingOwner,
		snapshot.UpdatedAt,
		values,
	)
}

// ReparseKnowledge deletes existing document content and re-parses the knowledge asynchronously.
// This method reuses the logic from UpdateManualKnowledge for resource cleanup and async parsing.
func (s *knowledgeService) ReparseKnowledge(
	ctx context.Context,
	knowledgeID string,
	processOverrides *types.KnowledgeProcessOverrides,
) (*types.Knowledge, error) {
	return s.reparseKnowledgeWithIdentity(ctx, knowledgeID, processOverrides, "", "", nil, nil)
}

func (s *knowledgeService) reparseKnowledgeWithIdentity(
	ctx context.Context,
	knowledgeID string,
	processOverrides *types.KnowledgeProcessOverrides,
	requestedGeneration string,
	requestedOwner string,
	expectedSnapshot *types.KnowledgeReparseExpectedSnapshot,
	stableTracing *types.TracingContext,
) (*types.Knowledge, error) {
	logger.Info(ctx, "Start re-parsing knowledge")

	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	existing, err := s.repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
	if err != nil {
		logger.Errorf(ctx, "Failed to load knowledge: %v", err)
		return nil, err
	}
	stableChild := strings.TrimSpace(requestedGeneration) != "" || strings.TrimSpace(requestedOwner) != ""

	resumePreparation := false
	resumeSubmission := false
	reclaimFailed := false
	claimExpected := false
	resumeAttempt := 0
	if stableChild {
		resolution, attempt, resolveErr := resolveBatchReparseChild(
			existing, requestedGeneration, requestedOwner, expectedSnapshot,
		)
		if resolveErr != nil {
			return nil, resolveErr
		}
		resumeAttempt = attempt
		switch resolution {
		case batchReparseClaimExpected:
			claimExpected = true
		case batchReparseResumePreparation:
			resumePreparation = true
		case batchReparseResumeSubmission:
			resumeSubmission = true
		case batchReparseReclaimFailed:
			resumeAttempt = s.tracker().LatestAttempt(ctx, existing.ID)
			reclaimFailed = true
		case batchReparseStale:
			logger.Infof(ctx, "Batch reparse child %s generation %s no longer matches its durable snapshot; skipping",
				existing.ID, requestedGeneration)
			return existing, nil
		}
	}

	if !resumePreparation && !resumeSubmission && !reclaimFailed {
		if existing.ParseStatus == types.ParseStatusDeleting {
			return nil, werrors.NewBadRequestError("知识正在删除中，无法重新解析")
		}
		switch existing.ParseStatus {
		case types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCancelling:
			// Reparse cleanup deletes artifacts by knowledge ID. A second active
			// generation cannot be made safe by status alone because the status can
			// cycle back to processing (ABA) while the first worker is still alive.
			return nil, werrors.NewBadRequestError("知识正在处理中，请等待完成或先取消解析")
		}
	}

	// Get knowledge base configuration
	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, existing.KnowledgeBaseID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge base for reparse: %v", err)
		return nil, err
	}
	if kb == nil {
		return nil, errors.New("knowledge base is unavailable for reparse")
	}
	if resumeSubmission {
		if resumeAttempt <= 0 {
			resumeAttempt = s.tracker().LatestAttempt(ctx, existing.ID)
		}
		return s.resumeBatchReparseSubmission(ctx, existing, kb, resumeAttempt)
	}

	// When the caller supplies new overrides (e.g. via the reparse confirm
	// dialog), validate them against this knowledge's file type and stage the
	// metadata in memory. The CAS claim below persists it atomically with the
	// state transition so a concurrent delete cannot receive a stale metadata
	// write after it has claimed parse_status=deleting.
	if processOverrides != nil && !resumePreparation && !reclaimFailed {
		if err := ValidateProcessOverrides(ctx, kb, processOverrides, reparseFileTypes(existing)); err != nil {
			return nil, err
		}
		if err := existing.SetProcessOverrides(processOverrides); err != nil {
			logger.Errorf(ctx, "Failed to set process overrides on reparse: %v", err)
			return nil, err
		}
	}

	// Claim the exact row generation observed above before creating spans,
	// scrubbing Wiki work, cleaning resources or enqueueing descendants. Delete
	// uses parse_status=deleting as its durable claim; whichever CAS commits
	// first owns the row and the loser must stop without a full-row Save.
	originalStatus := existing.ParseStatus
	originalGeneration := existing.ProcessingGeneration
	originalOwner := existing.ProcessingOwner
	nextGeneration := uuid.NewString()
	if stableChild {
		nextGeneration = requestedGeneration
	}
	nextOwner := processownership.DocumentOwner(existing.ID, nextGeneration)
	claimTime := time.Now()
	claimValues := map[string]interface{}{
		"parse_status":           types.ParseStatusProcessing,
		"processed_at":           nil,
		"processing_generation":  nextGeneration,
		"processing_owner":       nextOwner,
		"processing_workflow_id": "",
		"processing_fanout":      nil,
		"updated_at":             claimTime,
	}
	if stableChild {
		claimValues["error_message"] = batchReparseMarker(batchReparsePreparing, nextGeneration, resumeAttempt)
	}
	if processOverrides != nil && !resumePreparation && !reclaimFailed {
		claimValues["metadata"] = existing.Metadata
	}
	if !resumePreparation {
		if stableChild && claimExpected {
			swapped, claimErr := claimBatchReparseExpectedSnapshot(
				ctx, s.repo, *expectedSnapshot, claimValues,
			)
			if claimErr != nil {
				return nil, fmt.Errorf("claim batch reparse expected snapshot: %w", claimErr)
			}
			if !swapped {
				logger.Infof(ctx, "Batch reparse child %s lost its exact expected snapshot before claim; skipping",
					existing.ID)
				return existing, nil
			}
		} else {
			if err := s.requireDocumentProcessingIdentitySwap(
				ctx,
				existing,
				originalStatus,
				originalGeneration,
				originalOwner,
				claimValues,
				"claim knowledge reparse",
			); err != nil {
				if errors.Is(err, errKnowledgeStateFenceConflict) {
					return nil, werrors.NewBadRequestError("知识状态已变更，无法重新解析")
				}
				return nil, err
			}
		}
	}
	existing.ParseStatus = types.ParseStatusProcessing
	existing.ProcessedAt = nil
	existing.ProcessingGeneration = nextGeneration
	existing.ProcessingOwner = nextOwner
	existing.ProcessingFanout = nil
	existing.UpdatedAt = claimTime
	if stableChild {
		existing.ErrorMessage = batchReparseMarker(batchReparsePreparing, nextGeneration, resumeAttempt)
	}

	markReparseFailed := func(expectedStatus string, failure error) {
		message := "reparse failed"
		if failure != nil {
			message = failure.Error()
		}
		now := time.Now()
		values := processingFailureValuesPreservingStorage(message, now)
		if !stableChild {
			values["processing_owner"] = ""
		}
		err := s.requireDocumentProcessingIdentitySwap(
			ctx,
			existing,
			expectedStatus,
			existing.ProcessingGeneration,
			existing.ProcessingOwner,
			values,
			"mark knowledge reparse failed",
		)
		if err != nil {
			// In particular, deleting is an expected winner here. Never fall
			// back to UpdateKnowledge: that would resurrect the row.
			logger.Warnf(ctx, "Failed to mark reparse failure for %s without crossing state fence: %v", existing.ID, err)
			return
		}
		existing.ParseStatus = types.ParseStatusFailed
		existing.ErrorMessage = message
		existing.PendingSubtasksCount = 0
		if !stableChild {
			existing.ProcessingOwner = ""
		}
		existing.UpdatedAt = now
	}

	// Completion facts are generation-scoped but the knowledge row is soft
	// deleted and reparsed in place. Bound the ledger immediately after the new
	// generation claim and before publishing any task for it. Redis can then be
	// rebuilt only from facts belonging to this generation.
	if err := cleanupKnowledgeFanoutCompletions(
		ctx,
		s.repo,
		existing.TenantID,
		existing.ID,
		existing.KnowledgeBaseID,
		nextGeneration,
	); err != nil {
		failure := fmt.Errorf("cleanup stale reparse fanout completions: %w", err)
		markReparseFailed(types.ParseStatusProcessing, failure)
		return existing, failure
	}

	// Allocate a fresh span tree only after this reparse owns the row. The
	// payload carries the attempt so worker retries cannot allocate another.
	reparseAttempt := resumeAttempt
	if reparseAttempt <= 0 {
		if root, n, err := s.tracker().OpenAttempt(ctx, existing.ID, ""); err == nil && root != nil {
			reparseAttempt = n
		} else if err != nil {
			logger.Warnf(ctx, "[Reparse] OpenAttempt failed for %s: %v (will fall back in worker)", existing.ID, err)
		}
	}
	if stableChild && reparseAttempt != resumeAttempt {
		marker := batchReparseMarker(batchReparsePreparing, nextGeneration, reparseAttempt)
		if err := s.requireDocumentProcessingIdentitySwap(
			ctx,
			existing,
			types.ParseStatusProcessing,
			existing.ProcessingGeneration,
			existing.ProcessingOwner,
			map[string]interface{}{"error_message": marker, "updated_at": time.Now()},
			"persist batch reparse attempt identity",
		); err != nil {
			return existing, err
		}
		existing.ErrorMessage = marker
	}

	processOverrides, _ = existing.ProcessOverrides()
	reparseEff := ResolveProcessConfig(kb, processOverrides)

	// Keep wiki's pending queue consistent across both manual and non-manual
	// paths. The destructive work (swapping old wiki contributions for new)
	// happens asynchronously inside mapOneDocument — see its oldPageSlugs
	// handling — once post-process re-enqueues wiki ingest. All we need to
	// do here is stop any stale pending ingest op from firing against the
	// pre-reparse chunk set.
	if kb != nil && kb.IsWikiEnabled() {
		s.prepareWikiForReparse(ctx, existing)
	}

	commitReparseToPending := func(prepared *preparedDocumentWorkflow) error {
		now := time.Now()
		errorMessage := ""
		if stableChild {
			errorMessage = batchReparseMarker(batchReparseReady, nextGeneration, reparseAttempt)
		}
		if err := s.commitPreparedReparseWorkflow(
			ctx, prepared, existing, kb.EmbeddingModelID, errorMessage, now,
		); err != nil {
			return err
		}
		existing.ParseStatus = types.ParseStatusPending
		existing.EnableStatus = "disabled"
		existing.Description = ""
		existing.ProcessedAt = nil
		existing.EmbeddingModelID = kb.EmbeddingModelID
		existing.PendingSubtasksCount = 0
		existing.ErrorMessage = errorMessage
		existing.UpdatedAt = now
		return nil
	}
	commitFailure := func(prepared *preparedDocumentWorkflow, err error) {
		markReparseFailed(types.ParseStatusProcessing, err)
		// A deterministic batch child reuses the same generation on retry, so
		// its invisible preparation must remain available. A standalone call
		// can safely cancel an unbound preparation and allocate a new generation
		// on the user's next request. Abort itself refuses an ambiguously-committed
		// binding, preserving accepted work.
		s.handleUncommittedReparseWorkflow(ctx, prepared, stableChild)
	}

	// For manual knowledge, use async manual processing (cleanup + re-indexing in worker)
	if existing.IsManual() {
		meta, metaErr := existing.ManualMetadata()
		if metaErr != nil || meta == nil {
			logger.Errorf(ctx, "Failed to get manual metadata for reparse: %v", metaErr)
			if metaErr == nil {
				metaErr = errors.New("manual knowledge metadata is empty")
			}
			markReparseFailed(types.ParseStatusProcessing, metaErr)
			return nil, werrors.NewBadRequestError("无法获取手工知识内容")
		}

		prepared, err := s.prepareManualProcessingWorkflowWithTracing(
			ctx, existing, meta.Content, true, reparseAttempt, stableTracing,
		)
		if err != nil {
			failure := fmt.Errorf("prepare manual reparse workflow: %w", err)
			markReparseFailed(types.ParseStatusProcessing, failure)
			return existing, failure
		}
		if err := commitReparseToPending(prepared); err != nil {
			logger.Errorf(ctx, "Failed to atomically bind manual reparse workflow: %v", err)
			commitFailure(prepared, err)
			return existing, err
		}
		s.dispatchPreparedDocumentWorkflow(ctx, prepared)
		return existing, nil
	}

	// For non-manual knowledge, cleanup synchronously then enqueue document processing
	logger.Infof(ctx, "Cleaning up existing resources for knowledge: %s", knowledgeID)
	if err := s.cleanupKnowledgeResources(ctx, existing); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_id": knowledgeID,
		})
		markReparseFailed(types.ParseStatusProcessing, err)
		return nil, err
	}

	prepared, err := s.prepareReparseDocumentWorkflow(
		ctx, existing, kb, reparseEff, reparseAttempt, stableTracing,
	)
	if err != nil {
		failure := fmt.Errorf("prepare document reparse workflow: %w", err)
		markReparseFailed(types.ParseStatusProcessing, failure)
		return existing, failure
	}
	if err := commitReparseToPending(prepared); err != nil {
		logger.Errorf(ctx, "Failed to atomically bind document reparse workflow: %v", err)
		commitFailure(prepared, err)
		return existing, err
	}
	logger.Infof(ctx,
		"Knowledge reparse accepted by durable document workflow: knowledge=%s workflow=%s",
		existing.ID, existing.ProcessingWorkflowID,
	)
	s.dispatchPreparedDocumentWorkflow(ctx, prepared)
	return existing, nil
}

// resumeBatchReparseSubmission resumes only the immutable workflow already
// bound by the Pending transaction. Rebuilding a root task here would allow a
// retry's transient tracing parent to change the plan hash and, more
// importantly, could publish work that was never atomically accepted.
func (s *knowledgeService) resumeBatchReparseSubmission(
	ctx context.Context,
	knowledge *types.Knowledge,
	_ *types.KnowledgeBase,
	_ int,
) (*types.Knowledge, error) {
	if err := s.resumeBoundDocumentWorkflow(ctx, knowledge); err != nil {
		return knowledge, fmt.Errorf("resume batch reparse workflow: %w", err)
	}
	return knowledge, nil
}

func (s *knowledgeService) prepareReparseDocumentWorkflow(
	ctx context.Context,
	knowledge *types.Knowledge,
	kb *types.KnowledgeBase,
	effective types.EffectiveProcessConfig,
	attempt int,
	stableTracing *types.TracingContext,
) (*preparedDocumentWorkflow, error) {
	if knowledge == nil || kb == nil {
		return nil, errors.New("prepare document reparse workflow: knowledge and knowledge base are required")
	}
	questionCount := effective.QuestionGenerationConfig.QuestionCount
	if questionCount <= 0 {
		questionCount = 3
	}
	lang, _ := types.LanguageFromContext(ctx)
	payload := types.DocumentProcessPayload{
		TenantID:                 knowledge.TenantID,
		KnowledgeID:              knowledge.ID,
		KnowledgeBaseID:          knowledge.KnowledgeBaseID,
		EnableMultimodel:         effective.EnableMultimodel,
		EnableQuestionGeneration: effective.QuestionGenerationConfig.Enabled,
		QuestionCount:            questionCount,
		Language:                 lang,
		Attempt:                  attempt,
	}
	queue := types.QueueDefault
	switch {
	case knowledge.FilePath != "":
		payload.FilePath = knowledge.FilePath
		payload.FileName = knowledge.FileName
		payload.FileType = getFileType(knowledge.FileName)
		queue = s.documentQueueForStoredDocument(ctx, kb, knowledge, payload)
	case knowledge.Type == "file_url" && knowledge.Source != "":
		payload.FileURL = knowledge.Source
		payload.FileName = knowledge.FileName
		payload.FileType = knowledge.FileType
	case knowledge.Type == "url" && knowledge.Source != "":
		payload.URL = knowledge.Source
	default:
		return nil, errors.New("knowledge has no parseable content (no file, URL, or manual content)")
	}
	documentProcessOwnershipPayload(knowledge, &payload)
	if stableTracing != nil {
		payload.TracingContext = *stableTracing
	} else {
		langfuse.InjectTracing(ctx, &payload)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal document reparse task: %w", err)
	}
	opts := documentProcessTaskOptionsForQueue(
		s.config,
		queue,
		asynq.MaxRetry(3),
		documentProcessStableTaskID(knowledge, queue),
	)
	prepared, err := s.prepareDocumentWorkflow(
		ctx, asynq.NewTask(types.TypeDocumentProcess, payloadBytes), opts...,
	)
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

// CancelKnowledgeParse marks an in-progress parse as cancelled by the user.
//
// Semantics (kept aligned with deletion, but partial work is preserved):
//   - an exact generation CAS first publishes durable "cancelling" intent;
//   - queued work is removed and active lifecycle workers must disappear from
//     two consecutive inspector snapshots before the owner is released;
//   - only then does the row become "cancelled". Reparse rejects "cancelling",
//     closing the old-worker-write/new-generation ABA window;
//   - partial chunks/index already written remain available until reparse
//     performs its normal resource cleanup.
//   - Idempotent: re-calling on an already-cancelled row is a no-op.
//
// Errors:
//   - ParseStatusCompleted / ParseStatusFailed: the parse has already finished.
//   - ParseStatusDeleting: a delete is in progress; cancel cannot supersede it.
func (s *knowledgeService) CancelKnowledgeParse(
	ctx context.Context, knowledgeID string,
) (*types.Knowledge, error) {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	existing, err := s.repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
	if err != nil {
		logger.Errorf(ctx, "CancelKnowledgeParse: failed to load knowledge: %v", err)
		return nil, err
	}
	if existing == nil {
		return nil, werrors.NewNotFoundError("knowledge not found")
	}

	switch existing.ParseStatus {
	case types.ParseStatusCancelled:
		// Idempotent — still attempt the dequeue in case earlier calls
		// raced an enqueue, but skip the row update / span close path.
		s.dequeueKnowledgeTasks(ctx, knowledgeID)
		return existing, nil
	case types.ParseStatusCancelling:
		// Resume the durable barrier left by a prior timed-out cancel request.
	case types.ParseStatusCompleted, types.ParseStatusFailed:
		return nil, werrors.NewBadRequestError("解析已结束，无法取消")
	case types.ParseStatusDeleting:
		return nil, werrors.NewBadRequestError("知识正在删除中，无法取消解析")
	case types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing:
		// Cancellable. `finalizing` is the post-process fan-out window
		// where graph-extract / summary / question subtasks are still
		// running; cancel here stops the LLM cost they would burn.
	default:
		return nil, werrors.NewBadRequestError("知识状态异常，无法取消解析")
	}

	if existing.ParseStatus != types.ParseStatusCancelling {
		now := time.Now()
		claimed, claimErr := compareAndSwapProcessingGeneration(
			ctx,
			s.repo,
			existing.TenantID,
			existing.ID,
			existing.KnowledgeBaseID,
			existing.ProcessingGeneration,
			[]string{existing.ParseStatus},
			map[string]interface{}{
				"parse_status":  types.ParseStatusCancelling,
				"error_message": "用户正在取消解析",
				"updated_at":    now,
			},
		)
		if claimErr != nil {
			logger.Errorf(ctx, "CancelKnowledgeParse: failed to claim cancellation: %v", claimErr)
			return nil, claimErr
		}
		if !claimed {
			current, loadErr := s.repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
			if loadErr == nil && current != nil && current.ParseStatus == types.ParseStatusCancelled {
				return current, nil
			}
			if loadErr != nil && !errors.Is(loadErr, apprepo.ErrKnowledgeNotFound) {
				return nil, loadErr
			}
			return nil, werrors.NewBadRequestError("知识状态已变更，无法取消解析")
		}
		existing.ParseStatus = types.ParseStatusCancelling
		existing.ErrorMessage = "用户正在取消解析"
		existing.UpdatedAt = now
	}

	// A status checkpoint alone does not close heartbeat→artifact-write TOCTOU.
	// Keep the owner durable and forbid reparse while cancelling, then require
	// two empty lifecycle-task snapshots before releasing that owner.
	if err := s.quiesceKnowledgeDeletion(ctx, []*types.Knowledge{existing}); err != nil {
		return nil, fmt.Errorf("cancel knowledge parse quiescence: %w", err)
	}

	now := time.Now()
	cancelled, err := compareAndSwapProcessingGeneration(
		ctx,
		s.repo,
		existing.TenantID,
		existing.ID,
		existing.KnowledgeBaseID,
		existing.ProcessingGeneration,
		[]string{types.ParseStatusCancelling},
		map[string]interface{}{
			"parse_status":           types.ParseStatusCancelled,
			"error_message":          "用户已取消解析",
			"pending_subtasks_count": 0,
			"processing_owner":       "",
			"processing_fanout":      nil,
			"updated_at":             now,
		},
	)
	if err != nil {
		logger.Errorf(ctx, "CancelKnowledgeParse: failed to finalize cancellation: %v", err)
		return nil, err
	}
	if !cancelled {
		// The read snapshot lost to delete, reparse, move, or another cancel.
		// Re-read only to preserve idempotency for the last case; never issue an
		// ID-only fallback that could cancel the winning generation.
		current, loadErr := s.repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
		if loadErr == nil && current != nil && current.ParseStatus == types.ParseStatusCancelled {
			s.dequeueKnowledgeTasks(ctx, knowledgeID)
			return current, nil
		}
		if loadErr != nil && !errors.Is(loadErr, apprepo.ErrKnowledgeNotFound) {
			return nil, loadErr
		}
		return nil, werrors.NewBadRequestError("知识状态已变更，无法取消解析")
	}
	existing.ParseStatus = types.ParseStatusCancelled
	existing.ErrorMessage = "用户已取消解析"
	existing.PendingSubtasksCount = 0
	existing.ProcessingOwner = ""
	existing.ProcessingFanout = nil
	existing.UpdatedAt = now
	logger.Infof(ctx, "Knowledge %s marked as cancelled by user", knowledgeID)

	// Close the active attempt span tree so the UI stops showing "进行中"
	// for the cancelled run. AbortAttempt cascade-cancels every still-
	// running descendant (multimodal per-image, postprocess subtasks,
	// graph chunks) BEFORE closing the root, otherwise the trace
	// viewer would leave those striped/running bars hanging forever
	// because workers exit via their abort-guard without ever calling
	// FailSpan on their own subspan. Best-effort: nil tracker / missing
	// attempt no-ops.
	if attempt := s.tracker().LatestAttempt(ctx, knowledgeID); attempt > 0 {
		s.tracker().AbortAttempt(ctx, knowledgeID, attempt,
			"USER_CANCELLED", "用户已取消解析", "用户已取消解析")
	}

	// Best-effort dequeue. Failures here don't block the cancel — the
	// downstream tasks will still self-abort at their entry guards.
	s.dequeueKnowledgeTasks(ctx, knowledgeID)
	// Wiki ingest lives in its own per-KB pending queue (task_pending_ops)
	// rather than asynq, so dequeueKnowledgeTasks above can't see it.
	// Mirror the deletion path's scrub so a cancelled knowledge doesn't
	// get picked up by the next 30s batch and burn a wiki LLM call on a
	// doc the user already abandoned. The in-flight worker would skip it
	// at isWikiKnowledgeAborted anyway, but scrubbing avoids waking the
	// batch in the first place.
	s.scrubWikiPendingIngest(ctx, existing.KnowledgeBaseID, knowledgeID, "cancel")
	return existing, nil
}

// dequeueKnowledgeTasks asks the task inspector to remove any queued
// tasks for this knowledge and signal active workers to stop. Safe to
// call when the inspector is a no-op (Lite mode).
func (s *knowledgeService) dequeueKnowledgeTasks(ctx context.Context, knowledgeID string) {
	if s.taskInspector == nil {
		return
	}
	if _, _, err := s.taskInspector.CancelTasksForKnowledge(ctx, knowledgeID); err != nil {
		logger.Warnf(ctx, "CancelKnowledgeParse: dequeue best-effort failed for %s: %v", knowledgeID, err)
	}
}

func (s *knowledgeService) updateChunkVector(ctx context.Context, kbID string, chunks []*types.Chunk) error {
	// Get embedding model from knowledge base
	sourceKB, err := s.kbService.GetKnowledgeBaseByID(ctx, kbID)
	if err != nil {
		return err
	}
	embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, sourceKB.EmbeddingModelID)
	if err != nil {
		return err
	}

	// Initialize composite retrieve engine from tenant configuration
	indexInfo := make([]*types.IndexInfo, 0, len(chunks))
	ids := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.KnowledgeBaseID != kbID {
			logger.Warnf(ctx, "Knowledge base ID mismatch: %s != %s", chunk.KnowledgeBaseID, kbID)
			continue
		}
		indexInfo = append(indexInfo, &types.IndexInfo{
			Content:         chunk.Content,
			SourceID:        chunk.ID,
			SourceType:      types.ChunkSourceType,
			ChunkID:         chunk.ID,
			KnowledgeID:     chunk.KnowledgeID,
			KnowledgeBaseID: chunk.KnowledgeBaseID,
			IsEnabled:       true,
		})
		ids = append(ids, chunk.ID)
	}

	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, types.MustTenantIDFromContext(ctx), sourceKB.VectorStoreID)
	if err != nil {
		return err
	}

	// Delete old vector representation of the chunk
	err = retrieveEngine.DeleteByChunkIDList(ctx, ids, embeddingModel.GetDimensions(), sourceKB.Type)
	if err != nil {
		return err
	}

	// Index updated chunk content with new vector representation
	err = retrieveEngine.BatchIndex(ctx, embeddingModel, indexInfo)
	if err != nil {
		return err
	}
	return nil
}

func (s *knowledgeService) UpdateImageInfo(
	ctx context.Context,
	knowledgeID string,
	chunkID string,
	imageInfo string,
) error {
	var images []*types.ImageInfo
	if err := json.Unmarshal([]byte(imageInfo), &images); err != nil {
		logger.Errorf(ctx, "Failed to unmarshal image info: %v", err)
		return err
	}
	if len(images) != 1 {
		logger.Warnf(ctx, "Expected exactly one image info, got %d", len(images))
		return nil
	}
	image := images[0]

	// Retrieve all chunks with the given parent chunk ID
	chunk, err := s.chunkService.GetChunkByID(ctx, chunkID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get chunk: %v", err)
		return err
	}
	chunk.ImageInfo = imageInfo
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	chunkChildren, err := s.chunkService.ListChunkByParentID(ctx, tenantID, chunkID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"parent_chunk_id": chunkID,
			"tenant_id":       tenantID,
		})
		return err
	}
	logger.Infof(ctx, "Found %d chunks with parent chunk ID: %s", len(chunkChildren), chunkID)

	// Iterate through each chunk and update its content based on the image information
	updateChunk := []*types.Chunk{chunk}
	var addChunk []*types.Chunk

	// Track whether we've found OCR and caption child chunks for this image
	hasOCRChunk := false
	hasCaptionChunk := false

	for i, child := range chunkChildren {
		// Skip chunks that are not image types
		var cImageInfo []*types.ImageInfo
		err = json.Unmarshal([]byte(child.ImageInfo), &cImageInfo)
		if err != nil {
			logger.Warnf(ctx, "Failed to unmarshal image %s info: %v", child.ID, err)
			continue
		}
		if len(cImageInfo) == 0 {
			continue
		}
		if cImageInfo[0].OriginalURL != image.OriginalURL {
			logger.Warnf(ctx, "Skipping chunk ID: %s, image URL mismatch: %s != %s",
				child.ID, cImageInfo[0].OriginalURL, image.OriginalURL)
			continue
		}

		// Mark that we've found chunks for this image
		switch child.ChunkType {
		case types.ChunkTypeImageCaption:
			hasCaptionChunk = true
			// Update caption if it has changed
			if image.Caption != cImageInfo[0].Caption {
				child.Content = image.Caption
				child.ImageInfo = imageInfo
				updateChunk = append(updateChunk, chunkChildren[i])
			}
		case types.ChunkTypeImageOCR:
			hasOCRChunk = true
			// Update OCR if it has changed
			if image.OCRText != cImageInfo[0].OCRText {
				child.Content = image.OCRText
				child.ImageInfo = imageInfo
				updateChunk = append(updateChunk, chunkChildren[i])
			}
		}
	}

	// Create a new caption chunk if it doesn't exist and we have caption data
	if !hasCaptionChunk && image.Caption != "" {
		captionChunk := &types.Chunk{
			ID:              uuid.New().String(),
			TenantID:        tenantID,
			KnowledgeID:     chunk.KnowledgeID,
			KnowledgeBaseID: chunk.KnowledgeBaseID,
			Content:         image.Caption,
			ChunkType:       types.ChunkTypeImageCaption,
			ParentChunkID:   chunk.ID,
			ImageInfo:       imageInfo,
		}
		addChunk = append(addChunk, captionChunk)
		logger.Infof(ctx, "Created new caption chunk ID: %s for image URL: %s", captionChunk.ID, image.OriginalURL)
	}

	// Create a new OCR chunk if it doesn't exist and we have OCR data
	if !hasOCRChunk && image.OCRText != "" {
		ocrChunk := &types.Chunk{
			ID:              uuid.New().String(),
			TenantID:        tenantID,
			KnowledgeID:     chunk.KnowledgeID,
			KnowledgeBaseID: chunk.KnowledgeBaseID,
			Content:         image.OCRText,
			ChunkType:       types.ChunkTypeImageOCR,
			ParentChunkID:   chunk.ID,
			ImageInfo:       imageInfo,
		}
		addChunk = append(addChunk, ocrChunk)
		logger.Infof(ctx, "Created new OCR chunk ID: %s for image URL: %s", ocrChunk.ID, image.OriginalURL)
	}
	logger.Infof(ctx, "Updated %d chunks out of %d total chunks", len(updateChunk), len(chunkChildren)+1)

	if len(addChunk) > 0 {
		err := s.chunkService.CreateChunks(ctx, addChunk)
		if err != nil {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{
				"add_chunk_size": len(addChunk),
			})
			return err
		}
	}

	// Update the chunks
	for _, c := range updateChunk {
		err := s.chunkService.UpdateChunk(ctx, c)
		if err != nil {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{
				"chunk_id":     c.ID,
				"knowledge_id": c.KnowledgeID,
			})
			return err
		}
	}

	// Update the chunk vector
	err = s.updateChunkVector(ctx, chunk.KnowledgeBaseID, append(updateChunk, addChunk...))
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"chunk_id":     chunk.ID,
			"knowledge_id": chunk.KnowledgeID,
		})
		return err
	}

	// Update the knowledge file hash
	knowledge, err := s.repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge: %v", err)
		return err
	}
	fileHash := calculateStr(knowledgeID, knowledge.FileHash, imageInfo)
	knowledge.FileHash = fileHash
	err = s.repo.UpdateKnowledge(ctx, knowledge)
	if err != nil {
		logger.Warnf(ctx, "Failed to update knowledge file hash: %v", err)
	}

	logger.Infof(ctx, "Updated chunk successfully, chunk ID: %s, knowledge ID: %s", chunk.ID, chunk.KnowledgeID)
	return nil
}

// ProcessManualUpdate handles Asynq manual knowledge update tasks.
// It performs cleanup of old indexes/chunks (when NeedCleanup is true) and re-indexes the content.
func (s *knowledgeService) ProcessManualUpdate(ctx context.Context, t *asynq.Task) error {
	var payload types.ManualProcessPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "failed to unmarshal manual process task payload: %v", err)
		return fmt.Errorf("unmarshal manual process task payload: %w", err)
	}

	ctx = logger.WithRequestID(ctx, payload.RequestId)
	ctx = logger.WithField(ctx, "manual_process", payload.KnowledgeID)
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.TenantID == 0 || strings.TrimSpace(payload.KnowledgeID) == "" ||
		strings.TrimSpace(payload.KnowledgeBaseID) == "" {
		return errors.New("manual processing: complete tenant, KB and knowledge identity is required")
	}
	if strings.TrimSpace(payload.ProcessingGeneration) == "" && strings.TrimSpace(payload.ProcessingOwner) == "" {
		logger.Errorf(ctx, "ProcessManualUpdate: generation and owner are required; refusing unfenced task")
		return processownership.RepairLegacyTask(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, "manual processing",
		)
	}
	if strings.TrimSpace(payload.ProcessingGeneration) == "" || strings.TrimSpace(payload.ProcessingOwner) == "" {
		return errors.New("manual processing: partial processing identity is invalid")
	}

	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		logger.Errorf(ctx, "ProcessManualUpdate: failed to get tenant: %v", err)
		if errors.Is(err, apprepo.ErrTenantNotFound) {
			return nil
		}
		return fmt.Errorf("get manual processing tenant %d: %w", payload.TenantID, err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	knowledge, err := s.repo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		logger.Errorf(ctx, "ProcessManualUpdate: failed to get knowledge: %v", err)
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("get manual processing knowledge %s: %w", payload.KnowledgeID, err)
	}
	if knowledge == nil {
		logger.Warnf(ctx, "ProcessManualUpdate: knowledge not found: %s", payload.KnowledgeID)
		return nil
	}
	if !knowledge.IsManual() || knowledge.TenantID != payload.TenantID ||
		knowledge.ID != payload.KnowledgeID || knowledge.KnowledgeBaseID != payload.KnowledgeBaseID {
		logger.Warnf(ctx, "ProcessManualUpdate: knowledge identity changed, skipping: %s", payload.KnowledgeID)
		return nil
	}
	if knowledge.ProcessingGeneration != payload.ProcessingGeneration {
		logger.Infof(ctx, "ProcessManualUpdate: processing identity superseded for %s, skipping", payload.KnowledgeID)
		return nil
	}

	// processChunks atomically commits chunks/indexes, consumes the core owner,
	// and persists the exact downstream plan before publishing fanout tasks. A
	// publication error therefore returns here on the same manual task retry
	// with owner="". Replay only the durable plan; never clean up or execute the
	// expensive core write path a second time.
	if corefanout.HasCommittedPlan(knowledge) {
		if err := s.replayCommittedCoreFanout(ctx, knowledge); err != nil {
			return fmt.Errorf("recover committed manual fanout: %w", err)
		}
		return nil
	}
	if knowledge.ProcessingOwner != payload.ProcessingOwner {
		logger.Infof(ctx, "ProcessManualUpdate: processing identity superseded for %s, skipping", payload.KnowledgeID)
		return nil
	}
	if knowledge.ParseStatus != types.ParseStatusPending && knowledge.ParseStatus != types.ParseStatusProcessing {
		// Only pending -> processing is claimable. In particular, a duplicate
		// worker that loaded the same generation after the winner changed the
		// row to processing must stop before cleanup; processing -> processing
		// would recreate the storage-reset/double-decrement race.
		logger.Infof(ctx, "ProcessManualUpdate: generation %s is not claimable in status %s for %s, skipping",
			payload.ProcessingGeneration, knowledge.ParseStatus, payload.KnowledgeID)
		return nil
	}

	if knowledge.ParseStatus == types.ParseStatusPending {
		claimTime := time.Now()
		claimValues := map[string]interface{}{
			"parse_status": types.ParseStatusProcessing,
			"updated_at":   claimTime,
		}
		preserveMoveRecoveryMarker := isKnowledgeMoveReparseMarker(knowledge.ErrorMessage)
		if !preserveMoveRecoveryMarker {
			// Deliberately omit this column for move-recovery markers. Writing the
			// value read above would still race the queued-marker CAS.
			claimValues["error_message"] = ""
		}
		if err := s.requireDocumentProcessingIdentitySwap(
			ctx,
			knowledge,
			types.ParseStatusPending,
			payload.ProcessingGeneration,
			payload.ProcessingOwner,
			claimValues,
			"claim manual processing generation",
		); err != nil {
			if errors.Is(err, errKnowledgeStateFenceConflict) {
				logger.Infof(ctx, "ProcessManualUpdate: generation %s lost its claim for %s, skipping",
					payload.ProcessingGeneration, payload.KnowledgeID)
				return nil
			}
			return err
		}
		knowledge.ParseStatus = types.ParseStatusProcessing
		if !preserveMoveRecoveryMarker {
			knowledge.ErrorMessage = ""
		}
		knowledge.UpdatedAt = claimTime
	}

	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID)
	if err != nil {
		logger.Errorf(ctx, "ProcessManualUpdate: failed to get knowledge base: %v", err)
		return fmt.Errorf("get manual processing knowledge base %s: %w", payload.KnowledgeBaseID, err)
	}

	// Reparse producers allocate the attempt before publishing the stable
	// processing task. Legacy/create payloads still allocate here.
	attempt := payload.Attempt
	if attempt <= 0 {
		if root, n, err := s.tracker().OpenAttempt(ctx, knowledge.ID, payload.LangfuseTraceID); err == nil && root != nil {
			attempt = n
		} else if err != nil {
			logger.Warnf(ctx, "ProcessManualUpdate: OpenAttempt failed for %s: %v", knowledge.ID, err)
		}
	}
	ctx = withAttempt(ctx, attempt)

	// Cleanup old resources (indexes, chunks, graph) for update operations
	if payload.NeedCleanup {
		if err := s.heartbeatActiveProcessing(ctx, knowledge, "fence manual cleanup"); err != nil {
			if errors.Is(err, errKnowledgeStateFenceConflict) {
				return nil
			}
			return err
		}
		if err := s.cleanupKnowledgeResources(ctx, knowledge); err != nil {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{
				"knowledge_id": payload.KnowledgeID,
			})
			// Cleanup can fail after only a subset of external resources were
			// removed. Keep the generation nonterminal and retry; publishing failed
			// here would make leaked vectors/chunks look final.
			return fmt.Errorf("cleanup old manual resources: %w", err)
		}
		if err := s.heartbeatActiveProcessing(ctx, knowledge, "fence post-cleanup manual processing"); err != nil {
			if errors.Is(err, errKnowledgeStateFenceConflict) {
				return nil
			}
			return err
		}
	}

	// Run manual processing (image resolution + chunking + embedding) synchronously within the worker
	if err := s.triggerManualProcessing(ctx, kb, knowledge, payload.Content, true); err != nil {
		return fmt.Errorf("process manual knowledge %s: %w", knowledge.ID, err)
	}
	return nil
}

// replayCommittedCoreFanout is the shared exact recovery boundary for normal
// documents and manual content. The repository assertion fails closed: without
// the durable completion ledger a replay cannot safely infer fan-in progress.
func (s *knowledgeService) replayCommittedCoreFanout(
	ctx context.Context,
	knowledge *types.Knowledge,
) error {
	completionStore, ok := s.repo.(processownership.DurableFanoutCompletionStore)
	if !ok || completionStore == nil {
		return errors.New("durable completion store is unavailable")
	}
	return corefanout.Replay(ctx, s.task, s.redisClient, completionStore, knowledge)
}

func (s *knowledgeService) analyzeStoredDocumentForGuard(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	payload types.DocumentProcessPayload,
) (fileguard.Report, error) {
	fileName := strings.TrimSpace(payload.FileName)
	if fileName == "" {
		fileName = knowledge.FileName
	}
	fileType := strings.TrimSpace(payload.FileType)
	if fileType == "" {
		fileType = knowledge.FileType
	}
	if !fileguard.NeedsContentAnalysis(fileName, fileType) {
		return fileguard.AnalyzeSize(fileName, fileType, knowledge.FileSize), nil
	}

	fileService, err := s.auxiliaryFileServiceForPath(
		ctx, kb, knowledge.KnowledgeBaseID, knowledge.ID, payload.FilePath,
	)
	if err != nil {
		return fileguard.Report{}, err
	}
	fileReader, err := fileService.GetFile(ctx, payload.FilePath)
	if err != nil {
		return fileguard.Report{}, err
	}
	defer fileReader.Close()

	staged, err := os.CreateTemp("", "weknora-fileguard-*")
	if err != nil {
		return fileguard.Report{}, err
	}
	stagedName := staged.Name()
	defer func() {
		_ = staged.Close()
		_ = os.Remove(stagedName)
	}()
	maxSource := secutils.GetMaxKnowledgeSourceFileSize()
	written, err := io.Copy(staged, io.LimitReader(fileReader, maxSource+1))
	if err != nil {
		return fileguard.Report{}, err
	}
	if written > maxSource {
		return fileguard.Report{}, fmt.Errorf("logical document exceeds configured source limit of %d bytes", maxSource)
	}
	if written != knowledge.FileSize {
		return fileguard.Report{}, fmt.Errorf(
			"stored source size mismatch: read %d bytes, expected %d", written, knowledge.FileSize,
		)
	}
	return fileguard.AnalyzeReaderAt(fileName, fileType, staged, written), nil
}

func (s *knowledgeService) documentQueueForStoredDocument(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	payload types.DocumentProcessPayload,
) string {
	report, err := s.analyzeStoredDocumentForGuard(ctx, kb, knowledge, payload)
	if err != nil {
		logger.Warnf(ctx, "failed to inspect stored file for queue routing knowledge_id=%s: %v", knowledge.ID, err)
		return types.QueueDocument
	}
	return documentProcessQueueForReport(report)
}

func (s *knowledgeService) rerouteHeavyDocumentIfNeeded(
	ctx context.Context,
	knowledge *types.Knowledge,
	payload types.DocumentProcessPayload,
	report fileguard.Report,
	maxRetry int,
) (bool, error) {
	// The document-level scheduler already limits whole workflows per instance.
	// Re-enqueuing a large file onto a separate task-type queue would release its
	// document slot halfway through and recreate the starvation this scheduler
	// is designed to remove. Keep the report for validation/logging only.
	if report.IsHeavy() {
		logger.Infof(ctx, "Heavy document remains in document workflow queue: knowledge_id=%s reasons=%s",
			knowledge.ID, strings.Join(report.HeavyReasons, "；"))
	}
	return false, nil
}

// ProcessDocument handles Asynq document processing tasks
func (s *knowledgeService) ProcessDocument(ctx context.Context, t *asynq.Task) error {
	var payload types.DocumentProcessPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "failed to unmarshal document process task payload: %v", err)
		return fmt.Errorf("unmarshal document process task payload: %w", err)
	}

	ctx = logger.WithRequestID(ctx, payload.RequestId)
	ctx = logger.WithField(ctx, "document_process", payload.KnowledgeID)
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.Language != "" {
		ctx = context.WithValue(ctx, types.LanguageContextKey, payload.Language)
	}
	if payload.TenantID == 0 || strings.TrimSpace(payload.KnowledgeID) == "" ||
		strings.TrimSpace(payload.KnowledgeBaseID) == "" {
		return errors.New("document processing: complete tenant, KB and knowledge identity is required")
	}
	if strings.TrimSpace(payload.ProcessingGeneration) == "" && strings.TrimSpace(payload.ProcessingOwner) == "" {
		logger.Errorf(ctx, "ProcessDocument: complete processing identity is required; refusing unfenced task")
		return processownership.RepairLegacyTask(
			ctx, s.repo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, "document processing",
		)
	}
	if strings.TrimSpace(payload.ProcessingGeneration) == "" || strings.TrimSpace(payload.ProcessingOwner) == "" {
		return errors.New("document processing: partial processing identity is invalid")
	}

	// 获取任务重试信息，用于判断是否是最后一次重试
	retryCount, maxRetry, _ := taskRetryMetadata(ctx)
	isLastRetry := retryCount >= maxRetry

	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		logger.Errorf(ctx, "failed to get tenant: %v", err)
		return fmt.Errorf("get document tenant %d: %w", payload.TenantID, err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	logger.Infof(ctx, "Processing document task: knowledge_id=%s, file_path=%s, retry=%d/%d",
		payload.KnowledgeID, payload.FilePath, retryCount, maxRetry)

	// 幂等性检查：获取knowledge记录
	knowledge, err := s.repo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		logger.Errorf(ctx, "failed to get knowledge: %v", err)
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("get document knowledge %s: %w", payload.KnowledgeID, err)
	}

	if knowledge == nil {
		return nil
	}

	if knowledge.TenantID != payload.TenantID || knowledge.ID != payload.KnowledgeID ||
		knowledge.KnowledgeBaseID != payload.KnowledgeBaseID ||
		knowledge.ProcessingGeneration != payload.ProcessingGeneration {
		logger.Infof(ctx, "ProcessDocument: stale knowledge base or generation for %s, skipping", payload.KnowledgeID)
		return nil
	}

	// A successful core commit consumes the owner and persists the exact fanout
	// plan. Retries after that boundary may only replay downstream tasks.
	if knowledge.ParseStatus == types.ParseStatusProcessing &&
		knowledge.ProcessingOwner == "" && knowledge.ProcessedAt != nil {
		if err := s.replayCommittedCoreFanout(ctx, knowledge); err != nil {
			return fmt.Errorf("recover committed document fanout: %w", err)
		}
		return nil
	}

	switch knowledge.ParseStatus {
	case types.ParseStatusPending:
		if knowledge.ProcessingOwner != payload.ProcessingOwner {
			logger.Infof(ctx, "ProcessDocument: pending owner superseded for %s, skipping", payload.KnowledgeID)
			return nil
		}
		claimTime := time.Now()
		claimValues := map[string]interface{}{
			"parse_status": types.ParseStatusProcessing,
			"updated_at":   claimTime,
		}
		preserveMoveRecoveryMarker := isKnowledgeMoveReparseMarker(knowledge.ErrorMessage)
		if !preserveMoveRecoveryMarker {
			claimValues["error_message"] = ""
		}
		if err := s.requireDocumentProcessingIdentitySwap(
			ctx,
			knowledge,
			types.ParseStatusPending,
			payload.ProcessingGeneration,
			payload.ProcessingOwner,
			claimValues,
			"claim document processing generation",
		); err != nil {
			if errors.Is(err, errKnowledgeStateFenceConflict) {
				logger.Infof(ctx, "ProcessDocument: processing claim lost for %s, skipping", payload.KnowledgeID)
				return nil
			}
			return err
		}
		knowledge.ParseStatus = types.ParseStatusProcessing
		knowledge.UpdatedAt = claimTime
		if !preserveMoveRecoveryMarker {
			knowledge.ErrorMessage = ""
		}
	case types.ParseStatusProcessing:
		if knowledge.ProcessingOwner != payload.ProcessingOwner {
			logger.Infof(ctx, "ProcessDocument: active owner superseded for %s, skipping", payload.KnowledgeID)
			return nil
		}
		// Same stable task owner is an Asynq retry. It may safely rebuild the
		// pre-commit artifacts; a core-committed row took the branch above.
	default:
		logger.Infof(ctx, "ProcessDocument: terminal or non-claimable status %s for %s, skipping",
			knowledge.ParseStatus, payload.KnowledgeID)
		return nil
	}

	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("get document knowledge base %s: %w", payload.KnowledgeBaseID, err)
	}
	if kb == nil || kb.ID != knowledge.KnowledgeBaseID || kb.TenantID != knowledge.TenantID {
		if markErr := s.markActiveProcessingFailed(ctx, knowledge, "knowledge base identity mismatch", "mark knowledge base mismatch failed"); markErr != nil {
			return markErr
		}
		return nil
	}

	processOverrides, _ := knowledge.ProcessOverrides()
	eff := ResolveProcessConfig(kb, processOverrides)

	// Resolve the attempt for span tracking. The enqueue site sets
	// payload.Attempt to a fresh number for the initial parse and to
	// max+1 for each user-initiated reparse. Asynq retries within a
	// single user action keep the same payload (so retries record
	// onto the same attempt). For payloads predating this code we
	// fall back to OpenAttempt.
	attempt := payload.Attempt
	if attempt <= 0 {
		if root, n, err := s.tracker().OpenAttempt(ctx, knowledge.ID, payload.LangfuseTraceID); err == nil && root != nil {
			attempt = n
		}
	}
	ctx = withAttempt(ctx, attempt)

	// 检查多模态配置（仅对文件导入）
	if payload.FilePath != "" && !payload.EnableMultimodel && IsImageType(payload.FileType) {
		logger.GetLogger(ctx).WithField("knowledge_id", knowledge.ID).
			WithField("error", ErrImageNotParse).Errorf("processDocument image without enable multimodel")
		if err := s.markActiveProcessingFailed(ctx, knowledge, ErrImageNotParse.Error(), "mark image configuration failure"); err != nil {
			return err
		}
		return nil
	}

	// 检查音频ASR配置（仅对文件导入）
	if payload.FilePath != "" && IsAudioType(payload.FileType) && !eff.ASRConfig.IsASREnabled() {
		logger.GetLogger(ctx).WithField("knowledge_id", knowledge.ID).
			Errorf("processDocument audio without ASR model configured")
		if err := s.markActiveProcessingFailed(ctx, knowledge, "上传音频文件需要设置ASR语音识别模型", "mark audio configuration failure"); err != nil {
			return err
		}
		return nil
	}

	// 视频文件不再支持入库解析
	if payload.FilePath != "" && IsVideoType(payload.FileType) {
		logger.GetLogger(ctx).WithField("knowledge_id", knowledge.ID).
			Errorf("processDocument video not supported")
		if err := s.markActiveProcessingFailed(ctx, knowledge, "暂不支持视频文件", "mark unsupported video failure"); err != nil {
			return err
		}
		return nil
	}

	// New pipeline: convert -> store images -> chunk -> vectorize -> multimodal tasks
	var convertResult *types.ReadResult
	var chunks []types.ParsedChunk

	if payload.FileURL != "" {
		// file_url import: SSRF re-check (防 DNS 重绑定), download, persist, then delegate to convert()
		if err := secutils.ValidateURLForSSRF(payload.FileURL); err != nil {
			logger.Errorf(ctx, "File URL rejected for SSRF protection in ProcessDocument: %s, err: %v", payload.FileURL, err)
			if markErr := s.markActiveProcessingFailed(ctx, knowledge, "File URL is not allowed for security reasons", "mark file URL security failure"); markErr != nil {
				return markErr
			}
			return nil
		}

		resolvedFileName := payload.FileName
		resolvedFileType := payload.FileType
		download, err := downloadFileFromURL(
			ctx, payload.FileURL, &resolvedFileName, &resolvedFileType,
		)
		if err != nil {
			logger.Errorf(ctx, "Failed to download file from URL: %s, error: %v", payload.FileURL, err)
			if isLastRetry {
				if markErr := s.markActiveProcessingFailed(ctx, knowledge, err.Error(), "mark file URL download failure"); markErr != nil {
					return markErr
				}
			}
			return fmt.Errorf("failed to download file from URL: %w", err)
		}
		defer download.Close()

		if resolvedFileType != "" && !allowedFileURLExtensions[strings.ToLower(resolvedFileType)] {
			logger.Errorf(ctx, "Unsupported file type resolved from file URL: %s", resolvedFileType)
			if markErr := s.markActiveProcessingFailed(ctx, knowledge,
				fmt.Sprintf("unsupported file type: %s", resolvedFileType), "mark unsupported file URL type"); markErr != nil {
				return markErr
			}
			return nil
		}
		guardReport := fileguard.AnalyzeReaderAt(
			resolvedFileName, resolvedFileType, download.File, download.Size,
		)
		if err := guardReport.ValidationError(); err != nil {
			logger.Warnf(ctx, "File URL preflight rejected %s: %s", resolvedFileName, err.Error())
			if markErr := s.markActiveProcessingFailed(ctx, knowledge, err.Error(), "mark file URL preflight failure"); markErr != nil {
				return markErr
			}
			return nil
		}

		metadataValues := map[string]interface{}{}
		if resolvedFileName != "" && knowledge.FileName == "" {
			knowledge.FileName = resolvedFileName
			metadataValues["file_name"] = resolvedFileName
		}
		if resolvedFileType != "" && knowledge.FileType == "" {
			knowledge.FileType = resolvedFileType
			metadataValues["file_type"] = resolvedFileType
		}
		knowledge.FileSize = download.Size
		metadataValues["file_size"] = download.Size
		if len(metadataValues) > 0 {
			metadataValues["updated_at"] = time.Now()
			if err := s.updateActiveProcessingColumns(ctx, knowledge, metadataValues, "persist resolved file URL metadata"); err != nil {
				return err
			}
		}

		fileSvc, storageErr := s.plannedAuxiliaryFileService(
			ctx, kb, knowledge, knowledgeaux.KindFileURLTemp,
		)
		if storageErr != nil {
			return storageErr
		}
		streamFileSvc, ok := fileSvc.(auxiliaryStreamSaver)
		if !ok {
			return errors.New("file URL storage does not support bounded stream commits")
		}
		if _, err := download.File.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewind downloaded file: %w", err)
		}
		filePath, err := streamFileSvc.SaveReader(
			ctx, download.File, download.Size, download.ContentType,
			payload.TenantID, resolvedFileName,
		)
		if err != nil {
			if isLastRetry {
				if markErr := s.markActiveProcessingFailed(ctx, knowledge, err.Error(), "mark file URL storage failure"); markErr != nil {
					return markErr
				}
			}
			return fmt.Errorf("failed to save downloaded file: %w", err)
		}

		payload.FilePath = filePath
		payload.FileName = resolvedFileName
		payload.FileType = resolvedFileType
		payload.FileURL = ""
		if guardReport.NeedsSplit() {
			logger.Infof(ctx,
				"[document split] downloaded source exceeds per-part workload limits knowledge=%s reasons=%s",
				knowledge.ID, strings.Join(guardReport.SplitReasons, "；"),
			)
			if splitErr := s.prepareDocumentSplit(
				ctx, kb, knowledge, payload, guardReport,
			); splitErr != nil {
				_, failErr := s.failKnowledge(
					ctx, knowledge, isLastRetry,
					"failed to prepare downloaded physical document split: %v", splitErr,
				)
				return failErr
			}
			return nil
		}
		if rerouted, err := s.rerouteHeavyDocumentIfNeeded(ctx, knowledge, payload, guardReport, maxRetry); err != nil {
			return err
		} else if rerouted {
			return nil
		}
		convertResult, err = s.convert(ctx, payload, kb, knowledge, eff, isLastRetry)
		if err != nil {
			return err
		}
		if convertResult == nil {
			return nil
		}
	} else if payload.URL != "" {
		// URL import
		convertResult, err = s.convert(ctx, payload, kb, knowledge, eff, isLastRetry)
		if err != nil {
			return err
		}
		if convertResult == nil {
			return nil
		}
		// Update knowledge title from extracted page title if not already set
		if knowledge.Title == "" || knowledge.Title == payload.URL {
			if extractedTitle := convertResult.Metadata["title"]; extractedTitle != "" {
				updatedAt := time.Now()
				if err := s.updateActiveProcessingColumns(ctx, knowledge, map[string]interface{}{
					"title":      extractedTitle,
					"updated_at": updatedAt,
				}, "persist extracted URL title"); err != nil {
					logger.Warnf(ctx, "Failed to update knowledge title from extracted page title: %v", err)
				} else {
					knowledge.Title = extractedTitle
					knowledge.UpdatedAt = updatedAt
					logger.Infof(ctx, "Updated knowledge title to extracted page title: %s", extractedTitle)
				}
			}
		}
	} else if len(payload.Passages) > 0 {
		// Text passage import - direct chunking, no conversion needed
		passageChunks := make([]types.ParsedChunk, 0, len(payload.Passages))
		start, end := 0, 0
		for i, p := range payload.Passages {
			if p == "" {
				continue
			}
			end += len([]rune(p))
			passageChunks = append(passageChunks, types.ParsedChunk{
				Content: p,
				Seq:     i,
				Start:   start,
				End:     end,
			})
			start = end
		}
		passageOpts := ProcessChunksOptions{
			EnableQuestionGeneration: payload.EnableQuestionGeneration,
			QuestionCount:            payload.QuestionCount,
		}
		return s.processChunks(ctx, kb, knowledge, passageChunks, passageOpts)
	} else {
		// File import
		if payload.FilePath != "" {
			guardReport, err := s.analyzeStoredDocumentForGuard(ctx, kb, knowledge, payload)
			if err != nil {
				logger.Errorf(ctx, "failed to inspect file before parsing: %v", err)
				_, failErr := s.failKnowledge(ctx, knowledge, isLastRetry, "failed to inspect file before parsing: %v", err)
				return failErr
			}
			if validationErr := guardReport.ValidationError(); validationErr != nil {
				logger.Warnf(ctx, "Stored file preflight rejected %s: %s", payload.FileName, validationErr.Error())
				if markErr := s.markActiveProcessingFailed(ctx, knowledge, validationErr.Error(), "mark stored file preflight failure"); markErr != nil {
					return markErr
				}
				return nil
			}
			if guardReport.NeedsSplit() {
				logger.Infof(ctx,
					"[document split] source exceeds per-part workload limits knowledge=%s reasons=%s",
					knowledge.ID, strings.Join(guardReport.SplitReasons, "；"),
				)
				if splitErr := s.prepareDocumentSplit(ctx, kb, knowledge, payload, guardReport); splitErr != nil {
					logger.Errorf(ctx, "failed to prepare physical document split: %v", splitErr)
					_, failErr := s.failKnowledge(
						ctx, knowledge, isLastRetry,
						"failed to prepare physical document split: %v", splitErr,
					)
					return failErr
				}
				// The durable part plan now owns the rest of this generation.
				// Returning releases the whole-document scheduler slot.
				return nil
			}
			if rerouted, err := s.rerouteHeavyDocumentIfNeeded(ctx, knowledge, payload, guardReport, maxRetry); err != nil {
				return err
			} else if rerouted {
				return nil
			}
		}
		convertResult, err = s.convert(ctx, payload, kb, knowledge, eff, isLastRetry)
		if err != nil {
			return err
		}
		if convertResult == nil {
			return nil
		}
	}

	// Step 1.5: ASR transcription for audio files
	if convertResult != nil && convertResult.IsAudio && len(convertResult.AudioData) > 0 {
		if !eff.ASRConfig.IsASREnabled() {
			logger.Error(ctx, "Audio file detected but ASR is not configured")
			if markErr := s.markActiveProcessingFailed(ctx, knowledge,
				"ASR model is not configured for audio transcription", "mark ASR configuration failure"); markErr != nil {
				return markErr
			}
			return nil
		}

		logger.Infof(ctx, "[ASR] Starting audio transcription for knowledge %s, audio size=%d bytes",
			knowledge.ID, len(convertResult.AudioData))

		asrModel, err := s.modelService.GetASRModel(ctx, eff.ASRConfig.ModelID)
		if err != nil {
			logger.Errorf(ctx, "[ASR] Failed to get ASR model: %v", err)
			if markErr := s.markActiveProcessingFailed(ctx, knowledge,
				fmt.Sprintf("failed to get ASR model: %v", err), "mark ASR model lookup failure"); markErr != nil {
				return markErr
			}
			return nil
		}

		transcriptionResult, err := asrModel.Transcribe(ctx, convertResult.AudioData, knowledge.FileName)
		if err != nil {
			logger.Errorf(ctx, "[ASR] Transcription failed: %v", err)
			if isLastRetry {
				if markErr := s.markActiveProcessingFailed(ctx, knowledge,
					fmt.Sprintf("audio transcription failed: %v", err), "mark audio transcription failure"); markErr != nil {
					return markErr
				}
			}
			return fmt.Errorf("audio transcription failed: %w", err)
		}

		var transcribedText string
		if transcriptionResult != nil {
			transcribedText = transcriptionResult.Text
		}

		if transcribedText == "" {
			logger.Warn(ctx, "[ASR] Transcription returned empty text")
			transcribedText = "[No speech detected in audio file]"
		}

		logger.Infof(ctx, "[ASR] Transcription completed, text length=%d", len(transcribedText))
		// Replace the audio placeholder with the transcribed text
		convertResult.MarkdownContent = transcribedText
		convertResult.IsAudio = false
		convertResult.AudioData = nil
	}

	// Step 2: Store images and update markdown references
	var storedImages []docparser.StoredImage

	if s.imageResolver != nil && convertResult != nil {
		fileSvc, storageErr := s.plannedAuxiliaryFileService(
			ctx, kb, knowledge, knowledgeaux.KindFanoutImage,
		)
		if storageErr != nil {
			return fmt.Errorf("prepare tracked extracted-image storage: %w", storageErr)
		}
		tenantID, _ := ctx.Value(types.TenantIDContextKey).(uint64)
		updatedMarkdown, images, resolveErr := s.imageResolver.ResolveAndStore(ctx, convertResult, fileSvc, tenantID)
		if resolveErr != nil {
			logger.Warnf(ctx, "Image resolution partially failed: %v", resolveErr)
		}
		if updatedMarkdown != "" {
			convertResult.MarkdownContent = updatedMarkdown
		}
		storedImages = images

		// Resolve remote http(s) images (e.g. markdown external URLs) → download + upload to storage.
		// ResolveAndStore handles inline bytes and base64; ResolveRemoteImages handles http/https URLs.
		updatedContent, remoteImages, remoteErr := s.imageResolver.ResolveRemoteImages(ctx, convertResult.MarkdownContent, fileSvc, tenantID)
		if remoteErr != nil {
			logger.Warnf(ctx, "Remote image resolution partially failed: %v", remoteErr)
		}
		if len(remoteImages) > 0 {
			logger.Infof(ctx, "Resolved %d remote images for knowledge %s", len(remoteImages), knowledge.ID)
			convertResult.MarkdownContent = updatedContent
			storedImages = append(storedImages, remoteImages...)
		}

		logger.Infof(ctx, "Resolved %d total images for knowledge %s", len(storedImages), knowledge.ID)
	}

	// Step 3: Split into chunks using Go chunker
	chunkCfg := buildSplitterConfigFromChunking(eff.ChunkingConfig)

	processOpts := ProcessChunksOptions{
		EnableQuestionGeneration: payload.EnableQuestionGeneration,
		QuestionCount:            payload.QuestionCount,
		EnableMultimodel:         payload.EnableMultimodel,
		StoredImages:             storedImages,
	}

	if convertResult != nil {
		processOpts.Metadata = convertResult.Metadata
	}

	if eff.ChunkingConfig.EnableParentChild {
		parentCfg, childCfg := buildParentChildConfigs(eff.ChunkingConfig, chunkCfg)
		pcResult := chunker.SplitParentChild(convertResult.MarkdownContent, parentCfg, childCfg)
		chunks = make([]types.ParsedChunk, len(pcResult.Children))
		for i, c := range pcResult.Children {
			chunks[i] = types.ParsedChunk{
				Content:       c.Content,
				ContextHeader: c.ContextHeader,
				Seq:           c.Seq,
				Start:         c.Start,
				End:           c.End,
				ParentIndex:   c.ParentIndex,
			}
		}
		parentChunks := make([]types.ParsedParentChunk, len(pcResult.Parents))
		for i, p := range pcResult.Parents {
			parentChunks[i] = types.ParsedParentChunk{Content: p.Content, Seq: p.Seq, Start: p.Start, End: p.End}
		}
		processOpts.ParentChunks = parentChunks
		logger.Infof(ctx, "Split document into %d parent + %d child chunks for knowledge %s",
			len(pcResult.Parents), len(pcResult.Children), knowledge.ID)
	} else {
		splitChunks := chunker.Split(convertResult.MarkdownContent, chunkCfg)
		chunks = make([]types.ParsedChunk, len(splitChunks))
		for i, c := range splitChunks {
			chunks[i] = types.ParsedChunk{
				Content:       c.Content,
				ContextHeader: c.ContextHeader,
				Seq:           c.Seq,
				Start:         c.Start,
				End:           c.End,
			}
		}
		logger.Infof(ctx, "Split document into %d chunks for knowledge %s", len(chunks), knowledge.ID)
	}

	// Step 4: Process chunks (vectorize + index + enqueue async tasks)
	return s.processChunks(ctx, kb, knowledge, chunks, processOpts)
}

// convert handles both file and URL reading using a unified ReadRequest.
func (s *knowledgeService) convert(
	ctx context.Context,
	payload types.DocumentProcessPayload,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	eff types.EffectiveProcessConfig,
	isLastRetry bool,
) (*types.ReadResult, error) {
	// Stage tracking: docreader. Mark the stage as running here so the
	// timeline reflects "DocReader" the moment a worker picks the task
	// up — before that, the stage stays "pending" from the initial
	// upload. Failure/skip transitions are emitted at the specific
	// failure points below; success is emitted at the bottom.
	docInput := types.JSONMap{
		"file_name": payload.FileName,
		"file_type": payload.FileType,
		"is_url":    payload.URL != "",
	}
	if payload.URL != "" {
		docInput["url"] = payload.URL
	}
	s.beginStage(ctx, knowledge.ID, types.StageDocReader, docInput)
	isURL := payload.URL != ""
	fileType := payload.FileType
	tenantOverrides := s.getParserEngineOverridesFromContext(ctx)
	var uploadOverrides map[string]string
	if processOverrides, err := knowledge.ProcessOverrides(); err == nil && processOverrides != nil {
		uploadOverrides = processOverrides.ParserEngineOverrides
	}
	mergedOverrides := MergeParserEngineOverrides(tenantOverrides, uploadOverrides)

	if isURL {
		if err := secutils.ValidateURLForSSRF(payload.URL); err != nil {
			logger.Errorf(ctx, "URL rejected for SSRF protection: %s, err: %v", payload.URL, err)
			if markErr := s.markActiveProcessingFailed(ctx, knowledge,
				"URL is not allowed for security reasons", "mark URL security failure"); markErr != nil {
				return nil, markErr
			}
			s.failStage(ctx, knowledge.ID, types.StageDocReader,
				werrors.ErrCodeDocReaderParseFailed, "URL rejected for security reasons", err)
			return nil, nil
		}
	}

	parserEngine := eff.ChunkingConfig.ResolveParserEngine(fileType)
	if isURL {
		parserEngine = eff.ChunkingConfig.ResolveParserEngine("url")
	}

	logger.Infof(ctx, "[convert] kb=%s fileType=%s isURL=%v engine=%q rules=%+v",
		kb.ID, fileType, isURL, parserEngine, eff.ChunkingConfig.ParserEngineRules)

	var reader interfaces.DocReader = s.resolveDocReader(ctx, parserEngine, fileType, isURL, mergedOverrides)
	if reader == nil {
		logger.Errorf(ctx, "[convert] no doc reader for kb=%s knowledge=%s fileType=%s engine=%q isURL=%v",
			kb.ID, knowledge.ID, fileType, parserEngine, isURL)
		message := "Document parsing service is not configured. Please use text/paragraph import or set DOCREADER_ADDR."
		if markErr := s.markActiveProcessingFailed(ctx, knowledge, message, "mark docreader unavailable"); markErr != nil {
			return nil, markErr
		}
		s.failStage(ctx, knowledge.ID, types.StageDocReader,
			werrors.ErrCodeDocReaderUnavailable, message, nil)
		return nil, nil
	}

	req := &types.ReadRequest{
		URL:                   payload.URL,
		Title:                 knowledge.Title,
		ParserEngine:          parserEngine,
		RequestID:             payload.RequestId,
		ParserEngineOverrides: mergedOverrides,
	}

	if !isURL {
		fileService, err := s.auxiliaryFileServiceForPath(
			ctx, kb, knowledge.KnowledgeBaseID, knowledge.ID, payload.FilePath,
		)
		if err != nil {
			s.failStage(ctx, knowledge.ID, types.StageDocReader,
				werrors.ErrCodeDocReaderParseFailed, "failed to resolve file storage", err)
			return s.failKnowledge(ctx, knowledge, isLastRetry, "failed to resolve file storage: %v", err)
		}
		fileReader, err := fileService.GetFile(ctx, payload.FilePath)
		if err != nil {
			s.failStage(ctx, knowledge.ID, types.StageDocReader,
				werrors.ErrCodeDocReaderParseFailed, "failed to get file", err)
			return s.failKnowledge(ctx, knowledge, isLastRetry, "failed to get file: %v", err)
		}
		defer fileReader.Close()
		contentBytes, err := io.ReadAll(fileReader)
		if err != nil {
			s.failStage(ctx, knowledge.ID, types.StageDocReader,
				werrors.ErrCodeDocReaderParseFailed, "failed to read file", err)
			return s.failKnowledge(ctx, knowledge, isLastRetry, "failed to read file: %v", err)
		}
		req.FileContent = contentBytes
		req.FileName = payload.FileName
		req.FileType = fileType
	}

	result, err := s.callDocReaderWithTimeout(ctx, reader, req)
	if err != nil {
		// Distinguish DocReader timeout (a knowable user-facing
		// failure) from generic read errors so the UI can suggest
		// "split this large file" specifically when relevant.
		code := werrors.ErrCodeDocReaderParseFailed
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "docreader call timeout") {
			code = werrors.ErrCodeDocReaderTimeout
		}
		s.failStage(ctx, knowledge.ID, types.StageDocReader,
			code, "document read failed", err)
		return s.failKnowledge(ctx, knowledge, isLastRetry, "document read failed: %v", err)
	}
	if result.Error != "" {
		logger.Errorf(ctx, "[convert] parser returned error kb=%s knowledge=%s file=%q type=%s engine=%q: %s",
			kb.ID, knowledge.ID, req.FileName, fileType, parserEngine, result.Error)
		if markErr := s.markActiveProcessingFailed(ctx, knowledge, result.Error, "mark docreader result failure"); markErr != nil {
			return nil, markErr
		}
		s.failStage(ctx, knowledge.ID, types.StageDocReader,
			werrors.ErrCodeDocReaderParseFailed, result.Error, nil)
		return nil, nil
	}
	docOutput := types.JSONMap{
		"text_length":  len(result.MarkdownContent),
		"images_found": len(result.ImageRefs),
		"is_audio":     result.IsAudio,
	}
	if pages := result.Metadata["pages"]; pages != "" {
		docOutput["pages"] = pages
	}
	s.endStage(ctx, knowledge.ID, types.StageDocReader, docOutput)
	return result, nil
}

// callDocReaderWithTimeout wraps the DocReader RPC in a child context whose
// deadline is min(parent_deadline, DocReaderCallTimeout). Without this cap,
// a hung docreader (network partition, GC pause, OCR runaway) silently
// burns the whole DocumentProcessTimeout budget and pins a worker for hours
// — the #1 cause of "knowledge stuck in processing" reports.
//
// On timeout we annotate the error so retries / dead-letter consumers can
// distinguish "docreader was slow" from "docreader returned an error".
func (s *knowledgeService) callDocReaderWithTimeout(
	ctx context.Context, reader interfaces.DocReader, req *types.ReadRequest,
) (*types.ReadResult, error) {
	timeout := 30 * time.Minute
	if s.config != nil && s.config.KnowledgeBase != nil && s.config.KnowledgeBase.DocReaderCallTimeout > 0 {
		timeout = s.config.KnowledgeBase.DocReaderCallTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	result, err := reader.Read(callCtx, req)
	elapsed := time.Since(start)
	if err != nil {
		// Promote DeadlineExceeded into a clearer message; retain underlying
		// error via %w so errors.Is(callCtx.Err(), context.DeadlineExceeded)
		// still works for upstream classification.
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			logger.Errorf(ctx, "[convert] docreader call timed out after %s (limit %s) for %q",
				elapsed, timeout, req.FileName)
			return nil, fmt.Errorf("docreader call timeout after %s: %w", timeout, err)
		}
		return nil, err
	}
	logger.Infof(ctx, "[convert] docreader call ok in %s for %q", elapsed, req.FileName)
	return result, nil
}

// isLikelyRateLimitError performs a fuzzy classification of an error as a
// rate-limit / quota / backpressure failure. We only need a hint — the
// caller maps to one of two error_codes so the UI can offer "retry later"
// vs. "fix configuration" advice. False positives are harmless (the
// detail is preserved in error_detail anyway).
func isLikelyRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"rate limit", "ratelimit", "429", "too many requests", "quota"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// Returns nil when the required service is unavailable.
func (s *knowledgeService) resolveDocReader(ctx context.Context, engine, fileType string, isURL bool, overrides map[string]string) interfaces.DocReader {
	switch engine {
	case docparser.SimpleEngineName:
		return &docparser.SimpleFormatReader{}
	case docparser.WeKnoraCloudEngineName:
		creds := s.tenantService.GetWeKnoraCloudCredentials(ctx)
		if creds == nil {
			logger.Warnf(ctx, "[resolveDocReader] WeKnoraCloud: no tenant credentials (fileType=%s)", fileType)
			return nil
		}
		reader, err := docparser.NewWeKnoraCloudSignedDocumentReader(creds.AppID, creds.AppSecret)
		if err != nil {
			logger.Errorf(ctx, "[resolveDocReader] WeKnoraCloud reader init failed: %v", err)
			return nil
		}
		return reader
	case "mineru":
		return docparser.NewMinerUReader(overrides)
	case "mineru_cloud":
		return docparser.NewMinerUCloudReader(overrides)
	case "paddleocr_vl":
		return docparser.NewPaddleOCRVLReader(overrides)
	case "paddleocr_vl_cloud":
		return docparser.NewPaddleOCRVLCloudReader(overrides)
	case "builtin":
		// 明确指定使用 builtin 引擎（docreader），不使用 simple format 兜底
		return s.documentReader
	default:
		// 未指定引擎时的兜底逻辑：simple format 使用 Go 原生处理，其他使用 docreader
		if !isURL && docparser.IsSimpleFormat(fileType) {
			return &docparser.SimpleFormatReader{}
		}
		return s.documentReader
	}
}

// failKnowledge marks knowledge as failed (only on last retry) and returns an error.
func (s *knowledgeService) failKnowledge(
	ctx context.Context,
	knowledge *types.Knowledge,
	isLastRetry bool,
	format string,
	args ...interface{},
) (*types.ReadResult, error) {
	errMsg := fmt.Sprintf(format, args...)
	if isLastRetry {
		if err := s.markActiveProcessingFailed(ctx, knowledge, errMsg, "mark document processing retry exhausted"); err != nil {
			return nil, errors.Join(fmt.Errorf(format, args...), err)
		}
	}
	return nil, fmt.Errorf(format, args...)
}

// ProcessKnowledgeListReparse handles Asynq knowledge list reparse tasks.
func (s *knowledgeService) ProcessKnowledgeListReparse(ctx context.Context, t *asynq.Task) error {
	var payload types.KnowledgeListReparsePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "Failed to unmarshal knowledge list reparse payload: %v", err)
		return err
	}

	logger.Infof(ctx, "Processing knowledge list reparse task for %d knowledge items", len(payload.KnowledgeIDs))
	if len(payload.KnowledgeIDs) > 1 {
		return dispatchKnowledgeListReparseChildren(ctx, s.repo, s.task, payload, t.Payload())
	}

	tenant, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get tenant %d: %v", payload.TenantID, err)
		return err
	}

	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)

	if len(payload.KnowledgeIDs) == 0 {
		return errors.New("knowledge list reparse: no knowledge IDs")
	}
	id := payload.KnowledgeIDs[0]
	reparse := s.ReparseKnowledge
	if payload.ProcessingGeneration != "" || payload.ProcessingOwner != "" {
		reparse = func(
			ctx context.Context,
			knowledgeID string,
			overrides *types.KnowledgeProcessOverrides,
		) (*types.Knowledge, error) {
			return s.reparseKnowledgeWithIdentity(
				ctx,
				knowledgeID,
				overrides,
				payload.ProcessingGeneration,
				payload.ProcessingOwner,
				payload.ExpectedSnapshot,
				&payload.TracingContext,
			)
		}
	}
	if _, err := reparse(ctx, id, payload.ProcessConfig); err != nil {
		logger.Errorf(ctx, "Failed to reparse knowledge %s: %v", id, err)
		// A single child owns exactly one knowledge ID, so returning its error
		// gives that item an independent retry/dead-letter lifecycle.
		return fmt.Errorf("reparse knowledge %s: %w", id, err)
	}
	logger.Infof(ctx, "Knowledge list reparse task finished: 1 submitted, 0 failed")
	return nil
}

func dispatchKnowledgeListReparseChildren(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	enqueuer interfaces.TaskEnqueuer,
	payload types.KnowledgeListReparsePayload,
	originalPayload []byte,
) error {
	if repo == nil {
		return errors.New("knowledge list reparse: knowledge repository is unavailable")
	}
	if enqueuer == nil {
		return errors.New("knowledge list reparse: task enqueuer is unavailable")
	}
	batchID := strings.TrimSpace(payload.BatchID)
	if batchID == "" {
		// Compatibility for an in-flight parent created before BatchID shipped.
		// Its immutable raw payload provides the same deterministic identity on
		// every retry.
		digest := sha256.Sum256(originalPayload)
		batchID = "legacy-" + hex.EncodeToString(digest[:16])
	}

	var enqueueErrors []error
	for _, knowledgeID := range payload.KnowledgeIDs {
		expected, ok := payload.ExpectedSnapshots[knowledgeID]
		if !ok {
			enqueueErrors = append(enqueueErrors,
				fmt.Errorf("publish reparse child %s: durable expected snapshot is missing", knowledgeID))
			continue
		}
		if err := processownership.ValidateBatchReparseSnapshot(expected); err != nil ||
			expected.TenantID != payload.TenantID || expected.KnowledgeID != knowledgeID {
			if err == nil {
				err = errors.New("snapshot identity does not match parent payload")
			}
			enqueueErrors = append(enqueueErrors,
				fmt.Errorf("publish reparse child %s: %w", knowledgeID, err))
			continue
		}
		current, err := repo.GetKnowledgeByID(ctx, payload.TenantID, knowledgeID)
		if err != nil {
			if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
				logger.Infof(ctx, "Batch reparse child became stale before publication: knowledge=%s missing", knowledgeID)
				continue
			}
			enqueueErrors = append(enqueueErrors,
				fmt.Errorf("reload reparse child %s before publication: %w", knowledgeID, err))
			continue
		}
		if !processownership.BatchReparseSnapshotMatches(current, expected) {
			logger.Infof(ctx, "Batch reparse child became stale before publication: knowledge=%s", knowledgeID)
			continue
		}

		child := payload
		child.BatchID = batchID
		child.KnowledgeIDs = []string{knowledgeID}
		child.ExpectedSnapshot = &expected
		child.ExpectedSnapshots = nil
		child.ProcessingGeneration, child.ProcessingOwner = processownership.BatchReparseIdentity(
			payload.TenantID, batchID, knowledgeID,
		)
		if child.ProcessingGeneration == "" || child.ProcessingOwner == "" {
			enqueueErrors = append(enqueueErrors,
				fmt.Errorf("derive reparse child identity %s: incomplete tenant, batch, or knowledge identity", knowledgeID))
			continue
		}
		childBytes, err := json.Marshal(child)
		if err != nil {
			enqueueErrors = append(enqueueErrors,
				fmt.Errorf("marshal reparse child %s: %w", knowledgeID, err))
			continue
		}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", payload.TenantID, batchID, knowledgeID)))
		taskID := "batch-reparse-item:" + hex.EncodeToString(digest[:16])
		childTask := asynq.NewTask(types.TypeKnowledgeListReparse, childBytes)
		_, err = enqueuer.Enqueue(
			childTask,
			asynq.Queue(types.QueueLow),
			asynq.MaxRetry(3),
			asynq.Retention(processownership.GenerationTaskRetention),
			asynq.TaskID(taskID),
		)
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			logger.Infof(ctx, "Batch reparse child already persisted: knowledge=%s task=%s", knowledgeID, taskID)
			continue
		}
		if err != nil {
			enqueueErrors = append(enqueueErrors,
				fmt.Errorf("enqueue reparse child %s: %w", knowledgeID, err))
		}
	}
	if len(enqueueErrors) > 0 {
		return errors.Join(enqueueErrors...)
	}
	logger.Infof(ctx, "Knowledge list reparse fan-out persisted: batch=%s children=%d", batchID, len(payload.KnowledgeIDs))
	return nil
}
