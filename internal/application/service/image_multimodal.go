package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/utils/ollama"
	"github.com/Tencent/WeKnora/internal/models/vlm"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const (
	vlmOCRPrompt = "<system_prompt>\n" +
		"You are an OCR assistant. Your task is to extract all body text content from this document image and output in pure Markdown format.\n" +
		"</system_prompt>\n\n" +
		"<instructions>\n" +
		"1. Ignore headers and footers.\n" +
		"2. Use Markdown table syntax for tables.\n" +
		"3. Use LaTeX format for formulas (wrapped with $ or $$).\n" +
		"4. Organize content in the original reading order.\n" +
		"5. Output ONLY the extracted text content. Do NOT include any HTML tags, reasoning, or unrelated comments.\n" +
		"6. If there is absolutely no recognizable text content in the image, reply ONLY with: No text content.\n" +
		"</instructions>"
	vlmOCRScannedPDFPrompt = "<system_prompt>\n" +
		"You are an OCR and document layout extraction assistant. The input image is a page from a scanned PDF document.\n" +
		"Your task is to carefully extract all text and layout structure from the image, and output the result in pure Markdown format.\n" +
		"</system_prompt>\n\n" +
		"<instructions>\n" +
		"1. Ignore headers, footers, and page numbers.\n" +
		"2. Preserve the original document's paragraph and hierarchical structure as much as possible.\n" +
		"3. If there are tables, use Markdown table syntax to represent them.\n" +
		"4. If there are mathematical formulas, use LaTeX format wrapped in $ or $$.\n" +
		"5. Output ONLY the extracted text content. Do NOT include any HTML tags, reasoning, or unrelated comments.\n" +
		"6. If there is absolutely no recognizable text content in the image, reply ONLY with: No text content.\n" +
		"</instructions>"
	vlmCaptionPrompt = "Provide a brief and concise description of the main content of the image in Chinese"
)

// ImageMultimodalService handles image:multimodal asynq tasks.
// It reads images from storage (via FileService for provider:// URLs),
// performs OCR and VLM caption, and creates child chunks.
type ImageMultimodalService struct {
	chunkService   interfaces.ChunkService
	modelService   interfaces.ModelService
	kbService      interfaces.KnowledgeBaseService
	knowledgeRepo  interfaces.KnowledgeRepository
	tenantRepo     interfaces.TenantRepository
	retrieveEngine interfaces.RetrieveEngineRegistry
	ownership      retriever.TenantStoreOwnership
	ollamaService  *ollama.OllamaService
	taskEnqueuer   interfaces.TaskEnqueuer
	redisClient    *redis.Client
	// fileSvc is the globally configured default FileService used as a fallback
	// when the tenant-scoped storage config cannot produce a usable service
	// (e.g. images were saved using the global MINIO_* env vars while the
	// tenant's StorageEngineConfig.MinIO is empty). Mirrors the write-side
	// fallback in knowledgeService.resolveFileService.
	fileSvc interfaces.FileService

	// spanTracker records this image's subspan under the parent attempt's
	// multimodal stage. nil-safe — falls back to no-op via tracker().
	spanTracker SpanTracker
}

func NewImageMultimodalService(
	chunkService interfaces.ChunkService,
	modelService interfaces.ModelService,
	kbService interfaces.KnowledgeBaseService,
	knowledgeRepo interfaces.KnowledgeRepository,
	tenantRepo interfaces.TenantRepository,
	retrieveEngine interfaces.RetrieveEngineRegistry,
	ownership retriever.TenantStoreOwnership,
	ollamaService *ollama.OllamaService,
	taskEnqueuer interfaces.TaskEnqueuer,
	redisClient *redis.Client,
	fileSvc interfaces.FileService,
	spanTracker SpanTracker,
) interfaces.TaskHandler {
	return &ImageMultimodalService{
		chunkService:   chunkService,
		modelService:   modelService,
		kbService:      kbService,
		knowledgeRepo:  knowledgeRepo,
		tenantRepo:     tenantRepo,
		retrieveEngine: retrieveEngine,
		ownership:      ownership,
		ollamaService:  ollamaService,
		taskEnqueuer:   taskEnqueuer,
		redisClient:    redisClient,
		fileSvc:        fileSvc,
		spanTracker:    spanTracker,
	}
}

// tracker returns a usable SpanTracker — falls back to a no-op when the
// service was constructed without one.
func (s *ImageMultimodalService) tracker() SpanTracker {
	if s.spanTracker == nil {
		return noopSpanTracker{}
	}
	return s.spanTracker
}

// Handle implements asynq handler for TypeImageMultimodal.
func (s *ImageMultimodalService) Handle(ctx context.Context, task *asynq.Task) (retErr error) {
	var payload types.ImageMultimodalPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal image multimodal payload: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.TenantID == 0 || payload.KnowledgeID == "" || payload.KnowledgeBaseID == "" {
		return errors.New("image multimodal: complete tenant, KB and knowledge identity is required")
	}
	if payload.ProcessingGeneration == "" {
		return processownership.RepairLegacyTask(
			ctx, s.knowledgeRepo, payload.TenantID, payload.KnowledgeID,
			payload.KnowledgeBaseID, "image multimodal",
		)
	}
	if payload.ChunkID == "" {
		return errors.New("image multimodal: chunk identity is required")
	}

	logger.Infof(ctx, "[ImageMultimodal] Processing image: chunk=%s, url=%s, ocr=%v, caption=%v",
		payload.ChunkID, payload.ImageURL, payload.EnableOCR, payload.EnableCaption)

	if payload.Language != "" {
		ctx = context.WithValue(ctx, types.LanguageContextKey, payload.Language)
	}

	knowledge, current, err := s.currentProcessingGeneration(ctx, payload)
	if err != nil {
		return fmt.Errorf("validate image multimodal generation: %w", err)
	}
	if !current {
		logger.Infof(ctx, "[ImageMultimodal] Stale tenant/KB/generation for %s, skipping image %s",
			payload.KnowledgeID, payload.ImageURL)
		return nil
	}
	parentChunk, err := s.chunkService.GetChunkByIDOnly(ctx, payload.ChunkID)
	if err != nil {
		return fmt.Errorf("load multimodal parent chunk: %w", err)
	}
	if parentChunk == nil ||
		parentChunk.TenantID != payload.TenantID ||
		parentChunk.KnowledgeID != payload.KnowledgeID ||
		parentChunk.KnowledgeBaseID != payload.KnowledgeBaseID ||
		(parentChunk.ProcessingGeneration != "" &&
			parentChunk.ProcessingGeneration != payload.ProcessingGeneration) {
		return errors.New("image multimodal: parent chunk identity mismatch")
	}
	plan, err := processownership.ParseFanoutPlan(knowledge.ProcessingFanout)
	if err != nil {
		return fmt.Errorf("load image durable fanout plan: %w", err)
	}
	completionStore, ok := s.knowledgeRepo.(processownership.DurableFanoutCompletionStore)
	if !ok || completionStore == nil {
		return errors.New("image multimodal: durable fanout completion repository is unavailable")
	}
	// If this stable task is retrying only because the last postprocess
	// enqueue failed, its SADD completion marker is already durable. Replay
	// that enqueue without paying for OCR/caption or creating chunks twice.
	item := processownership.ImageFanoutItem(payload.ImageIndex)
	done, remaining, err := processownership.DurableFanoutItemCompleted(
		ctx, completionStore, s.redisClient, plan, item,
	)
	if err != nil {
		return fmt.Errorf("read durable image fan-in completion: %w", err)
	}
	if done {
		if remaining <= 0 {
			if err := s.enqueueKnowledgePostProcessTask(ctx, payload); err != nil {
				return err
			}
			return processownership.ClearFanIn(ctx, s.redisClient, payload.KnowledgeID, payload.ProcessingGeneration)
		}
		return nil
	}

	// Open a per-image subspan under the parent attempt's multimodal
	// stage. If the parent stage row is missing (legacy in-flight
	// task, or the upstream code shipped without span tracking), the
	// tracker is a no-op so we silently fall back to the existing
	// counter-based finalize semantics.
	tracker := s.tracker()
	var imgSpan *Span
	if payload.Attempt > 0 {
		parent := tracker.LookupStage(ctx, payload.KnowledgeID, payload.Attempt, types.StageMultimodal)
		if parent != nil {
			name := fmt.Sprintf("multimodal.image[%d]", payload.ImageIndex)
			imgSpan = tracker.BeginSubSpan(ctx, parent, name, types.SpanKindGeneration, types.JSONMap{
				"image_url":         payload.ImageURL,
				"image_source_type": payload.ImageSourceType,
				"enable_ocr":        payload.EnableOCR,
				"enable_caption":    payload.EnableCaption,
				"parent_chunk_id":   payload.ChunkID,
			})
		}
	}

	// Output map populated as we go — the deferred close picks it up.
	// Captures real VLM results (model id, byte count, OCR/caption
	// previews, downstream chunk counts) so the trace viewer can answer
	// "what did this image actually produce?" without joining back to
	// the chunks table.
	imgOut := types.JSONMap{}

	// finalize-once semantics: on success we always decrement the parent's
	// pending counter. On failure we only decrement when this is the last
	// asynq retry, so a permanently-failing single image cannot leave the
	// parent knowledge stuck in "processing" forever — which was the #1
	// cause of "stuck parsing" reports. Intermediate retries skip finalize
	// so we don't double-count and prematurely trigger post-process.
	var handleErr error
	skipFanIn := false
	defer func() {
		// Finalize the image subspan with the actual outcome — not the
		// finalize-counter outcome. The counter logic counts a "tried"
		// image regardless of inner success; the span surface tells the
		// UI whether THIS specific image worked.
		if imgSpan != nil {
			if handleErr == nil {
				tracker.EndSpan(ctx, imgSpan, imgOut)
			} else if isFinalAsynqAttempt(ctx) {
				tracker.FailSpan(ctx, imgSpan,
					"MULTIMODAL_VLM_FAILED",
					handleErr.Error(),
					handleErr)
			}
		}
		if !skipFanIn && (handleErr == nil || isFinalAsynqAttempt(ctx)) {
			if err := s.checkAndFinalizeAllImages(ctx, payload, plan, completionStore); err != nil {
				retErr = errors.Join(retErr, err)
			}
		} else {
			logger.Infof(ctx,
				"[ImageMultimodal] Skip finalize on retryable error for %s (will count on last attempt)",
				payload.ImageURL)
		}
	}()

	vlmModel, vlmCfg, err := s.resolveVLM(ctx, payload.KnowledgeBaseID, payload.KnowledgeID)
	if err != nil {
		handleErr = fmt.Errorf("resolve VLM: %w", err)
		return handleErr
	}
	// Capture the resolved VLM model id (or "legacy_inline" for the
	// legacy inline-config path) so the trace shows WHICH model handled
	// this image. Without this, debugging "VLM is slow" requires a
	// separate hop to the KB config.
	if id := strings.TrimSpace(vlmCfg.ModelID); id != "" {
		imgOut["vlm_model_id"] = id
	} else {
		imgOut["vlm_model_id"] = "legacy_inline"
	}

	// Read image bytes. A provider:// URL must be resolved via FileService —
	// it must NEVER be handed to the HTTP downloader (which would fail with
	// "unsupported URL scheme"). On unrecoverable read failure for a single
	// image, skip it (deferred finalize will count it).
	imgBytes, readErr := s.readImageBytes(ctx, payload)
	if readErr != nil {
		logger.Errorf(ctx, "[ImageMultimodal] Skip unreadable image %s: %v", payload.ImageURL, readErr)
		imgOut["skipped"] = "unreadable_image"
		imgOut["read_error"] = readErr.Error()
		return nil
	}
	imgOut["image_bytes"] = len(imgBytes)

	imageInfo := types.ImageInfo{
		URL:         payload.ImageURL,
		OriginalURL: payload.ImageURL,
	}

	if payload.EnableOCR {
		prompt := vlmOCRPrompt
		if payload.ImageSourceType == "scanned_pdf" {
			prompt = vlmOCRScannedPDFPrompt
			logger.Infof(ctx, "[ImageMultimodal] Using scanned PDF prompt for OCR: %s", payload.ImageURL)
			imgOut["ocr_prompt"] = "scanned_pdf"
		} else {
			imgOut["ocr_prompt"] = "default"
		}

		ocrText, ocrErr := vlmModel.Predict(ctx, [][]byte{imgBytes}, prompt)
		if ocrErr != nil {
			logger.Warnf(ctx, "[ImageMultimodal] OCR failed for %s: %v", payload.ImageURL, ocrErr)
			imgOut["ocr_error"] = ocrErr.Error()
		} else {
			ocrText = sanitizeOCRText(ocrText)
			if ocrText != "" {
				imageInfo.OCRText = ocrText
				imgOut["ocr_chars"] = len([]rune(ocrText))
				imgOut["ocr_preview"] = previewText(ocrText, 200)
			} else {
				logger.Warnf(ctx, "[ImageMultimodal] OCR returned empty/invalid content for %s, discarded", payload.ImageURL)
				imgOut["ocr_chars"] = 0
				imgOut["ocr_skipped"] = "empty_or_invalid"
			}
		}
	}

	caption, capErr := vlmModel.Predict(ctx, [][]byte{imgBytes}, vlmCaptionPrompt)
	if capErr != nil {
		logger.Warnf(ctx, "[ImageMultimodal] Caption failed for %s: %v", payload.ImageURL, capErr)
		imgOut["caption_error"] = capErr.Error()
	} else if caption != "" {
		imageInfo.Caption = caption
		imgOut["caption_chars"] = len([]rune(caption))
		imgOut["caption_preview"] = previewText(caption, 200)
	}

	// Build child chunks for OCR and caption results
	imageInfoJSON, _ := json.Marshal([]types.ImageInfo{imageInfo})
	var newChunks []*types.Chunk

	if imageInfo.OCRText != "" {
		newChunks = append(newChunks, &types.Chunk{
			ID:                   stableImageChunkID(payload, "ocr"),
			TenantID:             payload.TenantID,
			KnowledgeID:          payload.KnowledgeID,
			KnowledgeBaseID:      payload.KnowledgeBaseID,
			Content:              imageInfo.OCRText,
			ChunkType:            types.ChunkTypeImageOCR,
			ParentChunkID:        payload.ChunkID,
			ChunkIndex:           parentChunk.ChunkIndex,
			StartAt:              parentChunk.StartAt,
			EndAt:                parentChunk.EndAt,
			IsEnabled:            true,
			Flags:                types.ChunkFlagRecommended,
			ImageInfo:            string(imageInfoJSON),
			ProcessingGeneration: payload.ProcessingGeneration,
			SplitPartIndex:       parentChunk.SplitPartIndex,
			SourceLocator:        append(types.JSON(nil), parentChunk.SourceLocator...),
			CreatedAt:            time.Now(),
			UpdatedAt:            time.Now(),
		})
	}

	if imageInfo.Caption != "" {
		newChunks = append(newChunks, &types.Chunk{
			ID:                   stableImageChunkID(payload, "caption"),
			TenantID:             payload.TenantID,
			KnowledgeID:          payload.KnowledgeID,
			KnowledgeBaseID:      payload.KnowledgeBaseID,
			Content:              imageInfo.Caption,
			ChunkType:            types.ChunkTypeImageCaption,
			ParentChunkID:        payload.ChunkID,
			ChunkIndex:           parentChunk.ChunkIndex,
			StartAt:              parentChunk.StartAt,
			EndAt:                parentChunk.EndAt,
			IsEnabled:            true,
			Flags:                types.ChunkFlagRecommended,
			ImageInfo:            string(imageInfoJSON),
			ProcessingGeneration: payload.ProcessingGeneration,
			SplitPartIndex:       parentChunk.SplitPartIndex,
			SourceLocator:        append(types.JSON(nil), parentChunk.SourceLocator...),
			CreatedAt:            time.Now(),
			UpdatedAt:            time.Now(),
		})
	}
	imgOut["chunks_created"] = len(newChunks)

	if len(newChunks) == 0 {
		// Deferred finalize will count this image on success.
		imgOut["skipped"] = "no_extracted_content"
		return nil
	}

	// VLM calls can run for minutes. Re-read the exact identity immediately
	// before the first durable chunk/index write so a reparse/cancel that won
	// while the model was running cannot inject stale chunks into the new run.
	_, current, err = s.currentProcessingGeneration(ctx, payload)
	if err != nil {
		handleErr = fmt.Errorf("revalidate image multimodal generation: %w", err)
		return handleErr
	}
	if !current {
		skipFanIn = true
		imgOut["skipped"] = "stale_processing_generation"
		logger.Infof(ctx, "[ImageMultimodal] Generation changed before chunk write for %s, skipping", payload.KnowledgeID)
		return nil
	}

	// Persist chunks
	if err := upsertGenerationChunks(ctx, s.chunkService, newChunks); err != nil {
		handleErr = fmt.Errorf("upsert multimodal chunks: %w", err)
		return handleErr
	}
	for _, c := range newChunks {
		logger.Infof(ctx, "[ImageMultimodal] Created %s chunk %s for image %s, len=%d",
			c.ChunkType, c.ID, payload.ImageURL, len(c.Content))
	}

	// Index chunks so they can be retrieved
	if err := s.indexChunks(ctx, payload, newChunks); err != nil {
		handleErr = err
		return handleErr
	}
	imgOut["indexed"] = true

	// Enqueue question generation for the caption/OCR content if KB has it enabled.
	// During initial processChunks, question generation is skipped for image-type
	// knowledge because the text chunk is just a markdown reference. Now that we
	// have real textual content (caption/OCR), we can generate questions.
	// Note: for documents with multiple images (e.g. PDFs), we also wait until
	// all images are processed before triggering summary/question generation.
	// Deferred finalize handles the parent knowledge counter.
	return nil
}

func stableImageChunkID(payload types.ImageMultimodalPayload, kind string) string {
	name := fmt.Sprintf(
		"%d:%s:%s:%s:%d:%s",
		payload.TenantID,
		payload.KnowledgeBaseID,
		payload.KnowledgeID,
		payload.ProcessingGeneration,
		payload.ImageIndex,
		kind,
	)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

func (s *ImageMultimodalService) currentProcessingGeneration(
	ctx context.Context,
	payload types.ImageMultimodalPayload,
) (*types.Knowledge, bool, error) {
	if s.knowledgeRepo == nil {
		return nil, false, errors.New("knowledge repository is unavailable")
	}
	knowledge, err := s.knowledgeRepo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if err != nil {
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	current := knowledge != nil &&
		knowledge.KnowledgeBaseID == payload.KnowledgeBaseID &&
		knowledge.ProcessingGeneration == payload.ProcessingGeneration &&
		knowledge.ParseStatus == types.ParseStatusProcessing
	return knowledge, current, nil
}

// isFinalAsynqAttempt reports whether the current task context belongs to the
// last retry attempt before asynq archives the task as a dead-letter. We use
// this to flip multimodal finalize semantics: during normal retries we skip
// counter decrement (the retry might still succeed), but on the final attempt
// we count the image regardless of outcome so a permanently-failing image
// cannot pin the parent knowledge in "processing" forever.
//
// Returns false when the values are unavailable (e.g. when the handler is
// invoked outside an asynq worker, as in unit tests). Treating that case as
// "not final" keeps test ergonomics — tests should drive finalize explicitly.
func isFinalAsynqAttempt(ctx context.Context) bool {
	retried, maxRetry, ok := taskRetryMetadata(ctx)
	if !ok {
		return false
	}
	return retried >= maxRetry
}

// indexChunks indexes the newly created multimodal chunks into the retrieval engine
// so they can participate in semantic search.
func (s *ImageMultimodalService) indexChunks(
	ctx context.Context,
	payload types.ImageMultimodalPayload,
	chunks []*types.Chunk,
) error {
	kb, err := s.kbService.GetKnowledgeBaseByIDOnly(ctx, payload.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("get KB for multimodal indexing: %w", err)
	}
	if kb == nil || kb.ID != payload.KnowledgeBaseID || kb.TenantID != payload.TenantID {
		return errors.New("get KB for multimodal indexing: identity mismatch")
	}

	// Skip vector/keyword indexing when the KB has no embedding-based pipeline enabled
	// (e.g. Wiki-only KBs). Without this check, GetEmbeddingModel would fail because
	// EmbeddingModelID is intentionally empty for such KBs. The multimodal chunks
	// themselves are already persisted in the DB above, so skipping index here is safe.
	if !kb.NeedsEmbeddingModel() {
		logger.Infof(ctx, "[ImageMultimodal] Vector/keyword indexing disabled for KB %s, skipping index for %d multimodal chunks",
			kb.ID, len(chunks))
		// Still mark chunks as indexed so downstream finalization sees a consistent state.
		var statusErr error
		for _, chunk := range chunks {
			dbChunk, gerr := s.chunkService.GetChunkByIDOnly(ctx, chunk.ID)
			if gerr != nil {
				statusErr = errors.Join(statusErr, fmt.Errorf("fetch chunk %s for status update: %w", chunk.ID, gerr))
				continue
			}
			dbChunk.Status = int(types.ChunkStatusIndexed)
			if uerr := s.chunkService.UpdateChunk(ctx, dbChunk); uerr != nil {
				statusErr = errors.Join(statusErr, fmt.Errorf("mark chunk %s indexed: %w", chunk.ID, uerr))
			}
		}
		return statusErr
	}

	embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		return fmt.Errorf("get embedding model for multimodal indexing: %w", err)
	}

	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		return fmt.Errorf("get tenant for multimodal indexing: %w", err)
	}
	// The factory's unbound path reads TenantInfo from ctx; make sure it's there.
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	// Resolve engine via the factory using the KB's VectorStore binding
	// (nil → tenant effective engines fallback; verified tenant ownership otherwise).
	engine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, payload.TenantID, kb.VectorStoreID)
	if err != nil {
		return fmt.Errorf("initialize multimodal retrieve engine: %w", err)
	}

	indexInfoList := make([]*types.IndexInfo, 0, len(chunks))
	for _, chunk := range chunks {
		indexInfoList = append(indexInfoList, &types.IndexInfo{
			Content:         chunk.Content,
			SourceID:        chunk.ID,
			SourceType:      types.ChunkSourceType,
			ChunkID:         chunk.ID,
			KnowledgeID:     chunk.KnowledgeID,
			KnowledgeBaseID: chunk.KnowledgeBaseID,
		})
	}

	if err := engine.BatchIndex(ctx, embeddingModel, indexInfoList); err != nil {
		return fmt.Errorf("batch index multimodal chunks: %w", err)
	}

	// Mark chunks as indexed.
	// Must re-fetch from DB because the in-memory objects lack auto-generated fields
	// (e.g. seq_id), and GORM Save would overwrite them with zero values.
	var statusErr error
	for _, chunk := range chunks {
		dbChunk, err := s.chunkService.GetChunkByIDOnly(ctx, chunk.ID)
		if err != nil {
			statusErr = errors.Join(statusErr, fmt.Errorf("fetch indexed chunk %s: %w", chunk.ID, err))
			continue
		}
		dbChunk.Status = int(types.ChunkStatusIndexed)
		if err := s.chunkService.UpdateChunk(ctx, dbChunk); err != nil {
			statusErr = errors.Join(statusErr, fmt.Errorf("mark indexed chunk %s: %w", chunk.ID, err))
		}
	}
	if statusErr != nil {
		return statusErr
	}

	logger.Infof(ctx, "[ImageMultimodal] Indexed %d multimodal chunks for image %s", len(chunks), payload.ImageURL)
	return nil
}

// resolveVLM creates a vlm.VLM instance for the given knowledge base,
// supporting both new-style (ModelID) and legacy (inline BaseURL) configs.
// Per-upload process_overrides on the knowledge entry take precedence over KB defaults.
func (s *ImageMultimodalService) resolveVLM(ctx context.Context, kbID, knowledgeID string) (vlm.VLM, types.VLMConfig, error) {
	kb, err := s.kbService.GetKnowledgeBaseByIDOnly(ctx, kbID)
	if err != nil {
		return nil, types.VLMConfig{}, fmt.Errorf("get knowledge base %s: %w", kbID, err)
	}
	if kb == nil {
		return nil, types.VLMConfig{}, fmt.Errorf("knowledge base %s not found", kbID)
	}

	var processOverrides *types.KnowledgeProcessOverrides
	if knowledgeID != "" && s.knowledgeRepo != nil {
		if k, kerr := s.knowledgeRepo.GetKnowledgeByIDOnly(ctx, knowledgeID); kerr == nil && k != nil {
			processOverrides, _ = k.ProcessOverrides()
		}
	}
	vlmCfg := ResolveProcessConfig(kb, processOverrides).VLMConfig
	if !vlmCfg.IsEnabled() {
		return nil, types.VLMConfig{}, fmt.Errorf("VLM is not enabled for knowledge base %s", kbID)
	}

	// New-style: resolve model through ModelService
	if vlmCfg.ModelID != "" {
		model, err := s.modelService.GetVLMModel(ctx, vlmCfg.ModelID)
		return model, vlmCfg, err
	}

	// Legacy: create VLM from inline config
	model, err := vlm.NewVLMFromLegacyConfig(vlmCfg, s.ollamaService)
	return model, vlmCfg, err
}

// resolveFileServiceForPayload resolves tenant/KB scoped file service for reading provider:// URLs.
// Falls back to the globally configured default FileService when the tenant's
// StorageEngineConfig does not carry a usable configuration for the URL's provider.
// This mirrors the write-side fallback in knowledgeService.resolveFileService
// and is required because images can be saved using global STORAGE_TYPE/MINIO_*
// env vars while tenant.StorageEngineConfig.MinIO is left empty (issue #1282).
func (s *ImageMultimodalService) resolveFileServiceForPayload(ctx context.Context, payload types.ImageMultimodalPayload) interfaces.FileService {
	tenant, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil || tenant == nil {
		logger.Warnf(ctx, "[ImageMultimodal] GetTenantByID failed: tenant=%d err=%v", payload.TenantID, err)
		return s.fileSvc
	}

	provider := types.ParseProviderScheme(payload.ImageURL)
	if provider == "" {
		kb, kbErr := s.kbService.GetKnowledgeBaseByIDOnly(ctx, payload.KnowledgeBaseID)
		if kbErr != nil {
			logger.Warnf(ctx, "[ImageMultimodal] GetKnowledgeBaseByIDOnly failed: kb=%s err=%v", payload.KnowledgeBaseID, kbErr)
		} else if kb != nil {
			provider = strings.ToLower(strings.TrimSpace(kb.GetStorageProvider()))
		}
	}

	baseDir := strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR"))
	fileSvc, _, svcErr := filesvc.NewFileServiceFromStorageConfig(provider, tenant.StorageEngineConfig, baseDir)
	if svcErr != nil {
		logger.Warnf(ctx, "[ImageMultimodal] resolve file service failed (falling back to default): tenant=%d provider=%s err=%v",
			payload.TenantID, provider, svcErr)
		return s.fileSvc
	}
	return fileSvc
}

// readImageBytes loads the image bytes for a multimodal payload.
//   - For provider:// URLs (local://, minio://, s3://, cos://, ...) it reads via
//     the resolved FileService and NEVER falls back to HTTP — handing a
//     provider:// URL to the HTTP downloader is what caused issue #1282.
//   - For legacy in-flight payloads with ImageLocalPath set, it tries the local
//     file before falling back to the URL.
//   - For plain http(s):// URLs it uses the SSRF-safe downloader.
func (s *ImageMultimodalService) readImageBytes(ctx context.Context, payload types.ImageMultimodalPayload) ([]byte, error) {
	if types.ParseProviderScheme(payload.ImageURL) != "" {
		fileSvc := s.resolveFileServiceForPayload(ctx, payload)
		if fileSvc == nil {
			return nil, fmt.Errorf("no file service available for %s", payload.ImageURL)
		}
		reader, err := fileSvc.GetFile(ctx, payload.ImageURL)
		if err != nil {
			return nil, fmt.Errorf("file service get %s: %w", payload.ImageURL, err)
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", payload.ImageURL, err)
		}
		return data, nil
	}

	if payload.ImageLocalPath != "" {
		if data, err := os.ReadFile(payload.ImageLocalPath); err == nil {
			return data, nil
		} else {
			logger.Warnf(ctx, "[ImageMultimodal] Local file %s not available (%v), falling back to URL", payload.ImageLocalPath, err)
		}
	}

	data, err := downloadImageFromURL(payload.ImageURL)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", payload.ImageURL, err)
	}
	logger.Infof(ctx, "[ImageMultimodal] Image downloaded from URL, len=%d", len(data))
	return data, nil
}

// downloadImageFromURL downloads image bytes from an HTTP(S) URL.
func downloadImageFromURL(imageURL string) ([]byte, error) {
	return secutils.DownloadBytes(imageURL)
}

func (s *ImageMultimodalService) checkAndFinalizeAllImages(
	ctx context.Context,
	payload types.ImageMultimodalPayload,
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
		processownership.ImageFanoutItem(payload.ImageIndex),
	)
	if err != nil {
		return fmt.Errorf("complete image fan-in item: %w", err)
	}

	if remaining <= 0 {
		logger.Infof(ctx, "[ImageMultimodal] All images processed for knowledge %s. Finalizing...", payload.KnowledgeID)
		if err := s.enqueueKnowledgePostProcessTask(completionCtx, payload); err != nil {
			// Keep both the total and SADD completion set. The Asynq retry sees
			// this item's durable marker and replays only the stable postprocess
			// enqueue instead of repeating OCR/chunk writes.
			return fmt.Errorf("enqueue postprocess after image fan-in: %w", err)
		}
		if err := processownership.ClearFanIn(
			completionCtx, s.redisClient, payload.KnowledgeID, payload.ProcessingGeneration,
		); err != nil {
			logger.Warnf(ctx, "[ImageMultimodal] Failed to clear completed fan-in keys for %s: %v",
				payload.KnowledgeID, err)
		}
	}
	return nil
}

func (s *ImageMultimodalService) enqueueKnowledgePostProcessTask(
	ctx context.Context,
	payload types.ImageMultimodalPayload,
) error {
	if s.taskEnqueuer == nil {
		return errors.New("image multimodal: postprocess task enqueuer is unavailable")
	}

	taskPayload := types.KnowledgePostProcessPayload{
		TracingContext:       payload.TracingContext,
		TenantID:             payload.TenantID,
		KnowledgeID:          payload.KnowledgeID,
		KnowledgeBaseID:      payload.KnowledgeBaseID,
		ProcessingGeneration: payload.ProcessingGeneration,
		Language:             payload.Language,
		Attempt:              payload.Attempt,
	}
	if err := processownership.EnqueuePostProcess(s.taskEnqueuer, taskPayload); err != nil {
		return err
	}
	logger.Infof(ctx, "[ImageMultimodal] Enqueued/replayed post process task for %s generation %s",
		payload.KnowledgeID, payload.ProcessingGeneration)
	return nil
}
