package service

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	apprepo "github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/custom/modules/fileguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/infrastructure/chunker"
	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

const (
	splitManifestMaxBytes = 16 * 1024 * 1024
	splitChunkIndexStride = 1_000_000
	splitPositionStride   = 1_000_000_000
	splitBoundaryRunes    = 512
)

type auxiliaryStreamSaver interface {
	SaveReader(
		context.Context, io.ReadSeeker, int64, string, uint64, string,
	) (string, error)
}

type splitImageMapping struct {
	ChunkID    string `json:"chunk_id"`
	ImageURL   string `json:"image_url"`
	SourceType string `json:"source_type,omitempty"`
}

func (s *knowledgeService) prepareDocumentSplit(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	payload types.DocumentProcessPayload,
	report fileguard.Report,
) error {
	if s.splitManager == nil {
		return errors.New("large document requires physical splitting, but split manager is unavailable")
	}
	if existing, err := s.splitManager.GetPlanForGeneration(
		ctx, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration,
	); err == nil {
		if existing.SourceSize != knowledge.FileSize {
			return errors.New("existing document split plan does not match the stored source")
		}
		return s.splitManager.DispatchPlan(ctx, existing.ID)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("load existing document split plan: %w", err)
	}

	splitter, ok := s.documentReader.(interfaces.DocumentSplitter)
	if !ok || splitter == nil {
		return errors.New("large document requires the streaming format-aware splitter")
	}
	sourceService, err := s.auxiliaryFileServiceForPath(
		ctx, kb, knowledge.KnowledgeBaseID, knowledge.ID, payload.FilePath,
	)
	if err != nil {
		return fmt.Errorf("resolve split source storage: %w", err)
	}
	source, err := sourceService.GetFile(ctx, payload.FilePath)
	if err != nil {
		return fmt.Errorf("open split source: %w", err)
	}
	stagedSource, err := os.CreateTemp("", "weknora-document-source-*")
	if err != nil {
		_ = source.Close()
		return fmt.Errorf("create split source stage: %w", err)
	}
	stagedSourceName := stagedSource.Name()
	defer func() {
		_ = stagedSource.Close()
		_ = os.Remove(stagedSourceName)
	}()
	sourceDigest := sha256.New()
	written, copyErr := io.Copy(
		io.MultiWriter(stagedSource, sourceDigest),
		io.LimitReader(source, knowledge.FileSize+1),
	)
	closeErr := source.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("stage split source: %w", errors.Join(copyErr, closeErr))
	}
	if written != knowledge.FileSize {
		return fmt.Errorf(
			"stored split source size mismatch: read %d bytes, expected %d",
			written, knowledge.FileSize,
		)
	}
	sourceSHA256 := hex.EncodeToString(sourceDigest.Sum(nil))
	if _, err := stagedSource.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind split source: %w", err)
	}

	archive, err := os.CreateTemp("", "weknora-document-split-*.zip")
	if err != nil {
		return fmt.Errorf("create split archive stage: %w", err)
	}
	archiveName := archive.Name()
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archiveName)
	}()

	result, err := splitter.Split(ctx, &types.DocumentSplitRequest{
		FileName: payload.FileName, FileType: payload.FileType,
		SourceSize: knowledge.FileSize, SourceSHA256: sourceSHA256,
		MinimumParts: report.RequiredParts,
		TargetRatio:  s.splitManager.Config().TargetRatio,
		RequestID:    payload.RequestId,
		Source:       stagedSource, Destination: archive,
	})
	if err != nil {
		return fmt.Errorf("split logical document: %w", err)
	}
	if err := archive.Sync(); err != nil {
		return fmt.Errorf("sync split archive: %w", err)
	}
	info, err := archive.Stat()
	if err != nil {
		return err
	}
	if info.Size() != result.ArchiveSize {
		return errors.New("split archive size changed after transport verification")
	}

	manifest, entries, err := validateSplitArchive(
		archive, info.Size(), knowledge, result,
		sourceSHA256, payload.FileName, payload.FileType,
		s.splitManager.Config().ArchiveMaxParts,
		s.splitManager.Config().MaxExpansionRatio,
	)
	if err != nil {
		return err
	}
	partStorage, err := s.plannedAuxiliaryFileService(
		ctx, kb, knowledge, knowledgeaux.KindSplitInput,
	)
	if err != nil {
		return fmt.Errorf("prepare split part storage: %w", err)
	}
	streamStorage, ok := partStorage.(auxiliaryStreamSaver)
	if !ok || streamStorage == nil {
		return errors.New("split part storage does not support bounded streaming")
	}

	parts := make([]*documentsplit.Part, 0, len(manifest.Parts))
	storedPaths := make([]string, 0, len(manifest.Parts))
	cleanupStored := func(cause error) error {
		if len(storedPaths) == 0 || s.auxObjects == nil {
			return cause
		}
		cleanupErr := s.auxObjects.DeletePaths(
			context.WithoutCancel(ctx), knowledge.TenantID, knowledge.KnowledgeBaseID,
			knowledge.ID, effectiveAuxiliaryProvider(ctx, kb), storedPaths,
		)
		return errors.Join(cause, cleanupErr)
	}
	for index, metadata := range manifest.Parts {
		entry := entries[index]
		staged, err := stageAndVerifySplitEntry(entry, metadata)
		if err != nil {
			return cleanupStored(err)
		}
		stagedName := staged.Name()
		path, saveErr := streamStorage.SaveReader(
			ctx, staged, metadata.SizeBytes,
			secutils.GetContentTypeByExt("."+metadata.FileType),
			knowledge.TenantID, metadata.FileName,
		)
		_ = staged.Close()
		_ = os.Remove(stagedName)
		if saveErr != nil {
			return cleanupStored(fmt.Errorf("store split part %d: %w", index, saveErr))
		}
		storedPaths = append(storedPaths, path)
		locator := types.JSON(append([]byte(nil), metadata.Locator...))
		metrics := types.JSON(append([]byte(nil), metadata.Metrics...))
		parts = append(parts, &documentsplit.Part{
			PartIndex: index, FileName: metadata.FileName, FileType: metadata.FileType,
			InputPath: path, InputSize: metadata.SizeBytes, InputSHA256: metadata.SHA256,
			Locator: locator, Metrics: metrics,
		})
	}

	rules, _ := json.Marshal(struct {
		Reasons []string       `json:"reasons"`
		Metrics map[string]any `json:"metrics"`
	}{report.SplitReasons, report.Metrics})
	rulesDigest := sha256.Sum256(rules)
	plan, err := s.splitManager.CreatePlan(ctx, &documentsplit.Plan{
		TenantID: knowledge.TenantID, KnowledgeBaseID: knowledge.KnowledgeBaseID,
		KnowledgeID: knowledge.ID, ProcessingGeneration: knowledge.ProcessingGeneration,
		ProcessingOwner: knowledge.ProcessingOwner,
		SourcePath:      payload.FilePath, SourceName: payload.FileName,
		SourceType: payload.FileType, SourceSize: knowledge.FileSize,
		SourceSHA256: sourceSHA256, PlannerVersion: manifest.PlannerVersion,
		RulesHash: hex.EncodeToString(rulesDigest[:]), Strategy: manifest.Strategy,
		TotalPartBytes: manifest.TotalPartBytes, TargetRatio: manifest.TargetRatio,
	}, parts)
	if err != nil {
		return cleanupStored(fmt.Errorf("persist split plan: %w", err))
	}
	logger.Infof(ctx,
		"[document split] planned logical document knowledge=%s strategy=%s parts=%d source_bytes=%d",
		knowledge.ID, plan.Strategy, plan.PartCount, plan.SourceSize,
	)
	return s.splitManager.DispatchPlan(ctx, plan.ID)
}

func validateSplitArchive(
	archive *os.File,
	archiveSize int64,
	knowledge *types.Knowledge,
	result *types.DocumentSplitResult,
	sourceSHA256, sourceName, sourceType string,
	maxParts int,
	maxExpansionRatio float64,
) (documentsplit.Manifest, map[int]*zip.File, error) {
	var manifest documentsplit.Manifest
	if archive == nil || knowledge == nil || result == nil || archiveSize <= 0 {
		return manifest, nil, errors.New("invalid split archive input")
	}
	reader, err := zip.NewReader(archive, archiveSize)
	if err != nil {
		return manifest, nil, fmt.Errorf("open split archive: %w", err)
	}
	var manifestEntry *zip.File
	partEntries := make(map[string]*zip.File)
	var expanded int64
	if maxExpansionRatio < 1 || maxExpansionRatio > 100 {
		maxExpansionRatio = 12
	}
	maxExpanded := max(
		int64(512*1024*1024),
		int64(float64(knowledge.FileSize)*maxExpansionRatio),
	)
	for _, entry := range reader.File {
		clean := filepath.ToSlash(filepath.Clean(entry.Name))
		if clean != entry.Name || strings.HasPrefix(clean, "/") ||
			clean == "." || strings.HasPrefix(clean, "../") || entry.FileInfo().IsDir() {
			return manifest, nil, fmt.Errorf("split archive contains unsafe entry %q", entry.Name)
		}
		expanded += int64(entry.UncompressedSize64)
		if expanded > maxExpanded {
			return manifest, nil, errors.New("split archive expansion exceeds safety bound")
		}
		switch {
		case clean == "manifest.json":
			if manifestEntry != nil || entry.UncompressedSize64 > splitManifestMaxBytes {
				return manifest, nil, errors.New("split archive has an invalid manifest")
			}
			manifestEntry = entry
		case strings.HasPrefix(clean, "parts/") && filepath.Base(clean) != "":
			if _, duplicate := partEntries[clean]; duplicate {
				return manifest, nil, fmt.Errorf("duplicate split archive entry %q", clean)
			}
			partEntries[clean] = entry
		default:
			return manifest, nil, fmt.Errorf("unexpected split archive entry %q", clean)
		}
	}
	if manifestEntry == nil {
		return manifest, nil, errors.New("split archive manifest is missing")
	}
	stream, err := manifestEntry.Open()
	if err != nil {
		return manifest, nil, err
	}
	decoder := json.NewDecoder(io.LimitReader(stream, splitManifestMaxBytes+1))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&manifest)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); !errors.Is(trailingErr, io.EOF) {
			if trailingErr == nil {
				err = errors.New("split archive manifest contains trailing JSON")
			} else {
				err = trailingErr
			}
		}
	}
	closeErr := stream.Close()
	if err != nil || closeErr != nil {
		return manifest, nil, fmt.Errorf("decode split archive manifest: %w", errors.Join(err, closeErr))
	}
	if manifest.SchemaVersion != 1 || manifest.PlannerVersion == "" ||
		manifest.PlannerVersion != result.PlannerVersion ||
		manifest.PartCount != result.PartCount ||
		manifest.PartCount != len(manifest.Parts) ||
		manifest.PartCount <= 0 || manifest.PartCount > maxParts ||
		manifest.Source.SizeBytes != knowledge.FileSize ||
		!strings.EqualFold(manifest.Source.SHA256, sourceSHA256) ||
		!strings.EqualFold(
			strings.TrimPrefix(manifest.Source.FileType, "."),
			strings.TrimPrefix(sourceType, "."),
		) ||
		filepath.Base(manifest.Source.FileName) != filepath.Base(sourceName) {
		return manifest, nil, errors.New("split archive manifest identity mismatch")
	}
	entries := make(map[int]*zip.File, len(manifest.Parts))
	var total int64
	for index, part := range manifest.Parts {
		expectedName := fmt.Sprintf("part-%06d.", index+1)
		partType := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(part.FileType), "."))
		expectedExtension := partType
		switch partType {
		case "text":
			expectedExtension = "txt"
		case "markdown":
			expectedExtension = "md"
		}
		hardLimit, supportedType := fileguard.HardSizeLimit(partType)
		shaBytes, shaErr := hex.DecodeString(part.SHA256)
		if part.Index != index || !strings.HasPrefix(part.FileName, expectedName) ||
			filepath.Base(part.FileName) != part.FileName ||
			strings.TrimPrefix(strings.ToLower(filepath.Ext(part.FileName)), ".") != expectedExtension ||
			!supportedType || part.SizeBytes <= 0 || part.SizeBytes > hardLimit ||
			shaErr != nil || len(shaBytes) != sha256.Size ||
			!validSplitManifestObject(part.Locator) ||
			!validSplitManifestObject(part.Metrics) {
			return manifest, nil, fmt.Errorf("split manifest part %d is invalid", index)
		}
		entry, ok := partEntries["parts/"+part.FileName]
		if !ok || entry.Method != zip.Store || entry.Flags&0x1 != 0 ||
			int64(entry.UncompressedSize64) != part.SizeBytes {
			return manifest, nil, fmt.Errorf("split archive part %d is missing or has wrong size", index)
		}
		entries[index] = entry
		total += part.SizeBytes
	}
	if len(partEntries) != len(manifest.Parts) || total != manifest.TotalPartBytes {
		return manifest, nil, errors.New("split archive part totals do not match manifest")
	}
	return manifest, entries, nil
}

func validSplitManifestObject(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var value map[string]any
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func stageAndVerifySplitEntry(
	entry *zip.File, metadata documentsplit.ManifestPart,
) (*os.File, error) {
	source, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer source.Close()
	staged, err := os.CreateTemp("", "weknora-document-part-*")
	if err != nil {
		return nil, err
	}
	failed := func(cause error) (*os.File, error) {
		name := staged.Name()
		_ = staged.Close()
		_ = os.Remove(name)
		return nil, cause
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(staged, digest), io.LimitReader(source, metadata.SizeBytes+1))
	if err != nil {
		return failed(err)
	}
	if written != metadata.SizeBytes || !strings.EqualFold(
		hex.EncodeToString(digest.Sum(nil)), metadata.SHA256,
	) {
		return failed(fmt.Errorf("split part %d hash or size mismatch", metadata.Index))
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return failed(err)
	}
	return staged, nil
}

func (s *knowledgeService) ProcessDocumentSplitPart(
	ctx context.Context, task *asynq.Task,
) (retErr error) {
	if s.splitManager == nil {
		return errors.New("document split manager is unavailable")
	}
	var payload documentsplit.PartPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("decode document split part payload: %w", err)
	}
	if payload.TenantID == 0 || payload.KnowledgeID == "" ||
		payload.KnowledgeBaseID == "" || payload.ProcessingGeneration == "" ||
		payload.PlanID == "" || payload.PartID == "" || payload.PartIndex < 0 {
		return errors.New("document split part payload has incomplete identity")
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	partCtx, cancelPart := context.WithCancel(ctx)
	releaseExecution, err := s.splitManager.RegisterPartExecution(cancelPart)
	if err != nil {
		cancelPart()
		return err
	}
	defer releaseExecution()
	defer cancelPart()

	part, epoch, err := s.splitManager.ClaimPart(partCtx, payload)
	if errors.Is(err, documentsplit.ErrStalePart) ||
		errors.Is(err, documentsplit.ErrPartLeased) {
		return nil
	}
	if err != nil {
		return err
	}
	terminalAttempt := part.Attempt >= s.splitManager.Config().MaxRetry
	leaseCompleted := false
	defer func() {
		if leaseCompleted {
			return
		}
		// A stale/cancelled parent is an acknowledged delivery, but the durable
		// part lease still needs to be released immediately. Otherwise delete
		// and cancel barriers wait for the full lease duration even though this
		// worker has already stopped.
		releaseCause := retErr
		if releaseCause == nil {
			releaseCause = documentsplit.ErrStalePart
		}
		releaseErr := s.splitManager.ReleasePart(
			context.WithoutCancel(ctx), part, epoch, releaseCause,
		)
		if retErr != nil {
			retErr = errors.Join(retErr, releaseErr)
		} else if releaseErr != nil && !errors.Is(releaseErr, documentsplit.ErrLeaseLost) {
			retErr = releaseErr
		}
		if terminalAttempt && retErr != nil &&
			!isDurableTaskDeferred(retErr) {
			if knowledge, loadErr := s.repo.GetKnowledgeByID(
				context.WithoutCancel(ctx), payload.TenantID, payload.KnowledgeID,
			); loadErr == nil && knowledge != nil &&
				knowledge.ProcessingGeneration == payload.ProcessingGeneration {
				retErr = errors.Join(retErr, s.markActiveProcessingFailed(
					context.WithoutCancel(ctx), knowledge,
					fmt.Sprintf("physical part %d failed after %d attempts: %v",
						payload.PartIndex+1, part.Attempt, retErr),
					"mark terminal document split part failure",
				))
			}
			// The durable failure is already visible; ACK this delivery instead
			// of creating a second terminal transition in generic dead-letter
			// middleware.
			retErr = nil
		}
	}()

	tenant, err := s.tenantRepo.GetTenantByID(partCtx, payload.TenantID)
	if err != nil {
		return fmt.Errorf("load split part tenant: %w", err)
	}
	partCtx = context.WithValue(partCtx, types.TenantInfoContextKey, tenant)
	knowledge, err := s.repo.GetKnowledgeByID(partCtx, payload.TenantID, payload.KnowledgeID)
	if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load split part knowledge: %w", err)
	}
	if knowledge.KnowledgeBaseID != payload.KnowledgeBaseID ||
		knowledge.ProcessingGeneration != payload.ProcessingGeneration ||
		knowledge.ParseStatus != types.ParseStatusProcessing ||
		knowledge.ProcessingOwner == "" {
		return nil
	}
	if err := s.heartbeatActiveProcessing(partCtx, knowledge, "physical split part claimed"); err != nil {
		if errors.Is(err, errKnowledgeStateFenceConflict) {
			return nil
		}
		return err
	}
	kb, err := s.kbService.GetKnowledgeBaseByID(partCtx, payload.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("load split part knowledge base: %w", err)
	}
	if kb == nil || kb.TenantID != payload.TenantID {
		return errors.New("split part knowledge base identity mismatch")
	}

	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		interval := s.splitManager.Config().LeaseDuration / 3
		if interval < 10*time.Second {
			interval = 10 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-partCtx.Done():
				return
			case <-ticker.C:
				if hbErr := s.splitManager.HeartbeatPart(
					context.WithoutCancel(partCtx), part.ID, epoch,
				); hbErr != nil {
					select {
					case heartbeatErr <- hbErr:
					default:
					}
					cancelPart()
					return
				}
				_ = s.heartbeatActiveProcessing(
					context.WithoutCancel(partCtx), knowledge, "physical split part heartbeat",
				)
			}
		}
	}()
	defer func() {
		cancelPart()
		<-heartbeatDone
	}()

	eff := ResolveProcessConfig(kb, func() *types.KnowledgeProcessOverrides {
		overrides, _ := knowledge.ProcessOverrides()
		return overrides
	}())
	budgetedChunkConfig := s.applyEmbeddingTokenBudget(
		partCtx,
		kb,
		knowledge.Title,
		buildSplitterConfigFromChunking(eff.ChunkingConfig),
	)
	eff.ChunkingConfig.TokenLimit = budgetedChunkConfig.TokenLimit
	result, storedImages, err := s.parsePhysicalDocumentPart(
		partCtx, kb, knowledge, part, eff,
	)
	if err != nil {
		select {
		case hbErr := <-heartbeatErr:
			return errors.Join(err, hbErr)
		default:
			return err
		}
	}
	chunks, imageMappings, markdownChars, err := buildPhysicalPartChunks(
		knowledge, part, eff, result, storedImages,
	)
	if err != nil {
		return err
	}
	if err := s.heartbeatActiveProcessing(
		partCtx, knowledge, "physical split part ready to stage",
	); err != nil {
		if errors.Is(err, errKnowledgeStateFenceConflict) {
			return nil
		}
		return err
	}
	if err := s.splitManager.HeartbeatPart(partCtx, part.ID, epoch); err != nil {
		return fmt.Errorf("fence split part before staging: %w", err)
	}

	var engine *retriever.CompositeRetrieveEngine
	var embedder embedding.Embedder
	if kb.NeedsEmbeddingModel() {
		model, modelErr := s.modelService.GetEmbeddingModel(partCtx, kb.EmbeddingModelID)
		if modelErr != nil {
			return fmt.Errorf("load split part embedding model: %w", modelErr)
		}
		embedder = model
		engine, err = retriever.CreateRetrieveEngineForKB(
			partCtx, s.retrieveEngine, s.ownership, tenant.ID, kb.VectorStoreID,
		)
		if err != nil {
			return fmt.Errorf("resolve split part retrieve engine: %w", err)
		}
		oldIDs, listErr := s.splitManager.ListPartChunkIDs(
			partCtx, part.TenantID, part.KnowledgeID,
			part.ProcessingGeneration, part.PartIndex,
		)
		if listErr != nil {
			return listErr
		}
		if len(oldIDs) > 0 {
			if err := s.splitManager.HeartbeatPart(partCtx, part.ID, epoch); err != nil {
				return fmt.Errorf("fence split part before replacing prior index: %w", err)
			}
			if deleteErr := engine.DeleteByChunkIDList(
				partCtx, oldIDs, model.GetDimensions(), knowledge.Type,
			); deleteErr != nil {
				return fmt.Errorf("delete prior split part index: %w", deleteErr)
			}
		}
	}
	if err := s.splitManager.HeartbeatPart(partCtx, part.ID, epoch); err != nil {
		return fmt.Errorf("fence split part before replacing chunks: %w", err)
	}
	if err := s.splitManager.DeletePartChunks(
		partCtx, part.TenantID, part.KnowledgeID,
		part.ProcessingGeneration, part.PartIndex,
	); err != nil {
		return fmt.Errorf("delete prior split part chunks: %w", err)
	}
	if len(chunks) > 0 {
		if err := s.chunkService.CreateChunks(partCtx, chunks); err != nil {
			return fmt.Errorf("stage split part chunks: %w", err)
		}
	}

	var storageBytes int64
	if engine != nil && embedder != nil {
		batchSize := s.splitManager.Config().FinalizeBatchSize
		for start := 0; start < len(chunks); start += batchSize {
			end := min(start+batchSize, len(chunks))
			indexInfo := make([]*types.IndexInfo, 0, end-start)
			for _, chunk := range chunks[start:end] {
				if chunk.ChunkType != types.ChunkTypeText {
					continue
				}
				content := chunk.EmbeddingContent()
				if title := strings.TrimSpace(knowledge.Title); title != "" {
					content = title + "\n" + content
				}
				indexInfo = append(indexInfo, &types.IndexInfo{
					Content: content, SourceID: chunk.ID, SourceType: types.ChunkSourceType,
					ChunkID: chunk.ID, KnowledgeID: knowledge.ID,
					KnowledgeBaseID: knowledge.KnowledgeBaseID, IsEnabled: false,
				})
			}
			if len(indexInfo) == 0 {
				continue
			}
			if err := s.splitManager.HeartbeatPart(partCtx, part.ID, epoch); err != nil {
				return fmt.Errorf("fence split part before vector batch: %w", err)
			}
			storageBytes += engine.EstimateStorageSize(partCtx, embedder, indexInfo)
			if err := engine.BatchIndex(partCtx, embedder, indexInfo); err != nil {
				return fmt.Errorf("stage split part vector batch: %w", err)
			}
		}
	}
	firstID, lastID := firstAndLastTextChunk(chunks)
	mappings, err := json.Marshal(imageMappings)
	if err != nil {
		return err
	}
	if err := s.heartbeatActiveProcessing(
		partCtx, knowledge, "physical split part ready to commit",
	); err != nil {
		if errors.Is(err, errKnowledgeStateFenceConflict) {
			return nil
		}
		return err
	}
	if err := s.splitManager.HeartbeatPart(partCtx, part.ID, epoch); err != nil {
		return fmt.Errorf("fence split part before completion: %w", err)
	}
	_, err = s.splitManager.CompletePart(partCtx, part, epoch, documentsplit.PartCompletion{
		MarkdownChars: markdownChars, ChunkCount: countTextChunks(chunks),
		StorageBytes: storageBytes, FirstChunkID: firstID, LastChunkID: lastID,
		ImageMappings: types.JSON(mappings),
	})
	if err == nil {
		leaseCompleted = true
	}
	return err
}

func (s *knowledgeService) parsePhysicalDocumentPart(
	ctx context.Context,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	part *documentsplit.Part,
	eff types.EffectiveProcessConfig,
) (*types.ReadResult, []docparser.StoredImage, error) {
	fileService, err := s.auxiliaryFileServiceForPath(
		ctx, kb, knowledge.KnowledgeBaseID, knowledge.ID, part.InputPath,
	)
	if err != nil {
		return nil, nil, err
	}
	stream, err := fileService.GetFile(ctx, part.InputPath)
	if err != nil {
		return nil, nil, err
	}
	defer stream.Close()
	// MAX_FILE_SIZE_MB is the remote parser transport ceiling. Simple
	// formats (notably 100 MB audio formats) are parsed in-process and are
	// governed by their stricter format-specific preflight instead.
	if !docparser.IsSimpleFormat(part.FileType) &&
		part.InputSize > int64(secutils.GetMaxFileSize()) {
		return nil, nil, fmt.Errorf("split part %d exceeds parser transport ceiling", part.PartIndex)
	}
	digest := sha256.New()
	content, err := io.ReadAll(io.TeeReader(
		io.LimitReader(stream, part.InputSize+1), digest,
	))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(content)) != part.InputSize ||
		!strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), part.InputSHA256) {
		return nil, nil, errors.New("stored split part hash or size mismatch")
	}
	guard := fileguard.AnalyzeBytes(part.FileName, part.FileType, content)
	if err := guard.ValidationError(); err != nil {
		return nil, nil, fmt.Errorf("split part safety preflight: %w", err)
	}
	if guard.NeedsSplit() {
		return nil, nil, fmt.Errorf(
			"splitter produced a part that still exceeds parser workload limits: %s",
			strings.Join(guard.SplitReasons, "；"),
		)
	}

	processOverrides, _ := knowledge.ProcessOverrides()
	var uploadOverrides map[string]string
	if processOverrides != nil {
		uploadOverrides = processOverrides.ParserEngineOverrides
	}
	overrides := MergeParserEngineOverrides(
		s.getParserEngineOverridesFromContext(ctx), uploadOverrides,
	)
	parserEngine := eff.ChunkingConfig.ResolveParserEngine(part.FileType)
	reader := s.resolveDocReader(ctx, parserEngine, part.FileType, false, overrides)
	if reader == nil {
		return nil, nil, errors.New("no parser is available for physical split part")
	}
	result, err := s.callDocReaderWithTimeout(ctx, reader, &types.ReadRequest{
		FileContent: content, FileName: part.FileName, FileType: part.FileType,
		Title: knowledge.Title, ParserEngine: parserEngine,
		RequestID:             fmt.Sprintf("split:%s:%06d", part.PlanID, part.PartIndex),
		ParserEngineOverrides: overrides,
	})
	content = nil
	if err != nil {
		return nil, nil, fmt.Errorf("parse physical split part %d: %w", part.PartIndex, err)
	}
	if result == nil || result.Error != "" {
		if result == nil {
			return nil, nil, errors.New("physical split parser returned no result")
		}
		return nil, nil, errors.New(result.Error)
	}
	if result.IsAudio && len(result.AudioData) > 0 {
		if !eff.ASRConfig.IsASREnabled() {
			return nil, nil, errors.New("ASR model is not configured for audio split parts")
		}
		asrModel, err := s.modelService.GetASRModel(ctx, eff.ASRConfig.ModelID)
		if err != nil {
			return nil, nil, err
		}
		transcription, err := asrModel.Transcribe(ctx, result.AudioData, part.FileName)
		if err != nil {
			return nil, nil, err
		}
		if transcription == nil || strings.TrimSpace(transcription.Text) == "" {
			result.MarkdownContent = "[No speech detected in this source time range]"
		} else {
			result.MarkdownContent = transcription.Text
		}
		result.IsAudio = false
		result.AudioData = nil
	}

	var stored []docparser.StoredImage
	if s.imageResolver != nil {
		imageStorage, err := s.plannedAuxiliaryFileService(
			ctx, kb, knowledge, knowledgeaux.KindSplitImage,
		)
		if err != nil {
			return nil, nil, err
		}
		updated, localImages, resolveErr := s.imageResolver.ResolveAndStore(
			ctx, result, imageStorage, knowledge.TenantID,
		)
		if updated != "" {
			result.MarkdownContent = updated
		}
		if resolveErr != nil {
			logger.Warnf(ctx, "split part inline image resolution degraded: %v", resolveErr)
		}
		updated, remoteImages, remoteErr := s.imageResolver.ResolveRemoteImages(
			ctx, result.MarkdownContent, imageStorage, knowledge.TenantID,
		)
		if updated != "" {
			result.MarkdownContent = updated
		}
		if remoteErr != nil {
			logger.Warnf(ctx, "split part remote image resolution degraded: %v", remoteErr)
		}
		stored = append(stored, localImages...)
		stored = append(stored, remoteImages...)
	}
	return result, stored, nil
}

func buildPhysicalPartChunks(
	knowledge *types.Knowledge,
	part *documentsplit.Part,
	eff types.EffectiveProcessConfig,
	result *types.ReadResult,
	storedImages []docparser.StoredImage,
) ([]*types.Chunk, []splitImageMapping, int64, error) {
	if result == nil {
		return nil, nil, 0, errors.New("physical part result is nil")
	}
	var locator map[string]any
	if err := json.Unmarshal(part.Locator, &locator); err != nil {
		return nil, nil, 0, fmt.Errorf("decode physical part locator: %w", err)
	}
	content, inherited := normalizePhysicalPartMarkdown(result.MarkdownContent, locator)
	logicalHeader := logicalLocatorHeader(locator)
	if inherited != "" {
		logicalHeader += "\n" + inherited
	}
	logicalHeader = strings.TrimSpace(logicalHeader)
	locatorRefiner, err := documentsplit.NewChunkLocatorRefiner(
		locator, content, part.PartIndex,
	)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("build chunk source locator: %w", err)
	}
	now := time.Now()
	baseIndex := part.PartIndex * splitChunkIndexStride
	basePosition := part.PartIndex * splitPositionStride
	namespace := uuid.NewSHA1(
		uuid.NameSpaceOID,
		[]byte("weknora:physical-split:"+knowledge.ProcessingGeneration),
	)
	newID := func(kind string, index int) string {
		return uuid.NewSHA1(namespace, []byte(fmt.Sprintf(
			"%s:%08d:%08d", kind, part.PartIndex, index,
		))).String()
	}
	makeChunk := func(
		id string, body, contextHeader string, chunkIndex, start, end int,
		chunkType types.ChunkType,
	) *types.Chunk {
		return &types.Chunk{
			ID: id, TenantID: knowledge.TenantID, KnowledgeID: knowledge.ID,
			KnowledgeBaseID: knowledge.KnowledgeBaseID, Content: body,
			ContextHeader: contextHeader, ChunkIndex: chunkIndex, IsEnabled: false,
			CreatedAt: now, UpdatedAt: now, StartAt: start, EndAt: end,
			ChunkType: chunkType, ProcessingGeneration: knowledge.ProcessingGeneration,
			SplitPartIndex: part.PartIndex,
			SourceLocator: locatorRefiner.Locator(
				start-basePosition, end-basePosition, body,
			),
		}
	}

	cfg := buildSplitterConfigFromChunking(eff.ChunkingConfig)
	var chunks []*types.Chunk
	if eff.ChunkingConfig.EnableParentChild {
		parentCfg, childCfg := buildParentChildConfigs(eff.ChunkingConfig, cfg)
		split := chunker.SplitParentChild(content, parentCfg, childCfg)
		if len(split.Parents) >= splitChunkIndexStride/2 ||
			len(split.Children) >= splitChunkIndexStride/2 {
			return nil, nil, 0, errors.New("physical split part produced too many chunks")
		}
		parentIDs := make([]string, len(split.Parents))
		for index, parent := range split.Parents {
			id := newID("parent", index)
			parentIDs[index] = id
			chunk := makeChunk(
				id, parent.Content, logicalHeader,
				baseIndex+index, basePosition+parent.Start, basePosition+parent.End,
				types.ChunkTypeParentText,
			)
			if index > 0 {
				chunks[len(chunks)-1].NextChunkID = id
				chunk.PreChunkID = chunks[len(chunks)-1].ID
			}
			chunks = append(chunks, chunk)
		}
		var previousChild *types.Chunk
		for index, child := range split.Children {
			if strings.TrimSpace(child.Content) == "" {
				continue
			}
			contextHeader := strings.TrimSpace(strings.Join(
				[]string{logicalHeader, child.ContextHeader}, "\n",
			))
			chunk := makeChunk(
				newID("child", index), child.Content, contextHeader,
				baseIndex+splitChunkIndexStride/2+index,
				basePosition+child.Start, basePosition+child.End,
				types.ChunkTypeText,
			)
			if child.ParentIndex >= 0 && child.ParentIndex < len(parentIDs) {
				chunk.ParentChunkID = parentIDs[child.ParentIndex]
			}
			if previousChild != nil {
				previousChild.NextChunkID = chunk.ID
				chunk.PreChunkID = previousChild.ID
			}
			chunks = append(chunks, chunk)
			previousChild = chunk
		}
	} else {
		split := chunker.Split(content, cfg)
		if len(split) >= splitChunkIndexStride {
			return nil, nil, 0, errors.New("physical split part produced too many chunks")
		}
		var previous *types.Chunk
		for index, item := range split {
			if strings.TrimSpace(item.Content) == "" {
				continue
			}
			contextHeader := strings.TrimSpace(strings.Join(
				[]string{logicalHeader, item.ContextHeader}, "\n",
			))
			chunk := makeChunk(
				newID("text", index), item.Content, contextHeader,
				baseIndex+index, basePosition+item.Start, basePosition+item.End,
				types.ChunkTypeText,
			)
			if previous != nil {
				previous.NextChunkID = chunk.ID
				chunk.PreChunkID = previous.ID
			}
			chunks = append(chunks, chunk)
			previous = chunk
		}
	}
	textChunks := make([]*types.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.ChunkType == types.ChunkTypeText {
			textChunks = append(textChunks, chunk)
		}
	}
	mappings := make([]splitImageMapping, 0, len(storedImages))
	for _, image := range storedImages {
		chunkID := ""
		for _, chunk := range textChunks {
			if strings.Contains(chunk.Content, image.ServingURL) {
				chunkID = chunk.ID
				break
			}
		}
		if chunkID == "" && len(textChunks) > 0 {
			chunkID = textChunks[0].ID
		}
		if chunkID != "" {
			mappings = append(mappings, splitImageMapping{
				ChunkID: chunkID, ImageURL: image.ServingURL,
				SourceType: result.Metadata["image_source_type"],
			})
		}
	}
	return chunks, mappings, int64(len([]rune(content))), nil
}

func normalizePhysicalPartMarkdown(content string, locator map[string]any) (string, string) {
	lines := strings.Split(content, "\n")
	kind, _ := locator["kind"].(string)
	rowStart := intFromJSON(locator["row_start"])
	if (kind == "sheet_range" || kind == "record_range") && rowStart > 1 {
		first := firstNonBlankLine(lines, 0)
		for first >= 0 && strings.HasPrefix(strings.TrimSpace(lines[first]), "#") {
			first = firstNonBlankLine(lines, first+1)
		}
		second := firstNonBlankLine(lines, first+1)
		headerContext, _ := locator["header_context"].(string)
		headerRepeated, _ := locator["header_repeated"].(bool)
		if headerRepeated && first >= 0 {
			if second > first && strings.HasPrefix(strings.TrimSpace(lines[first]), "|") &&
				isMarkdownTableSeparator(lines[second]) {
				lines = append(lines[:first], lines[second+1:]...)
			} else {
				lines = append(lines[:first], lines[first+1:]...)
			}
			return strings.TrimSpace(strings.Join(lines, "\n")),
				"列标题：" + strings.TrimSpace(headerContext)
		}
		if first >= 0 && second > first &&
			strings.HasPrefix(strings.TrimSpace(lines[first]), "|") &&
			isMarkdownTableSeparator(lines[second]) {
			header := strings.TrimSpace(lines[first])
			lines = append(lines[:first], lines[second+1:]...)
			return strings.TrimSpace(strings.Join(lines, "\n")), "列标题：" + header
		}
	}
	lineStart := intFromJSON(locator["line_start"])
	if kind == "line_range" && lineStart > 1 {
		var headings []string
		cut := 0
		for cut < len(lines) {
			trimmed := strings.TrimSpace(lines[cut])
			if trimmed == "" {
				cut++
				continue
			}
			if strings.HasPrefix(trimmed, "#") {
				headings = append(headings, trimmed)
				cut++
				continue
			}
			break
		}
		if len(headings) > 0 {
			return strings.TrimSpace(strings.Join(lines[cut:], "\n")),
				"章节路径：" + strings.Join(headings, " / ")
		}
	}
	return strings.TrimSpace(content), ""
}

func firstNonBlankLine(lines []string, start int) int {
	if start < 0 {
		start = 0
	}
	for index := start; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) != "" {
			return index
		}
	}
	return -1
}

func isMarkdownTableSeparator(line string) bool {
	trimmed := strings.Trim(strings.TrimSpace(line), "| ")
	if trimmed == "" {
		return false
	}
	for _, column := range strings.Split(trimmed, "|") {
		column = strings.Trim(strings.TrimSpace(column), ":")
		if len(column) < 3 || strings.Trim(column, "-") != "" {
			return false
		}
	}
	return true
}

func logicalLocatorHeader(locator map[string]any) string {
	var base string
	switch kind, _ := locator["kind"].(string); kind {
	case "pages":
		base = fmt.Sprintf("原文页码：%d–%d",
			intFromJSON(locator["page_start"]), intFromJSON(locator["page_end"]))
	case "sheet_range":
		base = fmt.Sprintf("工作表：%v；原始行：%d–%d；原始列：%d–%d",
			locator["sheet"], intFromJSON(locator["row_start"]), intFromJSON(locator["row_end"]),
			intFromJSON(locator["column_start"]), intFromJSON(locator["column_end"]))
	case "record_range":
		base = fmt.Sprintf("原始记录：%d–%d；原始列：%d–%d",
			intFromJSON(locator["row_start"]), intFromJSON(locator["row_end"]),
			intFromJSON(locator["column_start"]), intFromJSON(locator["column_end"]))
	case "line_range":
		base = fmt.Sprintf("原文行：%d–%d",
			intFromJSON(locator["line_start"]), intFromJSON(locator["line_end"]))
		if _, ok := locator["character_start"]; ok {
			base += fmt.Sprintf("；字符：%d–%d",
				intFromJSON(locator["character_start"]), intFromJSON(locator["character_end"]))
		}
	case "time_range":
		base = fmt.Sprintf("原始时间轴：%.2fs–%.2fs",
			floatFromJSON(locator["start_seconds"]), floatFromJSON(locator["end_seconds"]))
	case "image_tile":
		base = fmt.Sprintf("原图区域：x=%d–%d, y=%d–%d",
			intFromJSON(locator["x_start"]), intFromJSON(locator["x_end"]),
			intFromJSON(locator["y_start"]), intFromJSON(locator["y_end"]))
		if intFromJSON(locator["frame_count"]) > 1 {
			base = fmt.Sprintf("原图帧：%d/%d；%s",
				intFromJSON(locator["frame_index"]), intFromJSON(locator["frame_count"]), base)
		}
	case "json_items":
		base = fmt.Sprintf("JSON 原始条目：%d–%d",
			intFromJSON(locator["item_start"]), intFromJSON(locator["item_end"]))
	case "json_path_records":
		base = fmt.Sprintf("JSON 原始路径：%v 至 %v",
			locator["path_start"], locator["path_end"])
	case "spine_items":
		base = fmt.Sprintf("电子书章节：%d–%d",
			intFromJSON(locator["spine_start"]), intFromJSON(locator["spine_end"]))
	case "dom_units":
		unitStart := intFromJSON(locator["unit_start"])
		unitEnd := intFromJSON(locator["unit_end"])
		if continuation, _ := locator["resource_continuation"].(bool); continuation {
			base = fmt.Sprintf(
				"网页归档嵌入资源：%d；首次引用内容区块：%d–%d",
				intFromJSON(locator["resource_index"])+1, unitStart, unitEnd,
			)
			if intFromJSON(locator["frame_count"]) > 1 {
				base += fmt.Sprintf("；原图帧：%d/%d",
					intFromJSON(locator["frame_index"]),
					intFromJSON(locator["frame_count"]))
			}
			if _, ok := locator["x_start"]; ok {
				base += fmt.Sprintf("；原图区域：x=%d–%d, y=%d–%d",
					intFromJSON(locator["x_start"]), intFromJSON(locator["x_end"]),
					intFromJSON(locator["y_start"]), intFromJSON(locator["y_end"]))
			}
		} else {
			base = fmt.Sprintf(
				"网页归档内容区块：%d–%d", unitStart, unitEnd,
			)
			if _, ok := locator["segment_start"]; ok {
				base += fmt.Sprintf("；结构片段：%d–%d/%d",
					intFromJSON(locator["segment_start"])+1,
					intFromJSON(locator["segment_end"])+1,
					intFromJSON(locator["segment_count"]))
			}
		}
	default:
		raw, _ := json.Marshal(locator)
		base = "原文位置：" + string(raw)
	}
	if normalizedFrom, _ := locator["normalized_from"].(string); strings.TrimSpace(normalizedFrom) != "" {
		base += "；由原始 " + strings.ToUpper(strings.TrimSpace(normalizedFrom)) + " 保真渲染"
	}
	if header, _ := locator["header_context"].(string); strings.TrimSpace(header) != "" {
		base += "；列标题：" + strings.TrimSpace(header)
	}
	return base
}

func intFromJSON(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func floatFromJSON(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	default:
		return 0
	}
}

func firstAndLastTextChunk(chunks []*types.Chunk) (string, string) {
	first, last := "", ""
	for _, chunk := range chunks {
		if chunk.ChunkType != types.ChunkTypeText {
			continue
		}
		if first == "" {
			first = chunk.ID
		}
		last = chunk.ID
	}
	return first, last
}

func countTextChunks(chunks []*types.Chunk) int {
	count := 0
	for _, chunk := range chunks {
		if chunk.ChunkType == types.ChunkTypeText {
			count++
		}
	}
	return count
}

func (s *knowledgeService) ProcessDocumentSplitFinalize(
	ctx context.Context, task *asynq.Task,
) (retErr error) {
	if s.splitManager == nil {
		return errors.New("document split manager is unavailable")
	}
	var payload documentsplit.FinalizePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("decode document split finalize payload: %w", err)
	}
	if payload.TenantID == 0 || payload.KnowledgeID == "" ||
		payload.KnowledgeBaseID == "" || payload.ProcessingGeneration == "" ||
		payload.ProcessingOwner == "" || payload.PlanID == "" {
		return errors.New("document split finalize payload has incomplete identity")
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	retryCount, maxRetry, _ := taskRetryMetadata(ctx)
	isLastRetry := retryCount >= maxRetry

	tenant, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		return fmt.Errorf("load split finalizer tenant: %w", err)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)
	knowledge, err := s.repo.GetKnowledgeByID(ctx, payload.TenantID, payload.KnowledgeID)
	if errors.Is(err, apprepo.ErrKnowledgeNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if knowledge.KnowledgeBaseID != payload.KnowledgeBaseID ||
		knowledge.ProcessingGeneration != payload.ProcessingGeneration {
		return nil
	}
	// Core commit succeeded but publication/cleanup was interrupted. The exact
	// durable fanout on the knowledge row is the only replay source.
	if knowledge.ProcessingOwner == "" && knowledge.ProcessedAt != nil {
		if len(knowledge.ProcessingFanout) > 0 {
			if err := s.replayCommittedCoreFanout(ctx, knowledge); err != nil {
				return err
			}
		}
		if err := s.cleanupPhysicalSplitInputs(ctx, knowledge, payload.PlanID); err != nil {
			return err
		}
		return s.splitManager.CompletePlan(ctx, payload.PlanID)
	}
	if knowledge.ParseStatus != types.ParseStatusProcessing ||
		knowledge.ProcessingOwner != payload.ProcessingOwner {
		return nil
	}
	plan, err := s.splitManager.ClaimFinalize(ctx, payload)
	if errors.Is(err, documentsplit.ErrStalePart) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if retErr == nil || !isLastRetry ||
			isDurableTaskDeferred(retErr) {
			return
		}
		retErr = errors.Join(
			retErr,
			s.splitManager.FailPlan(context.WithoutCancel(ctx), plan.ID, retErr),
			s.markActiveProcessingFailed(
				context.WithoutCancel(ctx), knowledge,
				fmt.Sprintf("physical document finalization failed: %v", retErr),
				"mark terminal physical document finalization failure",
			),
		)
	}()

	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID)
	if err != nil {
		return err
	}
	if kb == nil || kb.TenantID != payload.TenantID {
		return errors.New("split finalizer knowledge base identity mismatch")
	}
	parts, err := s.splitManager.ListParts(ctx, plan.ID)
	if err != nil {
		return err
	}
	if len(parts) != plan.PartCount {
		return documentsplit.ErrPlanIncomplete
	}
	var totalStorage int64
	var textChunkCount int
	var allMappings []splitImageMapping
	for index, part := range parts {
		if part.PartIndex != index || part.State != documentsplit.PartCompleted {
			return documentsplit.ErrPlanIncomplete
		}
		totalStorage += part.StorageBytes
		textChunkCount += part.DraftChunks
		if len(part.ImageMappings) > 0 {
			var mappings []splitImageMapping
			if err := json.Unmarshal(part.ImageMappings, &mappings); err != nil {
				return fmt.Errorf("decode split image mappings for part %d: %w", index, err)
			}
			allMappings = append(allMappings, mappings...)
		}
	}
	effectiveUsed := tenant.StorageUsed - knowledge.StorageSize
	if effectiveUsed < 0 {
		effectiveUsed = 0
	}
	if tenant.StorageQuota > 0 && effectiveUsed+totalStorage > tenant.StorageQuota {
		return errors.New("存储空间不足")
	}

	var embedder embedding.Embedder
	var engine *retriever.CompositeRetrieveEngine
	if kb.NeedsEmbeddingModel() {
		embedder, err = s.modelService.GetEmbeddingModel(ctx, kb.EmbeddingModelID)
		if err != nil {
			return err
		}
		engine, err = retriever.CreateRetrieveEngineForKB(
			ctx, s.retrieveEngine, s.ownership, tenant.ID, kb.VectorStoreID,
		)
		if err != nil {
			return err
		}
	}

	// Part workers need sparse index ranges so they can commit concurrently
	// without collisions. Once every part is durable, collapse those internal
	// coordinates into the contiguous logical order of a normal document.
	// This keeps paging, agent deep-reading, summaries, questions and graph
	// extraction indistinguishable from a one-shot parse.
	if err := s.splitManager.NormalizeGenerationTextChunkIndexes(
		ctx, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration,
	); err != nil {
		return fmt.Errorf("normalize split logical chunk order: %w", err)
	}

	// Re-embed only boundary-first chunks with the previous physical part's
	// semantic tail. This recreates the overlap a one-shot chunker would have
	// seen without duplicating source text or citations.
	if engine != nil {
		if err := s.reindexPhysicalBoundaries(
			ctx, engine, embedder, knowledge, parts,
		); err != nil {
			return err
		}
		cursor := documentsplit.GenerationChunkCursor{ChunkIndex: -1}
		for {
			chunks, err := s.splitManager.ListGenerationChunksAfter(
				ctx, knowledge.TenantID, knowledge.ID,
				knowledge.ProcessingGeneration, cursor,
				s.splitManager.Config().FinalizeBatchSize,
			)
			if err != nil {
				return err
			}
			if len(chunks) == 0 {
				break
			}
			statuses := make(map[string]bool, len(chunks))
			for _, chunk := range chunks {
				cursor = documentsplit.GenerationChunkCursor{
					ChunkIndex: chunk.ChunkIndex,
					ChunkID:    chunk.ID,
				}
				if chunk.ChunkType == types.ChunkTypeText {
					statuses[chunk.ID] = true
				}
			}
			if len(statuses) > 0 {
				if err := engine.BatchUpdateChunkEnabledStatus(ctx, statuses); err != nil {
					return fmt.Errorf("enable split vector generation: %w", err)
				}
			}
			if len(chunks) < s.splitManager.Config().FinalizeBatchSize {
				break
			}
		}
	}

	// Publish only after every new vector is ready. PublishGeneration performs
	// the old-disable/new-enable swap atomically in the relational store, so a
	// crash can leave either the old logical document or the new one visible,
	// never a partially parsed mix. Enabled-but-unpublished vectors are harmless
	// because their database chunks remain disabled.
	if err := s.splitManager.PublishGeneration(
		ctx, knowledge.TenantID, knowledge.ID,
		knowledge.ProcessingGeneration, parts,
	); err != nil {
		return fmt.Errorf("publish split chunk generation: %w", err)
	}

	// Retire the now-disabled previous generation by stable chunk ID. Deleting
	// by knowledge ID would also erase the newly published vectors.
	for {
		oldIDs, err := s.splitManager.ListOldChunkIDs(
			ctx, knowledge.TenantID, knowledge.ID,
			knowledge.ProcessingGeneration, "", s.splitManager.Config().FinalizeBatchSize,
		)
		if err != nil {
			return err
		}
		if len(oldIDs) == 0 {
			break
		}
		if engine != nil {
			if err := engine.DeleteByChunkIDList(
				ctx, oldIDs, embedder.GetDimensions(), knowledge.Type,
			); err != nil {
				return fmt.Errorf("delete prior generation vectors: %w", err)
			}
		}
		if err := s.splitManager.DeleteOldChunksByIDs(
			ctx, knowledge.TenantID, knowledge.ID,
			knowledge.ProcessingGeneration, oldIDs,
		); err != nil {
			return fmt.Errorf("delete prior generation chunks: %w", err)
		}
	}
	if err := s.graphEngine.DelGraph(ctx, []types.NameSpace{{
		KnowledgeBase: knowledge.KnowledgeBaseID, Knowledge: knowledge.ID,
	}}); err != nil {
		return fmt.Errorf("delete previous logical document graph: %w", err)
	}

	processOverrides, _ := knowledge.ProcessOverrides()
	eff := ResolveProcessConfig(kb, processOverrides)
	storedImages := make([]docparser.StoredImage, 0, len(allMappings))
	parsedForImages := make([]types.ParsedChunk, 0, len(allMappings))
	for _, mapping := range allMappings {
		storedImages = append(storedImages, docparser.StoredImage{ServingURL: mapping.ImageURL})
		parsedForImages = append(parsedForImages, types.ParsedChunk{
			Content: mapping.ImageURL, ChunkID: mapping.ChunkID,
		})
	}
	fanout, err := buildDocumentFanoutPlan(
		ctx, knowledge, kb,
		ProcessChunksOptions{
			EnableMultimodel: eff.EnableMultimodel,
			StoredImages:     storedImages,
			Metadata: map[string]string{
				"image_source_type": firstImageSourceType(allMappings),
			},
		},
		parsedForImages,
	)
	if err != nil {
		return err
	}
	fanoutBytes, err := processownership.MarshalFanoutPlan(fanout)
	if err != nil {
		return err
	}
	expectedStatus := knowledge.ParseStatus
	expectedOwner := knowledge.ProcessingOwner
	now := time.Now()
	finalizeIndexedKnowledgeState(
		knowledge, totalStorage, textChunkCount,
		eff.EnableMultimodel && len(allMappings) > 0, now,
	)
	if knowledge.ParseStatus == types.ParseStatusProcessing {
		knowledge.ProcessingFanout = types.JSON(fanoutBytes)
	} else {
		knowledge.ProcessingFanout = nil
	}
	knowledge.ProcessingOwner = ""
	finalized, err := finalizeKnowledgeWithStorageOwned(
		ctx, s.repo, knowledge, expectedStatus,
		knowledge.ProcessingGeneration, expectedOwner, totalStorage,
	)
	if err != nil {
		return err
	}
	if !finalized {
		return errKnowledgeStateFenceConflict
	}
	if knowledge.ParseStatus == types.ParseStatusProcessing {
		completionStore, ok := s.repo.(processownership.DurableFanoutCompletionStore)
		if !ok || completionStore == nil {
			return errors.New("split finalizer durable completion store is unavailable")
		}
		if err := processownership.DispatchFanout(
			ctx, s.task, s.redisClient, fanout, completionStore,
		); err != nil {
			return fmt.Errorf("dispatch split logical document fanout: %w", err)
		}
	}
	if err := s.cleanupPhysicalSplitInputs(ctx, knowledge, plan.ID); err != nil {
		return err
	}
	return s.splitManager.CompletePlan(ctx, plan.ID)
}

func (s *knowledgeService) reindexPhysicalBoundaries(
	ctx context.Context,
	engine *retriever.CompositeRetrieveEngine,
	embedder embedding.Embedder,
	knowledge *types.Knowledge,
	parts []*documentsplit.Part,
) error {
	if engine == nil || embedder == nil || len(parts) < 2 {
		return nil
	}
	batch := make([]*types.IndexInfo, 0, s.splitManager.Config().FinalizeBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := engine.BatchIndex(ctx, embedder, batch); err != nil {
			return fmt.Errorf("reindex split boundary context: %w", err)
		}
		batch = batch[:0]
		return nil
	}
	for index := 1; index < len(parts); index++ {
		if parts[index-1].LastChunkID == "" || parts[index].FirstChunkID == "" {
			continue
		}
		chunks, err := s.chunkRepo.ListChunksByID(
			ctx, knowledge.TenantID,
			[]string{parts[index-1].LastChunkID, parts[index].FirstChunkID},
		)
		if err != nil {
			return err
		}
		byID := make(map[string]*types.Chunk, len(chunks))
		for _, chunk := range chunks {
			byID[chunk.ID] = chunk
		}
		previous, current := byID[parts[index-1].LastChunkID], byID[parts[index].FirstChunkID]
		if previous == nil || current == nil {
			return errors.New("split boundary chunks are missing")
		}
		if temporalLocatorsOverlap(previous.SourceLocator, current.SourceLocator) {
			if trimmed, removed := trimExactBoundaryOverlap(
				previous.Content, current.Content, splitBoundaryRunes,
			); removed > 0 && strings.TrimSpace(trimmed) != "" {
				current.Content = trimmed
				current.UpdatedAt = time.Now()
				if err := s.chunkService.UpdateChunks(
					ctx, []*types.Chunk{current},
				); err != nil {
					return fmt.Errorf("trim overlapping audio transcription: %w", err)
				}
			}
		}
		contextHeader := ""
		var locator map[string]any
		if json.Unmarshal(current.SourceLocator, &locator) == nil {
			contextHeader = logicalLocatorHeader(locator)
		}
		previousTail := tailRunes(previous.Content, splitBoundaryRunes)
		content := strings.TrimSpace(strings.Join([]string{
			knowledge.Title,
			contextHeader,
			"前一原文位置的连续上下文：" + previousTail,
			current.Content,
		}, "\n\n"))
		batch = append(batch, &types.IndexInfo{
			Content: content, SourceID: current.ID, SourceType: types.ChunkSourceType,
			ChunkID: current.ID, KnowledgeID: knowledge.ID,
			KnowledgeBaseID: knowledge.KnowledgeBaseID, IsEnabled: false,
		})
		if len(batch) >= s.splitManager.Config().FinalizeBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func temporalLocatorsOverlap(previous, current types.JSON) bool {
	var before, after map[string]any
	if json.Unmarshal(previous, &before) != nil || json.Unmarshal(current, &after) != nil {
		return false
	}
	if before["kind"] != "time_range" || after["kind"] != "time_range" {
		return false
	}
	return floatFromJSON(before["end_seconds"]) > floatFromJSON(after["start_seconds"])
}

func trimExactBoundaryOverlap(previous, current string, maximum int) (string, int) {
	before := []rune(strings.TrimSpace(previous))
	after := []rune(strings.TrimSpace(current))
	if len(before) == 0 || len(after) == 0 || maximum <= 0 {
		return current, 0
	}
	limit := min(maximum, len(before), len(after))
	// Six runes is long enough to avoid deleting ordinary repeated discourse
	// markers while still catching roughly two seconds of Chinese speech.
	const minimum = 6
	for count := limit; count >= minimum; count-- {
		if string(before[len(before)-count:]) != string(after[:count]) {
			continue
		}
		trimmed := strings.TrimSpace(string(after[count:]))
		if trimmed == "" {
			return current, 0
		}
		return trimmed, count
	}
	return current, 0
}

func tailRunes(content string, count int) string {
	runes := []rune(content)
	if count <= 0 || len(runes) <= count {
		return content
	}
	return string(runes[len(runes)-count:])
}

func firstImageSourceType(mappings []splitImageMapping) string {
	for _, mapping := range mappings {
		if mapping.SourceType != "" {
			return mapping.SourceType
		}
	}
	return ""
}

func (s *knowledgeService) cleanupPhysicalSplitInputs(
	ctx context.Context, knowledge *types.Knowledge, planID string,
) error {
	if s.auxObjects == nil || knowledge == nil {
		return nil
	}
	parts, err := s.splitManager.ListParts(ctx, planID)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.InputPath != "" {
			paths = append(paths, part.InputPath)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	return s.auxObjects.DeletePaths(
		ctx, knowledge.TenantID, knowledge.KnowledgeBaseID,
		knowledge.ID, "", paths,
	)
}

func (s *knowledgeService) loadLogicalSummaryChunks(
	ctx context.Context,
	knowledge *types.Knowledge,
	generation string,
) ([]*types.Chunk, int64, error) {
	if knowledge == nil {
		return nil, 0, errors.New("summary knowledge is nil")
	}
	if s.splitManager == nil {
		chunks, err := s.chunkService.ListChunksByKnowledgeID(ctx, knowledge.ID)
		if err != nil {
			return nil, 0, err
		}
		filtered := filterTextChunks(chunks)
		return filtered, int64(len(filtered)), nil
	}
	if _, err := s.splitManager.GetPlanForGeneration(
		ctx, knowledge.TenantID, knowledge.ID, generation,
	); errors.Is(err, gorm.ErrRecordNotFound) {
		chunks, listErr := s.chunkService.ListChunksByKnowledgeID(ctx, knowledge.ID)
		filtered := filterTextChunks(chunks)
		return filtered, int64(len(filtered)), listErr
	} else if err != nil {
		return nil, 0, err
	}
	const maximumSummaryStrata = int64(64)
	return loadGenerationChunkStrata(
		ctx, s.splitManager, knowledge.TenantID, knowledge.ID, generation,
		[]types.ChunkType{types.ChunkTypeText}, maximumSummaryStrata,
	)
}

func filterTextChunks(chunks []*types.Chunk) []*types.Chunk {
	filtered := make([]*types.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk != nil && chunk.ChunkType == types.ChunkTypeText {
			filtered = append(filtered, chunk)
		}
	}
	return filtered
}
