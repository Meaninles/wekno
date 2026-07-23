package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"golang.org/x/sync/errgroup"
)

// collectImageURLs extracts unique provider:// image URLs from image_info JSON strings.
func collectImageURLs(ctx context.Context, imageInfos []string) []string {
	seen := make(map[string]struct{})
	var urls []string
	for _, info := range imageInfos {
		if info == "" {
			continue
		}
		var images []*types.ImageInfo
		if err := json.Unmarshal([]byte(info), &images); err != nil {
			logger.Warnf(ctx, "Failed to parse image_info JSON: %v", err)
			continue
		}
		for _, img := range images {
			if img.URL != "" {
				if _, exists := seen[img.URL]; !exists {
					seen[img.URL] = struct{}{}
					urls = append(urls, img.URL)
				}
			}
		}
	}
	return urls
}

// deleteExtractedImages deletes all extracted image files from storage.
// Standalone function — callable from both knowledgeService and knowledgeBaseService.
// Callers that own a durable delete intent should return the joined error so a
// retry can finish cleanup; legacy callers may intentionally ignore it.
func deleteExtractedImages(ctx context.Context, fileSvc interfaces.FileService, imageURLs []string) error {
	if len(imageURLs) == 0 {
		return nil
	}
	if fileSvc == nil {
		return errors.New("delete extracted images: file service is unavailable")
	}
	logger.Infof(ctx, "Deleting %d extracted images", len(imageURLs))
	var errs []error
	for _, url := range imageURLs {
		if err := fileSvc.DeleteFile(ctx, url); err != nil {
			logger.Errorf(ctx, "Failed to delete extracted image %s: %v", url, err)
			errs = append(errs, fmt.Errorf("delete extracted image %s: %w", url, err))
		}
	}
	return errors.Join(errs...)
}

// unscopedChunkImageInfoRepository is implemented by the production GORM
// chunk repository. It deliberately stays out of the broad ChunkRepository
// interface: only crash-recoverable deletion is allowed to inspect metadata
// on soft-deleted chunks.
type unscopedChunkImageInfoRepository interface {
	ListImageInfoByKnowledgeIDsUnscoped(
		ctx context.Context,
		tenantID uint64,
		knowledgeIDs []string,
	) ([]interfaces.ChunkImageInfo, error)
}

type knowledgeDeleteVectorGroupKey struct {
	KnowledgeBaseID  string
	VectorStoreID    string
	EmbeddingModelID string
	KnowledgeType    string
}

func buildKnowledgeDeleteVectorGroups(
	knowledges []*types.Knowledge,
	knowledgeBases map[string]*types.KnowledgeBase,
) (map[knowledgeDeleteVectorGroupKey][]string, error) {
	groups := make(map[knowledgeDeleteVectorGroupKey][]string)
	for _, knowledge := range knowledges {
		if knowledge == nil {
			return nil, errors.New("knowledge delete batch: nil knowledge in vector cleanup plan")
		}
		kb := knowledgeBases[knowledge.KnowledgeBaseID]
		if kb == nil {
			return nil, fmt.Errorf("knowledge delete batch: KB %s disappeared from routing snapshot", knowledge.KnowledgeBaseID)
		}
		storeID := ""
		if kb.VectorStoreID != nil {
			storeID = strings.TrimSpace(*kb.VectorStoreID)
		}
		key := knowledgeDeleteVectorGroupKey{
			KnowledgeBaseID:  knowledge.KnowledgeBaseID,
			VectorStoreID:    storeID,
			EmbeddingModelID: knowledge.EmbeddingModelID,
			KnowledgeType:    knowledge.Type,
		}
		groups[key] = append(groups[key], knowledge.ID)
	}
	return groups, nil
}

func (s *knowledgeService) listDeleteImageInfo(
	ctx context.Context,
	tenantID uint64,
	knowledgeIDs []string,
) ([]interfaces.ChunkImageInfo, error) {
	repo, ok := s.chunkRepo.(unscopedChunkImageInfoRepository)
	if !ok || repo == nil {
		return nil, errors.New("knowledge delete: unscoped image metadata repository is unavailable")
	}
	rows, err := repo.ListImageInfoByKnowledgeIDsUnscoped(ctx, tenantID, knowledgeIDs)
	if err != nil {
		return nil, fmt.Errorf("knowledge delete: snapshot extracted image metadata: %w", err)
	}
	return rows, nil
}

func (s *knowledgeService) requiredFileServiceForPath(
	ctx context.Context,
	kb *types.KnowledgeBase,
	path string,
) (interfaces.FileService, error) {
	if kb == nil {
		return nil, errors.New("knowledge delete: knowledge base is unavailable for storage routing")
	}
	fileSvc, err := s.resolveFileServiceForPath(ctx, kb, path)
	if err != nil {
		return nil, fmt.Errorf("knowledge delete: resolve path %q for KB %s: %w", path, kb.ID, err)
	}
	if fileSvc == nil {
		return nil, fmt.Errorf("knowledge delete: no file service can resolve path %q for KB %s", path, kb.ID)
	}
	return fileSvc, nil
}

func (s *knowledgeService) deleteExtractedImagesForKB(
	ctx context.Context,
	kb *types.KnowledgeBase,
	imageURLs []string,
) error {
	var errs []error
	for _, imageURL := range imageURLs {
		fileSvc, err := s.requiredFileServiceForPath(ctx, kb, imageURL)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, deleteExtractedImages(ctx, fileSvc, []string{imageURL}))
	}
	return errors.Join(errs...)
}

func (s *knowledgeService) dequeueCommittedDeleteIntents(
	ctx context.Context,
	tenantID uint64,
	knowledgeIDs []string,
) {
	rows, err := s.repo.GetKnowledgeBatch(ctx, tenantID, knowledgeIDs)
	if err != nil {
		logger.Warnf(ctx, "knowledge delete: failed to inspect committed intents before task cancellation: %v", err)
		return
	}
	for _, row := range rows {
		if row != nil && row.ParseStatus == types.ParseStatusDeleting {
			s.dequeueKnowledgeTasks(ctx, row.ID)
		}
	}
}

// DeleteKnowledge deletes a knowledge entry and all related resources
func (s *knowledgeService) DeleteKnowledge(ctx context.Context, id string) error {
	// Keep every entry point on the same durable mark → quiesce → resnapshot
	// → cleanup → finalize pipeline. Historically this single-item path had a
	// weaker best-effort cancellation boundary and could race active parsers.
	return s.DeleteKnowledgeList(ctx, []string{id})
}

type wikiKnowledgeDeletionPlan struct {
	knowledge    *types.Knowledge
	pages        []*types.WikiPage
	sourceChunks []string
	pendingOp    *types.TaskPendingOp
}

// prepareWikiKnowledgeDeletion is the hard durability boundary for source
// deletion. One database transaction locks every knowledge row, exposes the
// deleting intent, removes obsolete ingest work, and upserts a durable retract.
// A worker therefore never sees a deleting status that can later be rolled
// back independently. Page reconciliation and Redis wake-ups happen only after
// commit; both are idempotent and backed by the PostgreSQL operation.
func (s *knowledgeService) prepareWikiKnowledgeDeletion(
	ctx context.Context,
	knowledges []*types.Knowledge,
) error {
	if len(knowledges) == 0 {
		return nil
	}
	if s.wikiDeleteCoord == nil {
		return errors.New("wiki delete coordinator is unavailable")
	}

	plans := make([]*wikiKnowledgeDeletionPlan, 0, len(knowledges))
	requests := make([]wikidelete.Request, 0, len(knowledges))
	for _, knowledge := range knowledges {
		plan, err := s.buildWikiKnowledgeDeletionPlan(ctx, knowledge)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
		requests = append(requests, wikidelete.Request{
			TenantID:        knowledge.TenantID,
			KnowledgeID:     knowledge.ID,
			KnowledgeBaseID: knowledge.KnowledgeBaseID,
			PendingOp:       plan.pendingOp,
		})
	}
	if err := s.wikiDeleteCoord.Prepare(ctx, requests); err != nil {
		return fmt.Errorf("prepare durable Wiki deletion: %w", err)
	}

	var activationErrs []error
	for _, plan := range plans {
		if err := s.activateWikiKnowledgeDeletion(ctx, plan); err != nil {
			activationErrs = append(activationErrs, err)
		}
	}
	return errors.Join(activationErrs...)
}

func (s *knowledgeService) buildWikiKnowledgeDeletionPlan(
	ctx context.Context,
	knowledge *types.Knowledge,
) (*wikiKnowledgeDeletionPlan, error) {
	if knowledge == nil || knowledge.ID == "" || knowledge.KnowledgeBaseID == "" || knowledge.TenantID == 0 {
		return nil, errors.New("wiki delete plan: complete knowledge identity is required")
	}

	pages, err := s.wikiRepo.ListBySourceRef(ctx, knowledge.KnowledgeBaseID, knowledge.ID)
	if err != nil {
		// This snapshot is the synchronous visibility barrier: sole-source
		// pages are deleted and shared pages are quarantined before external
		// cleanup/finalization can continue. Unknown page state must therefore
		// fail closed and retry, not expose deleted-source prose until the
		// asynchronous retract happens to run.
		return nil, fmt.Errorf("wiki delete plan: snapshot pages for %s: %w", knowledge.ID, err)
	}
	chunkIDRepo, ok := s.chunkRepo.(unscopedChunkIDRepository)
	if !ok || chunkIDRepo == nil {
		return nil, errors.New("wiki delete plan: chunk repository is unavailable")
	}
	sourceChunks, err := chunkIDRepo.ListChunkIDsByKnowledgeIDUnscoped(ctx, knowledge.TenantID, knowledge.ID)
	if err != nil {
		return nil, fmt.Errorf("wiki delete plan: snapshot source chunks for %s: %w", knowledge.ID, err)
	}
	sourceChunks = stableStringUnion(sourceChunks)

	docTitle := knowledge.Title
	if docTitle == "" {
		docTitle = knowledge.FileName
	}
	if docTitle == "" {
		docTitle = knowledge.ID
	}
	docSummary := knowledge.Description
	knownSlugs := make([]string, 0, len(pages))
	for _, page := range pages {
		if page == nil {
			continue
		}
		if page.PageType == types.WikiPageTypeSummary && page.Summary != "" {
			docSummary = page.Summary
		}
		if page.Slug != "" && page.PageType != types.WikiPageTypeIndex && page.PageType != types.WikiPageTypeLog {
			knownSlugs = append(knownSlugs, page.Slug)
		}
	}
	lang, _ := types.LanguageFromContext(ctx)
	op := WikiPendingOp{
		Op:           WikiOpRetract,
		KnowledgeID:  knowledge.ID,
		DocTitle:     docTitle,
		DocSummary:   docSummary,
		Language:     lang,
		PageSlugs:    stableStringUnion(knownSlugs),
		SourceChunks: sourceChunks,
	}
	payload, err := json.Marshal(op)
	if err != nil {
		return nil, fmt.Errorf("wiki delete plan: marshal retract for %s: %w", knowledge.ID, err)
	}
	return &wikiKnowledgeDeletionPlan{
		knowledge:    knowledge,
		pages:        pages,
		sourceChunks: sourceChunks,
		pendingOp: &types.TaskPendingOp{
			TenantID: knowledge.TenantID,
			TaskType: wikiTaskType,
			Scope:    wikiTaskScope,
			ScopeID:  knowledge.KnowledgeBaseID,
			Op:       WikiOpRetract,
			DedupKey: knowledge.ID,
			Payload:  payload,
		},
	}, nil
}

func (s *knowledgeService) activateWikiKnowledgeDeletion(
	ctx context.Context,
	plan *wikiKnowledgeDeletionPlan,
) error {
	if plan == nil || plan.knowledge == nil {
		return errors.New("wiki delete activation: plan is required")
	}
	knowledge := plan.knowledge
	s.markKnowledgeDeletedForWiki(ctx, knowledge.KnowledgeBaseID, knowledge.ID)
	if err := enqueueWikiTrigger(s.task, WikiIngestPayload{
		TenantID:        knowledge.TenantID,
		KnowledgeBaseID: knowledge.KnowledgeBaseID,
	}, wikiFollowUpDelay, true); err != nil {
		logger.Warnf(ctx, "wiki delete: durable retract persisted but trigger degraded for %s: %v", knowledge.ID, err)
	}

	for _, page := range plan.pages {
		if page == nil || page.PageType == types.WikiPageTypeIndex || page.PageType == types.WikiPageTypeLog {
			continue
		}
		// Always archive, including sole-source pages. The durable reducer will
		// delete a sole-source page; retaining it briefly keeps provenance
		// available for retries. The repository call locks the current row,
		// unions concurrent source markers, and increments version to fence stale
		// content writers.
		if err := s.wikiRepo.QuarantineForDelete(
			ctx, knowledge.KnowledgeBaseID, page.Slug, knowledge.ID,
		); err != nil && !errors.Is(err, repository.ErrWikiPageNotFound) {
			return fmt.Errorf("wiki delete: quarantine page %s: %w", page.Slug, err)
		}
	}
	indexPage, err := s.wikiService.GetIndex(ctx, knowledge.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("wiki delete: load index quarantine for %s: %w", knowledge.ID, err)
	}
	if indexPage == nil {
		return fmt.Errorf("wiki delete: load index quarantine for %s: service returned nil", knowledge.ID)
	}
	if err := s.wikiRepo.QuarantineForDelete(
		ctx, knowledge.KnowledgeBaseID, indexPage.Slug, knowledge.ID,
	); err != nil {
		return fmt.Errorf("wiki delete: persist index quarantine for %s: %w", knowledge.ID, err)
	}
	return nil
}

// markKnowledgeDeletedForWiki writes a short-TTL tombstone so any wiki_ingest
// task still running or queued for this knowledge can short-circuit before
// resurrecting a page with a stale source_ref. No-op when Redis is absent.
func (s *knowledgeService) markKnowledgeDeletedForWiki(ctx context.Context, kbID, knowledgeID string) {
	if s.redisClient == nil || kbID == "" || knowledgeID == "" {
		return
	}
	key := WikiDeletedTombstoneKey(kbID, knowledgeID)
	if err := s.redisClient.Set(ctx, key, "1", wikiDeletedTTL).Err(); err != nil {
		logger.Warnf(ctx, "wiki cleanup: failed to write tombstone %s: %v", key, err)
	}
}

// scrubWikiPendingIngest removes queued WikiOpIngest entries for a knowledge
// from task_pending_ops. Used by both the delete path (we're about to
// soft-delete the doc, no point ingesting it) and the reparse path (the
// old chunks are about to vanish, so any pending ingest would either race
// with the cleanup or no-op on an empty chunk set — and the post-process
// task will enqueue a fresh ingest once new chunks land anyway).
//
// Retract entries stay put — delete still needs them to unlink referencing
// pages, and reparse never enqueues retracts for the doc being reparsed.
// We pass op=WikiOpIngest and the generation-key prefix so every stale ingest
// generation is removed while retract rows stay intact.
func (s *knowledgeService) scrubWikiPendingIngest(ctx context.Context, kbID, knowledgeID, reason string) {
	if s.taskPendingRepo == nil || kbID == "" || knowledgeID == "" {
		return
	}
	dedupPrefix, prefixErr := wikiqueue.IngestDedupPrefix(knowledgeID)
	if prefixErr != nil {
		logger.Warnf(ctx, "wiki %s: invalid pending ingest identity for knowledge %s: %v", reason, knowledgeID, prefixErr)
		return
	}
	if err := s.taskPendingRepo.DeleteByDedupKeyPrefix(ctx, wikiTaskType, wikiTaskScope, kbID, dedupPrefix, WikiOpIngest); err != nil {
		logger.Warnf(ctx, "wiki %s: failed to scrub pending ingest ops for knowledge %s: %v", reason, knowledgeID, err)
		return
	}
	logger.Infof(ctx, "wiki %s: scrubbed pending ingest ops for knowledge %s", reason, knowledgeID)
}

// prepareWikiForReparse is the reparse counterpart to
// cleanupWikiOnKnowledgeDelete. It aligns reparse with the same "pending
// queue hygiene" the delete path already enforces, without taking any
// destructive action against existing pages.
//
// Why no retract / tombstone here: reparse is not a "K is gone" event, it's
// a "K's contribution is about to be swapped for a new version" event. The
// actual swap happens asynchronously inside mapOneDocument (see its
// oldPageSlugs handling) — that's where we have both the old page set and
// the freshly extracted candidate slugs, which is exactly the information
// the WikiPageModifyPrompt needs to do a correct replace-not-append.
//
// So the only thing worth doing synchronously at reparse time is keeping
// the Redis pending list clean so the re-ingest enqueued by
// KnowledgePostProcess doesn't race with a stale ingest op that would
// fire mid-flight against zero chunks.
func (s *knowledgeService) prepareWikiForReparse(ctx context.Context, knowledge *types.Knowledge) {
	if knowledge == nil {
		return
	}
	kbID := knowledge.KnowledgeBaseID
	knowledgeID := knowledge.ID
	if kbID == "" || knowledgeID == "" {
		return
	}
	s.scrubWikiPendingIngest(ctx, kbID, knowledgeID, "reparse")
}

// removeSourceRef removes entries from source_refs that match a knowledge ID.
// Handles both old format ("knowledgeID") and new format ("knowledgeID|title").
func removeSourceRef(refs types.StringArray, knowledgeID string) types.StringArray {
	var result types.StringArray
	prefix := knowledgeID + "|"
	for _, ref := range refs {
		if ref == knowledgeID || strings.HasPrefix(ref, prefix) {
			continue
		}
		result = append(result, ref)
	}
	return result
}

func removeChunkRefs(refs types.StringArray, removed map[string]bool) types.StringArray {
	if len(refs) == 0 || len(removed) == 0 {
		return refs
	}
	result := make(types.StringArray, 0, len(refs))
	for _, ref := range refs {
		if removed[ref] {
			continue
		}
		result = append(result, ref)
	}
	return result
}

const (
	knowledgeDeleteQuiesceTimeout = 30 * time.Second
	knowledgeDeleteQuiescePoll    = 250 * time.Millisecond
)

// quiesceKnowledgeDeletion establishes a hard hand-off between document
// workers and destructive cleanup. Merely setting parse_status=deleting or
// sending an asynq cancellation is insufficient: an already-running worker
// may still be inside a vector/chunk write. We therefore keep cancelling new
// descendants and wait until Redis proves that no document-lifecycle task is
// queued or active for any target. Wiki tasks are deliberately excluded; the
// durable retract queue owns their independent cleanup.
func (s *knowledgeService) quiesceKnowledgeDeletion(
	ctx context.Context,
	knowledges []*types.Knowledge,
) error {
	return quiesceKnowledgeDeletionWithInspector(ctx, s.taskInspector, knowledges)
}

func quiesceKnowledgeDeletionWithInspector(
	ctx context.Context,
	taskInspector interfaces.TaskInspector,
	knowledges []*types.Knowledge,
) error {
	if len(knowledges) == 0 {
		return nil
	}
	if taskInspector == nil {
		return errors.New("knowledge delete: task inspector unavailable for quiescence barrier")
	}

	targets := make([]interfaces.KnowledgeTaskTarget, 0, len(knowledges))
	for _, knowledge := range knowledges {
		if knowledge == nil || strings.TrimSpace(knowledge.ID) == "" || strings.TrimSpace(knowledge.KnowledgeBaseID) == "" {
			return errors.New("knowledge delete: invalid target at quiescence barrier")
		}
		targets = append(targets, interfaces.KnowledgeTaskTarget{
			KnowledgeBaseID: knowledge.KnowledgeBaseID,
			KnowledgeID:     knowledge.ID,
		})
	}

	waitCtx, cancel := context.WithTimeout(ctx, knowledgeDeleteQuiesceTimeout)
	defer cancel()
	ticker := time.NewTicker(knowledgeDeleteQuiescePoll)
	defer ticker.Stop()
	emptySnapshots := 0
	toCancel := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		toCancel[target.KnowledgeID] = struct{}{}
	}

	for {
		for _, target := range targets {
			if _, shouldCancel := toCancel[target.KnowledgeID]; !shouldCancel {
				continue
			}
			if _, _, err := taskInspector.CancelTasksForKnowledge(waitCtx, target.KnowledgeID); err != nil {
				return fmt.Errorf("knowledge delete: cancel lifecycle tasks for %s: %w", target.KnowledgeID, err)
			}
		}
		live, err := taskInspector.DocumentLifecycleTaskKnowledgeIDs(waitCtx, targets)
		if err != nil {
			return fmt.Errorf("knowledge delete: inspect lifecycle task quiescence: %w", err)
		}
		if len(live) == 0 {
			emptySnapshots++
			// Queue snapshots are atomic per Asynq queue, not across every
			// queue. Requiring two cancellation+snapshot rounds closes the
			// hand-off window where a parent in a later-scanned queue fans out
			// a child into an earlier-scanned queue while the first scan runs.
			if emptySnapshots >= 2 {
				return nil
			}
		} else {
			emptySnapshots = 0
		}
		// The first round purges queued descendants for every target. Later
		// rounds repeat the expensive destructive scan only for IDs the single
		// batched liveness probe still reports, rather than O(all IDs × backlog)
		// on every 250ms tick.
		toCancel = make(map[string]struct{}, len(live))
		for knowledgeID := range live {
			toCancel[knowledgeID] = struct{}{}
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf(
				"knowledge delete: lifecycle tasks did not quiesce for %d document(s): %w",
				len(live), waitCtx.Err(),
			)
		case <-ticker.C:
		}
	}
}

// DeleteKnowledgeList deletes knowledge entries and all related resources.
// Direct service callers are already operating on authoritative IDs; queued
// user operations additionally call deleteKnowledgeListExpected to re-check
// the KB identity captured at enqueue time.
func (s *knowledgeService) DeleteKnowledgeList(ctx context.Context, ids []string) error {
	return s.deleteKnowledgeListInBatches(ctx, ids, "")
}

const knowledgeDeleteWorkerBatchSize = 25

func normalizeKnowledgeDeleteIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func (s *knowledgeService) knowledgeDeleteContext(
	ctx context.Context,
) (context.Context, *types.Tenant, error) {
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return ctx, nil, errors.New("knowledge delete: tenant context is required")
	}
	if tenant, ok := types.TenantInfoFromContext(ctx); ok && tenant != nil {
		if tenant.ID != tenantID {
			return ctx, nil, errors.New("knowledge delete: tenant context identity mismatch")
		}
		return ctx, tenant, nil
	}
	tenant, err := s.tenantRepo.GetTenantByID(ctx, tenantID)
	if err != nil {
		return ctx, nil, fmt.Errorf("knowledge delete: load tenant %d: %w", tenantID, err)
	}
	if tenant == nil || tenant.ID != tenantID {
		return ctx, nil, fmt.Errorf("knowledge delete: tenant %d is unavailable", tenantID)
	}
	return context.WithValue(ctx, types.TenantInfoContextKey, tenant), tenant, nil
}

func validateKnowledgeDeleteKB(
	knowledges []*types.Knowledge,
	expectedKnowledgeBaseID string,
) error {
	if expectedKnowledgeBaseID == "" {
		return nil
	}
	for _, knowledge := range knowledges {
		if knowledge == nil || knowledge.KnowledgeBaseID != expectedKnowledgeBaseID {
			actual := "<nil>"
			knowledgeID := "<nil>"
			if knowledge != nil {
				actual = knowledge.KnowledgeBaseID
				knowledgeID = knowledge.ID
			}
			return fmt.Errorf(
				"knowledge delete batch: knowledge %s moved from expected KB %s to %s",
				knowledgeID, expectedKnowledgeBaseID, actual,
			)
		}
	}
	return nil
}

func (s *knowledgeService) beginKnowledgeDeletionBatch(
	ctx context.Context,
	tenantID uint64,
	ids []string,
	expectedKnowledgeBaseID string,
) error {
	knowledges, err := s.repo.GetKnowledgeBatch(ctx, tenantID, ids)
	if err != nil {
		return err
	}
	if len(knowledges) == 0 {
		return nil
	}
	if err := validateKnowledgeDeleteKB(knowledges, expectedKnowledgeBaseID); err != nil {
		return err
	}
	intents := make([]wikidelete.Intent, 0, len(knowledges))
	for _, knowledge := range knowledges {
		docTitle := knowledge.Title
		if docTitle == "" {
			docTitle = knowledge.FileName
		}
		if docTitle == "" {
			docTitle = knowledge.ID
		}
		lang, _ := types.LanguageFromContext(ctx)
		payload, err := json.Marshal(WikiPendingOp{
			Op:          WikiOpRetract,
			KnowledgeID: knowledge.ID,
			DocTitle:    docTitle,
			DocSummary:  knowledge.Description,
			Language:    lang,
		})
		if err != nil {
			return fmt.Errorf("begin knowledge deletion: encode minimal Wiki retract: %w", err)
		}
		intents = append(intents, wikidelete.Intent{
			TenantID:        tenantID,
			KnowledgeID:     knowledge.ID,
			KnowledgeBaseID: knowledge.KnowledgeBaseID,
			PendingOp: &types.TaskPendingOp{
				TenantID: tenantID, TaskType: wikiTaskType, Scope: wikiTaskScope,
				ScopeID: knowledge.KnowledgeBaseID, Op: WikiOpRetract,
				DedupKey: knowledge.ID, Payload: payload,
			},
		})
	}
	if err := s.wikiDeleteCoord.Begin(ctx, intents); err != nil {
		return fmt.Errorf("begin durable knowledge deletion: %w", err)
	}
	triggeredKBs := make(map[string]struct{})
	for _, knowledge := range knowledges {
		s.markKnowledgeDeletedForWiki(ctx, knowledge.KnowledgeBaseID, knowledge.ID)
		if _, triggered := triggeredKBs[knowledge.KnowledgeBaseID]; triggered {
			continue
		}
		triggeredKBs[knowledge.KnowledgeBaseID] = struct{}{}
		if err := enqueueWikiTrigger(s.task, WikiIngestPayload{
			TenantID: tenantID, KnowledgeBaseID: knowledge.KnowledgeBaseID,
		}, wikiFollowUpDelay, true); err != nil {
			// PostgreSQL is authoritative; wikiqueue.Recovery republishes a
			// missing wake-up every minute.
			logger.Warnf(ctx, "knowledge delete: minimal retract trigger degraded for KB %s: %v", knowledge.KnowledgeBaseID, err)
		}
	}
	return nil
}

func (s *knowledgeService) deleteKnowledgeListInBatches(
	ctx context.Context,
	ids []string,
	expectedKnowledgeBaseID string,
) error {
	ids = normalizeKnowledgeDeleteIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	var tenantInfo *types.Tenant
	var err error
	ctx, tenantInfo, err = s.knowledgeDeleteContext(ctx)
	if err != nil {
		return err
	}

	// Phase 1 is intentionally separate and cheap: persist deletion intent
	// for the full root operation before any external cleanup. If the root task
	// later times out or dead-letters, every tail row is visible to Recovery.
	for start := 0; start < len(ids); start += knowledgeDeleteWorkerBatchSize {
		end := start + knowledgeDeleteWorkerBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := s.beginKnowledgeDeletionBatch(
			ctx, tenantInfo.ID, ids[start:end], expectedKnowledgeBaseID,
		); err != nil {
			return fmt.Errorf("claim knowledge delete batch [%d:%d]: %w", start, end, err)
		}
	}

	var batchErr error
	for start := 0; start < len(ids); start += knowledgeDeleteWorkerBatchSize {
		if err := ctx.Err(); err != nil {
			return errors.Join(batchErr, err)
		}
		end := start + knowledgeDeleteWorkerBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		// Continue with later bounded batches after an isolated failure. The
		// task still returns the joined error so Asynq retries; successfully
		// finalized rows disappear from the next retry's scoped read.
		if err := s.deleteKnowledgeListExpected(ctx, ids[start:end], expectedKnowledgeBaseID); err != nil {
			batchErr = errors.Join(batchErr, fmt.Errorf("delete knowledge batch [%d:%d]: %w", start, end, err))
		}
	}
	return batchErr
}

func (s *knowledgeService) deleteKnowledgeListExpected(
	ctx context.Context,
	ids []string,
	expectedKnowledgeBaseID string,
) error {
	if len(ids) == 0 {
		return nil
	}
	// The caller has already persisted deletion intent for the complete root
	// operation. From here onward every snapshot must be taken after active
	// document writers have crossed the quiescence barrier.
	tenantInfo, ok := types.TenantInfoFromContext(ctx)
	if !ok || tenantInfo == nil {
		return errors.New("knowledge delete batch: tenant info is unavailable")
	}
	knowledgeList, err := s.repo.GetKnowledgeBatch(ctx, tenantInfo.ID, ids)
	if err != nil {
		return err
	}

	if len(knowledgeList) == 0 {
		return nil
	}
	if err := validateKnowledgeDeleteKB(knowledgeList, expectedKnowledgeBaseID); err != nil {
		return err
	}

	// Marked deleting above. Wait before taking *any* resource/provenance
	// snapshot so an active parser cannot append a final chunk/image/page after
	// the snapshot and leave an orphan behind.
	if err := s.quiesceKnowledgeDeletion(ctx, knowledgeList); err != nil {
		return err
	}
	knowledgeList, err = s.repo.GetKnowledgeBatch(ctx, tenantInfo.ID, ids)
	if err != nil {
		return fmt.Errorf("knowledge delete batch: reload after quiescence: %w", err)
	}
	if len(knowledgeList) == 0 {
		return nil
	}
	if err := validateKnowledgeDeleteKB(knowledgeList, expectedKnowledgeBaseID); err != nil {
		return err
	}
	for _, knowledge := range knowledgeList {
		if knowledge.ParseStatus != types.ParseStatusDeleting {
			return fmt.Errorf(
				"knowledge delete batch: quiesced knowledge %s lost deleting claim (status=%s)",
				knowledge.ID, knowledge.ParseStatus,
			)
		}
	}
	deleteIDs := make([]string, 0, len(knowledgeList))
	knowledgeToKB := make(map[string]string, len(knowledgeList))
	knowledgeBases := make(map[string]*types.KnowledgeBase)
	for _, knowledge := range knowledgeList {
		deleteIDs = append(deleteIDs, knowledge.ID)
		knowledgeToKB[knowledge.ID] = knowledge.KnowledgeBaseID
		if _, loaded := knowledgeBases[knowledge.KnowledgeBaseID]; loaded {
			continue
		}
		kb, kbErr := s.kbService.GetKnowledgeBaseByID(ctx, knowledge.KnowledgeBaseID)
		if kbErr != nil {
			return fmt.Errorf("knowledge delete batch: load KB %s: %w", knowledge.KnowledgeBaseID, kbErr)
		}
		if kb == nil {
			return fmt.Errorf("knowledge delete batch: load KB %s: service returned nil without error", knowledge.KnowledgeBaseID)
		}
		knowledgeBases[knowledge.KnowledgeBaseID] = kb
	}

	// Read through soft deletes on every retry. Failure is a hard boundary:
	// finalizing without this snapshot would orphan extracted image objects.
	chunkImageInfos, err := s.listDeleteImageInfo(ctx, tenantInfo.ID, deleteIDs)
	if err != nil {
		return err
	}
	knowledgeImageInfos := make(map[string][]string) // knowledgeID -> []imageInfo JSON
	for _, ci := range chunkImageInfos {
		_, belongs := knowledgeToKB[ci.KnowledgeID]
		if !belongs {
			return fmt.Errorf("knowledge delete batch: image metadata belongs to unexpected knowledge %s", ci.KnowledgeID)
		}
		knowledgeImageInfos[ci.KnowledgeID] = append(knowledgeImageInfos[ci.KnowledgeID], ci.ImageInfo)
	}
	knowledgeAuxiliaryPaths := make(map[string][]string, len(knowledgeList))
	for _, knowledge := range knowledgeList {
		paths, pathErr := auxiliaryPathsFromKnowledge(knowledge)
		if pathErr != nil {
			return pathErr
		}
		paths = append(paths, collectImageURLs(ctx, knowledgeImageInfos[knowledge.ID])...)
		knowledgeAuxiliaryPaths[knowledge.ID] = uniqueNonEmptyStrings(paths)
	}

	// Refresh the durable retract and synchronous quarantine from the
	// post-barrier page/chunk snapshot. Begin already made the delete intent
	// crash-recoverable; a failure here leaves deleting rows for Recovery.
	if err := s.prepareWikiKnowledgeDeletion(ctx, knowledgeList); err != nil {
		return err
	}
	logger.Infof(ctx, "Prepared %d knowledge entries for durable deletion", len(knowledgeList))

	wg := errgroup.Group{}
	// 2. Delete knowledge embeddings from vector store
	wg.Go(func() error {
		tenantID := types.MustTenantIDFromContext(ctx)
		// A KB-bound store is intentionally not reachable through the tenant's
		// default engine set. Group by the complete routing identity so every
		// deletion reaches exactly the backend that received the vectors.
		group, groupErr := buildKnowledgeDeleteVectorGroups(knowledgeList, knowledgeBases)
		if groupErr != nil {
			return groupErr
		}
		for key, knowledgeIDs := range group {
			// Wiki-only knowledge never had embeddings written to the vector store,
			// and its EmbeddingModelID is intentionally empty. Skip the whole group
			// to avoid the spurious "model ID cannot be empty" failure.
			if strings.TrimSpace(key.EmbeddingModelID) == "" {
				logger.Infof(ctx, "Skipping vector store cleanup for %d knowledge entries without embedding model", len(knowledgeIDs))
				continue
			}
			var boundStoreID *string
			if key.VectorStoreID != "" {
				storeID := key.VectorStoreID
				boundStoreID = &storeID
			}
			retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
				ctx, s.retrieveEngine, s.ownership, tenantID, boundStoreID)
			if err != nil {
				return fmt.Errorf("knowledge delete batch: resolve vector store for KB %s: %w", key.KnowledgeBaseID, err)
			}
			embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, key.EmbeddingModelID)
			if err != nil {
				logger.GetLogger(ctx).WithField("error", err).Errorf("DeleteKnowledge get embedding model failed")
				return err
			}
			if err := retrieveEngine.DeleteByKnowledgeIDList(ctx, knowledgeIDs, embeddingModel.GetDimensions(), key.KnowledgeType); err != nil {
				logger.GetLogger(ctx).
					WithField("error", err).
					Errorf("DeleteKnowledge delete knowledge embedding failed")
				return err
			}
		}
		return nil
	})

	// 4. Delete all chunks associated with this knowledge
	wg.Go(func() error {
		if err := s.chunkService.DeleteByKnowledgeList(ctx, deleteIDs); err != nil {
			logger.GetLogger(ctx).WithField("error", err).Errorf("DeleteKnowledge delete chunks failed")
			return err
		}
		return nil
	})

	// 5. Delete source and derived objects through one lifecycle. The durable
	// registry is merged with chunk.image_info, processing_fanout and FAQ
	// metadata so retries retain evidence after chunks or task payloads vanish.
	wg.Go(func() error {
		var cleanupErr error
		for _, knowledge := range knowledgeList {
			paths := append([]string(nil), knowledgeAuxiliaryPaths[knowledge.ID]...)
			paths = append(paths, knowledge.FilePath)
			cleanupErr = errors.Join(cleanupErr, s.cleanupKnowledgeAuxiliaryForDelete(
				ctx, knowledgeBases[knowledge.KnowledgeBaseID], knowledge, paths,
			))
		}
		return cleanupErr
	})

	// Delete the knowledge graph
	wg.Go(func() error {
		namespaces := []types.NameSpace{}
		for _, knowledge := range knowledgeList {
			namespaces = append(
				namespaces,
				types.NameSpace{KnowledgeBase: knowledge.KnowledgeBaseID, Knowledge: knowledge.ID},
			)
		}
		if err := s.graphEngine.DelGraph(ctx, namespaces); err != nil {
			logger.GetLogger(ctx).WithField("error", err).Errorf("DeleteKnowledge delete knowledge graph failed")
			return err
		}
		return nil
	})

	if err = wg.Wait(); err != nil {
		return err
	}
	removedStorage, err := s.wikiDeleteCoord.Finalize(ctx, tenantInfo.ID, deleteIDs)
	if err != nil {
		return fmt.Errorf("finalize durable knowledge deletion batch: %w", err)
	}
	tenantInfo.StorageUsed -= removedStorage
	if tenantInfo.StorageUsed < 0 {
		tenantInfo.StorageUsed = 0
	}
	return nil
}

func (s *knowledgeService) cleanupKnowledgeResources(ctx context.Context, knowledge *types.Knowledge) error {
	return s.cleanupKnowledgeResourcesWithMoveFence(ctx, knowledge, false)
}

// cleanupKnowledgeResourcesWithinMoveFence is used only while the move
// repository holds both endpoint KB rows active for the complete callback.
// The specialized auxiliary cleanup avoids trying to upgrade that outer
// PostgreSQL parent lock from a second connection.
func (s *knowledgeService) cleanupKnowledgeResourcesWithinMoveFence(
	ctx context.Context,
	knowledge *types.Knowledge,
) error {
	return s.cleanupKnowledgeResourcesWithMoveFence(ctx, knowledge, true)
}

func (s *knowledgeService) cleanupKnowledgeResourcesWithMoveFence(
	ctx context.Context,
	knowledge *types.Knowledge,
	moveFenceHeld bool,
) error {
	logger.GetLogger(ctx).Infof("Cleaning knowledge resources before manual update, knowledge ID: %s", knowledge.ID)

	var cleanupErr error

	if knowledge.ParseStatus == types.ManualKnowledgeStatusDraft && knowledge.StorageSize == 0 {
		// Draft without indexed data, skip cleanup.
		return nil
	}

	tenantInfo, ok := types.TenantInfoFromContext(ctx)
	if !ok || tenantInfo == nil {
		return errors.New("cleanup knowledge resources: tenant info is unavailable")
	}

	// Storage and vector routing are properties of the source KB.  Never fall
	// back to the tenant's default engines/store when this lookup fails: doing
	// so can report a successful reparse/move while deleting the wrong backend
	// and leaving the source resources orphaned.
	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, knowledge.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("cleanup knowledge resources: load source KB %s: %w", knowledge.KnowledgeBaseID, err)
	}
	if kb == nil {
		return fmt.Errorf("cleanup knowledge resources: load source KB %s: service returned nil without error", knowledge.KnowledgeBaseID)
	}
	if knowledge.EmbeddingModelID != "" {
		retrieveEngine, err := retriever.CreateRetrieveEngineForKB(
			ctx, s.retrieveEngine, s.ownership, tenantInfo.ID, kb.VectorStoreID)
		if err != nil {
			logger.GetLogger(ctx).WithField("error", err).Error("Failed to init retrieve engine during cleanup")
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			embeddingModel, modelErr := s.modelService.GetEmbeddingModel(ctx, knowledge.EmbeddingModelID)
			if modelErr != nil {
				logger.GetLogger(ctx).WithField("error", modelErr).Error("Failed to get embedding model during cleanup")
				cleanupErr = errors.Join(cleanupErr, modelErr)
			} else {
				if err := retrieveEngine.DeleteByKnowledgeIDList(ctx, []string{knowledge.ID}, embeddingModel.GetDimensions(), knowledge.Type); err != nil {
					logger.GetLogger(ctx).WithField("error", err).Error("Failed to delete manual knowledge index")
					cleanupErr = errors.Join(cleanupErr, err)
				}
			}
		}
	}

	// Read image metadata through soft-deleted chunks.  A previous delivery may
	// have deleted the chunks and then failed to delete one image; the unscoped
	// snapshot is the durable retry evidence for that partially-completed
	// cleanup.
	chunkImageInfos, imgErr := s.listDeleteImageInfo(ctx, tenantInfo.ID, []string{knowledge.ID})
	if imgErr != nil {
		logger.GetLogger(ctx).WithField("error", imgErr).Error("Failed to collect image URLs for cleanup")
		return imgErr
	}
	var imageInfoStrs []string
	for _, ci := range chunkImageInfos {
		imageInfoStrs = append(imageInfoStrs, ci.ImageInfo)
	}
	imageURLs, imageDecodeErr := collectImageURLsStrict(imageInfoStrs)
	if imageDecodeErr != nil {
		return fmt.Errorf("decode extracted image metadata for cleanup: %w", imageDecodeErr)
	}
	metadataPaths, metadataErr := auxiliaryPathsFromKnowledge(knowledge)
	if metadataErr != nil {
		return metadataErr
	}
	imageURLs = uniqueNonEmptyStrings(append(imageURLs, metadataPaths...))

	if err := s.chunkService.DeleteChunksByKnowledgeID(ctx, knowledge.ID); err != nil {
		logger.GetLogger(ctx).WithField("error", err).Error("Failed to delete manual knowledge chunks")
		cleanupErr = errors.Join(cleanupErr, err)
	}

	// Source FilePath is intentionally absent: reparse/manual-update owns the
	// original upload. Every derived object uses the same durable lifecycle as
	// full deletion, including pre-fanout and FAQ artifacts.
	var derivedCleanupErr error
	if moveFenceHeld {
		derivedCleanupErr = s.cleanupDerivedKnowledgeAuxiliaryWithinMoveFence(ctx, kb, knowledge, imageURLs)
	} else {
		derivedCleanupErr = s.cleanupDerivedKnowledgeAuxiliary(ctx, kb, knowledge, imageURLs)
	}
	if derivedCleanupErr != nil {
		cleanupErr = errors.Join(cleanupErr, derivedCleanupErr)
	}

	namespace := types.NameSpace{KnowledgeBase: knowledge.KnowledgeBaseID, Knowledge: knowledge.ID}
	if err := s.graphEngine.DelGraph(ctx, []types.NameSpace{namespace}); err != nil {
		logger.GetLogger(ctx).WithField("error", err).Error("Failed to delete manual knowledge graph data")
		cleanupErr = errors.Join(cleanupErr, err)
	}

	if cleanupErr != nil {
		return cleanupErr
	}

	if knowledge.StorageSize > 0 {
		previousStorageSize := knowledge.StorageSize
		reset, err := resetKnowledgeStorage(ctx, s.repo, knowledge)
		if err != nil {
			return fmt.Errorf("atomically reset knowledge storage after cleanup: %w", err)
		}
		if !reset {
			return fmt.Errorf("atomically reset knowledge storage after cleanup: %w", errKnowledgeStateFenceConflict)
		}
		tenantInfo.StorageUsed -= previousStorageSize
		if tenantInfo.StorageUsed < 0 {
			tenantInfo.StorageUsed = 0
		}
		knowledge.StorageSize = 0
	}

	return nil
}

// ProcessKnowledgeListDelete handles Asynq knowledge list delete tasks
func (s *knowledgeService) ProcessKnowledgeListDelete(ctx context.Context, t *asynq.Task) error {
	var payload types.KnowledgeListDeletePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "Failed to unmarshal knowledge list delete payload: %v", err)
		return err
	}
	if payload.TenantID == 0 {
		return errors.New("knowledge list delete payload: tenant_id is required")
	}
	if len(payload.KnowledgeIDs) == 0 {
		return errors.New("knowledge list delete payload: knowledge_ids is required")
	}
	if strings.TrimSpace(payload.ExpectedKnowledgeBaseID) == "" {
		return errors.New("knowledge list delete payload: expected_knowledge_base_id is required")
	}
	payload.KnowledgeIDs = normalizeKnowledgeDeleteIDs(payload.KnowledgeIDs)
	if len(payload.KnowledgeIDs) == 0 {
		return errors.New("knowledge list delete payload: knowledge_ids contains no valid IDs")
	}
	if payload.RecoveryClaimedAt != nil {
		if len(payload.KnowledgeIDs) != 1 {
			return errors.New("knowledge list delete payload: recovery claim must target exactly one knowledge")
		}
		intent := wikidelete.Intent{
			TenantID:        payload.TenantID,
			KnowledgeID:     payload.KnowledgeIDs[0],
			KnowledgeBaseID: strings.TrimSpace(payload.ExpectedKnowledgeBaseID),
		}
		retryCount, _ := asynq.GetRetryCount(ctx)
		var claimed bool
		var err error
		if retryCount > 0 {
			claimed, err = s.wikiDeleteCoord.ContinueRecovery(ctx, intent)
		} else {
			claimed, err = s.wikiDeleteCoord.ClaimRecovery(ctx, intent, payload.RecoveryClaimedAt.UTC())
		}
		if err != nil {
			return err
		}
		if !claimed {
			logger.Infof(ctx,
				"Ignoring obsolete knowledge delete recovery for %s (generation %s)",
				payload.KnowledgeIDs[0], payload.RecoveryClaimedAt.UTC().Format(time.RFC3339Nano),
			)
			return nil
		}
	}

	logger.Infof(ctx, "Processing knowledge list delete task for %d knowledge items", len(payload.KnowledgeIDs))

	// Get tenant info
	tenant, err := s.tenantRepo.GetTenantByID(ctx, payload.TenantID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get tenant %d: %v", payload.TenantID, err)
		return err
	}

	// Set context values
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)

	// Delete knowledge list
	if err := s.deleteKnowledgeListInBatches(
		ctx, payload.KnowledgeIDs, strings.TrimSpace(payload.ExpectedKnowledgeBaseID),
	); err != nil {
		logger.Errorf(ctx, "Failed to delete knowledge list: %v", err)
		return err
	}

	logger.Infof(ctx, "Successfully deleted %d knowledge items", len(payload.KnowledgeIDs))
	return nil
}
