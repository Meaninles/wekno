package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"gorm.io/gorm"
)

// copyOwnedObject performs a real copy of srcPath into a NEW object owned by
// (tenantID, knowledgeID) using the destination FileService, returning the new
// provider:// path. The same-backend check lives inside dstSvc.CopyFile, which
// returns file.ErrCrossBackendCopy when srcPath belongs to a different provider;
// that error is propagated unchanged so callers can fail the clone explicitly.
// srcSvc is accepted for symmetry with the read side but is not used directly:
// server-side copies are issued by the destination service.
func copyOwnedObject(
	ctx context.Context,
	srcSvc, dstSvc interfaces.FileService,
	srcPath string,
	tenantID uint64,
	knowledgeID string,
) (string, error) {
	_ = srcSvc // reserved for future cross-backend streaming fallback
	return dstSvc.CopyFile(ctx, srcPath, tenantID, knowledgeID)
}

// cloneChunkImageInfo parses a chunk's image_info JSON, copies every referenced
// object into a NEW object owned by (tenantID, knowledgeID), and returns the
// re-serialized image_info plus the list of newly-created object URLs (for
// rollback on failure). urlCache dedups identical source objects across chunks
// so the same source image is copied at most once per clone.
//
// An empty srcImageInfo yields ("", nil, nil). A JSON parse failure returns an
// error (the clone fails) rather than silently inheriting the shared-reference
// bug. When an image's OriginalURL points at the same object as its URL (the
// common case for extracted images), OriginalURL is rewritten to the new path
// too; an OriginalURL from a different/external source is preserved.
func cloneChunkImageInfo(
	ctx context.Context,
	dstSvc interfaces.FileService,
	srcImageInfo string,
	tenantID uint64,
	knowledgeID string,
	urlCache map[string]string,
) (newImageInfo string, copiedURLs []string, err error) {
	return cloneChunkImageInfoWithCopier(srcImageInfo, urlCache, func(path string) (string, error) {
		return copyOwnedObject(ctx, dstSvc, dstSvc, path, tenantID, knowledgeID)
	})
}

func cloneChunkImageInfoWithCopier(
	srcImageInfo string,
	urlCache map[string]string,
	copyObject func(string) (string, error),
) (newImageInfo string, copiedURLs []string, err error) {
	if srcImageInfo == "" {
		return "", nil, nil
	}

	var images []*types.ImageInfo
	if err := json.Unmarshal([]byte(srcImageInfo), &images); err != nil {
		return "", nil, fmt.Errorf("failed to parse chunk image_info JSON: %w", err)
	}

	for _, img := range images {
		if img == nil || img.URL == "" {
			continue
		}
		originalMatchedURL := img.OriginalURL == img.URL

		newURL, cached := urlCache[img.URL]
		if !cached {
			newURL, err = copyObject(img.URL)
			if err != nil {
				return "", copiedURLs, fmt.Errorf("failed to copy chunk image %q: %w", img.URL, err)
			}
			urlCache[img.URL] = newURL
			copiedURLs = append(copiedURLs, newURL)
		}

		if originalMatchedURL {
			img.OriginalURL = newURL
		}
		img.URL = newURL
	}

	out, err := json.Marshal(images)
	if err != nil {
		return "", copiedURLs, fmt.Errorf("failed to re-serialize chunk image_info: %w", err)
	}
	return string(out), copiedURLs, nil
}

func (s *knowledgeService) cloneChunkImageInfoBound(
	ctx context.Context,
	srcKB *types.KnowledgeBase,
	srcKnowledge *types.Knowledge,
	dstSvc interfaces.FileService,
	srcImageInfo string,
	tenantID uint64,
	knowledgeID string,
	urlCache map[string]string,
) (string, []string, error) {
	tracked, ok := dstSvc.(*auxiliaryPlannedFileService)
	if !ok || tracked == nil || srcKB == nil || srcKnowledge == nil {
		return "", nil, fmt.Errorf("bound chunk image copy dependencies are unavailable")
	}
	return cloneChunkImageInfoWithCopier(srcImageInfo, urlCache, func(sourcePath string) (string, error) {
		sourceService, err := s.auxiliaryFileServiceForPath(
			ctx, srcKB, srcKnowledge.KnowledgeBaseID, srcKnowledge.ID, sourcePath,
		)
		if err != nil {
			return "", fmt.Errorf("resolve source image binding: %w", err)
		}
		return tracked.copyFromBoundService(ctx, sourceService, sourcePath, tenantID, knowledgeID, 0)
	})
}

func (s *knowledgeService) CloneKnowledgeBase(ctx context.Context, srcID, dstID string) error {
	srcKB, dstKB, err := s.kbService.CopyKnowledgeBase(ctx, srcID, dstID)
	if err != nil {
		logger.Errorf(ctx, "Failed to copy knowledge base: %v", err)
		return err
	}

	addKnowledge, err := s.repo.AminusB(ctx, srcKB.TenantID, srcKB.ID, dstKB.TenantID, dstKB.ID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge: %v", err)
		return err
	}

	delKnowledge, err := s.repo.AminusB(ctx, dstKB.TenantID, dstKB.ID, srcKB.TenantID, srcKB.ID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge: %v", err)
		return err
	}
	logger.Infof(ctx, "Knowledge after update to add: %d, delete: %d", len(addKnowledge), len(delKnowledge))

	batch := 10
	g, gctx := errgroup.WithContext(ctx)
	for ids := range slices.Chunk(delKnowledge, batch) {
		g.Go(func() error {
			err := s.DeleteKnowledgeList(gctx, ids)
			if err != nil {
				logger.Errorf(gctx, "delete partial knowledge %v: %v", ids, err)
				return err
			}
			return nil
		})
	}
	err = g.Wait()
	if err != nil {
		logger.Errorf(ctx, "delete total knowledge %d: %v", len(delKnowledge), err)
		return err
	}

	// Copy context out of auto-stop task
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(batch)
	for _, knowledge := range addKnowledge {
		g.Go(func() error {
			srcKn, err := s.repo.GetKnowledgeByID(gctx, srcKB.TenantID, knowledge)
			if err != nil {
				logger.Errorf(gctx, "get knowledge %s: %v", knowledge, err)
				return err
			}
			err = s.cloneKnowledge(gctx, srcKn, dstKB)
			if err != nil {
				logger.Errorf(gctx, "clone knowledge %s: %v", knowledge, err)
				return err
			}
			return nil
		})
	}
	err = g.Wait()
	if err != nil {
		logger.Errorf(ctx, "add total knowledge %d: %v", len(addKnowledge), err)
		return err
	}
	return nil
}

// CloneChunk clone chunks from one knowledge to another
// This method transfers a chunk from a source knowledge document to a target knowledge document
// It handles the creation of new chunks in the target knowledge and updates the vector database accordingly
// Parameters:
//   - ctx: Context with authentication and request information
//   - src: Source knowledge document containing the chunk to move
//   - dst: Target knowledge document where the chunk will be moved
//
// Returns:
//   - error: Any error encountered during the move operation
//
// This method handles the chunk transfer logic, including creating new chunks in the target knowledge
// and updating the vector database representation of the moved chunks.
// It also ensures that the chunk's relationships (like pre and next chunk IDs) are maintained
// by mapping the source chunk IDs to the new target chunk IDs.
func (s *knowledgeService) CloneChunk(ctx context.Context, src, dst *types.Knowledge) (err error) {
	chunkPage := 1
	chunkPageSize := 100
	srcTodst := map[string]string{}
	tagIDMapping := map[string]string{} // srcTagID -> dstTagID
	targetChunks := make([]*types.Chunk, 0, 10)
	chunkType := []types.ChunkType{
		types.ChunkTypeText, types.ChunkTypeParentText, types.ChunkTypeSummary,
		types.ChunkTypeImageCaption, types.ChunkTypeImageOCR,
	}

	// Resolve the destination FileService so extracted images can be copied
	// into objects owned by the destination knowledge. urlCache dedups identical
	// source images across chunks; copiedURLs accumulates new objects so they can
	// be cleaned up if the clone fails partway through.
	srcKB, srcKBErr := s.kbService.GetKnowledgeBaseByID(ctx, src.KnowledgeBaseID)
	if srcKBErr != nil {
		return fmt.Errorf("failed to load source knowledge base for image copy: %w", srcKBErr)
	}
	dstKB, dstKBErr := s.kbService.GetKnowledgeBaseByID(ctx, dst.KnowledgeBaseID)
	if dstKBErr != nil {
		return fmt.Errorf("failed to load destination knowledge base for image copy: %w", dstKBErr)
	}
	dstSvc, dstSvcErr := s.plannedAuxiliaryFileService(ctx, dstKB, dst, knowledgeaux.KindCloneImage)
	if dstSvcErr != nil {
		return fmt.Errorf("prepare tracked destination image storage: %w", dstSvcErr)
	}
	urlCache := map[string]string{}

	for {
		sourceChunks, _, err := s.chunkRepo.ListPagedChunksByKnowledgeID(ctx,
			src.TenantID,
			src.ID,
			&types.Pagination{
				Page:     chunkPage,
				PageSize: chunkPageSize,
			},
			chunkType,
			"",
			"",
			"",
			"",
			"",
		)
		chunkPage++
		if err != nil {
			return err
		}
		if len(sourceChunks) == 0 {
			break
		}
		now := time.Now()
		for _, sourceChunk := range sourceChunks {
			// Map TagID to target knowledge base
			targetTagID := ""
			if sourceChunk.TagID != "" {
				if mappedTagID, ok := tagIDMapping[sourceChunk.TagID]; ok {
					targetTagID = mappedTagID
				} else {
					// Try to find or create the tag in target knowledge base
					targetTagID = s.getOrCreateTagInTarget(ctx, src.TenantID, dst.TenantID, dst.KnowledgeBaseID, sourceChunk.TagID, tagIDMapping)
				}
			}

			// Deep-copy extracted images into objects owned by the destination
			// knowledge so deleting the source never breaks this clone.
			newImageInfo, _, copyErr := s.cloneChunkImageInfoBound(
				ctx, srcKB, src, dstSvc, sourceChunk.ImageInfo, dst.TenantID, dst.ID, urlCache)
			if copyErr != nil {
				err = fmt.Errorf("clone chunk image copy failed: %w", copyErr)
				return err
			}
			targetChunk := &types.Chunk{
				ID:              uuid.New().String(),
				TenantID:        dst.TenantID,
				KnowledgeID:     dst.ID,
				KnowledgeBaseID: dst.KnowledgeBaseID,
				TagID:           targetTagID,
				Content:         sourceChunk.Content,
				ChunkIndex:      sourceChunk.ChunkIndex,
				IsEnabled:       sourceChunk.IsEnabled,
				Flags:           sourceChunk.Flags,
				Status:          sourceChunk.Status,
				StartAt:         sourceChunk.StartAt,
				EndAt:           sourceChunk.EndAt,
				PreChunkID:      sourceChunk.PreChunkID,
				NextChunkID:     sourceChunk.NextChunkID,
				ChunkType:       sourceChunk.ChunkType,
				ParentChunkID:   sourceChunk.ParentChunkID,
				Metadata:        sourceChunk.Metadata,
				ContentHash:     sourceChunk.ContentHash,
				ImageInfo:       newImageInfo,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			targetChunks = append(targetChunks, targetChunk)
			srcTodst[sourceChunk.ID] = targetChunk.ID
		}
	}
	for _, targetChunk := range targetChunks {
		if val, ok := srcTodst[targetChunk.PreChunkID]; ok {
			targetChunk.PreChunkID = val
		} else {
			targetChunk.PreChunkID = ""
		}
		if val, ok := srcTodst[targetChunk.NextChunkID]; ok {
			targetChunk.NextChunkID = val
		} else {
			targetChunk.NextChunkID = ""
		}
		if val, ok := srcTodst[targetChunk.ParentChunkID]; ok {
			targetChunk.ParentChunkID = val
		} else {
			targetChunk.ParentChunkID = ""
		}
	}
	for chunks := range slices.Chunk(targetChunks, chunkPageSize) {
		err := s.chunkRepo.CreateChunks(ctx, chunks)
		if err != nil {
			return err
		}
	}

	tenantID := types.MustTenantIDFromContext(ctx)
	// Route CopyIndices via the source KB's bound store. This function does
	// not handle cross-store copies — embeddings written by different
	// VectorStore backends are not bit-compatible, so callers that allow
	// source/target KBs to bind to different stores must perform their own
	// cross-store migration before invoking this.
	var sourceStoreID *string
	if srcKB != nil {
		sourceStoreID = srcKB.VectorStoreID
	}
	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, tenantID, sourceStoreID)
	if err != nil {
		return err
	}
	embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, dst.EmbeddingModelID)
	if err != nil {
		return err
	}
	if err := retrieveEngine.CopyIndices(ctx, src.KnowledgeBaseID, dst.KnowledgeBaseID,
		map[string]string{src.ID: dst.ID},
		srcTodst,
		embeddingModel.GetDimensions(),
		dst.Type,
	); err != nil {
		return err
	}
	return nil
}

const (
	kbCloneProgressKeyPrefix = "kb_clone_progress:"
	kbCloneProgressTTL       = 24 * time.Hour
)

// getKBCloneProgressKey returns the Redis key for storing KB clone progress
func getKBCloneProgressKey(taskID string) string {
	return kbCloneProgressKeyPrefix + taskID
}

// ProcessKBClone handles Asynq knowledge base clone tasks
func (s *knowledgeService) ProcessKBClone(ctx context.Context, t *asynq.Task) error {
	var payload types.KBClonePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("failed to unmarshal KB clone payload: %w", err)
	}

	// Add tenant ID to context
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)

	// Get tenant info and add to context
	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get tenant info: %v", err)
		return fmt.Errorf("failed to get tenant info: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	// Check if this is the last retry
	retryCount, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	isLastRetry := retryCount >= maxRetry

	logger.Infof(ctx, "Processing KB clone task: %s, source: %s, target: %s, retry: %d/%d",
		payload.TaskID, payload.SourceID, payload.TargetID, retryCount, maxRetry)

	// Helper function to handle errors - only mark as failed on last retry
	handleError := func(progress *types.KBCloneProgress, err error, message string) {
		if isLastRetry {
			progress.Status = types.KBCloneStatusFailed
			progress.Error = err.Error()
			progress.Message = message
			progress.UpdatedAt = time.Now().Unix()
			_ = s.saveKBCloneProgress(ctx, progress)
		}
	}

	// Update progress to processing
	progress := &types.KBCloneProgress{
		TaskID:    payload.TaskID,
		SourceID:  payload.SourceID,
		TargetID:  payload.TargetID,
		Status:    types.KBCloneStatusProcessing,
		Progress:  0,
		Message:   "Starting knowledge base clone...",
		UpdatedAt: time.Now().Unix(),
	}
	if err := s.saveKBCloneProgress(ctx, progress); err != nil {
		logger.Errorf(ctx, "Failed to update KB clone progress: %v", err)
	}

	// Get source and target knowledge bases
	srcKB, dstKB, err := s.kbService.CopyKnowledgeBase(ctx, payload.SourceID, payload.TargetID)
	if err != nil {
		logger.Errorf(ctx, "Failed to copy knowledge base: %v", err)
		handleError(progress, err, "Failed to copy knowledge base configuration")
		return err
	}

	// Use different sync strategies based on knowledge base type
	if srcKB.Type == types.KnowledgeBaseTypeFAQ {
		return s.cloneFAQKnowledgeBase(ctx, srcKB, dstKB, progress, handleError)
	}

	// Document type: use Knowledge-level diff based on file_hash
	addKnowledge, err := s.repo.AminusB(ctx, srcKB.TenantID, srcKB.ID, dstKB.TenantID, dstKB.ID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge to add: %v", err)
		handleError(progress, err, "Failed to calculate knowledge difference")
		return err
	}

	delKnowledge, err := s.repo.AminusB(ctx, dstKB.TenantID, dstKB.ID, srcKB.TenantID, srcKB.ID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get knowledge to delete: %v", err)
		handleError(progress, err, "Failed to calculate knowledge difference")
		return err
	}

	totalOperations := len(addKnowledge) + len(delKnowledge)
	progress.Total = totalOperations
	progress.Message = fmt.Sprintf("Found %d knowledge to add, %d to delete", len(addKnowledge), len(delKnowledge))
	progress.UpdatedAt = time.Now().Unix()
	_ = s.saveKBCloneProgress(ctx, progress)

	logger.Infof(ctx, "Knowledge after update to add: %d, delete: %d", len(addKnowledge), len(delKnowledge))

	processedCount := 0
	batch := 10

	// Delete knowledge in target that doesn't exist in source
	g, gctx := errgroup.WithContext(ctx)
	for ids := range slices.Chunk(delKnowledge, batch) {
		g.Go(func() error {
			err := s.DeleteKnowledgeList(gctx, ids)
			if err != nil {
				logger.Errorf(gctx, "delete partial knowledge %v: %v", ids, err)
				return err
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		logger.Errorf(ctx, "delete total knowledge %d: %v", len(delKnowledge), err)
		handleError(progress, err, "Failed to delete knowledge")
		return err
	}

	processedCount += len(delKnowledge)
	if totalOperations > 0 {
		progress.Progress = processedCount * 100 / totalOperations
	}
	progress.Processed = processedCount
	progress.Message = fmt.Sprintf("Deleted %d knowledge, cloning %d...", len(delKnowledge), len(addKnowledge))
	progress.UpdatedAt = time.Now().Unix()
	_ = s.saveKBCloneProgress(ctx, progress)

	// Clone knowledge from source to target
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(batch)
	for _, knowledge := range addKnowledge {
		g.Go(func() error {
			srcKn, err := s.repo.GetKnowledgeByID(gctx, srcKB.TenantID, knowledge)
			if err != nil {
				logger.Errorf(gctx, "get knowledge %s: %v", knowledge, err)
				return err
			}
			err = s.cloneKnowledge(gctx, srcKn, dstKB)
			if err != nil {
				logger.Errorf(gctx, "clone knowledge %s: %v", knowledge, err)
				return err
			}

			// Update progress
			processedCount++
			if totalOperations > 0 {
				progress.Progress = processedCount * 100 / totalOperations
			}
			progress.Processed = processedCount
			progress.Message = fmt.Sprintf("Cloned %d/%d knowledge", processedCount-len(delKnowledge), len(addKnowledge))
			progress.UpdatedAt = time.Now().Unix()
			_ = s.saveKBCloneProgress(ctx, progress)

			return nil
		})
	}
	if err := g.Wait(); err != nil {
		logger.Errorf(ctx, "add total knowledge %d: %v", len(addKnowledge), err)
		handleError(progress, err, "Failed to clone knowledge")
		return err
	}

	// Mark as completed
	progress.Status = types.KBCloneStatusCompleted
	progress.Progress = 100
	progress.Processed = totalOperations
	progress.Message = "Knowledge base clone completed successfully"
	progress.UpdatedAt = time.Now().Unix()
	if err := s.saveKBCloneProgress(ctx, progress); err != nil {
		logger.Errorf(ctx, "Failed to update KB clone progress to completed: %v", err)
	}

	logger.Infof(ctx, "KB clone task completed: %s", payload.TaskID)
	return nil
}

// cloneFAQKnowledgeBase handles FAQ knowledge base cloning with chunk-level incremental sync
func (s *knowledgeService) cloneFAQKnowledgeBase(
	ctx context.Context,
	srcKB, dstKB *types.KnowledgeBase,
	progress *types.KBCloneProgress,
	handleError func(*types.KBCloneProgress, error, string),
) (retErr error) {
	// Deep-copy extracted FAQ images into objects owned by the destination KB.
	// urlCache dedups identical source images across chunks; copiedURLs tracks
	// new objects for durable cleanup if the clone fails partway through.
	var dstSvc interfaces.FileService
	var dstKnowledge *types.Knowledge
	imageURLCache := map[string]string{}

	// Get source FAQ knowledge first (FAQ KB has exactly one Knowledge entry)
	srcKnowledgeList, err := s.repo.ListKnowledgeByKnowledgeBaseID(ctx, srcKB.TenantID, srcKB.ID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get source FAQ knowledge: %v", err)
		handleError(progress, err, "Failed to get source FAQ knowledge")
		return err
	}
	if len(srcKnowledgeList) == 0 {
		// Source has no FAQ knowledge, nothing to clone
		progress.Status = types.KBCloneStatusCompleted
		progress.Progress = 100
		progress.Message = "Source FAQ knowledge base is empty"
		progress.UpdatedAt = time.Now().Unix()
		_ = s.saveKBCloneProgress(ctx, progress)
		return nil
	}
	srcKnowledge := srcKnowledgeList[0]

	// Get chunk-level differences based on content_hash
	chunksToAdd, chunksToDelete, err := s.chunkRepo.FAQChunkDiff(ctx, srcKB.TenantID, srcKB.ID, dstKB.TenantID, dstKB.ID)
	if err != nil {
		logger.Errorf(ctx, "Failed to calculate FAQ chunk difference: %v", err)
		handleError(progress, err, "Failed to calculate FAQ chunk difference")
		return err
	}

	totalOperations := len(chunksToAdd) + len(chunksToDelete)
	progress.Total = totalOperations
	progress.Message = fmt.Sprintf("Found %d FAQ entries to add, %d to delete", len(chunksToAdd), len(chunksToDelete))
	progress.UpdatedAt = time.Now().Unix()
	_ = s.saveKBCloneProgress(ctx, progress)

	logger.Infof(ctx, "FAQ chunks to add: %d, delete: %d", len(chunksToAdd), len(chunksToDelete))

	// If nothing to do, mark as completed
	if totalOperations == 0 {
		progress.Status = types.KBCloneStatusCompleted
		progress.Progress = 100
		progress.Message = "FAQ knowledge base is already in sync"
		progress.UpdatedAt = time.Now().Unix()
		_ = s.saveKBCloneProgress(ctx, progress)
		return nil
	}

	// Route the FAQ clone through the source KB's bound store. Same
	// constraint as CloneChunk: callers must ensure source and target share
	// the same VectorStore (cross-store FAQ clone is not handled here).
	var sourceStoreID *string
	if srcKB != nil {
		sourceStoreID = srcKB.VectorStoreID
	}
	retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
		ctx, s.retrieveEngine, s.ownership, types.MustTenantIDFromContext(ctx), sourceStoreID)
	if err != nil {
		logger.Errorf(ctx, "Failed to init retrieve engine: %v", err)
		handleError(progress, err, "Failed to initialize retrieve engine")
		return err
	}

	// Get embedding model
	embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, dstKB.EmbeddingModelID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get embedding model: %v", err)
		handleError(progress, err, "Failed to get embedding model")
		return err
	}

	processedCount := 0

	// Delete FAQ chunks that don't exist in source
	if len(chunksToDelete) > 0 {
		// Delete from vector store
		if err := retrieveEngine.DeleteByChunkIDList(ctx, chunksToDelete, embeddingModel.GetDimensions(), types.KnowledgeTypeFAQ); err != nil {
			logger.Errorf(ctx, "Failed to delete FAQ chunks from vector store: %v", err)
			handleError(progress, err, "Failed to delete FAQ entries from vector store")
			return err
		}
		// Delete from database
		if err := s.chunkRepo.DeleteChunks(ctx, dstKB.TenantID, chunksToDelete); err != nil {
			logger.Errorf(ctx, "Failed to delete FAQ chunks from database: %v", err)
			handleError(progress, err, "Failed to delete FAQ entries from database")
			return err
		}
		processedCount += len(chunksToDelete)
		if totalOperations > 0 {
			progress.Progress = processedCount * 100 / totalOperations
		}
		progress.Processed = processedCount
		progress.Message = fmt.Sprintf("Deleted %d FAQ entries, adding %d...", len(chunksToDelete), len(chunksToAdd))
		progress.UpdatedAt = time.Now().Unix()
		_ = s.saveKBCloneProgress(ctx, progress)
	}

	// Get or create the FAQ knowledge entry in destination
	dstKnowledge, err = s.getOrCreateFAQKnowledge(ctx, dstKB, srcKnowledge)
	if err != nil {
		logger.Errorf(ctx, "Failed to get or create FAQ knowledge: %v", err)
		handleError(progress, err, "Failed to prepare FAQ knowledge entry")
		return err
	}
	dstSvc, err = s.plannedAuxiliaryFileService(ctx, dstKB, dstKnowledge, knowledgeaux.KindCloneImage)
	if err != nil {
		return fmt.Errorf("prepare tracked FAQ image storage: %w", err)
	}

	// Clone FAQ chunks from source to destination
	batch := 50
	tagIDMapping := map[string]string{} // srcTagID -> dstTagID
	for i := 0; i < len(chunksToAdd); i += batch {
		end := i + batch
		if end > len(chunksToAdd) {
			end = len(chunksToAdd)
		}
		batchIDs := chunksToAdd[i:end]

		// Get source chunks
		srcChunks, err := s.chunkRepo.ListChunksByID(ctx, srcKB.TenantID, batchIDs)
		if err != nil {
			logger.Errorf(ctx, "Failed to get source FAQ chunks: %v", err)
			handleError(progress, err, "Failed to get source FAQ entries")
			return err
		}

		// Create new chunks for destination
		newChunks := make([]*types.Chunk, 0, len(srcChunks))
		for _, srcChunk := range srcChunks {
			// Map TagID to target knowledge base
			targetTagID := ""
			if srcChunk.TagID != "" {
				if mappedTagID, ok := tagIDMapping[srcChunk.TagID]; ok {
					targetTagID = mappedTagID
				} else {
					// Try to find or create the tag in target knowledge base
					targetTagID = s.getOrCreateTagInTarget(ctx, srcKB.TenantID, dstKB.TenantID, dstKB.ID, srcChunk.TagID, tagIDMapping)
				}
			}

			// Deep-copy extracted images into objects owned by the destination
			// FAQ knowledge so deleting the source never breaks this clone.
			newImageInfo, _, copyErr := s.cloneChunkImageInfoBound(
				ctx, srcKB, srcKnowledge, dstSvc, srcChunk.ImageInfo,
				dstKB.TenantID, dstKnowledge.ID, imageURLCache)
			if copyErr != nil {
				logger.Errorf(ctx, "Failed to copy FAQ chunk images: %v", copyErr)
				handleError(progress, copyErr, "Failed to copy FAQ entry images")
				retErr = copyErr
				return retErr
			}
			newChunk := &types.Chunk{
				ID:              uuid.New().String(),
				TenantID:        dstKB.TenantID,
				KnowledgeID:     dstKnowledge.ID,
				KnowledgeBaseID: dstKB.ID,
				TagID:           targetTagID,
				Content:         srcChunk.Content,
				ChunkIndex:      srcChunk.ChunkIndex,
				IsEnabled:       srcChunk.IsEnabled,
				Flags:           srcChunk.Flags,
				ChunkType:       types.ChunkTypeFAQ,
				Metadata:        srcChunk.Metadata,
				ContentHash:     srcChunk.ContentHash,
				ImageInfo:       newImageInfo,
				Status:          int(types.ChunkStatusStored), // Initially stored, will be indexed
				CreatedAt:       time.Now(),
				UpdatedAt:       time.Now(),
			}
			newChunks = append(newChunks, newChunk)
		}

		// Save to database
		if err := s.chunkRepo.CreateChunks(ctx, newChunks); err != nil {
			logger.Errorf(ctx, "Failed to create FAQ chunks: %v", err)
			handleError(progress, err, "Failed to create FAQ entries")
			return err
		}

		// Index in vector store using existing method
		// This will index standard question + similar questions based on FAQConfig
		if err := s.indexFAQChunks(ctx, dstKB, dstKnowledge, newChunks, embeddingModel, false, false); err != nil {
			logger.Errorf(ctx, "Failed to index FAQ chunks: %v", err)
			handleError(progress, err, "Failed to index FAQ entries")
			return err
		}

		// Update chunk status to indexed
		for _, chunk := range newChunks {
			chunk.Status = int(types.ChunkStatusIndexed)
		}
		if err := s.chunkService.UpdateChunks(ctx, newChunks); err != nil {
			logger.Warnf(ctx, "Failed to update FAQ chunks status: %v", err)
			// Don't fail the whole operation for status update failure
		}

		processedCount += len(batchIDs)
		if totalOperations > 0 {
			progress.Progress = processedCount * 100 / totalOperations
		}
		progress.Processed = processedCount
		progress.Message = fmt.Sprintf("Added %d/%d FAQ entries", processedCount-len(chunksToDelete), len(chunksToAdd))
		progress.UpdatedAt = time.Now().Unix()
		_ = s.saveKBCloneProgress(ctx, progress)
	}

	// Mark as completed
	progress.Status = types.KBCloneStatusCompleted
	progress.Progress = 100
	progress.Processed = totalOperations
	progress.Message = "FAQ knowledge base clone completed successfully"
	progress.UpdatedAt = time.Now().Unix()
	if err := s.saveKBCloneProgress(ctx, progress); err != nil {
		logger.Errorf(ctx, "Failed to update KB clone progress to completed: %v", err)
	}

	return nil
}

// getOrCreateFAQKnowledge gets or creates the FAQ knowledge entry for a knowledge base
// If srcKnowledge is provided, it will copy relevant fields from source when creating new knowledge
func (s *knowledgeService) getOrCreateFAQKnowledge(ctx context.Context, kb *types.KnowledgeBase, srcKnowledge *types.Knowledge) (*types.Knowledge, error) {
	// FAQ knowledge base should have exactly one Knowledge entry
	knowledgeList, err := s.repo.ListKnowledgeByKnowledgeBaseID(ctx, kb.TenantID, kb.ID)
	if err != nil {
		return nil, err
	}

	if len(knowledgeList) > 0 {
		return knowledgeList[0], nil
	}

	// Create a new FAQ knowledge entry, copying from source if available
	knowledge := &types.Knowledge{
		ID:                   uuid.New().String(),
		TenantID:             kb.TenantID,
		KnowledgeBaseID:      kb.ID,
		Type:                 types.KnowledgeTypeFAQ,
		Channel:              types.ChannelWeb,
		Title:                "FAQ",
		ParseStatus:          "completed",
		EnableStatus:         "enabled",
		EmbeddingModelID:     kb.EmbeddingModelID,
		ProcessingGeneration: uuid.NewString(),
	}

	// Copy additional fields from source knowledge if available
	if srcKnowledge != nil {
		knowledge.Title = srcKnowledge.Title
		knowledge.Description = srcKnowledge.Description
		knowledge.Source = srcKnowledge.Source
		knowledge.Channel = srcKnowledge.Channel
		knowledge.Metadata = srcKnowledge.Metadata
	}

	if err := s.repo.CreateKnowledge(ctx, knowledge); err != nil {
		return nil, err
	}
	return knowledge, nil
}

// saveKBCloneProgress saves the KB clone progress to Redis
func (s *knowledgeService) saveKBCloneProgress(ctx context.Context, progress *types.KBCloneProgress) error {
	key := getKBCloneProgressKey(progress.TaskID)
	data, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("failed to marshal progress: %w", err)
	}
	return s.redisClient.Set(ctx, key, data, kbCloneProgressTTL).Err()
}

// SaveKBCloneProgress saves the KB clone progress to Redis (public method for handler use)
func (s *knowledgeService) SaveKBCloneProgress(ctx context.Context, progress *types.KBCloneProgress) error {
	return s.saveKBCloneProgress(ctx, progress)
}

// GetKBCloneProgress retrieves the progress of a knowledge base clone task
func (s *knowledgeService) GetKBCloneProgress(ctx context.Context, taskID string) (*types.KBCloneProgress, error) {
	key := getKBCloneProgressKey(taskID)
	data, err := s.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, werrors.NewNotFoundError("KB clone task not found")
		}
		return nil, fmt.Errorf("failed to get progress from Redis: %w", err)
	}

	var progress types.KBCloneProgress
	if err := json.Unmarshal(data, &progress); err != nil {
		return nil, fmt.Errorf("failed to unmarshal progress: %w", err)
	}
	return &progress, nil
}

const (
	knowledgeMoveProgressKeyPrefix       = "knowledge_move_progress:"
	knowledgeMoveProgressTTL             = 24 * time.Hour
	knowledgeMoveAttemptEnvelope         = "move_attempt:"
	knowledgeMoveAttemptDelimiter        = "|"
	knowledgeMoveRecoveryPrefix          = "move_recovery:"
	knowledgeMoveClaimed                 = knowledgeMoveRecoveryPrefix + "claimed:"
	knowledgeMoveRecoveryRequired        = knowledgeMoveRecoveryPrefix + "required:"
	knowledgeMoveRecoveryCleanupRequired = knowledgeMoveRecoveryPrefix + "source_reparse_cleanup_required:"
	knowledgeMoveRecoveryReparseRequired = knowledgeMoveRecoveryPrefix + "source_reparse_enqueue_required:"
	knowledgeMoveRecoveryReparseQueued   = knowledgeMoveRecoveryPrefix + "source_reparse_queued:"
	knowledgeMoveTargetCleanupRequired   = knowledgeMoveRecoveryPrefix + "target_reparse_cleanup_required:"
	knowledgeMoveTargetEnqueueRequired   = knowledgeMoveRecoveryPrefix + "target_reparse_enqueue_required:"
)

type knowledgeMoveFinalizerRepository interface {
	FinalizeReuseVectorKnowledgeMove(
		ctx context.Context,
		tenantID uint64,
		knowledgeID string,
		sourceKnowledgeBaseID string,
		targetKnowledgeBaseID string,
		expectedGeneration string,
		expectedOwner string,
		wikiMarker string,
		updatedAt time.Time,
	) (bool, error)
	FinalizeReparseKnowledgeMove(
		ctx context.Context,
		tenantID uint64,
		knowledgeID string,
		sourceKnowledgeBaseID string,
		targetKnowledgeBaseID string,
		expectedGeneration string,
		expectedOwner string,
		targetEmbeddingModelID string,
		moveMarker string,
		processingWorkflowID string,
		bindPreparedWorkflowTx func(*gorm.DB, func(*gorm.DB) error) error,
		updatedAt time.Time,
	) (bool, error)
}

type knowledgeMoveGenerationFailerRepository interface {
	FailKnowledgeMoveGeneration(
		ctx context.Context,
		tenantID uint64,
		knowledgeID string,
		expectedKnowledgeBaseID string,
		expectedGeneration string,
		expectedOwner string,
		expectedMarker string,
		errorMessage string,
	) (bool, error)
}

type knowledgeMoveRecoveryPreparerRepository interface {
	PrepareKnowledgeMoveReparseRecovery(
		ctx context.Context,
		tenantID uint64,
		knowledgeID string,
		knowledgeBaseID string,
		expectedGeneration string,
		expectedOwner string,
		expectedMarker string,
		newGeneration string,
		newOwner string,
		newMarker string,
		updatedAt time.Time,
	) (bool, error)
}

type knowledgeMoveScopeFencer interface {
	WithActiveKnowledgeMoveScope(
		ctx context.Context,
		tenantID uint64,
		sourceKnowledgeBaseID string,
		targetKnowledgeBaseID string,
		work func() error,
	) error
}

type knowledgeMoveRecoveryKBLoader interface {
	GetKnowledgeBaseByIDForMoveRecovery(
		ctx context.Context,
		id string,
		tenantID uint64,
	) (*types.KnowledgeBase, error)
}

func (s *knowledgeService) loadKnowledgeMoveKB(
	ctx context.Context,
	id string,
	tenantID uint64,
) (*types.KnowledgeBase, error) {
	if s == nil || s.kbService == nil || strings.TrimSpace(id) == "" || tenantID == 0 {
		return nil, errors.New("load knowledge move KB: complete identity is required")
	}
	active, activeErr := s.kbService.GetKnowledgeBaseByID(ctx, id)
	if activeErr == nil {
		if active == nil || active.ID != id || active.TenantID != tenantID {
			return nil, errors.New("load knowledge move KB: active tenant identity mismatch")
		}
		return active, nil
	}

	loader, ok := s.kbService.(knowledgeMoveRecoveryKBLoader)
	if !ok || loader == nil {
		return nil, activeErr
	}
	snapshot, recoveryErr := loader.GetKnowledgeBaseByIDForMoveRecovery(ctx, id, tenantID)
	if recoveryErr == nil {
		if snapshot == nil || snapshot.ID != id || snapshot.TenantID != tenantID {
			return nil, errors.New("load knowledge move KB: recovery tenant identity mismatch")
		}
		return snapshot, nil
	}
	return nil, errors.Join(activeErr, recoveryErr)
}

type knowledgeMoveFailure struct {
	cause            error
	compensated      bool
	recoveryRequired bool
}

func (e *knowledgeMoveFailure) Error() string { return e.cause.Error() }
func (e *knowledgeMoveFailure) Unwrap() error { return e.cause }

func newKnowledgeMoveFailure(cause error, compensated, recoveryRequired bool) error {
	if cause == nil {
		return nil
	}
	return &knowledgeMoveFailure{
		cause:            cause,
		compensated:      compensated,
		recoveryRequired: recoveryRequired,
	}
}

func isKnowledgeMoveRecoveryMarked(knowledge *types.Knowledge) bool {
	if knowledge == nil {
		return false
	}
	_, marker, ok := parseKnowledgeMoveAttemptMarker(knowledge.ErrorMessage)
	return ok && (strings.HasPrefix(marker, knowledgeMoveRecoveryRequired) ||
		strings.HasPrefix(marker, knowledgeMoveRecoveryCleanupRequired) ||
		strings.HasPrefix(marker, knowledgeMoveRecoveryReparseRequired) ||
		strings.HasPrefix(marker, knowledgeMoveRecoveryReparseQueued))
}

func knowledgeMoveAttemptMarker(attemptID, marker string) string {
	return knowledgeMoveAttemptEnvelope + strings.TrimSpace(attemptID) +
		knowledgeMoveAttemptDelimiter + marker
}

func parseKnowledgeMoveAttemptMarker(raw string) (attemptID string, marker string, ok bool) {
	if !strings.HasPrefix(raw, knowledgeMoveAttemptEnvelope) {
		return "", "", false
	}
	rest := strings.TrimPrefix(raw, knowledgeMoveAttemptEnvelope)
	separator := strings.Index(rest, knowledgeMoveAttemptDelimiter)
	if separator <= 0 || separator == len(rest)-1 {
		return "", "", false
	}
	attemptID = strings.TrimSpace(rest[:separator])
	marker = rest[separator+len(knowledgeMoveAttemptDelimiter):]
	if attemptID == "" || strings.Contains(attemptID, knowledgeMoveAttemptDelimiter) ||
		!strings.HasPrefix(marker, knowledgeMoveRecoveryPrefix) {
		return "", "", false
	}
	return attemptID, marker, true
}

func knowledgeMoveMarkerForAttempt(raw, attemptID string) (string, bool) {
	owner, marker, ok := parseKnowledgeMoveAttemptMarker(raw)
	return marker, ok && owner == strings.TrimSpace(attemptID)
}

func knowledgeMoveClaimMarker(attemptID, targetKnowledgeBaseID string) string {
	return knowledgeMoveAttemptMarker(attemptID, knowledgeMoveClaimed+targetKnowledgeBaseID)
}

func knowledgeMoveTargetCleanupMarker(attemptID, targetKnowledgeBaseID, generation string) string {
	return knowledgeMoveAttemptMarker(
		attemptID,
		knowledgeMoveTargetCleanupRequired+targetKnowledgeBaseID+":"+generation,
	)
}

func knowledgeMoveTargetEnqueueMarker(
	attemptID, sourceKnowledgeBaseID, targetKnowledgeBaseID, generation string,
) string {
	return knowledgeMoveAttemptMarker(
		attemptID,
		knowledgeMoveTargetEnqueueRequired+sourceKnowledgeBaseID+":"+targetKnowledgeBaseID+":"+generation,
	)
}

func isKnowledgeMoveTargetCleanupMarker(
	knowledge *types.Knowledge,
	targetKnowledgeBaseID string,
	attemptID string,
) bool {
	if knowledge == nil || targetKnowledgeBaseID == "" || knowledge.ProcessingGeneration == "" ||
		attemptID == "" {
		return false
	}
	return knowledge.ErrorMessage == knowledgeMoveTargetCleanupMarker(
		attemptID,
		targetKnowledgeBaseID,
		knowledge.ProcessingGeneration,
	)
}

func isSynchronousKnowledgeMoveRecoveryMarker(raw, attemptID string) bool {
	marker, ok := knowledgeMoveMarkerForAttempt(raw, attemptID)
	return ok && (strings.HasPrefix(marker, knowledgeMoveRecoveryRequired) ||
		strings.HasPrefix(marker, knowledgeMoveRecoveryCleanupRequired) ||
		strings.HasPrefix(marker, knowledgeMoveTargetCleanupRequired))
}

func isKnowledgeMoveReparseMarker(raw string) bool {
	_, marker, ok := parseKnowledgeMoveAttemptMarker(raw)
	return ok && (strings.HasPrefix(marker, knowledgeMoveRecoveryReparseRequired) ||
		strings.HasPrefix(marker, knowledgeMoveRecoveryReparseQueued))
}

func (s *knowledgeService) requireFinalizeReuseVectorKnowledgeMove(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	wikiMarker string,
	updatedAt time.Time,
) error {
	repo, ok := s.repo.(knowledgeMoveFinalizerRepository)
	if !ok || repo == nil {
		return errors.New("knowledge move finalizer repository is unavailable")
	}
	moved, err := repo.FinalizeReuseVectorKnowledgeMove(
		ctx,
		knowledge.TenantID,
		knowledge.ID,
		sourceKnowledgeBaseID,
		targetKnowledgeBaseID,
		knowledge.ProcessingGeneration,
		knowledge.ProcessingOwner,
		wikiMarker,
		updatedAt,
	)
	if err != nil {
		return fmt.Errorf("finalize reuse-vectors knowledge move: %w", err)
	}
	if !moved {
		return fmt.Errorf("finalize reuse-vectors knowledge move: %w", errKnowledgeStateFenceConflict)
	}
	return nil
}

func (s *knowledgeService) requirePrepareKnowledgeMoveReparseRecovery(
	ctx context.Context,
	knowledge *types.Knowledge,
	expectedGeneration string,
	expectedOwner string,
	expectedMarker string,
	newMarker string,
) error {
	repo, ok := s.repo.(knowledgeMoveRecoveryPreparerRepository)
	if !ok || repo == nil {
		return errors.New("knowledge move recovery preparer repository is unavailable")
	}
	prepared, err := repo.PrepareKnowledgeMoveReparseRecovery(
		ctx,
		knowledge.TenantID,
		knowledge.ID,
		knowledge.KnowledgeBaseID,
		expectedGeneration,
		expectedOwner,
		expectedMarker,
		knowledge.ProcessingGeneration,
		knowledge.ProcessingOwner,
		newMarker,
		time.Now(),
	)
	if err != nil {
		return fmt.Errorf("prepare knowledge move recovery reparse: %w", err)
	}
	if !prepared {
		return fmt.Errorf("prepare knowledge move recovery reparse: %w", errKnowledgeStateFenceConflict)
	}
	knowledge.ErrorMessage = newMarker
	return nil
}

func (s *knowledgeService) requireFinalizeReparseKnowledgeMove(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKnowledgeBaseID string,
	targetKnowledgeBaseID string,
	targetEmbeddingModelID string,
	moveMarker string,
	prepared *preparedDocumentWorkflow,
	updatedAt time.Time,
) error {
	repo, ok := s.repo.(knowledgeMoveFinalizerRepository)
	if !ok || repo == nil {
		return errors.New("knowledge move finalizer repository is unavailable")
	}
	var bindPreparedWorkflowTx func(*gorm.DB, func(*gorm.DB) error) error
	if prepared != nil {
		bindPreparedWorkflowTx = func(tx *gorm.DB, transition func(*gorm.DB) error) error {
			return s.bindPreparedDocumentWorkflowTransitionTx(tx, prepared, transition)
		}
	}
	moved, err := repo.FinalizeReparseKnowledgeMove(
		ctx,
		knowledge.TenantID,
		knowledge.ID,
		sourceKnowledgeBaseID,
		targetKnowledgeBaseID,
		knowledge.ProcessingGeneration,
		knowledge.ProcessingOwner,
		targetEmbeddingModelID,
		moveMarker,
		knowledge.ProcessingWorkflowID,
		bindPreparedWorkflowTx,
		updatedAt,
	)
	if err != nil {
		return fmt.Errorf("finalize reparse knowledge move: %w", err)
	}
	if !moved {
		return fmt.Errorf("finalize reparse knowledge move: %w", errKnowledgeStateFenceConflict)
	}
	return nil
}

func finalizeKnowledgeMoveAttempt(
	progress *types.KnowledgeMoveProgress,
	itemErrors []error,
	isLastRetry bool,
) error {
	progress.Progress = 100
	progress.UpdatedAt = time.Now().Unix()
	if len(itemErrors) == 0 {
		progress.Status = types.KBCloneStatusCompleted
		progress.Error = ""
		progress.Message = fmt.Sprintf(
			"Knowledge move completed: %d/%d succeeded",
			progress.Processed,
			progress.Total,
		)
		return nil
	}
	joined := errors.Join(itemErrors...)
	progress.Error = joined.Error()
	if isLastRetry {
		progress.Status = types.KBCloneStatusFailed
		progress.Message = fmt.Sprintf(
			"Knowledge move failed after retries: %d/%d item(s) failed",
			progress.Failed,
			progress.Total,
		)
	} else {
		progress.Status = types.KBCloneStatusProcessing
		progress.Message = fmt.Sprintf(
			"Knowledge move attempt incomplete: %d/%d item(s) will retry",
			progress.Failed,
			progress.Total,
		)
	}
	return joined
}

func knowledgeMoveRecoveryTaskID(knowledge *types.Knowledge) string {
	if knowledge == nil {
		return ""
	}
	attemptID, marker, ok := parseKnowledgeMoveAttemptMarker(knowledge.ErrorMessage)
	if !ok {
		return ""
	}
	for _, prefix := range []string{
		knowledgeMoveRecoveryCleanupRequired,
		knowledgeMoveRecoveryReparseRequired,
		knowledgeMoveRecoveryReparseQueued,
	} {
		if strings.HasPrefix(marker, prefix) {
			generation := strings.TrimPrefix(marker, prefix)
			if generation != "" {
				return "move-recovery-" + knowledge.ID + "-" + attemptID + "-" + generation
			}
		}
	}
	return ""
}

func knowledgeMoveReparseTaskID(knowledge *types.Knowledge, targetKBID string) string {
	if recoveryTaskID := knowledgeMoveRecoveryTaskID(knowledge); recoveryTaskID != "" {
		return recoveryTaskID
	}
	if knowledge == nil || knowledge.ID == "" || targetKBID == "" || knowledge.ProcessingGeneration == "" {
		return ""
	}
	// One stable ID fences an ambiguous enqueue response and every batch-level
	// retry while this document is handed to the target parser. Completed tasks
	// leave Asynq's active set, so a later user-initiated move back to the same
	// target can enqueue a new generation normally.
	return "move-reparse-" + knowledge.ID + "-" + targetKBID + "-" + knowledge.ProcessingGeneration
}

func getKnowledgeMoveProgressKey(taskID string) string {
	return knowledgeMoveProgressKeyPrefix + taskID
}

func (s *knowledgeService) saveKnowledgeMoveProgress(ctx context.Context, progress *types.KnowledgeMoveProgress) error {
	key := getKnowledgeMoveProgressKey(progress.TaskID)
	data, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("failed to marshal move progress: %w", err)
	}
	return s.redisClient.Set(ctx, key, data, knowledgeMoveProgressTTL).Err()
}

// SaveKnowledgeMoveProgress saves the knowledge move progress to Redis (public method for handler use)
func (s *knowledgeService) SaveKnowledgeMoveProgress(ctx context.Context, progress *types.KnowledgeMoveProgress) error {
	return s.saveKnowledgeMoveProgress(ctx, progress)
}

// GetKnowledgeMoveProgress retrieves the progress of a knowledge move task
func (s *knowledgeService) GetKnowledgeMoveProgress(ctx context.Context, taskID string) (*types.KnowledgeMoveProgress, error) {
	key := getKnowledgeMoveProgressKey(taskID)
	data, err := s.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, werrors.NewNotFoundError("Knowledge move task not found")
		}
		return nil, fmt.Errorf("failed to get move progress from Redis: %w", err)
	}

	var progress types.KnowledgeMoveProgress
	if err := json.Unmarshal(data, &progress); err != nil {
		return nil, fmt.Errorf("failed to unmarshal move progress: %w", err)
	}
	return &progress, nil
}

// ProcessKnowledgeMove handles Asynq knowledge move tasks
func (s *knowledgeService) ProcessKnowledgeMove(ctx context.Context, t *asynq.Task) error {
	var payload types.KnowledgeMovePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("failed to unmarshal knowledge move payload: %w", err)
	}
	payload.AttemptID = strings.TrimSpace(payload.AttemptID)
	if payload.AttemptID == "" {
		// Rolling-upgrade compatibility: TaskID was already immutable and
		// persisted in every legacy move payload, so it is a safe attempt owner.
		payload.AttemptID = strings.TrimSpace(payload.TaskID)
	}
	if payload.AttemptID == "" || strings.Contains(payload.AttemptID, knowledgeMoveAttemptDelimiter) {
		return errors.New("knowledge move payload: immutable attempt_id is required")
	}

	// Add tenant ID to context
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)

	// Get tenant info and add to context
	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		logger.Errorf(ctx, "ProcessKnowledgeMove: failed to get tenant info: %v", err)
		return fmt.Errorf("failed to get tenant info: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	// Check if this is the last retry
	retryCount, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)
	isLastRetry := retryCount >= maxRetry

	logger.Infof(ctx, "ProcessKnowledgeMove: task=%s, source=%s, target=%s, mode=%s, count=%d, retry=%d/%d",
		payload.TaskID, payload.SourceKBID, payload.TargetKBID, payload.Mode, len(payload.KnowledgeIDs), retryCount, maxRetry)

	// Helper function to handle errors - only mark as failed on last retry
	handleError := func(progress *types.KnowledgeMoveProgress, err error, message string) {
		if isLastRetry {
			progress.Status = types.KBCloneStatusFailed
			progress.Error = err.Error()
			progress.Message = message
			progress.UpdatedAt = time.Now().Unix()
			_ = s.saveKnowledgeMoveProgress(ctx, progress)
		}
	}

	// Update progress to processing
	progress := &types.KnowledgeMoveProgress{
		TaskID:     payload.TaskID,
		SourceKBID: payload.SourceKBID,
		TargetKBID: payload.TargetKBID,
		Status:     types.KBCloneStatusProcessing,
		Total:      len(payload.KnowledgeIDs),
		Progress:   0,
		Message:    "Starting knowledge move...",
		UpdatedAt:  time.Now().Unix(),
	}
	_ = s.saveKnowledgeMoveProgress(ctx, progress)

	// Active rows are used for new work. Exact-attempt recovery may need the
	// unscoped snapshot after either endpoint was tombstoned, so a committed
	// target marker can still be settled instead of being stranded forever.
	sourceKB, err := s.loadKnowledgeMoveKB(ctx, payload.SourceKBID, payload.TenantID)
	if err != nil {
		handleError(progress, err, "Failed to get source knowledge base")
		return err
	}
	targetKB, err := s.loadKnowledgeMoveKB(ctx, payload.TargetKBID, payload.TenantID)
	if err != nil {
		handleError(progress, err, "Failed to get target knowledge base")
		return err
	}

	// Tombstoned snapshots are recovery-only. Compatibility remains mandatory
	// before any new source-side claim.
	if !sourceKB.DeletedAt.Valid && !targetKB.DeletedAt.Valid && sourceKB.Type != targetKB.Type {
		err := fmt.Errorf("type mismatch: source=%s, target=%s", sourceKB.Type, targetKB.Type)
		handleError(progress, err, "Source and target knowledge bases must be the same type")
		return err
	}
	if !sourceKB.DeletedAt.Valid && !targetKB.DeletedAt.Valid && sourceKB.EmbeddingModelID != targetKB.EmbeddingModelID {
		err := fmt.Errorf("embedding model mismatch: source=%s, target=%s", sourceKB.EmbeddingModelID, targetKB.EmbeddingModelID)
		handleError(progress, err, "Source and target must use the same embedding model")
		return err
	}

	// Process each knowledge item. Any item failure fails this Asynq attempt so
	// fully compensated source rows are retried and recovery-marked rows retain
	// a durable retry path; successful target rows are idempotently skipped on
	// the next attempt.
	itemErrors := make([]error, 0)
	for i, knowledgeID := range payload.KnowledgeIDs {
		err := s.moveOneKnowledge(ctx, knowledgeID, sourceKB, targetKB, payload.Mode, payload.AttemptID)
		if err != nil {
			logger.Errorf(ctx, "ProcessKnowledgeMove: failed to move knowledge %s: %v", knowledgeID, err)
			progress.Failed++
			itemErrors = append(itemErrors, fmt.Errorf("move knowledge %s: %w", knowledgeID, err))
		}
		progress.Processed = i + 1
		if progress.Total > 0 {
			progress.Progress = progress.Processed * 100 / progress.Total
		}
		progress.Message = fmt.Sprintf("Moved %d/%d knowledge items", progress.Processed, progress.Total)
		progress.UpdatedAt = time.Now().Unix()
		_ = s.saveKnowledgeMoveProgress(ctx, progress)
	}

	attemptErr := finalizeKnowledgeMoveAttempt(progress, itemErrors, isLastRetry)
	_ = s.saveKnowledgeMoveProgress(ctx, progress)
	if attemptErr != nil {
		logger.Errorf(ctx,
			"ProcessKnowledgeMove: task=%s attempt failed, processed=%d, failed=%d",
			payload.TaskID, progress.Processed, progress.Failed)
		return attemptErr
	}

	logger.Infof(ctx, "ProcessKnowledgeMove: task=%s completed, processed=%d, failed=%d", payload.TaskID, progress.Processed, progress.Failed)
	return nil
}

// RepairKnowledgeMoveDeadLetter releases only lifecycle states still owned by
// the exhausted parent move. Synchronous source-side recovery phases become a
// fenced Failed row; a target/source Pending reparse gets one final idempotent
// child handoff and is failed only after any required source-Wiki transition
// has committed. Active child Processing/Finalizing generations are never
// touched by the parent dead letter.
func (s *knowledgeService) RepairKnowledgeMoveDeadLetter(
	ctx context.Context,
	t *asynq.Task,
	taskErr error,
) error {
	if t == nil {
		return errors.New("repair knowledge move dead letter: task is required")
	}
	var payload types.KnowledgeMovePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("repair knowledge move dead letter: decode payload: %w", err)
	}
	if payload.TenantID == 0 || payload.SourceKBID == "" || payload.TargetKBID == "" ||
		len(payload.KnowledgeIDs) == 0 {
		return errors.New("repair knowledge move dead letter: complete move identity is required")
	}
	payload.AttemptID = strings.TrimSpace(payload.AttemptID)
	if payload.AttemptID == "" {
		payload.AttemptID = strings.TrimSpace(payload.TaskID)
	}
	if payload.AttemptID == "" || strings.Contains(payload.AttemptID, knowledgeMoveAttemptDelimiter) {
		return errors.New("repair knowledge move dead letter: immutable attempt identity is required")
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	tenant, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		return fmt.Errorf("repair knowledge move dead letter: load tenant: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)

	errorText := "unknown task error"
	if taskErr != nil {
		errorText = taskErr.Error()
	}
	terminalMessage := "knowledge move exhausted retries: " + errorText
	if len(terminalMessage) > 2000 {
		terminalMessage = terminalMessage[:2000]
	}

	var sourceKB, targetKB *types.KnowledgeBase
	loadMoveKBs := func() error {
		if sourceKB == nil {
			sourceKB, err = s.loadKnowledgeMoveKB(ctx, payload.SourceKBID, payload.TenantID)
			if err != nil {
				return fmt.Errorf("load source knowledge base: %w", err)
			}
		}
		if targetKB == nil {
			targetKB, err = s.loadKnowledgeMoveKB(ctx, payload.TargetKBID, payload.TenantID)
			if err != nil {
				return fmt.Errorf("load target knowledge base: %w", err)
			}
		}
		return nil
	}

	failPendingReparse := func(knowledge *types.Knowledge, cause error) error {
		message := terminalMessage
		if cause != nil {
			message += "; final handoff: " + cause.Error()
			if len(message) > 2000 {
				message = message[:2000]
			}
		}
		return s.requireDocumentProcessingIdentitySwap(
			ctx,
			knowledge,
			types.ParseStatusPending,
			knowledge.ProcessingGeneration,
			knowledge.ProcessingOwner,
			map[string]interface{}{
				"parse_status":           types.ParseStatusFailed,
				"error_message":          message,
				"pending_subtasks_count": 0,
				"enrichment_status":      types.EnrichmentStatusNone,
				"wiki_status":            types.WikiStatusNone,
				"wiki_error_message":     "",
				"processing_owner":       "",
				"processing_fanout":      nil,
				"updated_at":             time.Now(),
			},
			"terminalize pending reparse after move dead letter",
		)
	}

	failer, _ := s.repo.(knowledgeMoveGenerationFailerRepository)
	var repairErrs []error
	for _, knowledgeID := range normalizeKnowledgeDeleteIDs(payload.KnowledgeIDs) {
		knowledge, loadErr := s.repo.GetKnowledgeByID(ctx, payload.TenantID, knowledgeID)
		if loadErr != nil {
			if errors.Is(loadErr, apprepo.ErrKnowledgeNotFound) {
				continue
			}
			repairErrs = append(repairErrs, fmt.Errorf("load knowledge %s: %w", knowledgeID, loadErr))
			continue
		}
		if knowledge == nil {
			continue
		}

		// The cross-KB CAS may have committed just before the final parent
		// attempt died. Settle its exact marker for every target status, including
		// reuse-vectors Completed and deletion-owned rows, before applying the
		// generic terminal-state filters below.
		exactWikiMarker := knowledgeMoveWikiPendingMarker(
			payload.AttemptID, payload.SourceKBID, payload.TargetKBID,
		)
		if knowledge.KnowledgeBaseID == payload.TargetKBID &&
			knowledge.ErrorMessage == exactWikiMarker {
			if err := loadMoveKBs(); err != nil {
				repairErrs = append(repairErrs, fmt.Errorf("repair target Wiki marker %s: %w", knowledge.ID, err))
				continue
			}
			if reconcileErr := s.reconcileWikiAfterKnowledgeMove(
				ctx, knowledge, sourceKB, targetKB,
			); reconcileErr != nil {
				repairErrs = append(repairErrs, fmt.Errorf("repair target Wiki marker %s: %w", knowledge.ID, reconcileErr))
				continue
			}
			if payload.Mode == "reparse" && knowledge.ParseStatus == types.ParseStatusPending &&
				!targetKB.DeletedAt.Valid {
				if handoffErr := s.handoffTargetKnowledgeReparse(
					ctx, knowledge, sourceKB, targetKB, payload.AttemptID,
				); handoffErr != nil {
					if failErr := failPendingReparse(knowledge, handoffErr); failErr != nil {
						repairErrs = append(repairErrs, errors.Join(handoffErr, failErr))
					}
				}
			}
			continue
		}
		if knowledge.ParseStatus == types.ParseStatusDeleting {
			continue
		}

		if knowledge.KnowledgeBaseID == payload.SourceKBID &&
			knowledge.ParseStatus == types.ParseStatusProcessing &&
			isSynchronousKnowledgeMoveRecoveryMarker(knowledge.ErrorMessage, payload.AttemptID) {
			if failer == nil {
				repairErrs = append(repairErrs, errors.New("knowledge move generation failer repository is unavailable"))
				continue
			}
			failed, failErr := failer.FailKnowledgeMoveGeneration(
				ctx,
				knowledge.TenantID,
				knowledge.ID,
				knowledge.KnowledgeBaseID,
				knowledge.ProcessingGeneration,
				knowledge.ProcessingOwner,
				knowledge.ErrorMessage,
				terminalMessage,
			)
			if failErr != nil {
				repairErrs = append(repairErrs, fmt.Errorf("fail synchronous move recovery %s: %w", knowledge.ID, failErr))
			} else if !failed {
				logger.Infof(ctx, "Move dead-letter repair skipped superseded source recovery %s", knowledge.ID)
			}
			continue
		}

		if payload.Mode != "reparse" || knowledge.ParseStatus != types.ParseStatusPending {
			continue
		}
		marker, markerOwned := knowledgeMoveMarkerForAttempt(knowledge.ErrorMessage, payload.AttemptID)
		if !markerOwned {
			// Pending with no marker has already completed the parent handoff;
			// a marker owned by another attempt belongs to a newer controller.
			continue
		}
		if err := loadMoveKBs(); err != nil {
			repairErrs = append(repairErrs, fmt.Errorf("repair pending reparse %s: %w", knowledge.ID, err))
			continue
		}

		switch knowledge.KnowledgeBaseID {
		case payload.TargetKBID:
			if !strings.HasPrefix(marker, knowledgeMoveWikiPendingPrefix) &&
				!strings.HasPrefix(marker, knowledgeMoveTargetEnqueueRequired) {
				continue
			}
			if targetKB.DeletedAt.Valid {
				// Whole-KB deletion owns the target. An exact wiki_pending marker was
				// handled above; enqueue_required must not publish a new child.
				continue
			}
			if reconcileErr := s.reconcileWikiAfterKnowledgeMove(ctx, knowledge, sourceKB, targetKB); reconcileErr != nil {
				// Keep Pending plus the exact wiki_pending marker. Replacing it with
				// a terminal error here would make source-Wiki cleanup unrecoverable.
				repairErrs = append(repairErrs, fmt.Errorf("repair target Wiki handoff %s: %w", knowledge.ID, reconcileErr))
				continue
			}
			if handoffErr := s.handoffTargetKnowledgeReparse(
				ctx, knowledge, sourceKB, targetKB, payload.AttemptID,
			); handoffErr != nil {
				if failErr := failPendingReparse(knowledge, handoffErr); failErr != nil {
					repairErrs = append(repairErrs, errors.Join(handoffErr, failErr))
				}
			}
		case payload.SourceKBID:
			if !strings.HasPrefix(marker, knowledgeMoveRecoveryReparseRequired) {
				continue
			}
			if sourceKB.DeletedAt.Valid {
				continue
			}
			if enqueueErr := s.enqueueMovedKnowledgeReparse(ctx, sourceKB, knowledge); enqueueErr != nil {
				if failErr := failPendingReparse(knowledge, enqueueErr); failErr != nil {
					repairErrs = append(repairErrs, errors.Join(enqueueErr, failErr))
				}
			}
		}
	}
	return errors.Join(repairErrs...)
}

func (s *knowledgeService) preflightReuseVectorMove(
	ctx context.Context,
	tenantID uint64,
	knowledge *types.Knowledge,
	sourceKB *types.KnowledgeBase,
) error {
	if knowledge == nil || sourceKB == nil {
		return errors.New("preflight reuse-vectors move: knowledge and source KB are required")
	}
	if knowledge.EmbeddingModelID == "" {
		return nil
	}
	engine, err := retriever.CreateRetrieveEngineForKB(
		ctx,
		s.retrieveEngine,
		s.ownership,
		tenantID,
		sourceKB.VectorStoreID,
	)
	if err != nil {
		return fmt.Errorf("preflight reuse-vectors move: resolve retrieval engine: %w", err)
	}
	if !engine.SupportsKnowledgeIndexMove() {
		return fmt.Errorf(
			"reuse_vectors is not supported by the source vector backend for KB %s; use reparse mode",
			sourceKB.ID,
		)
	}
	return nil
}

func validateKnowledgeMoveReparseSource(knowledge *types.Knowledge) error {
	if knowledge == nil {
		return errors.New("reparse move source knowledge is required")
	}
	if knowledge.Type == types.KnowledgeTypeFAQ {
		return errors.New("reparse move is not supported for FAQ knowledge; use reuse_vectors within the same store")
	}
	if !knowledge.IsManual() && strings.TrimSpace(knowledge.FilePath) == "" {
		return fmt.Errorf(
			"reparse move requires a persistent source file for knowledge %s",
			knowledge.ID,
		)
	}
	return nil
}

func (s *knowledgeService) markKnowledgeMoveRecoveryRequired(
	ctx context.Context,
	tenantID uint64,
	knowledge *types.Knowledge,
	sourceKBID string,
	attemptID string,
	cause error,
) error {
	if knowledge == nil || knowledge.TenantID != tenantID || knowledge.KnowledgeBaseID != sourceKBID ||
		strings.TrimSpace(attemptID) == "" {
		return errors.New("mark reuse-vectors move recovery: complete source attempt identity is required")
	}
	message := knowledgeMoveAttemptMarker(attemptID, knowledgeMoveRecoveryRequired+cause.Error())
	if len(message) > 2000 {
		message = message[:2000]
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	markErr := s.requireDocumentProcessingIdentitySwap(
		markCtx,
		knowledge,
		types.ParseStatusProcessing,
		knowledge.ProcessingGeneration,
		attemptID,
		map[string]interface{}{
			"error_message": message,
			"updated_at":    time.Now(),
		},
		"mark reuse-vectors move recovery required",
	)
	if markErr == nil {
		knowledge.ErrorMessage = message
	}
	return newKnowledgeMoveFailure(
		errors.Join(cause, markErr),
		false,
		true,
	)
}

func (s *knowledgeService) restoreClaimedReuseMove(
	ctx context.Context,
	tenantID uint64,
	knowledge *types.Knowledge,
	sourceKBID string,
	attemptID string,
	cause error,
) error {
	if knowledge == nil || knowledge.TenantID != tenantID || knowledge.KnowledgeBaseID != sourceKBID ||
		strings.TrimSpace(attemptID) == "" {
		return errors.New("restore reuse-vectors move: complete source attempt identity is required")
	}
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	restoreErr := s.requireDocumentProcessingIdentitySwap(
		restoreCtx,
		knowledge,
		types.ParseStatusProcessing,
		knowledge.ProcessingGeneration,
		attemptID,
		map[string]interface{}{
			"parse_status":     types.ParseStatusCompleted,
			"processing_owner": "",
			"error_message":    "",
			"updated_at":       time.Now(),
		},
		"restore reuse-vectors move claim",
	)
	if restoreErr == nil {
		knowledge.ParseStatus = types.ParseStatusCompleted
		knowledge.ProcessingOwner = ""
		knowledge.ErrorMessage = ""
		return newKnowledgeMoveFailure(cause, true, false)
	}
	return s.markKnowledgeMoveRecoveryRequired(
		ctx,
		tenantID,
		knowledge,
		sourceKBID,
		attemptID,
		errors.Join(cause, restoreErr),
	)
}

func (s *knowledgeService) recoverMarkedKnowledgeMove(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB *types.KnowledgeBase,
	attemptID string,
) error {
	if knowledge == nil || sourceKB == nil {
		return errors.New("recover knowledge move: knowledge and source KB are required")
	}
	if knowledge.KnowledgeBaseID != sourceKB.ID {
		return fmt.Errorf(
			"recover knowledge move: knowledge %s no longer belongs to source KB %s",
			knowledge.ID,
			sourceKB.ID,
		)
	}

	marker, owned := knowledgeMoveMarkerForAttempt(knowledge.ErrorMessage, attemptID)
	if !owned {
		return nil
	}
	if strings.HasPrefix(marker, knowledgeMoveRecoveryRequired) {
		if knowledge.ParseStatus != types.ParseStatusProcessing {
			return newKnowledgeMoveFailure(fmt.Errorf(
				"recover knowledge move: recovery-required row is in status %s",
				knowledge.ParseStatus,
			), false, true)
		}
		recoveryGeneration := uuid.NewString()
		expectedGeneration := knowledge.ProcessingGeneration
		expectedOwner := knowledge.ProcessingOwner
		expectedMarker := knowledge.ErrorMessage
		assignNewDocumentProcessingIdentity(knowledge)
		cleanupMarker := knowledgeMoveAttemptMarker(
			attemptID, knowledgeMoveRecoveryCleanupRequired+recoveryGeneration,
		)
		if err := s.requirePrepareKnowledgeMoveReparseRecovery(
			ctx, knowledge, expectedGeneration, expectedOwner, expectedMarker, cleanupMarker,
		); err != nil {
			return newKnowledgeMoveFailure(err, false, true)
		}
		if err := s.moveKnowledgeReparse(ctx, knowledge, sourceKB, sourceKB, attemptID); err != nil {
			return newKnowledgeMoveFailure(
				fmt.Errorf("recover knowledge move with source reparse: %w", err),
				false,
				true,
			)
		}
		if err := s.markKnowledgeMoveRecoveryReparseQueued(ctx, knowledge, sourceKB.ID, attemptID, recoveryGeneration); err != nil {
			return newKnowledgeMoveFailure(err, false, true)
		}
		return newKnowledgeMoveFailure(
			fmt.Errorf("knowledge %s recovered to source reparse; original move remains failed", knowledge.ID),
			false,
			true,
		)
	}

	if strings.HasPrefix(marker, knowledgeMoveRecoveryCleanupRequired) {
		if knowledge.ParseStatus != types.ParseStatusProcessing {
			return newKnowledgeMoveFailure(fmt.Errorf(
				"recover knowledge move: cleanup-required source row is in status %s",
				knowledge.ParseStatus,
			), false, true)
		}
		recoveryGeneration := strings.TrimPrefix(
			marker,
			knowledgeMoveRecoveryCleanupRequired,
		)
		if recoveryGeneration == "" {
			return newKnowledgeMoveFailure(errors.New(
				"recover knowledge move: cleanup marker has no recovery generation",
			), false, true)
		}
		if err := s.moveKnowledgeReparse(ctx, knowledge, sourceKB, sourceKB, attemptID); err != nil {
			return newKnowledgeMoveFailure(
				fmt.Errorf("resume source reparse cleanup: %w", err),
				false,
				true,
			)
		}
		if err := s.markKnowledgeMoveRecoveryReparseQueued(ctx, knowledge, sourceKB.ID, attemptID, recoveryGeneration); err != nil {
			return newKnowledgeMoveFailure(err, false, true)
		}
		return newKnowledgeMoveFailure(
			fmt.Errorf("knowledge %s recovered to source reparse; original move remains failed", knowledge.ID),
			false,
			true,
		)
	}

	if !strings.HasPrefix(marker, knowledgeMoveRecoveryReparseRequired) &&
		!strings.HasPrefix(marker, knowledgeMoveRecoveryReparseQueued) {
		return fmt.Errorf("recover knowledge move: unrecognized recovery marker")
	}
	switch knowledge.ParseStatus {
	case types.ParseStatusPending:
		if strings.HasPrefix(marker, knowledgeMoveRecoveryReparseRequired) {
			generation := strings.TrimPrefix(marker, knowledgeMoveRecoveryReparseRequired)
			if err := s.enqueueMovedKnowledgeReparse(ctx, sourceKB, knowledge); err != nil {
				return newKnowledgeMoveFailure(
					fmt.Errorf("resume source reparse enqueue: %w", err),
					false,
					true,
				)
			}
			if err := s.markKnowledgeMoveRecoveryReparseQueued(ctx, knowledge, sourceKB.ID, attemptID, generation); err != nil {
				return newKnowledgeMoveFailure(err, false, true)
			}
		}
		return newKnowledgeMoveFailure(
			fmt.Errorf("knowledge %s source reparse is queued", knowledge.ID),
			false,
			true,
		)
	case types.ParseStatusProcessing, types.ParseStatusFinalizing:
		// enqueue_required + Processing is a valid race: the stable child task
		// claimed Pending before the parent could flip the marker to queued. It is
		// no longer a synchronous move phase and must be owned by that child.
		return newKnowledgeMoveFailure(
			fmt.Errorf("knowledge %s source reparse is still %s", knowledge.ID, knowledge.ParseStatus),
			false,
			true,
		)
	case types.ParseStatusCompleted:
		if err := s.requireDocumentProcessingIdentitySwap(
			ctx,
			knowledge,
			types.ParseStatusCompleted,
			knowledge.ProcessingGeneration,
			knowledge.ProcessingOwner,
			map[string]interface{}{
				"error_message": "",
				"updated_at":    time.Now(),
			},
			"clear completed source-reparse recovery marker",
		); err != nil {
			return newKnowledgeMoveFailure(err, false, true)
		}
		knowledge.ErrorMessage = ""
		return newKnowledgeMoveFailure(
			fmt.Errorf("knowledge %s source recovery completed; retrying move", knowledge.ID),
			true,
			false,
		)
	default:
		return newKnowledgeMoveFailure(
			fmt.Errorf("knowledge %s source recovery ended in status %s", knowledge.ID, knowledge.ParseStatus),
			false,
			true,
		)
	}
}

func (s *knowledgeService) markKnowledgeMoveRecoveryReparseQueued(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKBID string,
	attemptID string,
	generation string,
) error {
	queuedMarker := knowledgeMoveAttemptMarker(attemptID, knowledgeMoveRecoveryReparseQueued+generation)
	err := s.requireDocumentProcessingIdentitySwap(
		ctx,
		knowledge,
		types.ParseStatusPending,
		knowledge.ProcessingGeneration,
		knowledge.ProcessingOwner,
		map[string]interface{}{
			"error_message": queuedMarker,
			"updated_at":    time.Now(),
		},
		"mark source move-recovery reparse queued",
	)
	if err == nil {
		knowledge.ErrorMessage = queuedMarker
	}
	return err
}

// moveOneKnowledge moves a single knowledge item from source KB to target KB.
func (s *knowledgeService) moveOneKnowledge(
	ctx context.Context,
	knowledgeID string,
	sourceKB, targetKB *types.KnowledgeBase,
	mode string,
	attemptID string,
) error {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	if sourceKB == nil || targetKB == nil || strings.TrimSpace(attemptID) == "" {
		return errors.New("move knowledge: source and target knowledge bases are required")
	}
	if sourceKB.ID == "" || targetKB.ID == "" ||
		(sourceKB.TenantID != 0 && sourceKB.TenantID != tenantID) ||
		(targetKB.TenantID != 0 && targetKB.TenantID != tenantID) {
		return errors.New("move knowledge: knowledge-base tenant identity mismatch")
	}

	// Get the knowledge item
	knowledge, err := s.repo.GetKnowledgeByID(ctx, tenantID, knowledgeID)
	if err != nil {
		// The tenant-scoped row disappeared after this batch task was queued.
		// Deletion owns cleanup and the controller must be allowed to drain.
		if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
			return nil
		}
		return fmt.Errorf("failed to get knowledge %s: %w", knowledgeID, err)
	}
	if knowledge == nil {
		return fmt.Errorf("failed to get knowledge %s: repository returned nil without error", knowledgeID)
	}
	markerAttemptID, _, hasAttemptMarker := parseKnowledgeMoveAttemptMarker(knowledge.ErrorMessage)
	if hasAttemptMarker && markerAttemptID != attemptID {
		return newKnowledgeMoveFailure(fmt.Errorf(
			"knowledge %s is owned by another move attempt %s",
			knowledge.ID,
			markerAttemptID,
		), false, true)
	}
	// Delete intentionally leaves the batch move controller queued while it
	// waits for per-document writers to quiesce. If the final move CAS committed
	// before deletion won, finish the exact marker-backed source Wiki handoff;
	// PrepareMove sees Deleting and therefore writes only the source retract.
	// Every other deleting item acknowledges without Wiki/vector mutation so
	// the delete barrier and move retry cannot wait on one another forever.
	if knowledge.ParseStatus == types.ParseStatusDeleting {
		if knowledge.KnowledgeBaseID == targetKB.ID &&
			knowledge.ErrorMessage == knowledgeMoveWikiPendingMarker(attemptID, sourceKB.ID, targetKB.ID) {
			return s.reconcileWikiAfterKnowledgeMove(ctx, knowledge, sourceKB, targetKB)
		}
		return nil
	}
	if mode != "reuse_vectors" && mode != "reparse" {
		return fmt.Errorf("unknown move mode: %s", mode)
	}

	// Retries are idempotent for items whose authoritative row already reached
	// the target. This lets a batch retry only its failed members. Reparse is
	// considered successfully handed off once its stable processing task is
	// pending/running; only terminal parse failures fail the move item.
	if knowledge.KnowledgeBaseID == targetKB.ID {
		if mode == "reparse" {
			if knowledge.ErrorMessage == knowledgeMoveWikiPendingMarker(attemptID, sourceKB.ID, targetKB.ID) {
				if err := s.reconcileWikiAfterKnowledgeMove(ctx, knowledge, sourceKB, targetKB); err != nil {
					return err
				}
			}
			if targetKB.DeletedAt.Valid {
				// A target tombstone owns the committed row. Wiki settlement above
				// clears an exact marker, but no parser may be published afterwards.
				return nil
			}
			switch knowledge.ParseStatus {
			case types.ParseStatusPending:
				if _, owned := knowledgeMoveMarkerForAttempt(knowledge.ErrorMessage, attemptID); !owned {
					// An empty marker means the parent handoff already committed. The
					// child owns Pending from this point, even before it claims Processing.
					return nil
				}
				return s.handoffTargetKnowledgeReparse(ctx, knowledge, sourceKB, targetKB, attemptID)
			case types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCompleted:
				return nil
			case types.ParseStatusFailed, types.ParseStatusCancelled:
				return fmt.Errorf("knowledge %s target reparse ended in status %s", knowledgeID, knowledge.ParseStatus)
			default:
				return fmt.Errorf("knowledge %s target reparse has unexpected status %s", knowledgeID, knowledge.ParseStatus)
			}
		}
		if knowledge.ParseStatus == types.ParseStatusCompleted {
			if knowledge.ErrorMessage == knowledgeMoveWikiPendingMarker(attemptID, sourceKB.ID, targetKB.ID) {
				return s.reconcileWikiAfterKnowledgeMove(ctx, knowledge, sourceKB, targetKB)
			}
			return nil
		}
	}
	if knowledge.KnowledgeBaseID == sourceKB.ID && hasKnowledgeMoveWikiPendingMarker(knowledge) {
		return fmt.Errorf(
			"knowledge %s has unfinished Wiki reconciliation from a previous move",
			knowledgeID,
		)
	}
	if knowledge.KnowledgeBaseID == sourceKB.ID && mode == "reparse" &&
		isKnowledgeMoveTargetCleanupMarker(knowledge, targetKB.ID, attemptID) {
		if knowledge.ParseStatus != types.ParseStatusProcessing {
			return newKnowledgeMoveFailure(fmt.Errorf(
				"knowledge %s target-reparse recovery marker is in status %s",
				knowledge.ID,
				knowledge.ParseStatus,
			), false, true)
		}
		return s.moveKnowledgeReparse(ctx, knowledge, sourceKB, targetKB, attemptID)
	}
	if knowledge.KnowledgeBaseID == sourceKB.ID && isKnowledgeMoveRecoveryMarked(knowledge) {
		return s.recoverMarkedKnowledgeMove(ctx, knowledge, sourceKB, attemptID)
	}
	if mode == "reuse_vectors" && knowledge.KnowledgeBaseID == sourceKB.ID &&
		knowledge.ParseStatus == types.ParseStatusProcessing {
		if knowledge.ProcessingOwner != attemptID {
			return newKnowledgeMoveFailure(fmt.Errorf(
				"knowledge %s processing is owned by another controller",
				knowledge.ID,
			), false, true)
		}
		return s.markKnowledgeMoveRecoveryRequired(
			ctx,
			tenantID,
			knowledge,
			sourceKB.ID,
			attemptID,
			fmt.Errorf("reuse-vectors move retry found an unmarked processing row"),
		)
	}
	if sourceKB.DeletedAt.Valid || targetKB.DeletedAt.Valid {
		return fmt.Errorf(
			"knowledge %s cannot start a move while source or target knowledge base is deleted",
			knowledge.ID,
		)
	}
	if knowledge.ParseStatus != types.ParseStatusCompleted {
		return fmt.Errorf("knowledge %s is not in completed status (current: %s)", knowledgeID, knowledge.ParseStatus)
	}
	if knowledge.KnowledgeBaseID != sourceKB.ID {
		return fmt.Errorf(
			"knowledge %s moved from source knowledge base %s to %s before move started",
			knowledgeID, sourceKB.ID, knowledge.KnowledgeBaseID,
		)
	}
	if mode == "reparse" {
		// Reject unreconstructable sources before the Completed -> Processing
		// claim and before any vector/chunk/auxiliary cleanup. FAQ knowledge is
		// an aggregate chunk container, not a source document, and therefore has
		// no persistent file from which this mode could rebuild it.
		if err := validateKnowledgeMoveReparseSource(knowledge); err != nil {
			return err
		}
	}
	// Reject a cross-store reuse_vectors move BEFORE mutating status, so a
	// rejected move leaves the knowledge untouched (Completed) rather than
	// stranded in Processing. reuse_vectors copies indices through the source
	// store only; a cross-store copy would corrupt vector data. The handler
	// rejects this synchronously — this is defense-in-depth for directly
	// enqueued tasks. Cross-store moves must use reparse mode.
	if mode == "reuse_vectors" && !sourceKB.SharesStoreWith(targetKB) {
		return fmt.Errorf(
			"reuse_vectors move across different vector stores is not supported "+
				"(source KB %s, target KB %s); use reparse mode", sourceKB.ID, targetKB.ID)
	}
	if mode == "reuse_vectors" {
		if err := s.preflightReuseVectorMove(ctx, tenantID, knowledge, sourceKB); err != nil {
			return err
		}
	}

	// Acquire the long source+target fence before publishing the claim. If a
	// target tombstone wins after preflight, the callback never runs and the
	// source remains byte-for-byte Completed. Once the callback starts, the same
	// fence spans claim, every external/destructive mutation, tag cleanup, and
	// the authoritative target commit.
	fencer, ok := s.repo.(knowledgeMoveScopeFencer)
	if !ok || fencer == nil {
		return errors.New("move knowledge: knowledge-base move scope fence is unavailable")
	}
	err = fencer.WithActiveKnowledgeMoveScope(
		ctx,
		tenantID,
		sourceKB.ID,
		targetKB.ID,
		func() error {
			claimTime := time.Now()
			claimValues := map[string]interface{}{
				"parse_status": types.ParseStatusProcessing,
				"updated_at":   claimTime,
			}
			if mode == "reparse" {
				assignNewDocumentProcessingIdentity(knowledge)
				claimValues["processing_generation"] = knowledge.ProcessingGeneration
				claimValues["processing_owner"] = knowledge.ProcessingOwner
				claimValues["processing_fanout"] = nil
				claimValues["processing_workflow_id"] = ""
				claimValues["pending_subtasks_count"] = 0
				claimValues["enrichment_status"] = types.EnrichmentStatusNone
				claimValues["wiki_status"] = types.WikiStatusNone
				claimValues["wiki_error_message"] = ""
				claimValues["error_message"] = knowledgeMoveTargetCleanupMarker(
					attemptID,
					targetKB.ID,
					knowledge.ProcessingGeneration,
				)
			} else {
				knowledge.ProcessingGeneration = uuid.NewString()
				knowledge.ProcessingOwner = attemptID
				knowledge.ProcessingWorkflowID = ""
				knowledge.ProcessingFanout = nil
				claimValues["processing_generation"] = knowledge.ProcessingGeneration
				claimValues["processing_owner"] = attemptID
				claimValues["processing_fanout"] = nil
				claimValues["processing_workflow_id"] = ""
				claimValues["error_message"] = knowledgeMoveClaimMarker(attemptID, targetKB.ID)
			}
			if claimErr := s.requireKnowledgeStateSwap(
				ctx,
				tenantID,
				knowledge.ID,
				sourceKB.ID,
				types.ParseStatusCompleted,
				claimValues,
				"claim knowledge move",
			); claimErr != nil {
				return claimErr
			}
			knowledge.ParseStatus = types.ParseStatusProcessing
			knowledge.ErrorMessage, _ = claimValues["error_message"].(string)
			knowledge.UpdatedAt = claimTime

			switch mode {
			case "reuse_vectors":
				return s.moveKnowledgeReuseVectorsFenced(ctx, knowledge, sourceKB, targetKB, attemptID)
			case "reparse":
				return s.moveKnowledgeReparseFenced(ctx, knowledge, sourceKB, targetKB, attemptID)
			default:
				return fmt.Errorf("unknown move mode: %s", mode)
			}
		},
	)
	if err != nil {
		if mode == "reuse_vectors" {
			return fmt.Errorf("reuse-vectors move: fenced claim and target commit: %w", err)
		}
		return fmt.Errorf("move knowledge reparse: fenced claim, cleanup and target commit: %w", err)
	}
	if mode == "reuse_vectors" {
		return s.finishKnowledgeMoveReuseVectors(ctx, knowledge, sourceKB, targetKB)
	}
	return s.finishKnowledgeMoveReparse(ctx, knowledge, sourceKB, targetKB, attemptID)
}

// moveKnowledgeReuseVectors moves knowledge by copying vector indices and updating DB references.
func (s *knowledgeService) moveKnowledgeReuseVectors(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB, targetKB *types.KnowledgeBase,
	attemptID string,
) error {
	if knowledge == nil || sourceKB == nil || targetKB == nil || strings.TrimSpace(attemptID) == "" {
		return errors.New("reuse-vectors move: knowledge and both knowledge bases are required")
	}
	if !sourceKB.SharesStoreWith(targetKB) {
		return fmt.Errorf(
			"reuse_vectors move across different vector stores is not supported "+
				"(source KB %s, target KB %s); use reparse mode", sourceKB.ID, targetKB.ID)
	}
	fencer, ok := s.repo.(knowledgeMoveScopeFencer)
	if !ok || fencer == nil {
		return errors.New("reuse-vectors move: knowledge-base move scope fence is unavailable")
	}
	if err := fencer.WithActiveKnowledgeMoveScope(
		ctx,
		knowledge.TenantID,
		sourceKB.ID,
		targetKB.ID,
		func() error {
			return s.moveKnowledgeReuseVectorsFenced(ctx, knowledge, sourceKB, targetKB, attemptID)
		},
	); err != nil {
		return fmt.Errorf("reuse-vectors move: fenced target commit: %w", err)
	}
	return s.finishKnowledgeMoveReuseVectors(ctx, knowledge, sourceKB, targetKB)
}

func (s *knowledgeService) finishKnowledgeMoveReuseVectors(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB, targetKB *types.KnowledgeBase,
) error {
	// Wiki settlement takes its own ordered KB -> knowledge locks. Run it only
	// after the long external-write fence is released so it never attempts to
	// upgrade a parent SHARE lock held by another database connection.
	return s.reconcileWikiAfterKnowledgeMove(ctx, knowledge, sourceKB, targetKB)
}

func (s *knowledgeService) moveKnowledgeReuseVectorsFenced(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB, targetKB *types.KnowledgeBase,
	attemptID string,
) error {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)

	// reuse_vectors copies index entries directly between KBs, which only works
	// inside the same VectorStore backend (CopyIndices is routed through the
	// source store). A cross-store reuse_vectors move would write target-KB rows
	// into the source store and then delete the source indices, corrupting data.
	// The MoveKnowledge handler rejects this up front; this is defense-in-depth
	// for any path that enqueues a move task directly. Cross-store moves must use
	// reparse mode (moveKnowledgeReparse), which re-indexes into the target store.
	if !sourceKB.SharesStoreWith(targetKB) {
		return fmt.Errorf(
			"reuse_vectors move across different vector stores is not supported "+
				"(source KB %s, target KB %s); use reparse mode", sourceKB.ID, targetKB.ID)
	}

	// 1. Snapshot the source-side resources needed for a compensating move.
	oldChunks, err := s.chunkRepo.ListChunksByKnowledgeID(ctx, tenantID, knowledge.ID)
	if err != nil {
		return s.restoreClaimedReuseMove(
			ctx, tenantID, knowledge, sourceKB.ID, attemptID, fmt.Errorf("failed to list chunks: %w", err),
		)
	}
	chunkIDs := make([]string, 0, len(oldChunks))
	for _, c := range oldChunks {
		chunkIDs = append(chunkIDs, c.ID)
	}
	tagMap, err := s.repo.GetKnowledgeTags(ctx, []string{knowledge.ID})
	if err != nil {
		return s.restoreClaimedReuseMove(
			ctx, tenantID, knowledge, sourceKB.ID, attemptID, fmt.Errorf("failed to snapshot knowledge tags: %w", err),
		)
	}
	sourceTagIDs := make([]string, 0, len(tagMap[knowledge.ID]))
	for _, tag := range tagMap[knowledge.ID] {
		if tag != nil {
			sourceTagIDs = append(sourceTagIDs, tag.ID)
		}
	}

	var indexMover *retriever.CompositeRetrieveEngine
	var indexDimension int
	indicesMoved := false
	chunksMoved := false
	tagsCleared := false

	// compensate returns the document to a coherent source-side Completed
	// state. It deliberately runs without the task cancellation signal: a
	// concurrent delete cancels/waits for this move, and cleanup must finish
	// before the delete worker removes the winning lifecycle state.
	compensate := func(cause error) error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		errList := []error{cause}
		rollbackOK := true

		if indicesMoved && indexMover != nil {
			if rollbackErr := indexMover.MoveKnowledgeIndices(
				cleanupCtx,
				targetKB.ID,
				sourceKB.ID,
				knowledge.ID,
				chunkIDs,
				indexDimension,
				sourceKB.Type,
			); rollbackErr != nil {
				rollbackOK = false
				errList = append(errList, fmt.Errorf("rollback moved indices: %w", rollbackErr))
			}
		}
		if chunksMoved {
			if rollbackErr := s.chunkRepo.MoveChunksByKnowledgeID(
				cleanupCtx, tenantID, knowledge.ID, sourceKB.ID,
			); rollbackErr != nil {
				rollbackOK = false
				errList = append(errList, fmt.Errorf("rollback moved chunks: %w", rollbackErr))
			}
		}
		if tagsCleared {
			if rollbackErr := s.repo.SetKnowledgeTags(cleanupCtx, knowledge.ID, sourceTagIDs); rollbackErr != nil {
				rollbackOK = false
				errList = append(errList, fmt.Errorf("rollback knowledge tags: %w", rollbackErr))
			}
		}

		// Never advertise Completed while any external compensation is
		// uncertain. If a concurrent delete already changed the state to
		// deleting, this CAS also refuses to resurrect the row.
		if rollbackOK {
			if rollbackErr := s.requireDocumentProcessingIdentitySwap(
				cleanupCtx,
				knowledge,
				types.ParseStatusProcessing,
				knowledge.ProcessingGeneration,
				attemptID,
				map[string]interface{}{
					"parse_status":     types.ParseStatusCompleted,
					"processing_owner": "",
					"error_message":    "",
					"updated_at":       time.Now(),
				},
				"rollback reuse-vectors knowledge move",
			); rollbackErr != nil {
				rollbackOK = false
				errList = append(errList, fmt.Errorf("rollback knowledge state: %w", rollbackErr))
			}
		}
		joined := errors.Join(errList...)
		if rollbackOK {
			knowledge.ParseStatus = types.ParseStatusCompleted
			knowledge.KnowledgeBaseID = sourceKB.ID
			knowledge.ProcessingOwner = ""
			knowledge.ErrorMessage = ""
			return newKnowledgeMoveFailure(joined, true, false)
		}
		return s.markKnowledgeMoveRecoveryRequired(ctx, tenantID, knowledge, sourceKB.ID, attemptID, joined)
	}

	// 2. Move vector indices with a KB-scoped, compensatable primitive. The
	// old CopyIndices + DeleteByKnowledgeIDList sequence was destructive on
	// shared collections because source and target use the same knowledge ID.
	if len(chunkIDs) > 0 && knowledge.EmbeddingModelID != "" {
		// Same VectorStore backend is guaranteed by the SharesStoreWith guard at
		// the top of this function, so routing the scoped move through the source
		// KB's binding also resolves the target's store.
		var sourceStoreID *string
		if sourceKB != nil {
			sourceStoreID = sourceKB.VectorStoreID
		}
		indexMover, err = retriever.CreateRetrieveEngineForKB(
			ctx, s.retrieveEngine, s.ownership, tenantID, sourceStoreID)
		if err != nil {
			return compensate(fmt.Errorf("failed to init retrieve engine: %w", err))
		}
		embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, knowledge.EmbeddingModelID)
		if err != nil {
			return compensate(fmt.Errorf("failed to get embedding model: %w", err))
		}
		indexDimension = embeddingModel.GetDimensions()
		if err := indexMover.MoveKnowledgeIndices(
			ctx,
			sourceKB.ID,
			targetKB.ID,
			knowledge.ID,
			chunkIDs,
			indexDimension,
			sourceKB.Type,
		); err != nil {
			// The mover has already attempted its own source-side rollback.
			// Restore the DB claim only when every backing engine explicitly
			// reports a complete rollback. Otherwise leave Processing so a
			// delete/recovery pass can clean both scoped locations.
			if retriever.KnowledgeIndexMoveRollbackComplete(err) {
				return compensate(fmt.Errorf("failed to move indices: %w", err))
			}
			return s.markKnowledgeMoveRecoveryRequired(
				ctx,
				tenantID,
				knowledge,
				sourceKB.ID,
				attemptID,
				fmt.Errorf("failed to move indices with uncertain backend state: %w", err),
			)
		}
		indicesMoved = true
	}

	// 3. Update chunks' knowledge_base_id in DB
	if err := s.chunkRepo.MoveChunksByKnowledgeID(ctx, tenantID, knowledge.ID, targetKB.ID); err != nil {
		return compensate(fmt.Errorf("failed to move chunks: %w", err))
	}
	chunksMoved = true

	// 4. Update knowledge record (tags are KB-scoped; clear relations before moving)
	if err := s.repo.DeleteKnowledgeTagRelations(ctx, knowledge.ID); err != nil {
		return compensate(fmt.Errorf("failed to clear knowledge tag relations: %w", err))
	}
	tagsCleared = true
	now := time.Now()
	wikiMarker := knowledgeMoveWikiPendingMarker(attemptID, sourceKB.ID, targetKB.ID)
	if err := s.requireFinalizeReuseVectorKnowledgeMove(
		ctx, knowledge, sourceKB.ID, targetKB.ID, wikiMarker, now,
	); err != nil {
		// A database response can be uncertain after commit. Inspect the
		// authoritative row before reversing external state: if the target
		// Completed transition committed, the move succeeded despite the
		// transport error; if target deletion already won, its cleanup owns
		// the target resources and must not be undone.
		inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		current, inspectErr := s.repo.GetKnowledgeByID(inspectCtx, tenantID, knowledge.ID)
		if inspectErr != nil {
			return s.markKnowledgeMoveRecoveryRequired(
				ctx,
				tenantID,
				knowledge,
				sourceKB.ID,
				attemptID,
				errors.Join(err, fmt.Errorf("inspect knowledge after uncertain move finalization: %w", inspectErr)),
			)
		}
		if current.KnowledgeBaseID == targetKB.ID {
			if current.ParseStatus == types.ParseStatusCompleted {
				knowledge.KnowledgeBaseID = targetKB.ID
				knowledge.ParseStatus = types.ParseStatusCompleted
				knowledge.ErrorMessage = current.ErrorMessage
				knowledge.UpdatedAt = current.UpdatedAt
				return nil
			}
			return newKnowledgeMoveFailure(fmt.Errorf(
				"finalize reuse-vectors knowledge move: target lifecycle advanced to %s: %w",
				current.ParseStatus,
				err,
			), false, true)
		}
		return compensate(err)
	}
	knowledge.KnowledgeBaseID = targetKB.ID
	knowledge.ParseStatus = types.ParseStatusCompleted
	knowledge.ErrorMessage = wikiMarker
	knowledge.UpdatedAt = now

	return nil
}

// moveKnowledgeReparse moves knowledge to target KB and re-parses it with target KB's configuration.
func (s *knowledgeService) moveKnowledgeReparse(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB, targetKB *types.KnowledgeBase,
	attemptID string,
) error {
	if knowledge == nil || sourceKB == nil || targetKB == nil || strings.TrimSpace(attemptID) == "" {
		return errors.New("move knowledge reparse: complete move attempt identity is required")
	}
	fencer, ok := s.repo.(knowledgeMoveScopeFencer)
	if !ok || fencer == nil {
		return errors.New("move knowledge reparse: knowledge-base move scope fence is unavailable")
	}
	if err := fencer.WithActiveKnowledgeMoveScope(
		ctx,
		knowledge.TenantID,
		sourceKB.ID,
		targetKB.ID,
		func() error {
			return s.moveKnowledgeReparseFenced(ctx, knowledge, sourceKB, targetKB, attemptID)
		},
	); err != nil {
		return fmt.Errorf("move knowledge reparse: fenced cleanup and target commit: %w", err)
	}
	return s.finishKnowledgeMoveReparse(ctx, knowledge, sourceKB, targetKB, attemptID)
}

func (s *knowledgeService) finishKnowledgeMoveReparse(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB, targetKB *types.KnowledgeBase,
	attemptID string,
) error {

	// Wiki settlement and task publication take their own ordered locks. Run
	// them only after the long parent scope is released, matching reuse_vectors.
	if sourceKB.ID != targetKB.ID {
		if err := s.reconcileWikiAfterKnowledgeMove(ctx, knowledge, sourceKB, targetKB); err != nil {
			return newKnowledgeMoveFailure(err, false, true)
		}
		if targetKB.DeletedAt.Valid {
			// Whole-target deletion owns this already-committed document. Source
			// Wiki settlement above is still required, but no target parser may be
			// published after the tombstone.
			return nil
		}
		return s.handoffTargetKnowledgeReparse(ctx, knowledge, sourceKB, targetKB, attemptID)
	}

	// Source-side recovery uses its own enqueue_required/queued marker pair.
	if err := s.enqueueMovedKnowledgeReparse(ctx, targetKB, knowledge); err != nil {
		return newKnowledgeMoveFailure(err, false, true)
	}
	return nil
}

func (s *knowledgeService) moveKnowledgeReparseFenced(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB, targetKB *types.KnowledgeBase,
	attemptID string,
) error {
	// Source-side recovery enters from the reuse-vector recovery marker and
	// therefore has not allocated a document-processing generation yet. Publish
	// the exact planned owner before destructive cleanup.
	if strings.TrimSpace(knowledge.ProcessingGeneration) == "" || strings.TrimSpace(knowledge.ProcessingOwner) == "" {
		previousGeneration := knowledge.ProcessingGeneration
		previousOwner := knowledge.ProcessingOwner
		expectedMarker := knowledge.ErrorMessage
		assignNewDocumentProcessingIdentity(knowledge)
		if err := s.requirePrepareKnowledgeMoveReparseRecovery(
			ctx, knowledge, previousGeneration, previousOwner, expectedMarker, expectedMarker,
		); err != nil {
			return err
		}
	}

	// 1. Clean up existing chunks and vector indices
	if err := s.cleanupKnowledgeResourcesWithinMoveFence(ctx, knowledge); err != nil {
		return newKnowledgeMoveFailure(
			fmt.Errorf("moveKnowledgeReparse: cleanup knowledge %s: %w", knowledge.ID, err),
			false,
			true,
		)
	}

	// 2. Update knowledge to belong to target KB. Tags are KB-scoped only for
	// a real cross-KB move; source-side recovery reparses in the same KB and
	// must preserve the user's existing tags.
	if sourceKB.ID != targetKB.ID {
		if err := s.repo.DeleteKnowledgeTagRelations(ctx, knowledge.ID); err != nil {
			return newKnowledgeMoveFailure(
				fmt.Errorf("failed to clear knowledge tag relations: %w", err),
				false,
				true,
			)
		}
	}
	now := time.Now()
	moveErrorMessage := knowledge.ErrorMessage
	if sourceKB.ID != targetKB.ID {
		moveErrorMessage = knowledgeMoveWikiPendingMarker(attemptID, sourceKB.ID, targetKB.ID)
	} else if marker, owned := knowledgeMoveMarkerForAttempt(knowledge.ErrorMessage, attemptID); owned && strings.HasPrefix(marker, knowledgeMoveRecoveryCleanupRequired) {
		recoveryGeneration := strings.TrimPrefix(
			marker,
			knowledgeMoveRecoveryCleanupRequired,
		)
		if recoveryGeneration == "" {
			return newKnowledgeMoveFailure(errors.New(
				"moveKnowledgeReparse: source recovery cleanup marker has no generation",
			), false, true)
		}
		moveErrorMessage = knowledgeMoveAttemptMarker(
			attemptID,
			knowledgeMoveRecoveryReparseRequired+recoveryGeneration,
		)
	}

	var prepared *preparedDocumentWorkflow
	if sourceKB.ID == targetKB.ID {
		var err error
		prepared, err = s.prepareMovedKnowledgeReparse(ctx, targetKB, knowledge)
		if err != nil {
			return newKnowledgeMoveFailure(
				fmt.Errorf("moveKnowledgeReparse: prepare source recovery workflow: %w", err),
				false,
				true,
			)
		}
		if err := prepared.attachKnowledge(knowledge); err != nil {
			return newKnowledgeMoveFailure(err, false, true)
		}
	}
	if err := s.requireFinalizeReparseKnowledgeMove(
		ctx,
		knowledge,
		sourceKB.ID,
		targetKB.ID,
		targetKB.EmbeddingModelID,
		moveErrorMessage,
		prepared,
		now,
	); err != nil {
		return newKnowledgeMoveFailure(err, false, true)
	}
	knowledge.KnowledgeBaseID = targetKB.ID
	knowledge.EmbeddingModelID = targetKB.EmbeddingModelID
	knowledge.ParseStatus = types.ParseStatusPending
	knowledge.ErrorMessage = moveErrorMessage
	knowledge.EnableStatus = "disabled"
	knowledge.Description = ""
	knowledge.ProcessedAt = nil
	knowledge.PendingSubtasksCount = 0
	knowledge.ProcessingFanout = nil
	knowledge.StorageSize = 0
	knowledge.UpdatedAt = now
	if prepared != nil {
		s.dispatchPreparedDocumentWorkflow(ctx, prepared)
	}

	return nil
}

func (s *knowledgeService) handoffTargetKnowledgeReparse(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB, targetKB *types.KnowledgeBase,
	attemptID string,
) error {
	if knowledge == nil || sourceKB == nil || targetKB == nil ||
		knowledge.KnowledgeBaseID != targetKB.ID || knowledge.ParseStatus != types.ParseStatusPending ||
		knowledge.ProcessingGeneration == "" || knowledge.ProcessingOwner == "" ||
		strings.TrimSpace(attemptID) == "" {
		return errors.New("handoff target knowledge reparse: complete pending processing identity is required")
	}
	if strings.TrimSpace(knowledge.ProcessingWorkflowID) != "" {
		// Wiki settlement already committed the exact workflow binding.  Retrying
		// the parent must resume that immutable plan, never synthesize a new root
		// payload/options tuple from mutable KB state.
		return s.resumeBoundDocumentWorkflow(ctx, knowledge)
	}
	return errors.New("handoff target knowledge reparse: Wiki settlement did not bind a durable workflow")
}

func (s *knowledgeService) buildMovedKnowledgeReparseTask(
	ctx context.Context,
	targetKB *types.KnowledgeBase,
	knowledge *types.Knowledge,
) (*asynq.Task, []asynq.Option, error) {
	if targetKB == nil || knowledge == nil || knowledge.TenantID == 0 ||
		strings.TrimSpace(knowledge.KnowledgeBaseID) == "" ||
		strings.TrimSpace(knowledge.ProcessingGeneration) == "" ||
		strings.TrimSpace(knowledge.ProcessingOwner) == "" {
		return nil, nil, errors.New("build moved knowledge reparse: complete processing identity is required")
	}
	if knowledge.IsManual() {
		meta, err := knowledge.ManualMetadata()
		if err != nil || meta == nil {
			return nil, nil, fmt.Errorf("failed to get manual metadata for reparse: %w", err)
		}
		requestID, _ := types.RequestIDFromContext(ctx)
		payload := types.ManualProcessPayload{
			RequestId:            requestID,
			TenantID:             knowledge.TenantID,
			KnowledgeID:          knowledge.ID,
			KnowledgeBaseID:      knowledge.KnowledgeBaseID,
			Content:              meta.Content,
			NeedCleanup:          false,
			ProcessingGeneration: knowledge.ProcessingGeneration,
			ProcessingOwner:      knowledge.ProcessingOwner,
		}
		langfuse.InjectTracing(ctx, &payload)
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal manual reparse payload: %w", err)
		}
		opts := []asynq.Option{asynq.Queue(types.QueueDefault), asynq.MaxRetry(3)}
		if moveTaskID := knowledgeMoveReparseTaskID(knowledge, targetKB.ID); moveTaskID != "" {
			opts = append(opts, asynq.TaskID(moveTaskID))
		}
		return asynq.NewTask(types.TypeManualProcess, payloadBytes), opts, nil
	}

	if knowledge.FilePath != "" {
		enableMultimodel := targetKB.IsMultimodalEnabled()
		enableQuestionGeneration := false
		questionCount := types.DefaultQuestionGenerationCount
		if targetKB.QuestionGenerationConfig != nil && targetKB.QuestionGenerationConfig.Enabled {
			enableQuestionGeneration = true
			questionCount = types.NormalizeQuestionGenerationCount(
				targetKB.QuestionGenerationConfig.QuestionCount,
			)
		}

		lang, _ := types.LanguageFromContext(ctx)
		taskPayload := types.DocumentProcessPayload{
			TenantID:                 knowledge.TenantID,
			KnowledgeID:              knowledge.ID,
			KnowledgeBaseID:          targetKB.ID,
			FilePath:                 knowledge.FilePath,
			FileName:                 knowledge.FileName,
			FileType:                 getFileType(knowledge.FileName),
			EnableMultimodel:         enableMultimodel,
			EnableQuestionGeneration: enableQuestionGeneration,
			QuestionCount:            questionCount,
			Language:                 lang,
		}
		documentProcessOwnershipPayload(knowledge, &taskPayload)

		langfuse.InjectTracing(ctx, &taskPayload)
		payloadBytes, err := json.Marshal(taskPayload)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal document process payload: %w", err)
		}

		opts := documentProcessTaskOptionsForQueue(
			s.config,
			s.documentQueueForStoredDocument(ctx, targetKB, knowledge, taskPayload),
			asynq.MaxRetry(3),
		)
		if moveTaskID := knowledgeMoveReparseTaskID(knowledge, targetKB.ID); moveTaskID != "" {
			opts = append(opts, asynq.TaskID(moveTaskID))
		}
		return asynq.NewTask(types.TypeDocumentProcess, payloadBytes), opts, nil
	}
	return nil, nil, fmt.Errorf("cannot reparse knowledge %s: source file path is empty", knowledge.ID)
}

func (s *knowledgeService) prepareMovedKnowledgeReparse(
	ctx context.Context,
	targetKB *types.KnowledgeBase,
	knowledge *types.Knowledge,
) (*preparedDocumentWorkflow, error) {
	task, opts, err := s.buildMovedKnowledgeReparseTask(ctx, targetKB, knowledge)
	if err != nil {
		return nil, err
	}
	return s.prepareDocumentWorkflow(ctx, task, opts...)
}

// enqueueMovedKnowledgeReparse is intentionally resume-only.  A move must
// first bind its exact workflow at the business-ready transaction boundary
// (same-source finalization or cross-KB Wiki settlement); rebuilding a task
// here would reintroduce the crash gap and can conflict with immutable tracing
// or producer TaskID fields in the persisted plan.
func (s *knowledgeService) enqueueMovedKnowledgeReparse(
	ctx context.Context,
	_ *types.KnowledgeBase,
	knowledge *types.Knowledge,
) error {
	if knowledge == nil || strings.TrimSpace(knowledge.ProcessingWorkflowID) == "" {
		return errors.New("resume moved knowledge reparse: durable workflow binding is missing")
	}
	if err := s.resumeBoundDocumentWorkflow(ctx, knowledge); err != nil {
		return fmt.Errorf("resume moved knowledge reparse: %w", err)
	}
	return nil
}

// getOrCreateTagInTarget finds or creates a tag in the target knowledge base based on the source tag.
// It looks up the source tag by ID, then tries to find a tag with the same name in the target KB.
// If not found, it creates a new tag with the same properties.
// The mapping is cached in tagIDMapping for subsequent lookups.
func (s *knowledgeService) getOrCreateTagInTarget(
	ctx context.Context,
	srcTenantID, dstTenantID uint64,
	dstKnowledgeBaseID string,
	srcTagID string,
	tagIDMapping map[string]string,
) string {
	// Get source tag
	srcTag, err := s.tagRepo.GetByID(ctx, srcTenantID, srcTagID)
	if err != nil || srcTag == nil {
		logger.Warnf(ctx, "Failed to get source tag %s: %v", srcTagID, err)
		tagIDMapping[srcTagID] = "" // Cache empty result to avoid repeated lookups
		return ""
	}

	// Try to find existing tag with same name in target KB
	dstTag, err := s.tagRepo.GetByName(ctx, dstTenantID, dstKnowledgeBaseID, srcTag.Name)
	if err == nil && dstTag != nil {
		tagIDMapping[srcTagID] = dstTag.ID
		return dstTag.ID
	}

	// Create new tag in target KB
	// "未分类" tag should have the lowest sort order to appear first
	sortOrder := srcTag.SortOrder
	if srcTag.Name == types.UntaggedTagName {
		sortOrder = -1
	}
	newTag := &types.KnowledgeTag{
		ID:              uuid.New().String(),
		TenantID:        dstTenantID,
		KnowledgeBaseID: dstKnowledgeBaseID,
		Name:            srcTag.Name,
		Color:           srcTag.Color,
		SortOrder:       sortOrder,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	if err := s.tagRepo.Create(ctx, newTag); err != nil {
		logger.Warnf(ctx, "Failed to create tag %s in target KB: %v", srcTag.Name, err)
		tagIDMapping[srcTagID] = "" // Cache empty result
		return ""
	}

	tagIDMapping[srcTagID] = newTag.ID
	logger.Infof(ctx, "Created tag %s (ID: %s) in target KB %s", newTag.Name, newTag.ID, dstKnowledgeBaseID)
	return newTag.ID
}
