package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/contentcache"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"golang.org/x/sync/errgroup"
)

// scheduleFollowUp enqueues another asynq trigger task if there are
// still pending ops in task_pending_ops for this KB. Returns true when
// a follow-up was scheduled.
//
// We use a short ProcessIn (5s) so the active-batch lock has time to
// release before the next worker tries to acquire it; otherwise we'd
// just bounce on ErrWikiIngestConcurrent and burn an asynq retry slot.
func (s *wikiIngestService) scheduleFollowUp(ctx context.Context, payload WikiIngestPayload) (bool, error) {
	if s.pendingRepo == nil {
		return false, errors.New("wiki ingest: pending-op repository is nil")
	}
	var (
		count int64
		err   error
	)
	if repo, ok := s.pendingRepo.(wikiDistributedMapRepository); ok && repo != nil {
		count, err = repo.CountWikiCommitReady(ctx, payload.TenantID, payload.KnowledgeBaseID)
	} else {
		count, err = s.pendingRepo.PendingCount(ctx, wikiTaskType, wikiTaskScope, payload.KnowledgeBaseID)
	}
	if err != nil {
		return false, fmt.Errorf("wiki ingest: count pending rows for follow-up: %w", err)
	}
	if count == 0 {
		return false, nil
	}

	logger.Infof(ctx, "wiki ingest: %d commit-ready operations pending for KB %s, scheduling follow-up", count, payload.KnowledgeBaseID)

	next := nextWikiWakePayload(payload)
	if err := enqueueWikiTriggerFenced(ctx, s.pendingRepo, s.task, next, wikiFollowUpDelay, true); err != nil {
		return false, fmt.Errorf("wiki ingest: enqueue follow-up: %w", err)
	}
	return true, nil
}

func (s *wikiIngestService) scheduleProviderCircuitFollowUp(
	ctx context.Context,
	payload WikiIngestPayload,
	providerErr error,
) (bool, error) {
	retryAfter, ok := modeladmission.CircuitRetryAfter(providerErr)
	if !ok {
		return false, nil
	}
	if retryAfter < wikiFollowUpDelay {
		retryAfter = wikiFollowUpDelay
	}
	retryAfter = modeladmission.SpreadProviderRetry(
		retryAfter,
		fmt.Sprintf(
			"wiki-commit\x00%d\x00%s",
			payload.TenantID,
			payload.KnowledgeBaseID,
		),
	)
	next := nextWikiWakePayload(payload)
	if err := enqueueWikiTriggerFenced(
		ctx, s.pendingRepo, s.task, next, retryAfter, true,
	); err != nil {
		return false, fmt.Errorf("wiki ingest: enqueue provider-circuit follow-up: %w", err)
	}
	logger.Infof(ctx,
		"wiki ingest: provider circuit open; deferred KB %s without consuming per-document fail_count for %s",
		payload.KnowledgeBaseID, retryAfter)
	return true, nil
}

func (s *wikiIngestService) scheduleAnyWikiFollowUp(
	ctx context.Context,
	payload WikiIngestPayload,
) (bool, error) {
	count, err := s.pendingRepo.PendingCount(
		ctx, wikiTaskType, wikiTaskScope, payload.KnowledgeBaseID,
	)
	if err != nil {
		return false, fmt.Errorf("wiki ingest: count all pending rows for terminal follow-up: %w", err)
	}
	if count == 0 {
		return false, nil
	}
	next := nextWikiWakePayload(payload)
	if err := enqueueWikiTriggerFenced(
		ctx, s.pendingRepo, s.task, next, wikiFollowUpDelay, true,
	); err != nil {
		return false, fmt.Errorf("wiki ingest: enqueue terminal follow-up: %w", err)
	}
	return true, nil
}

// nextWikiWakePayload flips between two stable payload identities. The current
// task can publish its opposite phase with Asynq Unique, while every concurrent
// contender resolves to that same opposite phase and is coalesced.
func nextWikiWakePayload(payload WikiIngestPayload) WikiIngestPayload {
	if payload.WakePhase == 0 {
		payload.WakePhase = 1
	} else {
		payload.WakePhase = 0
	}
	return payload
}

// scheduleLockConflictFollowUp replaces any number of contending wake-ups with
// one delayed, alternating Unique wake-up and lets every current Asynq task
// finish successfully. Returning
// ErrWikiIngestConcurrent used to depend on Asynq's IsFailure hook, but Asynq
// checks retry exhaustion before that hook; a task that had already consumed
// its budget on a real outage could therefore be archived merely because it
// next encountered a healthy owner lock. Durable work remains in PostgreSQL,
// and the active owner schedules a fenced successor during settlement.
//
// This disposable contention signal deliberately avoids the parent-KB row
// fence: dozens of losers taking that lock was itself a database thundering
// herd. A deletion racing this best-effort signal is safe because the durable
// queue/tombstone checks make a late wake-up a no-op.
func (s *wikiIngestService) scheduleLockConflictFollowUp(
	ctx context.Context,
	payload WikiIngestPayload,
) (bool, error) {
	next := nextWikiWakePayload(payload)
	if err := enqueueWikiTrigger(s.task, next, wikiLockConflictDelay, true); err != nil {
		return false, fmt.Errorf("wiki ingest: enqueue lock-conflict follow-up: %w", err)
	}
	return true, nil
}

// settleWikiQueue performs the durable queue bookkeeping after business work
// has finished. asynq cancels the handler context at its hard deadline; using
// that same context for DeleteByIDs/PendingCount/Enqueue was the production
// failure that left a successfully-processed five-row head in place and then
// reported status=success. Once an active business context reaches this
// hand-off, WithoutCancel preserves tenant/log values and allows bookkeeping
// to finish if the task deadline fires during settlement; a short timeout
// keeps shutdown bounded, while lease loss remains linked and aborts it.
func (s *wikiIngestService) settleWikiQueue(
	parentCtx context.Context,
	leaseCtx context.Context,
	payload WikiIngestPayload,
	trimIDs []int64,
	failedOps []WikiPendingOp,
) (bool, error) {
	return s.settleWikiQueueWithDeferrals(
		parentCtx,
		leaseCtx,
		payload,
		trimIDs,
		failedOps,
		nil,
		nil,
	)
}

// settleWikiQueueWithDeferrals extends normal settlement with provider work
// that was rejected before a remote call began. Real provider failures remain
// in failedOps and consume the bounded per-document budget. Deferred rows are
// only rotated and kept durable; the follow-up honors the shared circuit's
// retry window so one unavailable provider does not create a tight polling
// loop or block successfully completed rows from being acknowledged.
func (s *wikiIngestService) settleWikiQueueWithDeferrals(
	parentCtx context.Context,
	leaseCtx context.Context,
	payload WikiIngestPayload,
	trimIDs []int64,
	failedOps []WikiPendingOp,
	deferredOps []WikiPendingOp,
	providerDeferredErr error,
) (bool, error) {
	// A context that was already cancelled before settlement means the
	// business batch may be incomplete. Do not acknowledge or mutate any row;
	// returning the error lets Asynq retry the same durable head safely.
	if err := wikiWorkContextError(parentCtx); err != nil {
		return false, fmt.Errorf("wiki ingest: business context ended before queue settlement: %w", err)
	}
	if err := wikiWorkContextError(leaseCtx); err != nil {
		return false, fmt.Errorf("wiki ingest: active lease ended before queue settlement: %w", err)
	}

	settleCtx, cancel := newWikiQueueSettlementContext(parentCtx)
	defer cancel()
	// Parent task cancellation is deliberately detached after business work
	// completes, but lease loss is not: once another worker may own the KB,
	// this worker must stop queue mutations as well as page writes.
	stopLeasePropagation := context.AfterFunc(leaseCtx, cancel)
	defer stopLeasePropagation()

	var errs []error
	if err := s.trimPendingList(settleCtx, trimIDs); err != nil {
		errs = append(errs, err)
	}
	if err := s.requeueFailedOps(settleCtx, payload, failedOps); err != nil {
		errs = append(errs, fmt.Errorf("wiki ingest: record failed ops: %w", err))
	}
	if err := s.rotateWikiAttempts(settleCtx, deferredOps); err != nil {
		errs = append(errs, fmt.Errorf("wiki ingest: rotate provider-deferred ops: %w", err))
	}
	followUpScheduled := false
	var err error
	if providerDeferredErr != nil {
		followUpScheduled, err = s.scheduleProviderCircuitFollowUp(
			settleCtx,
			payload,
			providerDeferredErr,
		)
	}
	// Admission-backend failures have no provider Retry-After, and a provider
	// signal may race circuit recovery. Fall back to the ordinary durable
	// follow-up whenever no circuit-specific wake-up was published.
	if err == nil && !followUpScheduled {
		followUpScheduled, err = s.scheduleFollowUp(settleCtx, payload)
	}
	if err != nil {
		errs = append(errs, err)
	}
	if err := wikiWorkContextError(leaseCtx); err != nil {
		errs = append(errs, fmt.Errorf("wiki ingest: active lease ended during queue settlement: %w", err))
	}
	return followUpScheduled, errors.Join(errs...)
}

func newWikiQueueSettlementContext(parentCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parentCtx), wikiQueueSettlementTimeout)
}

func wikiWorkContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

// mergeFailedWikiOps maps reduce failures (reported by contributing
// knowledge ID) back to the durable pending rows. Existing map-phase failures
// are preserved, and each persisted op is returned at most once even when it
// contributed to several slugs that failed in the same batch.
func mergeFailedWikiOps(
	existing []WikiPendingOp,
	pending []WikiPendingOp,
	failedKnowledgeIDs map[string]struct{},
) []WikiPendingOp {
	merged := make([]WikiPendingOp, 0, len(existing)+len(failedKnowledgeIDs))
	seenDBIDs := make(map[int64]struct{}, len(existing)+len(failedKnowledgeIDs))
	seenLogical := make(map[string]struct{}, len(existing)+len(failedKnowledgeIDs))
	appendOnce := func(op WikiPendingOp) {
		if op.dbID != 0 {
			if _, seen := seenDBIDs[op.dbID]; seen {
				return
			}
			seenDBIDs[op.dbID] = struct{}{}
		} else {
			key := op.Op + "\x00" + op.KnowledgeID
			if _, seen := seenLogical[key]; seen {
				return
			}
			seenLogical[key] = struct{}{}
		}
		merged = append(merged, op)
	}
	for _, op := range existing {
		appendOnce(op)
	}
	for _, op := range pending {
		if _, failed := failedKnowledgeIDs[op.KnowledgeID]; failed {
			appendOnce(op)
		}
	}
	return merged
}

func removeTerminalStaleWikiOps(failed []WikiPendingOp, terminal map[string]struct{}) []WikiPendingOp {
	if len(failed) == 0 || len(terminal) == 0 {
		return failed
	}
	kept := failed[:0]
	for _, op := range failed {
		if _, stale := terminal[op.KnowledgeID]; stale {
			continue
		}
		kept = append(kept, op)
	}
	return kept
}

func wikiQueueTrimIDs(peekedIDs []int64, failedOps []WikiPendingOp) []int64 {
	failedIDSet := make(map[int64]struct{}, len(failedOps))
	for _, op := range failedOps {
		if op.dbID != 0 {
			failedIDSet[op.dbID] = struct{}{}
		}
	}
	trimIDs := make([]int64, 0, len(peekedIDs))
	for _, id := range peekedIDs {
		if _, fail := failedIDSet[id]; fail {
			continue
		}
		trimIDs = append(trimIDs, id)
	}
	return trimIDs
}

func wikiMapStatsBool(stats types.JSONMap, key string) bool {
	if len(stats) == 0 {
		return false
	}
	switch value := stats[key].(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		return err == nil && parsed
	case float64:
		return value != 0
	case float32:
		return value != 0
	case int:
		return value != 0
	case int64:
		return value != 0
	case json.Number:
		parsed, err := value.Float64()
		return err == nil && parsed != 0
	default:
		return false
	}
}

func wikiMapStatsInt(stats types.JSONMap, key string) int {
	if len(stats) == 0 {
		return 0
	}
	switch value := stats[key].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case uint:
		return int(value)
	case uint32:
		return int(value)
	case uint64:
		if uint64(int(value)) == value {
			return int(value)
		}
	case float32:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, err := value.Int64()
		if err == nil {
			return int(parsed)
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil {
			return parsed
		}
	}
	return 0
}

func wikiMapContentCacheKey(
	tenantID uint64,
	knowledgeID string,
	processingGeneration string,
	contentHash string,
	model chat.Chat,
	language string,
	granularity types.WikiExtractionGranularity,
	oldPageSlugs map[string]bool,
) contentcache.Key {
	slugs := make([]string, 0, len(oldPageSlugs))
	for slug := range oldPageSlugs {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	modelID := ""
	modelName := ""
	if model != nil {
		modelID = model.GetModelID()
		modelName = model.GetModelName()
	}
	return contentcache.Key{
		TenantID: tenantID,
		Kind:     contentcache.KindWikiMap,
		// wikiMapCheckpoint includes citation chunk IDs. Those IDs belong to
		// one immutable document generation even when two documents have
		// byte-identical text. Scope the cache identity accordingly: sharing a
		// checkpoint across documents or reparses would attach another
		// generation's chunks to the resulting Wiki pages, while concurrent
		// identical documents would race to persist different payloads under
		// one immutable key.
		ContentHash: contentcache.Digest(
			contentHash,
			knowledgeID,
			processingGeneration,
		),
		VersionHash: contentcache.Digest(
			"wiki-map-v3",
			modelID,
			modelName,
			language,
			string(granularity),
			strings.Join(slugs, "\n"),
			agent.WikiCandidateSlugPrompt,
			agent.WikiKnowledgeExtractPrompt,
			agent.WikiChunkCitationPrompt,
			agent.WikiSummaryPrompt,
		),
	}
}

// drainTerminalWikiQueueBatch acknowledges one batch without running any
// model work. It is used when the knowledge base itself was deleted, so no
// wiki page target remains to reconcile. (Wiki-disabled queues use the
// retract-aware drainDisabledWikiQueueBatch instead.) The caller still owns
// the per-KB active lock, and settleWikiQueue preserves the same
// cancellation/lease guarantees as a normal successful batch. If more rows
// remain, its non-Unique follow-up drains them in subsequent locked batches.
func (s *wikiIngestService) drainTerminalWikiQueueBatch(
	ctx context.Context,
	leaseCtx context.Context,
	payload WikiIngestPayload,
	batchSize int,
	reason string,
) (drained int, followUpScheduled bool, retErr error) {
	_, peekedIDs, err := s.peekPendingListIncludingRetractIntents(
		ctx, payload.KnowledgeBaseID, batchSize, true,
	)
	if err != nil {
		return 0, false, fmt.Errorf("wiki ingest: terminal queue drain (%s): %w", reason, err)
	}
	if len(peekedIDs) == 0 {
		logger.Infof(ctx, "wiki ingest: terminal queue drain (%s) found no pending rows for KB %s", reason, payload.KnowledgeBaseID)
		return 0, false, nil
	}

	followUpScheduled, err = s.settleWikiQueue(ctx, leaseCtx, payload, peekedIDs, nil)
	if err != nil {
		return len(peekedIDs), followUpScheduled, fmt.Errorf(
			"wiki ingest: terminal queue drain (%s) failed for KB %s: %w",
			reason,
			payload.KnowledgeBaseID,
			err,
		)
	}
	if !followUpScheduled {
		followUpScheduled, err = s.scheduleAnyWikiFollowUp(ctx, payload)
		if err != nil {
			return len(peekedIDs), false, err
		}
	}
	logger.Infof(
		ctx,
		"wiki ingest: terminal queue drain (%s) acknowledged %d rows for KB %s (followup=%v)",
		reason,
		len(peekedIDs),
		payload.KnowledgeBaseID,
		followUpScheduled,
	)
	return len(peekedIDs), followUpScheduled, nil
}

// reconcileDisabledWikiRetract performs the part of a retract that is safe
// and deterministic without an LLM. Pages solely backed by the removed
// knowledge are deleted; shared pages have that knowledge removed from
// source_refs. The body of a shared page is left untouched because its prose
// cannot be attribution-split safely without synthesis. The metadata cleanup
// is idempotent, so a partial failure can leave the durable op pending and be
// retried without harming pages already reconciled.
func (s *wikiIngestService) reconcileDisabledWikiRetract(
	ctx context.Context,
	kbID string,
	op WikiPendingOp,
) error {
	if s.wikiService == nil {
		return errors.New("wiki ingest: wiki page service is nil")
	}
	if op.KnowledgeID == "" {
		return errors.New("wiki ingest: disabled-Wiki retract has empty knowledge ID")
	}

	pagesBySlug := make(map[string]*types.WikiPage)
	pageOrder := make([]string, 0, len(op.PageSlugs))
	addPage := func(page *types.WikiPage) {
		if page == nil || page.Slug == "" {
			return
		}
		if page.PageType == types.WikiPageTypeIndex || page.PageType == types.WikiPageTypeLog {
			return
		}
		if _, exists := pagesBySlug[page.Slug]; !exists {
			pageOrder = append(pageOrder, page.Slug)
		}
		pagesBySlug[page.Slug] = page
	}

	var errs []error
	livePages, err := s.wikiService.ListPagesBySourceRef(ctx, kbID, op.KnowledgeID)
	if err != nil {
		errs = append(errs, fmt.Errorf("list pages by source ref: %w", err))
	} else {
		for _, page := range livePages {
			addPage(page)
		}
	}

	// The enqueue-time snapshot is a useful second source in case the
	// source-ref query races a prior partial cleanup. Read each explicit slug
	// with strict error classification; only a real not-found is a no-op.
	for _, slug := range op.PageSlugs {
		if slug == "" {
			continue
		}
		if _, exists := pagesBySlug[slug]; exists {
			continue
		}
		page, pageErr := s.wikiService.GetPageBySlug(ctx, kbID, slug)
		switch {
		case pageErr == nil && page != nil:
			addPage(page)
		case errors.Is(pageErr, repository.ErrWikiPageNotFound):
			continue
		case pageErr != nil:
			errs = append(errs, fmt.Errorf("get retract page %s: %w", slug, pageErr))
		default:
			errs = append(errs, fmt.Errorf("get retract page %s: service returned nil without error", slug))
		}
	}

	for _, slug := range pageOrder {
		if ctxErr := wikiWorkContextError(ctx); ctxErr != nil {
			return fmt.Errorf("wiki ingest: disabled-Wiki retract context ended: %w", ctxErr)
		}
		page := pagesBySlug[slug]
		remaining := removeSourceRef(page.SourceRefs, op.KnowledgeID)
		if len(remaining) == len(page.SourceRefs) {
			// A prior attempt (including one from an older binary) may already
			// have removed the source ref while leaving this exact deletion
			// quarantine behind. Complete only that source's marker; an explicit
			// slug without the marker remains an idempotent no-op so an unrelated
			// page is never archived just because it reused the slug later.
			pendingSources, markerErr := wikidelete.PendingSources(page)
			if markerErr != nil {
				errs = append(errs, fmt.Errorf("inspect disabled-Wiki retract quarantine on page %s: %w", slug, markerErr))
				continue
			}
			pendingThisSource := false
			for _, sourceID := range pendingSources {
				if sourceID == op.KnowledgeID {
					pendingThisSource = true
					break
				}
			}
			if !pendingThisSource {
				continue
			}
		}
		if len(remaining) == 0 {
			if deleteErr := s.wikiService.DeletePage(ctx, kbID, slug); deleteErr != nil &&
				!errors.Is(deleteErr, repository.ErrWikiPageNotFound) {
				errs = append(errs, fmt.Errorf("delete sole-source page %s: %w", slug, deleteErr))
			}
			continue
		}
		page.SourceRefs = remaining
		page.ChunkRefs = removeChunkRefsByID(page.ChunkRefs, op.SourceChunks)
		// Without a synthesis model we cannot safely remove deleted-source
		// prose from a shared page. Archive it so re-enabling Wiki never exposes
		// stale facts; a later re-ingest of surviving sources can rebuild it.
		page.Status = types.WikiPageStatusArchived
		if markerErr := wikidelete.MarkApplied(page, op.dbID); markerErr != nil {
			errs = append(errs, fmt.Errorf("mark disabled-Wiki retract applied on page %s: %w", slug, markerErr))
			continue
		}
		if markerErr := wikidelete.Complete(page, op.KnowledgeID); markerErr != nil {
			errs = append(errs, fmt.Errorf("complete disabled-Wiki retract quarantine on page %s: %w", slug, markerErr))
			continue
		}
		// Even after this source's marker is complete, keep the shared page
		// archived: its body can still contain prose from the removed source
		// and Wiki is disabled, so no model-backed rewrite is available.
		page.Status = types.WikiPageStatusArchived
		writeCtx := wikidelete.WithQuarantineClear(ctx, op.KnowledgeID)
		if updateErr := s.wikiService.UpdatePageMeta(writeCtx, page); updateErr != nil &&
			!errors.Is(updateErr, repository.ErrWikiPageNotFound) {
			errs = append(errs, fmt.Errorf("remove source ref from page %s: %w", slug, updateErr))
		}
	}
	return errors.Join(errs...)
}

// drainDisabledWikiQueueBatch terminally skips ingest operations, but retract
// operations first receive deterministic page/source-ref reconciliation. A
// failed/unknown operation is not acknowledged: it enters the normal
// fail_count and durable dead-letter flow, preserving an operator-visible
// audit instead of disappearing when Wiki is switched off.
func (s *wikiIngestService) drainDisabledWikiQueueBatch(
	ctx context.Context,
	leaseCtx context.Context,
	payload WikiIngestPayload,
	batchSize int,
) (drained int, followUpScheduled bool, retErr error) {
	pendingOps, peekedIDs, err := s.peekPendingList(ctx, payload.KnowledgeBaseID, batchSize)
	if err != nil {
		return 0, false, fmt.Errorf("wiki ingest: disabled-Wiki queue peek: %w", err)
	}
	if len(peekedIDs) == 0 {
		logger.Infof(ctx, "wiki ingest: disabled-Wiki queue has no pending rows for KB %s", payload.KnowledgeBaseID)
		return 0, false, nil
	}

	failedOps := make([]WikiPendingOp, 0)
	successfulRetracts := make([]WikiPendingOp, 0)
	skippedIngests := 0
	reconciledRetracts := 0
	for _, op := range pendingOps {
		if ctxErr := wikiWorkContextError(ctx); ctxErr != nil {
			return len(peekedIDs), false, fmt.Errorf("wiki ingest: disabled-Wiki queue context ended: %w", ctxErr)
		}
		switch op.Op {
		case WikiOpIngest:
			if op.KnowledgeID == "" {
				op.lastError = "disabled-Wiki ingest has empty knowledge ID"
				failedOps = append(failedOps, op)
			} else {
				if statusErr := s.recordWikiGenerationStatus(
					ctx,
					payload,
					op,
					types.WikiStatusNone,
					"",
				); statusErr != nil {
					op.lastError = fmt.Sprintf("record disabled-Wiki terminal status: %v", statusErr)
					failedOps = append(failedOps, op)
					logger.Warnf(ctx,
						"wiki ingest: failed to terminally cancel disabled-Wiki ingest for knowledge %s: %v",
						op.KnowledgeID,
						statusErr,
					)
					continue
				}
				skippedIngests++
				logger.Infof(ctx, "wiki ingest: terminally cancelled ingest for knowledge %s because Wiki is disabled", op.KnowledgeID)
			}
		case WikiOpRetract:
			preparedOp, authorized, prepareErr := s.prepareWikiRetract(
				ctx, payload.TenantID, payload.KnowledgeBaseID, op,
			)
			if prepareErr != nil {
				op.lastError = fmt.Sprintf("disabled-Wiki retract preflight failed: %v", prepareErr)
				failedOps = append(failedOps, op)
				logger.Warnf(ctx, "wiki ingest: disabled-Wiki retract preflight failed for knowledge %s: %v", op.KnowledgeID, prepareErr)
			} else if !authorized {
				preparedOp.lastError = "source document is active; stale retract cancelled without page changes"
				successfulRetracts = append(successfulRetracts, preparedOp)
				reconciledRetracts++
				logger.Warnf(ctx, "wiki ingest: disabled-Wiki stale retract cancelled for active knowledge %s", op.KnowledgeID)
			} else if cleanupErr := s.reconcileDisabledWikiRetract(ctx, payload.KnowledgeBaseID, preparedOp); cleanupErr != nil {
				op.lastError = cleanupErr.Error()
				failedOps = append(failedOps, op)
				logger.Warnf(ctx, "wiki ingest: disabled-Wiki retract cleanup failed for knowledge %s: %v", op.KnowledgeID, cleanupErr)
			} else {
				successfulRetracts = append(successfulRetracts, preparedOp)
				reconciledRetracts++
			}
		default:
			op.lastError = fmt.Sprintf("unsupported disabled-Wiki pending op %q", op.Op)
			failedOps = append(failedOps, op)
		}
	}
	if ctxErr := wikiWorkContextError(ctx); ctxErr != nil {
		return len(peekedIDs), false, fmt.Errorf("wiki ingest: disabled-Wiki queue context ended before settlement: %w", ctxErr)
	}

	// Disabled Wiki has no model-backed index rewrite. Reset the intro to a
	// neutral value after an authorized retract and archive shared pages above;
	// this guarantees that re-enabling cannot expose deleted-source prose from
	// either a page body or a stale generated intro.
	authorizedRetracts := make([]WikiPendingOp, 0, len(successfulRetracts))
	for _, op := range successfulRetracts {
		if op.lastError == "" {
			authorizedRetracts = append(authorizedRetracts, op)
		}
	}
	if len(authorizedRetracts) > 0 {
		appliedKnowledgeIDs := make([]string, 0, len(authorizedRetracts))
		appliedOpIDs := make([]int64, 0, len(authorizedRetracts))
		for _, op := range authorizedRetracts {
			appliedKnowledgeIDs = append(appliedKnowledgeIDs, op.KnowledgeID)
			appliedOpIDs = append(appliedOpIDs, op.dbID)
		}
		indexPage, indexErr := s.wikiService.GetIndex(ctx, payload.KnowledgeBaseID)
		if indexErr == nil && indexPage == nil {
			indexErr = errors.New("wiki index service returned nil page")
		}
		if indexErr == nil {
			indexPage.Content = "# Wiki Index\n\nWiki content will be refreshed when Wiki is enabled.\n"
			indexPage.Summary = "Wiki content will be refreshed when Wiki is enabled."
			indexErr = wikidelete.MarkApplied(indexPage, appliedOpIDs...)
		}
		if indexErr == nil {
			indexErr = wikidelete.Complete(indexPage, appliedKnowledgeIDs...)
		}
		if indexErr == nil {
			writeCtx := wikidelete.WithQuarantineClear(ctx, appliedKnowledgeIDs...)
			_, indexErr = s.wikiService.UpdatePage(writeCtx, indexPage)
		}
		if indexErr != nil {
			for _, op := range authorizedRetracts {
				op.lastError = fmt.Sprintf("reset disabled-Wiki index: %v", indexErr)
				failedOps = append(failedOps, op)
			}
		}
	}

	if len(successfulRetracts) > 0 && s.logEntrySvc != nil {
		entries := make([]*types.WikiLogEntry, 0, len(successfulRetracts))
		for _, op := range successfulRetracts {
			action := "retract"
			title := ""
			summary := ""
			if op.lastError != "" {
				action = "retract_cancelled"
				title = op.DocTitle
				summary = op.lastError
			}
			// A successful delete retract is operational provenance, not a
			// second copy of deleted source content. The finalizer purges older
			// ingest logs; keep only the non-sensitive identity and action.
			entries = append(entries, s.buildLogEntry(
				payload.TenantID, payload.KnowledgeBaseID, action,
				op.KnowledgeID, title, summary, nil, op.dbID,
			))
		}
		if logErr := s.logEntrySvc.AppendBatch(ctx, entries); logErr != nil {
			for _, op := range successfulRetracts {
				op.lastError = fmt.Sprintf("append disabled-Wiki log: %v", logErr)
				failedOps = append(failedOps, op)
			}
		}
	} else if len(successfulRetracts) > 0 {
		for _, op := range successfulRetracts {
			op.lastError = "append disabled-Wiki log: log entry service is nil"
			failedOps = append(failedOps, op)
		}
	}

	failedOps = mergeFailedWikiOps(failedOps, nil, nil)
	trimIDs := wikiQueueTrimIDs(peekedIDs, failedOps)
	followUpScheduled, err = s.settleWikiQueue(ctx, leaseCtx, payload, trimIDs, failedOps)
	if err != nil {
		return len(peekedIDs), followUpScheduled, fmt.Errorf(
			"wiki ingest: disabled-Wiki queue settlement failed for KB %s: %w",
			payload.KnowledgeBaseID,
			err,
		)
	}
	if !followUpScheduled {
		followUpScheduled, err = s.scheduleAnyWikiFollowUp(ctx, payload)
		if err != nil {
			return len(peekedIDs), false, err
		}
	}
	logger.Infof(
		ctx,
		"wiki ingest: disabled-Wiki queue settled %d rows for KB %s (ingest_cancelled=%d retract_reconciled=%d failed=%d followup=%v)",
		len(peekedIDs),
		payload.KnowledgeBaseID,
		skippedIngests,
		reconciledRetracts,
		len(failedOps),
		followUpScheduled,
	)
	return len(peekedIDs), followUpScheduled, nil
}

func (s *wikiIngestService) resolveRetractSlugSet(
	ctx context.Context,
	kbID string,
	op WikiPendingOp,
) (map[string]struct{}, error) {
	slugSet := make(map[string]struct{}, len(op.PageSlugs))
	for _, slug := range op.PageSlugs {
		if slug != "" {
			slugSet[slug] = struct{}{}
		}
	}
	if op.KnowledgeID == "" {
		return slugSet, nil
	}
	if s.wikiService == nil {
		return nil, errors.New("wiki ingest: wiki page service is nil")
	}
	livePages, err := s.wikiService.ListPagesBySourceRef(ctx, kbID, op.KnowledgeID)
	if err != nil {
		return nil, fmt.Errorf("list retract pages for knowledge %s: %w", op.KnowledgeID, err)
	}
	for _, page := range livePages {
		if page == nil || page.Slug == "" {
			continue
		}
		if page.PageType == types.WikiPageTypeIndex || page.PageType == types.WikiPageTypeLog {
			continue
		}
		slugSet[page.Slug] = struct{}{}
	}
	return slugSet, nil
}

func (s *wikiIngestService) ProcessWikiIngest(ctx context.Context, t *asynq.Task) (retErr error) {
	taskStartedAt := time.Now()
	retryCount, maxRetry, _ := taskRetryMetadata(ctx)

	var payload WikiIngestPayload
	exitStatus := "success"
	mode := "redis"
	lockAcquired := false
	pendingOpsCount := 0
	ingestOps := 0
	retractOps := 0
	ingestSucceeded := 0
	ingestFailed := 0
	retractHandled := 0
	indexRebuildAttempted := false
	indexRebuildSucceeded := false
	followUpScheduled := false
	totalPagesAffected := 0
	docPreview := make([]string, 0, 6)
	// Tunables resolved from KB.WikiConfig once we've loaded the KB.
	// Captured up here so the deferred stats log can observe them
	// regardless of which exit path we took.
	loggedBatchSize := 0
	loggedMapPar := 0
	loggedReducePar := 0

	defer func() {
		logger.Infof(
			ctx,
			"wiki ingest stats: kb=%s tenant=%d retry=%d/%d status=%s elapsed=%s mode=%s lock_acquired=%v pending_ops=%d ops(ingest=%d,retract=%d) ingest(success=%d,failed=%d) retract_handled=%d pages(total=%d) index(rebuild_attempted=%v,rebuild_succeeded=%v) followup=%v tunables(batch=%d,map_par=%d,reduce_par=%d) preview=%s",
			payload.KnowledgeBaseID,
			payload.TenantID,
			retryCount,
			maxRetry,
			exitStatus,
			time.Since(taskStartedAt).Round(time.Millisecond),
			mode,
			lockAcquired,
			pendingOpsCount,
			ingestOps,
			retractOps,
			ingestSucceeded,
			ingestFailed,
			retractHandled,
			totalPagesAffected,
			indexRebuildAttempted,
			indexRebuildSucceeded,
			followUpScheduled,
			loggedBatchSize,
			loggedMapPar,
			loggedReducePar,
			previewStringSlice(docPreview, 6),
		)
	}()
	// A fenced worker is obsolete, not failed: the newer epoch owns every
	// pending row. Acknowledge this disposable wake-up at the handler boundary
	// so neither Asynq nor the per-op retry path creates a dead letter. The
	// typed error remains visible to repositories and focused race tests.
	defer func() {
		if errors.Is(retErr, wikilease.ErrFenced) {
			exitStatus = "database_lease_fenced"
			logger.Warnf(ctx,
				"wiki ingest: obsolete database lease fenced for KB %s; newer owner will continue",
				payload.KnowledgeBaseID,
			)
			retErr = nil
		}
	}()

	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		exitStatus = "invalid_payload"
		return fmt.Errorf("wiki ingest: unmarshal payload: %w", err)
	}

	// Inject context
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if payload.Language != "" {
		ctx = context.WithValue(ctx, types.LanguageContextKey, payload.Language)
	}
	// Separate the task's business cancellation from lease ownership. The
	// business context follows the Asynq deadline and is also cancelled when
	// Redis renewal proves we no longer own the KB. The lease context is passed
	// into detached queue settlement so lock loss still aborts acknowledgements.
	ctx, cancelBusiness := context.WithCancelCause(ctx)
	defer cancelBusiness(nil)
	leaseCtx, cancelLease := context.WithCancelCause(context.Background())
	defer cancelLease(nil)
	if s.pendingRepo == nil {
		exitStatus = "pending_repo_missing"
		return errors.New("wiki ingest: pending-op repository is nil")
	}

	// Try to acquire the "active batch" flag (non-blocking).
	//
	// TTL is intentionally short (wikiActiveLockTTL ≈ 60s) so that if the
	// owning process dies without releasing the lock (crash, kill -9,
	// container restart), the orphaned key expires within ~1 minute and new
	// tasks aren't starved. A renew goroutine keeps the lock alive while
	// the handler is genuinely running.
	if s.redisClient != nil {
		activeKey := wikiActiveKeyPrefix + payload.KnowledgeBaseID
		lockToken := uuid.NewString()
		acquired, err := s.redisClient.SetNX(ctx, activeKey, lockToken, wikiActiveLockTTL).Result()
		if err != nil {
			exitStatus = "active_lock_acquire_failed"
			// Redis is the cross-process serialization boundary. Continuing
			// without it is unsafe in a multi-replica deployment because two
			// workers could peek, write, and acknowledge the same rows.
			return fmt.Errorf("wiki ingest: acquire active lock for KB %s: %w", payload.KnowledgeBaseID, err)
		} else if !acquired {
			exitStatus = "active_lock_conflict"
			scheduled, scheduleErr := s.scheduleLockConflictFollowUp(ctx, payload)
			followUpScheduled = scheduled
			if scheduleErr != nil {
				exitStatus = "active_lock_conflict_followup_failed"
				return scheduleErr
			}
			logger.Infof(ctx, "wiki ingest: another batch active for KB %s, coalesced a delayed lock-conflict wake-up", payload.KnowledgeBaseID)
			return nil
		}
		lockAcquired = acquired

		lockCtx, cancelLock := context.WithCancel(ctx)
		defer func() {
			cancelLock()
			releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), wikiActiveLockCommandTimeout)
			defer cancelRelease()
			if _, err := wikiActiveLockReleaseScript.Run(
				releaseCtx,
				s.redisClient,
				[]string{activeKey},
				lockToken,
			).Int64(); err != nil {
				logger.Warnf(releaseCtx, "wiki ingest: release active lock for KB %s failed: %v", payload.KnowledgeBaseID, err)
			}
		}()

		go func() {
			ticker := time.NewTicker(wikiActiveLockRenew)
			defer ticker.Stop()
			for {
				select {
				case <-lockCtx.Done():
					return
				case <-ticker.C:
					renewCtx, cancelRenew := context.WithTimeout(lockCtx, wikiActiveLockCommandTimeout)
					result, renewErr := wikiActiveLockRenewScript.Run(
						renewCtx,
						s.redisClient,
						[]string{activeKey},
						lockToken,
						wikiActiveLockTTL.Milliseconds(),
					).Int64()
					cancelRenew()
					if renewErr == nil && result != 0 {
						continue
					}
					var cause error
					if renewErr != nil {
						cause = fmt.Errorf("wiki ingest: renew active lock for KB %s: %w", payload.KnowledgeBaseID, renewErr)
					} else {
						cause = fmt.Errorf("wiki ingest: active lock ownership lost for KB %s", payload.KnowledgeBaseID)
					}
					logger.Errorf(context.Background(), "%v; cancelling current batch", cause)
					cancelLease(cause)
					cancelBusiness(cause)
					return
				}
			}
		}()
	} else {
		mode = "lite"
		// In-process mutual exclusion: mirrors the Redis SetNX lock above.
		if _, loaded := s.liteLocks.LoadOrStore(payload.KnowledgeBaseID, struct{}{}); loaded {
			exitStatus = "active_lock_conflict"
			scheduled, scheduleErr := s.scheduleLockConflictFollowUp(ctx, payload)
			followUpScheduled = scheduled
			if scheduleErr != nil {
				exitStatus = "active_lock_conflict_followup_failed"
				return scheduleErr
			}
			logger.Infof(ctx, "wiki ingest: another batch active for KB %s (lite lock), coalesced a delayed wake-up", payload.KnowledgeBaseID)
			return nil
		}
		lockAcquired = true
		defer s.liteLocks.Delete(payload.KnowledgeBaseID)
	}

	// Redis/Lite ownership is not authoritative because a paused worker can
	// outlive its TTL/cancellation. Advance the durable epoch under the KB row
	// lock before any queue peek, model call, or Wiki side effect. This
	// production extension is mandatory; tests provide an explicit in-memory
	// implementation instead of receiving an unsafe compatibility fallback.
	ctx = wikilease.Require(ctx)
	leaseAcquirer, ok := s.pendingRepo.(wikiDatabaseLeaseAcquirer)
	if !ok || leaseAcquirer == nil {
		exitStatus = "database_lease_repository_missing"
		return errors.New("wiki ingest: pending repository does not support mandatory database lease fencing")
	}
	databaseLease, err := leaseAcquirer.AcquireWikiIngestLease(
		ctx, payload.TenantID, payload.KnowledgeBaseID,
	)
	if errors.Is(err, wikilease.ErrTombstoneDrained) {
		exitStatus = "tombstone_queue_already_drained"
		return nil
	}
	if err != nil {
		exitStatus = "database_lease_acquire_failed"
		return fmt.Errorf("wiki ingest: acquire database lease for KB %s: %w", payload.KnowledgeBaseID, err)
	}
	ctx = wikilease.WithIdentity(ctx, databaseLease)

	kb, err := s.kbService.GetKnowledgeBaseByIDOnly(ctx, payload.KnowledgeBaseID)
	if err != nil && !errors.Is(err, repository.ErrKnowledgeBaseNotFound) {
		// Database/network failures are not terminal: leave every pending row
		// untouched and return the error so Asynq can retry.
		exitStatus = "get_kb_failed"
		return fmt.Errorf("wiki ingest: get KB: %w", err)
	}

	// A not-found result is terminal for this KB-scoped queue. There is no
	// page target left to update, so retaining rows would make the recovery
	// scanner re-trigger them forever. Drain under the same owner lock used by
	// normal batches. Only the explicit repository sentinel is deletion proof;
	// nil-without-error is an invariant violation and must retain pending rows.
	if errors.Is(err, repository.ErrKnowledgeBaseNotFound) {
		loggedBatchSize = wikiMaxDocsPerBatch
		exitStatus = "kb_not_found_queue_draining"
		pendingOpsCount, followUpScheduled, err = s.drainTerminalWikiQueueBatch(
			ctx,
			leaseCtx,
			payload,
			wikiMaxDocsPerBatch,
			"knowledge_base_not_found",
		)
		if err != nil {
			exitStatus = "kb_not_found_queue_drain_failed"
			return err
		}
		if ctxErr := wikiWorkContextError(ctx); ctxErr != nil {
			exitStatus = "context_done_after_terminal_drain"
			return fmt.Errorf("wiki ingest: task context ended after terminal queue drain: %w", ctxErr)
		}
		exitStatus = "kb_not_found_queue_drained"
		return nil
	}
	if kb == nil {
		exitStatus = "get_kb_invariant_failed"
		return fmt.Errorf("wiki ingest: get KB %s: service returned nil without error", payload.KnowledgeBaseID)
	}
	if payload.TenantID == 0 || kb.TenantID != payload.TenantID {
		exitStatus = "kb_tenant_mismatch"
		return fmt.Errorf(
			"wiki ingest: trigger tenant mismatch for KB %s (payload=%d database=%d)",
			payload.KnowledgeBaseID, payload.TenantID, kb.TenantID,
		)
	}

	// Resolve the batch size before checking Wiki enablement. A user may turn
	// Wiki off while a large queue is still pending; terminal draining should
	// keep the configured batch size and avoid loading a synthesis model.
	configuredBatchSize := kb.WikiConfig.IngestBatchSizeOrDefault(wikiMaxDocsPerBatch)
	loggedBatchSize = configuredBatchSize
	if !kb.IsWikiEnabled() {
		exitStatus = "wiki_disabled_queue_draining"
		pendingOpsCount, followUpScheduled, err = s.drainDisabledWikiQueueBatch(
			ctx,
			leaseCtx,
			payload,
			configuredBatchSize,
		)
		if err != nil {
			exitStatus = "wiki_disabled_queue_drain_failed"
			return err
		}
		if ctxErr := wikiWorkContextError(ctx); ctxErr != nil {
			exitStatus = "context_done_after_terminal_drain"
			return fmt.Errorf("wiki ingest: task context ended after terminal queue drain: %w", ctxErr)
		}
		exitStatus = "wiki_disabled_queue_drained"
		return nil
	}

	mapParallel, reduceParallel := wikiqueue.ResolveIngestParallelism(kb.WikiConfig)
	loggedMapPar = mapParallel
	loggedReducePar = reduceParallel
	_, distributedMap := s.pendingRepo.(wikiDistributedMapRepository)
	batchSize := wikiqueue.ResolveIngestBatchSize(kb.WikiConfig, wikiMaxDocsPerBatch)
	if distributedMap {
		// Document-local Map no longer extends this KB lock. Materialize the
		// configured batch (bounded by configuration validation/repository
		// clamp) so ready rows drain efficiently rather than one Map wave at a
		// time.
		batchSize = configuredBatchSize
	}
	loggedBatchSize = batchSize
	if !distributedMap && batchSize < configuredBatchSize {
		logger.Infof(ctx,
			"wiki ingest: limiting configured batch %d to one Map wave (%d) for KB %s",
			configuredBatchSize, batchSize, payload.KnowledgeBaseID)
	}

	// Peek the durable head before resolving model configuration. This lets an
	// empty trigger exit cheaply and, more importantly, lets permanent
	// per-KB configuration errors consume their bounded per-op retry budget
	// instead of returning before queue bookkeeping forever.
	pendingOps, peekedIDs, distributedMap, err := s.peekWikiCommitPendingList(
		ctx, payload.TenantID, payload.KnowledgeBaseID, batchSize,
	)
	if err != nil {
		exitStatus = "peek_pending_failed"
		return err
	}
	pendingOpsCount = len(pendingOps)
	if len(peekedIDs) == 0 {
		if distributedMap {
			exitStatus = "waiting_for_distributed_map"
			logger.Infof(ctx, "wiki ingest: no commit-ready operations for KB %s; distributed Map workers will wake materialization", payload.KnowledgeBaseID)
		} else {
			exitStatus = "no_pending_ops"
			logger.Infof(ctx, "wiki ingest: no pending operations for KB %s", payload.KnowledgeBaseID)
		}
		return nil
	}

	processableOps, preflightFailedOps, staleGenerationOps := s.preflightWikiPendingOps(ctx, payload, pendingOps)
	pendingOps = processableOps
	if staleGenerationOps > 0 {
		logger.Infof(ctx,
			"wiki ingest: terminally discarded %d stale generation op(s) for KB %s",
			staleGenerationOps, payload.KnowledgeBaseID)
	}
	settlePreflightFailures := func(status string) error {
		trimIDs := wikiQueueTrimIDs(peekedIDs, preflightFailedOps)
		followUpScheduled, err = s.settleWikiQueue(ctx, leaseCtx, payload, trimIDs, preflightFailedOps)
		if err != nil {
			exitStatus = status + "_settlement_failed"
			return err
		}
		if ctxErr := wikiWorkContextError(ctx); ctxErr != nil {
			exitStatus = "context_done_after_settlement"
			return fmt.Errorf("wiki ingest: task context ended after preflight settlement: %w", ctxErr)
		}
		exitStatus = status
		return nil
	}
	if len(pendingOps) == 0 {
		return settlePreflightFailures("unsupported_ops_recorded")
	}

	// New trigger payloads are intentionally locale-free so their serialized
	// form is stable for asynq.Unique. Preserve backward compatibility with
	// already-queued payloads that carry Language, otherwise use the first
	// durable per-op locale in this batch.
	if payload.Language == "" {
		for _, op := range pendingOps {
			if op.Language != "" {
				ctx = context.WithValue(ctx, types.LanguageContextKey, op.Language)
				break
			}
		}
	}
	lang := types.LanguageNameFromContext(ctx)

	chatModel, err := GetDerivativeChatModel(
		ctx, s.modelService, kb.DerivativeModelID,
	)
	if err != nil {
		// A dedicated derivative model may be temporarily unavailable (for
		// example, unpublished, isolated by policy, or waiting for its global
		// TPM lease). Preserve that outer deferred-work classification before
		// inspecting the wrapped cause: DeferredError intentionally unwraps to
		// errors such as ErrModelNotFound for diagnostics, but those causes must
		// not turn durable derivative work into a permanent dead letter.
		if modeladmission.IsModelWorkDeferred(err) {
			exitStatus = "derivative_model_deferred"
			return fmt.Errorf("wiki ingest: get derivative chat model: %w", err)
		}
		if errors.Is(err, ErrModelNotFound) || errors.Is(err, ErrChatModelConfiguration) {
			for _, op := range pendingOps {
				op.lastError = fmt.Sprintf("derivative model is permanently unavailable for KB %s: %v", kb.ID, err)
				preflightFailedOps = append(preflightFailedOps, op)
			}
			return settlePreflightFailures("synthesis_model_permanent_error_ops_recorded")
		}
		exitStatus = "get_chat_model_failed"
		return fmt.Errorf("wiki ingest: get chat model: %w", err)
	}

	// Resolve per-KB tunables once. WikiConfig.IngestBatchSize /
	// IngestMapParallel / IngestReduceParallel let operators on
	// 4w-document KBs raise the throughput knob (more docs per batch +
	// more concurrent LLM calls) without a code deploy. Zero falls back to
	// conservative provider-safe defaults; explicit per-KB values remain
	// authoritative and are not capped.
	logger.Infof(ctx, "wiki ingest: batch processing %d ops for KB %s", len(pendingOps), payload.KnowledgeBaseID)

	// Resolve extraction granularity once per batch. Historical rows with
	// empty/unknown values fall back to Standard via Normalize(). Failures
	// to load the KB (unlikely since we're already acting on it) also
	// degrade gracefully to Standard.
	granularity := types.WikiExtractionStandard
	if kb, kbErr := s.kbService.GetKnowledgeBaseByID(ctx, payload.KnowledgeBaseID); kbErr == nil && kb != nil && kb.WikiConfig != nil {
		granularity = kb.WikiConfig.ExtractionGranularity.Normalize()
	}

	batchCtx, batchFetchError := s.newWikiBatchContext(
		payload.KnowledgeBaseID, granularity,
	)

	// 1. MAP PHASE (Parallel extraction and generation of updates)
	var mapMu sync.Mutex
	failedOps := append([]WikiPendingOp(nil), preflightFailedOps...)
	var deferredMu sync.Mutex
	deferredKnowledgeIDs := make(map[string]struct{})
	var providerDeferredErr error
	recordProviderDeferred := func(stageErr error, knowledgeIDs ...string) {
		deferredMu.Lock()
		defer deferredMu.Unlock()
		if providerDeferredErr == nil {
			providerDeferredErr = stageErr
		}
		for _, knowledgeID := range knowledgeIDs {
			if knowledgeID != "" {
				deferredKnowledgeIDs[knowledgeID] = struct{}{}
			}
		}
	}
	slugUpdates := make(map[string][]SlugUpdate)
	var docResults []*docIngestResult
	cancelledRetractKnowledgeIDs := make(map[string]struct{})

	eg, mapCtx := errgroup.WithContext(ctx)
	eg.SetLimit(mapParallel) // Map phase limit (configurable via WikiConfig)

	for _, op := range pendingOps {
		op := op
		eg.Go(func() error {
			if op.Op == WikiOpRetract {
				preparedOp, authorized, prepareErr := s.prepareWikiRetract(
					mapCtx, payload.TenantID, payload.KnowledgeBaseID, op,
				)
				if prepareErr != nil {
					if mapCtx.Err() != nil {
						return fmt.Errorf("retract preflight for %s after context ended: %w", op.KnowledgeID, prepareErr)
					}
					op.lastError = fmt.Sprintf("retract preflight failed: %v", prepareErr)
					mapMu.Lock()
					failedOps = append(failedOps, op)
					docPreview = append(docPreview, fmt.Sprintf("retract_preflight_failed[%s]: %s",
						previewText(op.KnowledgeID, 24), previewText(prepareErr.Error(), 80)))
					mapMu.Unlock()
					return nil
				}
				if !authorized {
					mapMu.Lock()
					retractOps++
					retractHandled++
					cancelledRetractKnowledgeIDs[op.KnowledgeID] = struct{}{}
					docPreview = append(docPreview, fmt.Sprintf("retract_cancelled_active_source[%s]",
						previewText(op.KnowledgeID, 24)))
					mapMu.Unlock()
					logger.Warnf(mapCtx,
						"wiki ingest: cancelled stale retract for active knowledge %s; no pages changed",
						op.KnowledgeID)
					return nil
				}
				op = preparedOp
				// Resolve the authoritative page set at run-time. The caller
				// (knowledgeService.cleanupWikiOnKnowledgeDelete) captures
				// PageSlugs from a DB snapshot taken *before* this task fires,
				// but there is a window where:
				//   - cleanup ran before ingest → snapshot is empty, but a
				//     concurrent ingest may have already created pages by now
				//   - a previous ingest batch created new pages after cleanup
				//     captured its snapshot
				// Re-querying ListPagesBySourceRef here unions the caller's
				// slugs with whatever currently references the knowledge, so
				// no page is left un-retracted. It also lets us support
				// callers that deliberately enqueue retract with empty
				// PageSlugs as "figure it out yourself" — see
				// cleanupWikiOnKnowledgeDelete's comment (3).
				slugSet, err := s.resolveRetractSlugSet(mapCtx, payload.KnowledgeBaseID, op)
				if err != nil {
					if mapCtx.Err() != nil {
						return fmt.Errorf("retract lookup for %s after context ended: %w", op.KnowledgeID, err)
					}
					failedOp := op
					failedOp.lastError = fmt.Sprintf("retract page lookup failed: %v", err)
					mapMu.Lock()
					failedOps = append(failedOps, failedOp)
					docPreview = append(docPreview, fmt.Sprintf("retract_failed[%s]: %s", previewText(op.KnowledgeID, 24), previewText(err.Error(), 80)))
					mapMu.Unlock()
					logger.Warnf(mapCtx, "wiki ingest: retract lookup failed for %s; keeping pending op: %v", op.KnowledgeID, err)
					return nil
				}

				mapMu.Lock()
				retractOps++
				retractHandled++
				docPreview = append(docPreview, fmt.Sprintf("retract[%s]: %s (%d slugs)", previewText(op.KnowledgeID, 24), previewText(op.DocTitle, 48), len(slugSet)))
				for slug := range slugSet {
					slugUpdates[slug] = append(slugUpdates[slug], SlugUpdate{
						Slug:              slug,
						Type:              "retract",
						RetractDocContent: op.DocSummary,
						DocTitle:          op.DocTitle,
						KnowledgeID:       op.KnowledgeID,
						Language:          types.LanguageLocaleName(op.Language),
						SourceChunks:      append([]string(nil), op.SourceChunks...),
						SourceOpID:        op.dbID,
					})
				}
				mapMu.Unlock()
				return nil
			}
			if op.Op != WikiOpIngest {
				failedOp := op
				failedOp.lastError = fmt.Sprintf("unsupported wiki pending op %q", op.Op)
				mapMu.Lock()
				failedOps = append(failedOps, failedOp)
				docPreview = append(docPreview, fmt.Sprintf("unsupported_op[%s]: %q", previewText(op.KnowledgeID, 24), op.Op))
				mapMu.Unlock()
				logger.Warnf(mapCtx, "wiki ingest: unsupported pending op %q for knowledge %s; keeping row for dead-letter", op.Op, op.KnowledgeID)
				return nil
			}

			// Ingest
			mapMu.Lock()
			ingestOps++
			mapMu.Unlock()

			logger.Infof(mapCtx, "wiki ingest: processing document '%s' (%s)", op.DocTitle, op.KnowledgeID)
			result, updates, restored := s.restorePreparedWikiIngest(mapCtx, payload, op)
			var err error
			if distributedMap && !restored && op.MapFinished {
				logger.Infof(mapCtx,
					"wiki ingest: restored no-artifact distributed Map outcome for knowledge %s generation %s",
					op.KnowledgeID, op.ProcessingGeneration)
			} else if distributedMap && !restored {
				err = fmt.Errorf(
					"commit-ready Wiki row %d has no durable Map result",
					op.dbID,
				)
			} else if !restored {
				result, updates, err = s.mapOneDocument(mapCtx, chatModel, payload, op, batchCtx)
				if err == nil && result != nil {
					err = s.checkpointPreparedWikiIngest(mapCtx, payload, op, result, updates)
				} else if err == nil {
					err = s.checkpointWikiMapFinishedWithoutArtifacts(
						mapCtx, payload, op, "no Wiki artifacts were produced for this document",
					)
				}
			} else {
				logger.Infof(mapCtx,
					"wiki ingest: restored durable Map checkpoint for knowledge %s generation %s",
					op.KnowledgeID, op.ProcessingGeneration)
			}
			if err != nil {
				if len(wikiingestguard.StaleIdentities(err)) > 0 {
					logger.Infof(mapCtx,
						"wiki ingest: knowledge %s became stale before Map checkpoint; terminally discarding op",
						op.KnowledgeID)
					return nil
				}
				if mapCtx.Err() == nil && modeladmission.IsModelWorkDeferred(err) {
					// No provider call completed for this document (for
					// example a shared circuit reject). Keep only this row
					// pending and let unrelated documents in the same batch
					// finish. Settlement rotates it without consuming a real
					// provider attempt.
					recordProviderDeferred(err, op.KnowledgeID)
					logger.Infof(mapCtx,
						"wiki ingest: provider deferred Map for knowledge %s; keeping row pending",
						op.KnowledgeID)
					return nil
				}
				mapMu.Lock()
				ingestFailed++
				failedOp := op
				failedOp.lastError = err.Error()
				failedOps = append(failedOps, failedOp)
				mapMu.Unlock()
				logger.Warnf(mapCtx, "wiki ingest: failed to map knowledge %s: %v", op.KnowledgeID, err)
				if mapCtx.Err() != nil {
					return fmt.Errorf("map knowledge %s after context ended: %w", op.KnowledgeID, err)
				}
				return nil // Don't fail the whole batch
			}

			if result != nil {
				mapMu.Lock()
				ingestSucceeded++
				docResults = append(docResults, result)
				docPreview = append(docPreview, fmt.Sprintf("ingest[%s]: title=%s summary=%s", previewText(result.KnowledgeID, 24), previewText(result.DocTitle, 40), previewText(result.Summary, 64)))
				for _, u := range updates {
					slugUpdates[u.Slug] = append(slugUpdates[u.Slug], u)
				}
				mapMu.Unlock()

				// No fail-count reset needed: a successful op is added
				// to peekedIDs and gets DELETEd from task_pending_ops at
				// trim time, so there is no stale fail_count column to
				// scrub. Compare with the legacy Redis path, which kept
				// a separate wiki:failcount:<...> key alive for 24h
				// regardless of whether the original op had drained.
				//
				// Queue acknowledgement happens only after reduce + publish,
				// so a mapped document is not consumed before its wiki writes
				// have reached the batch's terminal bookkeeping point.
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		if modeladmission.IsModelWorkDeferred(err) {
			settleCtx, cancel := newWikiQueueSettlementContext(ctx)
			defer cancel()
			if rotateErr := s.rotateWikiAttempts(settleCtx, pendingOps); rotateErr != nil {
				exitStatus = "provider_deferred_rotation_failed"
				return rotateErr
			}
			scheduled, scheduleErr := s.scheduleProviderCircuitFollowUp(settleCtx, payload, err)
			followUpScheduled = scheduled
			if scheduleErr != nil {
				exitStatus = "provider_circuit_followup_failed"
				return scheduleErr
			}
			exitStatus = "provider_circuit_open"
			return nil
		}
		exitStatus = "map_phase_failed"
		return fmt.Errorf("wiki ingest: map phase: %w", err)
	}
	if err := wikiWorkContextError(ctx); err != nil {
		exitStatus = "map_context_done"
		return fmt.Errorf("wiki ingest: context ended during map phase: %w", err)
	}
	docResults, err = s.filterLiveDocIngestResults(
		ctx, payload.TenantID, payload.KnowledgeBaseID, docResults,
	)
	if err != nil {
		exitStatus = "map_generation_recheck_failed"
		return fmt.Errorf("wiki ingest: recheck mapped generations: %w", err)
	}
	if err := batchFetchError(); err != nil {
		exitStatus = "batch_lookup_failed"
		return fmt.Errorf("wiki ingest: mandatory batch lookup failed during map: %w", err)
	}

	// Plan the directory once for the whole batch BEFORE reduce. Reduce writes
	// pages in parallel, so it can't converge on shared folders on its own; this
	// single pass assigns every new entity/concept slug a coherent category_path
	// that reuses existing folders. Reduce then only applies the plan to pages
	// that don't already have a category (user-curated pages are never churned).
	plannedTaxonomy := s.planBatchTaxonomy(ctx, chatModel, kb, slugUpdates, lang)
	// Taxonomy planning may call both embedding and chat providers. Recheck the
	// exact document generations after those slow calls and before creating any
	// folder row. Stale contributors are removed from both the reduce plan and
	// the taxonomy result; the repository receives the surviving identities so
	// each folder INSERT repeats the check under the KB -> knowledge lock order.
	docResults, staleAfterTaxonomy, err := s.partitionLiveDocIngestResults(
		ctx, payload.TenantID, payload.KnowledgeBaseID, docResults,
	)
	if err != nil {
		exitStatus = "taxonomy_generation_recheck_failed"
		return fmt.Errorf("wiki ingest: recheck generations after taxonomy planning: %w", err)
	}
	if len(staleAfterTaxonomy) > 0 {
		staleKeys := make(map[string]struct{}, len(staleAfterTaxonomy))
		for _, identity := range staleAfterTaxonomy {
			staleKeys[identity.KnowledgeID+"\x00"+identity.ProcessingGeneration] = struct{}{}
		}
		for slug, updates := range slugUpdates {
			kept := updates[:0]
			for _, update := range updates {
				_, stale := staleKeys[update.KnowledgeID+"\x00"+update.ProcessingGeneration]
				if stale && update.ProcessingGeneration != "" {
					continue
				}
				kept = append(kept, update)
			}
			if len(kept) == 0 {
				delete(slugUpdates, slug)
				delete(plannedTaxonomy, slug)
				continue
			}
			slugUpdates[slug] = kept
		}
	}
	taxonomyCtx := wikiingestguard.WithValidation(
		ctx,
		wikiIngestIdentitiesForResults(payload.TenantID, payload.KnowledgeBaseID, docResults)...,
	)
	batchCtx.PlannedFolderID = s.resolvePlannedFolders(taxonomyCtx, kb, plannedTaxonomy)

	// 2. REDUCE PHASE (Parallel upserting grouped by Slug)
	egReduce, reduceCtx := errgroup.WithContext(ctx)
	egReduce.SetLimit(reduceParallel) // Reduce phase limit (LLM + DB concurrent connections, configurable)

	var reduceMu sync.Mutex
	var allPagesAffected []string
	var ingestPagesAffected []string
	var retractPagesAffected []string
	// failedAdditionSlugs collects entity/concept slugs whose page
	// generation LLM call failed (so the page was never written). The
	// post-reduce cleanup step uses this set to (a) strip dead [[slug]]
	// references from the same batch's summary pages, and (b) prune the
	// slugs out of the wiki log feed so users don't see clickable entries
	// pointing at missing pages.
	failedAdditionSlugs := make(map[string]struct{})
	// A non-context reduce failure means every pending op that contributed
	// to that slug is incomplete. Keep those rows for the existing
	// fail_count/dead-letter path; other documents in the batch may still be
	// acknowledged. The map is keyed by knowledge ID so one document that
	// contributed to several failed slugs is counted only once this round.
	failedContributionKnowledgeIDs := make(map[string]struct{})
	failedOpMessages := make(map[string][]string)
	terminalStaleKnowledgeIDs := make(map[string]struct{})
	markTerminalStaleLocked := func(stageErr error) map[string]struct{} {
		stale := make(map[string]struct{})
		for _, identity := range wikiingestguard.StaleIdentities(stageErr) {
			if identity.KnowledgeID == "" {
				continue
			}
			stale[identity.KnowledgeID] = struct{}{}
			terminalStaleKnowledgeIDs[identity.KnowledgeID] = struct{}{}
			delete(failedContributionKnowledgeIDs, identity.KnowledgeID)
			delete(failedOpMessages, identity.KnowledgeID)
		}
		return stale
	}
	markTerminalStale := func(identities []wikiingestguard.Identity) {
		if len(identities) == 0 {
			return
		}
		reduceMu.Lock()
		for _, identity := range identities {
			if identity.KnowledgeID == "" {
				continue
			}
			terminalStaleKnowledgeIDs[identity.KnowledgeID] = struct{}{}
			delete(failedContributionKnowledgeIDs, identity.KnowledgeID)
			delete(failedOpMessages, identity.KnowledgeID)
		}
		reduceMu.Unlock()
	}
	markTerminalStale(staleAfterTaxonomy)
	recordKnowledgeSetFailure := func(knowledgeIDs map[string]struct{}, stage string, stageErr error) {
		if stageErr == nil || len(knowledgeIDs) == 0 {
			return
		}
		message := fmt.Sprintf("%s: %v", stage, stageErr)
		reduceMu.Lock()
		stale := markTerminalStaleLocked(stageErr)
		for knowledgeID := range knowledgeIDs {
			if knowledgeID == "" {
				continue
			}
			if _, terminal := terminalStaleKnowledgeIDs[knowledgeID]; terminal {
				continue
			}
			if _, terminal := stale[knowledgeID]; terminal {
				continue
			}
			failedContributionKnowledgeIDs[knowledgeID] = struct{}{}
			failedOpMessages[knowledgeID] = append(failedOpMessages[knowledgeID], message)
		}
		reduceMu.Unlock()
	}
	recordSlugFailure := func(slug, stage string, stageErr error) {
		if stageErr == nil {
			return
		}
		message := fmt.Sprintf("%s %s: %v", stage, slug, stageErr)
		reduceMu.Lock()
		stale := markTerminalStaleLocked(stageErr)
		seen := make(map[string]struct{}, len(slugUpdates[slug]))
		for _, update := range slugUpdates[slug] {
			if update.Type == types.WikiPageTypeEntity || update.Type == types.WikiPageTypeConcept {
				failedAdditionSlugs[slug] = struct{}{}
			}
			if update.KnowledgeID == "" {
				continue
			}
			if _, terminal := stale[update.KnowledgeID]; terminal {
				continue
			}
			if _, terminal := terminalStaleKnowledgeIDs[update.KnowledgeID]; terminal {
				continue
			}
			failedContributionKnowledgeIDs[update.KnowledgeID] = struct{}{}
			if _, exists := seen[update.KnowledgeID]; !exists {
				failedOpMessages[update.KnowledgeID] = append(failedOpMessages[update.KnowledgeID], message)
				seen[update.KnowledgeID] = struct{}{}
			}
		}
		reduceMu.Unlock()
	}

	// Build the kid → wikiSpan lookup before kicking off reduce. Each
	// per-slug reduce attaches a postprocess.wiki.page[slug] subspan
	// under the FIRST contributing doc's wiki span — see comment in
	// reduceSlugUpdates for the multi-contributor attribution rule.
	kidToWikiSpan := make(map[string]*Span, len(docResults))
	for _, r := range docResults {
		if r != nil && r.WikiSpan != nil {
			kidToWikiSpan[r.KnowledgeID] = r.WikiSpan
		}
	}

	for slug, updates := range slugUpdates {
		slug := slug
		updates := updates
		egReduce.Go(func() error {
			changed, affectedType, additionFailed, err := s.reduceSlugUpdates(reduceCtx, chatModel, payload.KnowledgeBaseID, slug, updates, payload.TenantID, batchCtx, kidToWikiSpan)
			if err != nil {
				logger.Warnf(reduceCtx, "wiki ingest: reduce failed for slug %s: %v", slug, err)
				if reduceCtx.Err() != nil {
					return fmt.Errorf("reduce slug %s after context ended: %w", slug, err)
				}
				if modeladmission.IsModelWorkDeferred(err) {
					// A shared circuit/admission rejection happened before a
					// provider call. Keep only the contributing rows pending
					// and continue settling unrelated documents. This must not
					// mask a real timeout from another concurrent slug: those
					// failures still reach failedOps and consume their bounded
					// attempt budget below.
					contributors := make([]string, 0, len(updates))
					reduceMu.Lock()
					for _, update := range updates {
						if update.Type == types.WikiPageTypeEntity ||
							update.Type == types.WikiPageTypeConcept {
							failedAdditionSlugs[slug] = struct{}{}
						}
						if update.KnowledgeID != "" {
							contributors = append(contributors, update.KnowledgeID)
						}
					}
					reduceMu.Unlock()
					recordProviderDeferred(err, contributors...)
					return nil
				}
				// Attribute an unclassified non-context failure to every live source
				// op for this slug. This preserves partial-batch progress without
				// silently acknowledging documents whose page write failed.
				reduceMu.Lock()
				stale := markTerminalStaleLocked(err)
				seenContributors := make(map[string]struct{}, len(updates))
				for _, update := range updates {
					if update.Type == types.WikiPageTypeEntity || update.Type == types.WikiPageTypeConcept {
						failedAdditionSlugs[slug] = struct{}{}
					}
					if update.KnowledgeID == "" {
						continue
					}
					if _, terminal := stale[update.KnowledgeID]; terminal {
						continue
					}
					if _, terminal := terminalStaleKnowledgeIDs[update.KnowledgeID]; terminal {
						continue
					}
					failedContributionKnowledgeIDs[update.KnowledgeID] = struct{}{}
					if _, seen := seenContributors[update.KnowledgeID]; !seen {
						failedOpMessages[update.KnowledgeID] = append(
							failedOpMessages[update.KnowledgeID],
							fmt.Sprintf("%s: %v", slug, err),
						)
						seenContributors[update.KnowledgeID] = struct{}{}
					}
				}
				reduceMu.Unlock()
				return nil
			}
			if changed {
				reduceMu.Lock()
				allPagesAffected = append(allPagesAffected, slug)
				if affectedType == "ingest" {
					ingestPagesAffected = append(ingestPagesAffected, slug)
				} else if affectedType == "retract" {
					retractPagesAffected = append(retractPagesAffected, slug)
				}
				reduceMu.Unlock()
			}
			if additionFailed {
				recordSlugFailure(slug, "generate wiki page", errors.New("page synthesis failed"))
			}
			return nil
		})
	}
	if err := egReduce.Wait(); err != nil {
		if modeladmission.IsModelWorkDeferred(err) {
			settleCtx, cancel := newWikiQueueSettlementContext(ctx)
			defer cancel()
			if rotateErr := s.rotateWikiAttempts(settleCtx, pendingOps); rotateErr != nil {
				exitStatus = "provider_deferred_rotation_failed"
				return rotateErr
			}
			scheduled, scheduleErr := s.scheduleProviderCircuitFollowUp(settleCtx, payload, err)
			followUpScheduled = scheduled
			if scheduleErr != nil {
				exitStatus = "provider_circuit_followup_failed"
				return scheduleErr
			}
			exitStatus = "provider_circuit_open"
			return nil
		}
		exitStatus = "reduce_phase_failed"
		return fmt.Errorf("wiki ingest: reduce phase: %w", err)
	}
	if err := wikiWorkContextError(ctx); err != nil {
		exitStatus = "reduce_context_done"
		return fmt.Errorf("wiki ingest: context ended during reduce phase: %w", err)
	}
	if err := batchFetchError(); err != nil {
		exitStatus = "batch_lookup_failed"
		return fmt.Errorf("wiki ingest: mandatory batch lookup failed during reduce: %w", err)
	}
	docResults, staleAfterReduce, err := s.partitionLiveDocIngestResults(
		ctx, payload.TenantID, payload.KnowledgeBaseID, docResults,
	)
	if err != nil {
		exitStatus = "reduce_generation_recheck_failed"
		return fmt.Errorf("wiki ingest: recheck generations after reduce: %w", err)
	}
	markTerminalStale(staleAfterReduce)

	failedOps = mergeFailedWikiOps(failedOps, pendingOps, failedContributionKnowledgeIDs)
	failedOps = removeTerminalStaleWikiOps(failedOps, terminalStaleKnowledgeIDs)
	deferredMu.Lock()
	deferredKnowledgeSnapshot := make(map[string]struct{}, len(deferredKnowledgeIDs))
	for knowledgeID := range deferredKnowledgeIDs {
		deferredKnowledgeSnapshot[knowledgeID] = struct{}{}
	}
	deferredMu.Unlock()
	deferredOpsForSettlement := func() []WikiPendingOp {
		candidates := mergeFailedWikiOps(nil, pendingOps, deferredKnowledgeSnapshot)
		candidates = removeTerminalStaleWikiOps(candidates, terminalStaleKnowledgeIDs)
		failedIDs := make(map[int64]struct{}, len(failedOps))
		failedLogical := make(map[string]struct{}, len(failedOps))
		for _, failedOp := range failedOps {
			if failedOp.dbID > 0 {
				failedIDs[failedOp.dbID] = struct{}{}
			}
			failedLogical[failedOp.Op+"\x00"+failedOp.KnowledgeID] = struct{}{}
		}
		filtered := candidates[:0]
		for _, candidate := range candidates {
			if _, failed := failedIDs[candidate.dbID]; candidate.dbID > 0 && failed {
				continue
			}
			if _, failed := failedLogical[candidate.Op+"\x00"+candidate.KnowledgeID]; failed {
				continue
			}
			filtered = append(filtered, candidate)
		}
		return filtered
	}
	deferredOps := deferredOpsForSettlement()
	for i := range failedOps {
		if failures := failedOpMessages[failedOps[i].KnowledgeID]; len(failures) > 0 {
			failedOps[i].lastError = strings.Join(failures, "; ")
		}
	}
	failedKnowledgeIDs := make(map[string]struct{}, len(failedOps))
	for _, op := range failedOps {
		if op.KnowledgeID != "" {
			failedKnowledgeIDs[op.KnowledgeID] = struct{}{}
		}
	}
	for _, op := range deferredOps {
		if op.KnowledgeID != "" {
			failedKnowledgeIDs[op.KnowledgeID] = struct{}{}
		}
	}

	// Sanitize the doc summary pages produced by this batch BEFORE we
	// build log entries / rebuild the index. The summary LLM (run during
	// map) was free to inject [[entity/foo|name]] links to every slug it
	// saw extracted, but reduce may have failed to materialize some of
	// those slugs into actual pages. Rewrite those dead links to plain
	// text so the summary doesn't contain unresolvable references.
	if len(failedAdditionSlugs) > 0 && len(docResults) > 0 {
		for knowledgeID, sanitizeErr := range s.sanitizeDeadSummaryLinks(
			ctx, payload.TenantID, payload.KnowledgeBaseID, docResults, failedAdditionSlugs, batchCtx,
		) {
			recordKnowledgeSetFailure(
				map[string]struct{}{knowledgeID: {}}, "sanitize summary links", sanitizeErr,
			)
		}
	}
	liveBeforeLog, staleBeforeLog, err := s.partitionLiveDocIngestResults(
		ctx, payload.TenantID, payload.KnowledgeBaseID, docResults,
	)
	if err != nil {
		exitStatus = "log_generation_recheck_failed"
		return fmt.Errorf("wiki ingest: recheck generations before log: %w", err)
	}
	docResults = liveBeforeLog
	markTerminalStale(staleBeforeLog)

	totalPagesAffected = len(allPagesAffected)

	// Collect log entries for this batch and flush them in a single INSERT.
	// Historically each op triggered its own `GetLog + UpdatePage` round
	// trip, which rewrote the entire log page TEXT column and caused O(n^2)
	// write amplification as the log grew. AppendBatch writes one row per
	// event into wiki_log_entries instead.
	//
	logEntries := make([]*types.WikiLogEntry, 0, len(pendingOps)+len(docResults))
	logKnowledgeIDs := make(map[string]struct{}, len(pendingOps))
	logIngestIdentities := make([]wikiingestguard.Identity, 0, len(docResults))
	for _, op := range pendingOps {
		if op.Op == WikiOpRetract {
			if _, failed := failedKnowledgeIDs[op.KnowledgeID]; failed {
				continue
			}
			action := "retract"
			title := ""
			summary := ""
			var pages []types.WikiLogPageRef
			if _, cancelled := cancelledRetractKnowledgeIDs[op.KnowledgeID]; cancelled {
				action = "retract_cancelled"
				title = op.DocTitle
				summary = "Source document is active; stale retract was cancelled without changing Wiki pages."
				pages = nil
			}
			// Successful deletion retracts deliberately omit title, summary,
			// and page titles so the Wiki operation log cannot retain parsed
			// source content after the document itself is gone.
			logEntries = append(logEntries, s.buildLogEntry(payload.TenantID, payload.KnowledgeBaseID, action, op.KnowledgeID, title, summary, pages, op.dbID))
			logKnowledgeIDs[op.KnowledgeID] = struct{}{}
		}
	}
	for _, r := range docResults {
		if _, failed := failedKnowledgeIDs[r.KnowledgeID]; failed {
			continue
		}
		// Drop any slugs whose page generation failed in reduce so the
		// log feed never offers a clickable entry that 404s. The summary
		// page itself (slug = summary/<knowledgeID>) is always created
		// unconditionally upstream, so it survives the filter.
		pages := r.Pages
		if len(failedAdditionSlugs) > 0 {
			pages = pages[:0:0]
			for _, ref := range r.Pages {
				if _, bad := failedAdditionSlugs[ref.Slug]; bad {
					continue
				}
				pages = append(pages, ref)
			}
		}
		logEntries = append(logEntries, s.buildLogEntry(payload.TenantID, payload.KnowledgeBaseID, "ingest", r.KnowledgeID, r.DocTitle, r.Summary, pages, r.SourceOpID))
		logKnowledgeIDs[r.KnowledgeID] = struct{}{}
		logIngestIdentities = append(logIngestIdentities, wikiIngestIdentity(
			payload.TenantID, payload.KnowledgeBaseID, r.KnowledgeID, r.ProcessingGeneration,
		))
	}
	if len(logEntries) > 0 {
		if s.logEntrySvc == nil {
			logErr := errors.New("wiki log entry service is nil")
			logger.Warnf(ctx, "wiki ingest: cannot append %d log entries: %v", len(logEntries), logErr)
			recordKnowledgeSetFailure(logKnowledgeIDs, "append wiki log", logErr)
		} else if err := s.logEntrySvc.AppendBatch(
			wikiingestguard.WithValidation(ctx, logIngestIdentities...),
			logEntries,
		); err != nil {
			logger.Warnf(ctx, "wiki ingest: failed to append %d log entries: %v", len(logEntries), err)
			recordKnowledgeSetFailure(logKnowledgeIDs, "append wiki log", err)
		}
	}

	// Build per-operation changes for the Index Intro LLM prompt. Keeping the
	// durable row ID beside each fragment lets rebuildIndexPage skip changes it
	// already committed before a later log/settlement failure triggered retry.
	indexChanges := make([]wikiIndexChange, 0, len(pendingOps))
	indexKnowledgeIDs := make(map[string]struct{}, len(pendingOps))
	if len(docResults) > 0 {
		for _, r := range docResults {
			if _, failed := failedKnowledgeIDs[r.KnowledgeID]; failed {
				continue
			}
			indexChanges = append(indexChanges, wikiIndexChange{
				SourceOpID:           r.SourceOpID,
				KnowledgeID:          r.KnowledgeID,
				ProcessingGeneration: r.ProcessingGeneration,
				Description:          fmt.Sprintf("<document_added>\n<title>%s</title>\n<summary>%s</summary>\n</document_added>\n\n", r.DocTitle, r.Summary),
			})
			indexKnowledgeIDs[r.KnowledgeID] = struct{}{}
		}
	}
	for _, op := range pendingOps {
		if op.Op != WikiOpRetract {
			continue
		}
		if _, failed := failedKnowledgeIDs[op.KnowledgeID]; failed {
			continue
		}
		if _, cancelled := cancelledRetractKnowledgeIDs[op.KnowledgeID]; cancelled {
			continue
		}
		indexChanges = append(indexChanges, wikiIndexChange{
			SourceOpID:  op.dbID,
			KnowledgeID: op.KnowledgeID,
			Description: fmt.Sprintf("<document_removed>\n<title>%s</title>\n<summary>%s</summary>\n</document_removed>\n\n", op.DocTitle, op.DocSummary),
			Retract:     true,
		})
		indexKnowledgeIDs[op.KnowledgeID] = struct{}{}
	}

	// Rebuild index page
	if len(indexChanges) > 0 {
		indexRebuildAttempted = true
		logger.Infof(ctx, "wiki ingest: rebuilding index page")
		if err := s.rebuildIndexPage(ctx, chatModel, payload, indexChanges, lang); err != nil {
			logger.Warnf(ctx, "wiki ingest: rebuild index failed: %v", err)
			recordKnowledgeSetFailure(indexKnowledgeIDs, "rebuild wiki index", err)
			docPreview = append(docPreview, fmt.Sprintf("index_changes=%d", len(indexChanges)))
		} else {
			indexRebuildSucceeded = true
			docPreview = append(docPreview, fmt.Sprintf("index_changes=%d", len(indexChanges)))
		}
	}

	// Clean dead [[slug]] references whenever ANY page was touched this
	// batch (not just retracts). Reduce-phase failures can leave stale
	// references in pages we just rewrote (e.g. summary pages cite
	// failed entity slugs); sanitizeDeadSummaryLinks above handles the
	// well-known summary case, and this pass is the safety net for the
	// long tail (cross-doc citations, prior batches' lingering refs).
	// Dead-link cleanup: scoped to this batch's affected pages so the
	// pass scales with batch size, not with KB size. The lint
	// AutoFix path takes care of long-tail cleanup across the whole
	// KB out-of-band.
	if len(allPagesAffected) > 0 {
		logger.Infof(ctx, "wiki ingest: cleaning dead links")
		for slug, cleanupErr := range s.cleanDeadLinks(
			ctx, payload.TenantID, payload.KnowledgeBaseID, allPagesAffected, slugUpdates, batchCtx,
		) {
			recordSlugFailure(slug, "clean dead links", cleanupErr)
		}
	}

	if len(allPagesAffected) > 0 {
		// Build the freshRefs set: every (slug, title) pair this batch
		// successfully wrote, minus any that landed in failedAdditionSlugs.
		// These are the "newly-mentionable" pages — links to them will
		// not have appeared in older content yet, so injectCrossLinks
		// targets exactly the affected pages with this fresh ref set.
		freshRefs := make([]linkRef, 0, len(docResults)*4)
		for _, dr := range docResults {
			if dr == nil {
				continue
			}
			for _, p := range dr.Pages {
				if p.Slug == "" || p.Title == "" {
					continue
				}
				if _, bad := failedAdditionSlugs[p.Slug]; bad {
					continue
				}
				freshRefs = append(freshRefs, linkRef{slug: p.Slug, matchText: p.Title})
			}
		}

		logger.Infof(ctx, "wiki ingest: injecting cross links")
		for slug, crossLinkErr := range s.injectCrossLinks(
			ctx, payload.TenantID, payload.KnowledgeBaseID, allPagesAffected, freshRefs, slugUpdates, batchCtx,
		) {
			recordSlugFailure(slug, "inject cross links", crossLinkErr)
		}

		logger.Infof(ctx, "wiki ingest: publishing draft pages")
		for slug, publishErr := range s.publishDraftPages(
			ctx, payload.TenantID, payload.KnowledgeBaseID, allPagesAffected, slugUpdates,
		) {
			logger.Warnf(ctx, "wiki ingest: mandatory publication failed for slug %s: %v", slug, publishErr)
			recordSlugFailure(slug, "publish wiki page", publishErr)
		}
	}
	if err := wikiWorkContextError(ctx); err != nil {
		exitStatus = "postprocess_context_done"
		return fmt.Errorf("wiki ingest: context ended during post-processing: %w", err)
	}

	// Log/index/publication failures are attributed after reduce. Merge them
	// into the durable failed-op set now so each contributing document stays
	// pending (once per batch) and eventually carries the concrete reason into
	// task_dead_letters if retries are exhausted.
	liveBeforeSettlement, staleBeforeSettlement, err := s.partitionLiveDocIngestResults(
		ctx, payload.TenantID, payload.KnowledgeBaseID, docResults,
	)
	if err != nil {
		exitStatus = "settlement_generation_recheck_failed"
		return fmt.Errorf("wiki ingest: recheck generations before settlement: %w", err)
	}
	docResults = liveBeforeSettlement
	markTerminalStale(staleBeforeSettlement)
	failedOps = mergeFailedWikiOps(failedOps, pendingOps, failedContributionKnowledgeIDs)
	failedOps = removeTerminalStaleWikiOps(failedOps, terminalStaleKnowledgeIDs)
	deferredOps = deferredOpsForSettlement()
	for i := range failedOps {
		if failures := failedOpMessages[failedOps[i].KnowledgeID]; len(failures) > 0 {
			failedOps[i].lastError = strings.Join(failures, "; ")
		}
	}
	failedKnowledgeIDs = make(map[string]struct{}, len(failedOps))
	for _, op := range failedOps {
		if op.KnowledgeID != "" {
			failedKnowledgeIDs[op.KnowledgeID] = struct{}{}
		}
	}
	for _, op := range deferredOps {
		if op.KnowledgeID != "" {
			failedKnowledgeIDs[op.KnowledgeID] = struct{}{}
		}
	}

	// Close postprocess.wiki spans for every successfully-mapped doc.
	// Span duration now spans map + reduce + index rebuild + cleanup +
	// cross-link injection + publish, matching the wall-clock window
	// the user thinks of as "wiki processing for this knowledge".
	// Per-doc page write outcomes are summarised in the output so the
	// trace viewer can show how many of the doc's extracted pages
	// actually landed (vs. dropped because reduce-phase generation
	// failed).
	failedAdditionSlugCount := len(failedAdditionSlugs)
	for _, r := range docResults {
		if r == nil {
			continue
		}
		if r.WikiSpan == nil {
			continue
		}
		if failures := failedOpMessages[r.KnowledgeID]; len(failures) > 0 {
			materializationErr := errors.New(strings.Join(failures, "; "))
			s.tracker().FailSpan(ctx, r.WikiSpan, "WIKI_MATERIALIZATION_FAILED", materializationErr.Error(), materializationErr)
			continue
		}
		if _, deferred := deferredKnowledgeSnapshot[r.KnowledgeID]; deferred {
			deferredErr := providerDeferredErr
			if deferredErr == nil {
				deferredErr = errors.New("model work was deferred before provider execution")
			}
			s.tracker().FailSpan(
				ctx,
				r.WikiSpan,
				"WIKI_PROVIDER_DEFERRED",
				deferredErr.Error(),
				deferredErr,
			)
			continue
		}
		writtenPages := make([]map[string]string, 0, len(r.Pages))
		droppedPages := make([]map[string]string, 0)
		for _, p := range r.Pages {
			entry := map[string]string{
				"slug":  p.Slug,
				"title": previewText(p.Title, 80),
			}
			if _, bad := failedAdditionSlugs[p.Slug]; bad {
				droppedPages = append(droppedPages, entry)
				continue
			}
			writtenPages = append(writtenPages, entry)
		}
		output := types.JSONMap{
			"pages_written":         len(writtenPages),
			"pages_dropped":         len(droppedPages),
			"pages_total":           len(r.Pages),
			"failed_slug_writes":    failedAdditionSlugCount,
			"pages_written_preview": writtenPages,
		}
		if len(droppedPages) > 0 {
			output["pages_dropped_preview"] = droppedPages
		}
		for k, v := range r.MapStats {
			output[k] = v
		}
		s.tracker().EndSpan(ctx, r.WikiSpan, output)
	}
	// Failed-map docs already had FailSpan called inside
	// mapOneDocument (the failedOps path returns before reaching
	// docResults). Nothing extra to do here for them.

	// Build the trim set: rows that should be removed from
	// task_pending_ops. We start from the full peekedIDs (every row we
	// pulled, even ones de-duplicated by knowledge_id) and subtract
	// any unsettled op's dbID. Real failures stay for retry/dead-letter;
	// provider-deferred rows stay for budget-free rotation.
	if err := batchFetchError(); err != nil {
		exitStatus = "batch_lookup_failed"
		return fmt.Errorf("wiki ingest: mandatory batch lookup failed during post-processing: %w", err)
	}
	// Persist each successful document's terminal Wiki outcome before queue
	// acknowledgement.  If this write fails, no pending row is trimmed, so a
	// later trigger resumes from the durable Map checkpoint and retries the
	// generation-fenced status update.  Stale generations are benign no-ops.
	resultByKnowledgeID := make(map[string]*docIngestResult, len(docResults))
	for _, result := range docResults {
		if result != nil && result.KnowledgeID != "" {
			resultByKnowledgeID[result.KnowledgeID] = result
		}
	}
	for _, op := range pendingOps {
		if op.Op != WikiOpIngest {
			continue
		}
		if _, failed := failedKnowledgeIDs[op.KnowledgeID]; failed {
			continue
		}
		if _, stale := terminalStaleKnowledgeIDs[op.KnowledgeID]; stale {
			continue
		}
		status := types.WikiStatusCompleted
		detail := ""
		result := resultByKnowledgeID[op.KnowledgeID]
		switch {
		case result == nil:
			status = types.WikiStatusDegraded
			detail = strings.TrimSpace(op.MapOutcomeDetail)
			if detail == "" {
				detail = "no Wiki artifacts were produced for this document"
			}
		case wikiMapStatsBool(result.MapStats, "pass0_fallback"):
			status = types.WikiStatusDegraded
			detail = "candidate extraction used the reduced-quality fallback path"
		case wikiMapStatsBool(result.MapStats, "classify_degraded"):
			status = types.WikiStatusDegraded
			failures := wikiMapStatsInt(result.MapStats, "classify_failures")
			detail = fmt.Sprintf("Wiki citation classification completed with %d failed batch(es)", failures)
		}
		if statusErr := s.recordWikiGenerationStatus(ctx, payload, op, status, detail); statusErr != nil {
			exitStatus = "wiki_status_persist_failed"
			return statusErr
		}
	}
	unsettledOps := make([]WikiPendingOp, 0, len(failedOps)+len(deferredOps))
	unsettledOps = append(unsettledOps, failedOps...)
	unsettledOps = append(unsettledOps, deferredOps...)
	trimIDs := wikiQueueTrimIDs(peekedIDs, unsettledOps)
	if err := wikiWorkContextError(ctx); err != nil {
		exitStatus = "postprocess_context_done"
		return fmt.Errorf("wiki ingest: context ended before queue settlement: %w", err)
	}
	// Acknowledge successful rows, record failed attempts, and enqueue the
	// next wake-up using a detached, bounded context. All three operations are
	// attempted even if one fails so that a transient delete error cannot also
	// suppress recovery scheduling. Any failure is returned to asynq.
	followUpScheduled, err = s.settleWikiQueueWithDeferrals(
		ctx,
		leaseCtx,
		payload,
		trimIDs,
		failedOps,
		deferredOps,
		providerDeferredErr,
	)
	if err != nil {
		exitStatus = "queue_settlement_failed"
		logger.Warnf(ctx, "wiki ingest: queue settlement failed for KB %s: %v", payload.KnowledgeBaseID, err)
		return err
	}

	logger.Infof(ctx, "wiki ingest: batch completed for KB %s, %d ops, %d pages affected", payload.KnowledgeBaseID, len(pendingOps), len(allPagesAffected))

	// The queue is now durably settled, but a task whose hard deadline fired
	// must still be reported as an error. Returning nil here was the false
	// success observed in production and prevented Asynq's retry/dead-letter
	// machinery from reflecting the timeout truthfully.
	if ctxErr := wikiWorkContextError(ctx); ctxErr != nil {
		exitStatus = "context_done_after_settlement"
		return fmt.Errorf("wiki ingest: task context ended after durable settlement: %w", ctxErr)
	}
	return nil
}

func (s *wikiIngestService) mapOneDocument(
	ctx context.Context,
	chatModel chat.Chat,
	payload WikiIngestPayload,
	op WikiPendingOp,
	batchCtx *WikiBatchContext,
) (*docIngestResult, []SlugUpdate, error) {
	docStartedAt := time.Now()
	knowledgeID := op.KnowledgeID
	lang := types.LanguageLocaleName(op.Language)

	// Guard against the ingest/delete race: if the user deleted the doc while
	// this task was queued (wikiIngestDelay = 30s) or while an earlier stage
	// was in flight, we must NOT proceed to LLM extraction — doing so would
	// create wiki pages whose source_refs point at a ghost knowledge ID,
	// permanently unreachable via wiki_read_source_doc. This check must also
	// happen before opening the Wiki span: a delayed duplicate that arrives
	// after the exact generation already reached a terminal Wiki outcome is a
	// no-op, not a new observable execution. Opening first used to leave a
	// fresh `running` span behind when that obsolete worker was then killed.
	knowledgeGone, err := s.isWikiIngestGenerationStale(ctx, payload.TenantID, payload.KnowledgeBaseID, op)
	if err != nil {
		return nil, nil, err
	}
	if knowledgeGone {
		logger.Infof(ctx, "wiki ingest: knowledge %s generation %s is stale, skip map", knowledgeID, op.ProcessingGeneration)
		return nil, nil, nil
	}

	// Open a postprocess.wiki subspan under the parent attempt's
	// postprocess stage so the actual per-doc work (LLM extraction +
	// summary + classification) shows up in the trace tree. Returns
	// nil when the parent attempt is gone (no panic on missing
	// lookups — span tracker is best-effort).
	wikiSpan := s.beginWikiSubspan(ctx, knowledgeID, types.JSONMap{
		"language":              lang,
		"knowledge_base_id":     payload.KnowledgeBaseID,
		"processing_generation": op.ProcessingGeneration,
	})

	chunks, logicalChunkCount, err := s.loadWikiLogicalChunks(
		ctx, payload.TenantID, knowledgeID, op.ProcessingGeneration,
	)
	if err != nil {
		s.tracker().FailSpan(ctx, wikiSpan, "LIST_CHUNKS_FAILED", err.Error(), err)
		return nil, nil, fmt.Errorf("get chunks: %w", err)
	}
	if len(chunks) == 0 {
		logger.Infof(ctx, "wiki ingest: document %s has no chunks, skip", knowledgeID)
		s.tracker().SkipSpan(ctx, wikiSpan, "no_chunks")
		return nil, nil, nil
	}

	content := s.reconstructWikiLogicalContent(
		ctx, payload.TenantID, chunks, logicalChunkCount,
	)
	rawRuneCount := len([]rune(content))
	if len([]rune(content)) > maxContentForWiki {
		content = string([]rune(content)[:maxContentForWiki])
	}
	contentDigest := sha256.Sum256([]byte(content))
	contentHash := fmt.Sprintf("%x", contentDigest[:])
	mapCheckpoint := &wikiMapCheckpoint{
		Version:     wikiMapCheckpointVersion,
		ContentHash: contentHash,
	}
	mapCheckpointRestored := false
	if op.MapCheckpoint != nil &&
		op.MapCheckpoint.Version == wikiMapCheckpointVersion &&
		op.MapCheckpoint.ContentHash == contentHash {
		copy := *op.MapCheckpoint
		mapCheckpoint = &copy
		mapCheckpointRestored = true
	}
	logger.Infof(ctx, "wiki ingest: doc %s chunks=%d sampled=%d content_len(raw=%d,truncated=%d)",
		knowledgeID, logicalChunkCount, len(chunks), rawRuneCount, len([]rune(content)))

	// Refuse to run LLM-based extraction when the document carries no real
	// text — e.g. a scanned PDF whose pages were converted to images but where
	// VLM OCR produced nothing usable. Without this guard the LLM would have
	// only image markup left and would happily fabricate entities/concepts.
	if !hasSufficientTextContent(content) {
		logger.Warnf(ctx,
			"wiki ingest: doc %s has insufficient text content after stripping image markup (raw_len=%d), skipping LLM extraction",
			knowledgeID, rawRuneCount,
		)
		s.tracker().SkipSpan(ctx, wikiSpan, "insufficient_text_content")
		return nil, nil, nil
	}

	docTitle := knowledgeID
	if kn, err := s.knowledgeSvc.GetKnowledgeByIDOnly(ctx, knowledgeID); err == nil && kn != nil && kn.Title != "" {
		docTitle = kn.Title
	} else {
		for _, ch := range chunks {
			if ch.Content != "" {
				lines := strings.SplitN(ch.Content, "\n", 2)
				if len(lines) > 0 && len(lines[0]) > 0 && len(lines[0]) < 200 {
					docTitle = strings.TrimPrefix(strings.TrimSpace(lines[0]), "# ")
					break
				}
			}
		}
	}

	// Citation source reference. We deliberately use only the knowledge ID
	// (not docTitle, which is typically the upload filename) so the filename
	// does not leak into citation strings that downstream LLM prompts may
	// surface during wiki page editing.
	sourceRef := knowledgeID
	oldPageSlugs, oldPageChunkRefs, err := s.getExistingPageProvenanceForKnowledge(
		ctx, payload.KnowledgeBaseID, knowledgeID,
	)
	if err != nil {
		s.tracker().FailSpan(ctx, wikiSpan, "LIST_EXISTING_WIKI_PAGES_FAILED", err.Error(), err)
		return nil, nil, err
	}
	oldOwnedChunkRefs := make(map[string][]string, len(oldPageChunkRefs))
	if len(oldPageChunkRefs) > 0 {
		chunkIDRepo, ok := s.chunkRepo.(unscopedChunkIDRepository)
		if !ok || chunkIDRepo == nil {
			err := errors.New("wiki ingest: unscoped chunk repository is unavailable for reparse provenance")
			s.tracker().FailSpan(ctx, wikiSpan, "LIST_EXISTING_WIKI_CHUNKS_FAILED", err.Error(), err)
			return nil, nil, err
		}
		ownerChunkIDs, chunkErr := chunkIDRepo.ListChunkIDsByKnowledgeIDUnscoped(
			ctx, payload.TenantID, knowledgeID,
		)
		if chunkErr != nil {
			err := fmt.Errorf("wiki ingest: list historical chunk IDs for %s: %w", knowledgeID, chunkErr)
			s.tracker().FailSpan(ctx, wikiSpan, "LIST_EXISTING_WIKI_CHUNKS_FAILED", err.Error(), err)
			return nil, nil, err
		}
		oldOwnedChunkRefs = intersectWikiChunkRefsBySlug(oldPageChunkRefs, ownerChunkIDs)
	}
	wikiCacheKey := wikiMapContentCacheKey(
		payload.TenantID,
		knowledgeID,
		op.ProcessingGeneration,
		contentHash,
		chatModel,
		lang,
		batchCtx.ExtractionGranularity,
		oldPageSlugs,
	)
	wikiCacheRef := contentcache.Reference{
		KnowledgeID:          knowledgeID,
		ProcessingGeneration: op.ProcessingGeneration,
	}
	if s.contentCache != nil &&
		!(mapCheckpoint.ExtractionDone && mapCheckpoint.SummaryDone && mapCheckpoint.ClassificationDone) {
		var cached wikiMapCheckpoint
		hit, cacheErr := s.contentCache.GetJSON(ctx, wikiCacheKey, wikiCacheRef, &cached)
		if cacheErr != nil {
			logger.Warnf(ctx, "wiki ingest: Map cache lookup failed for %s: %v", knowledgeID, cacheErr)
			if errors.Is(cacheErr, contentcache.ErrCorruptPayload) {
				evictCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				if evictErr := s.contentCache.Evict(evictCtx, wikiCacheKey); evictErr != nil {
					logger.Warnf(ctx, "wiki ingest: corrupt Map cache eviction failed for %s: %v", knowledgeID, evictErr)
				}
				cancel()
			}
		} else if hit &&
			cached.Version == wikiMapCheckpointVersion &&
			cached.ContentHash == contentHash &&
			cached.ExtractionDone &&
			cached.SummaryDone &&
			cached.ClassificationDone {
			mapCheckpoint = &cached
			mapCheckpointRestored = true
			if checkpointErr := s.checkpointWikiMapProgress(ctx, payload, op, mapCheckpoint); checkpointErr != nil {
				s.tracker().FailSpan(ctx, wikiSpan, "MAP_CACHE_CHECKPOINT_FAILED", checkpointErr.Error(), checkpointErr)
				return nil, nil, checkpointErr
			}
			logger.Infof(ctx, "wiki ingest: restored shared Map cache for knowledge %s", knowledgeID)
		}
	}

	// Pass 0: lightweight candidate slug extraction (skeleton only).
	// On failure we fall back to the legacy single-shot extractor so the doc
	// still gets ingested, just without chunk-level citations.
	var (
		extractedEntities []extractedItem
		extractedConcepts []extractedItem
		slugItems         map[string]extractedItem
		pass0Failed       bool
	)
	extractSpan := s.tracker().BeginSubSpan(ctx, wikiSpan, "postprocess.wiki.extract", types.SpanKindSubSpan, types.JSONMap{
		"content_chars": utf8.RuneCountInString(content),
		"old_pages":     len(oldPageSlugs),
	})
	if mapCheckpoint.ExtractionDone {
		extractedEntities = append([]extractedItem(nil), mapCheckpoint.ExtractedEntities...)
		extractedConcepts = append([]extractedItem(nil), mapCheckpoint.ExtractedConcepts...)
		pass0Failed = mapCheckpoint.Pass0Failed
		slugItems = make(map[string]extractedItem, len(extractedEntities)+len(extractedConcepts))
		for _, item := range extractedEntities {
			if item.Slug != "" && item.Name != "" {
				slugItems[item.Slug] = item
			}
		}
		for _, item := range extractedConcepts {
			if item.Slug != "" && item.Name != "" {
				slugItems[item.Slug] = item
			}
		}
		logger.Infof(ctx, "wiki ingest: restored extraction checkpoint for %s", knowledgeID)
	} else {
		logger.Infof(ctx, "wiki ingest: pass 0 — extracting candidate slugs for %s", knowledgeID)
		extractedEntities, extractedConcepts, slugItems, err = s.extractCandidateSlugs(
			ctx, chatModel, payload.KnowledgeBaseID, content, lang, oldPageSlugs, batchCtx,
		)
		if err != nil {
			if isTransientLLMError(ctx, err) || modeladmission.IsModelWorkDeferred(err) {
				// The legacy extractor uses the same provider. Falling back on
				// transport/rate-limit/circuit failures only doubles pressure
				// and burns the document's durable Wiki retry budget.
				s.tracker().FailSpan(ctx, extractSpan, "EXTRACT_PROVIDER_UNAVAILABLE", err.Error(), err)
				s.tracker().FailSpan(ctx, wikiSpan, "EXTRACT_PROVIDER_UNAVAILABLE", err.Error(), err)
				return nil, nil, err
			}
			logger.Warnf(ctx, "wiki ingest: pass 0 failed for %s (%v) — falling back to legacy extractor", knowledgeID, err)
			pass0Failed = true
			extractedEntities, extractedConcepts, slugItems, err = s.extractEntitiesAndConceptsNoUpsert(
				ctx, chatModel, payload.KnowledgeBaseID, content, lang, oldPageSlugs, batchCtx,
			)
			if err != nil {
				logger.Warnf(ctx, "wiki ingest: legacy fallback also failed for %s: %v", knowledgeID, err)
				s.tracker().FailSpan(ctx, extractSpan, "EXTRACT_FAILED", err.Error(), err)
				s.tracker().FailSpan(ctx, wikiSpan, "EXTRACT_FAILED", err.Error(), err)
				return nil, nil, err
			}
		}
		mapCheckpoint.ExtractedEntities = append([]extractedItem(nil), extractedEntities...)
		mapCheckpoint.ExtractedConcepts = append([]extractedItem(nil), extractedConcepts...)
		mapCheckpoint.Pass0Failed = pass0Failed
		mapCheckpoint.ExtractionDone = true
		if err := s.checkpointWikiMapProgress(ctx, payload, op, mapCheckpoint); err != nil {
			s.tracker().FailSpan(ctx, extractSpan, "EXTRACT_CHECKPOINT_FAILED", err.Error(), err)
			s.tracker().FailSpan(ctx, wikiSpan, "EXTRACT_CHECKPOINT_FAILED", err.Error(), err)
			return nil, nil, err
		}
	}
	s.tracker().EndSpan(ctx, extractSpan, types.JSONMap{
		"entities":         len(extractedEntities),
		"concepts":         len(extractedConcepts),
		"pass0_fallback":   pass0Failed,
		"durable_resume":   mapCheckpointRestored,
		"entities_preview": previewExtractedItems(extractedEntities, 8),
		"concepts_preview": previewExtractedItems(extractedConcepts, 8),
	})

	// Build slug listing for Summary's wiki-link input.
	var summaryExtractedPages []string
	for slug := range slugItems {
		summaryExtractedPages = append(summaryExtractedPages, slug)
	}
	// Wiki summary slug is derived from the knowledge ID rather than the
	// docTitle (which is typically the upload filename). Filename-based slugs
	// like "summary/mx5280-pdf" expose the filename in cross-link contexts
	// that downstream LLM prompts read; a UUID-based slug is uglier but
	// hallucination-safe.
	summarySlug := fmt.Sprintf("summary/%s", slugify(knowledgeID))
	var slugListing string
	for _, slug := range summaryExtractedPages {
		if item, ok := slugItems[slug]; ok {
			aliases := ""
			if len(item.Aliases) > 0 {
				aliases = fmt.Sprintf(" (Aliases: %s)", strings.Join(item.Aliases, ", "))
			}
			slugListing += fmt.Sprintf("- [[%s]] = %s%s\n", slug, item.Name, aliases)
		} else {
			slugListing += fmt.Sprintf("- [[%s]]\n", slug)
		}
	}

	// Summary and chunk classification are independent given Pass 0 output —
	// run them in parallel. Summary handles wiki-link injection; classification
	// attaches concrete chunk IDs to each candidate slug.
	var (
		summaryContent         string
		summaryErr             error
		citations              map[string][]string
		newSlugs               []newSlugFromCitation
		batchCount             int
		classificationFailures int
		classificationErr      error
	)
	summaryAlreadyDone := mapCheckpoint.SummaryDone
	classificationAlreadyDone := mapCheckpoint.ClassificationDone
	if summaryAlreadyDone {
		summaryContent = mapCheckpoint.SummaryContent
	}
	if classificationAlreadyDone {
		citations = mapCheckpoint.Citations
		newSlugs = append([]newSlugFromCitation(nil), mapCheckpoint.NewSlugs...)
		batchCount = mapCheckpoint.ClassificationBatches
		classificationFailures = mapCheckpoint.ClassificationFailures
	}

	// Both calls run in parallel goroutines under the same wikiSpan
	// parent — their subspans will visually overlap in the trace view,
	// which correctly reflects their wall-clock concurrency.
	summarySpan := s.tracker().BeginSubSpan(ctx, wikiSpan, "postprocess.wiki.summary", types.SpanKindSubSpan, types.JSONMap{
		"content_chars":   utf8.RuneCountInString(content),
		"extracted_slugs": len(summaryExtractedPages),
	})
	var classifySpan *Span
	if !pass0Failed {
		classifySpan = s.tracker().BeginSubSpan(ctx, wikiSpan, "postprocess.wiki.classify", types.SpanKindSubSpan, types.JSONMap{
			"chunks":     len(chunks),
			"candidates": len(extractedEntities) + len(extractedConcepts),
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if summaryAlreadyDone {
			sumLine, sumBody := splitSummaryLine(summaryContent)
			s.tracker().EndSpan(ctx, summarySpan, types.JSONMap{
				"chars":          utf8.RuneCountInString(summaryContent),
				"summary_line":   previewText(sumLine, 160),
				"body_preview":   previewText(sumBody, 320),
				"durable_resume": true,
			})
			return
		}
		summaryContent, summaryErr = s.generateWithTemplate(ctx, chatModel, agent.WikiSummaryPrompt, map[string]string{
			"Content":        content,
			"Language":       lang,
			"ExtractedSlugs": slugListing,
		})
		if summaryErr != nil {
			s.tracker().FailSpan(ctx, summarySpan, "SUMMARY_FAILED", summaryErr.Error(), summaryErr)
		} else {
			sumLine, sumBody := splitSummaryLine(summaryContent)
			s.tracker().EndSpan(ctx, summarySpan, types.JSONMap{
				"chars":        utf8.RuneCountInString(summaryContent),
				"summary_line": previewText(sumLine, 160),
				"body_preview": previewText(sumBody, 320),
			})
		}
	}()
	go func() {
		defer wg.Done()
		if classificationAlreadyDone {
			if classifySpan != nil {
				s.tracker().EndSpan(ctx, classifySpan, types.JSONMap{
					"cited_slugs":      len(citations),
					"new_slugs":        len(newSlugs),
					"batches":          batchCount,
					"failed_batches":   classificationFailures,
					"durable_resume":   true,
					"top_cited":        topCitedSlugs(citations, 8),
					"new_slugs_sample": previewNewSlugs(newSlugs, 8),
				})
			}
			return
		}
		// Skip citation pass when Pass 0 has fallen back to the legacy path —
		// the legacy output already contains paraphrased Details, so chunk
		// citations would be redundant and we'd spend LLM calls for nothing.
		if pass0Failed {
			citations = map[string][]string{}
			return
		}
		candidatesXML := renderCandidateSlugsXML(extractedEntities, extractedConcepts)
		citations, newSlugs, batchCount, classificationFailures, classificationErr =
			s.classifyChunkCitations(ctx, chatModel, candidatesXML, chunks, lang, batchCtx)
		if classificationErr != nil {
			s.tracker().FailSpan(
				ctx, classifySpan, "CLASSIFICATION_FAILED",
				classificationErr.Error(), classificationErr,
			)
			return
		}
		s.tracker().EndSpan(ctx, classifySpan, types.JSONMap{
			"cited_slugs":      len(citations),
			"new_slugs":        len(newSlugs),
			"batches":          batchCount,
			"failed_batches":   classificationFailures,
			"degraded":         classificationFailures > 0,
			"top_cited":        topCitedSlugs(citations, 8),
			"new_slugs_sample": previewNewSlugs(newSlugs, 8),
		})
	}()
	wg.Wait()

	checkpointChanged := false
	if !summaryAlreadyDone && summaryErr == nil {
		mapCheckpoint.SummaryContent = summaryContent
		mapCheckpoint.SummaryDone = true
		checkpointChanged = true
	}
	if !classificationAlreadyDone && classificationErr == nil {
		mapCheckpoint.Citations = citations
		mapCheckpoint.NewSlugs = append([]newSlugFromCitation(nil), newSlugs...)
		mapCheckpoint.ClassificationBatches = batchCount
		mapCheckpoint.ClassificationFailures = classificationFailures
		mapCheckpoint.ClassificationDegraded = classificationFailures > 0
		mapCheckpoint.ClassificationDone = true
		checkpointChanged = true
	}
	if checkpointChanged {
		if err := s.checkpointWikiMapProgress(ctx, payload, op, mapCheckpoint); err != nil {
			s.tracker().FailSpan(ctx, wikiSpan, "MAP_CHECKPOINT_FAILED", err.Error(), err)
			return nil, nil, err
		}
	}
	if stageErr := errors.Join(summaryErr, classificationErr); stageErr != nil {
		logger.Errorf(ctx, "wiki ingest: incomplete map stages for %s, will requeue: %v", knowledgeID, stageErr)
		s.tracker().FailSpan(ctx, wikiSpan, "MAP_STAGE_FAILED", stageErr.Error(), stageErr)
		return nil, nil, fmt.Errorf("wiki map stage: %w", stageErr)
	}
	if s.contentCache != nil &&
		mapCheckpoint.ExtractionDone &&
		mapCheckpoint.SummaryDone &&
		mapCheckpoint.ClassificationDone {
		cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cacheErr := s.contentCache.PutJSON(
			cacheCtx,
			wikiCacheKey,
			mapCheckpoint,
			30*24*time.Hour,
			wikiCacheRef,
		)
		cancel()
		if cacheErr != nil && !errors.Is(cacheErr, contentcache.ErrPayloadTooLarge) {
			logger.Warnf(ctx, "wiki ingest: Map cache persist failed for %s: %v", knowledgeID, cacheErr)
		}
	}

	// Merge citations back into the item structs (non-failing; items without
	// citations simply keep their Description+Details fallback).
	var uncited int
	extractedEntities, extractedConcepts, uncited = mergeCitationsIntoItems(extractedEntities, extractedConcepts, citations, newSlugs)

	// Rebuild slugItems so stale entries (for slugs that did not survive the
	// merge) and brand-new slugs discovered by the citation pass are both
	// reflected in summaryExtractedPages tracking.
	slugItems = make(map[string]extractedItem, len(extractedEntities)+len(extractedConcepts))
	for _, item := range extractedEntities {
		if item.Slug != "" && item.Name != "" {
			slugItems[item.Slug] = item
		}
	}
	for _, item := range extractedConcepts {
		if item.Slug != "" && item.Name != "" {
			slugItems[item.Slug] = item
		}
	}

	// extractedPages records every wiki page this document materialized
	// (entities, concepts, plus the summary page appended below). The
	// slug is used for link/retract bookkeeping; the title is captured
	// for the log feed so the user sees "提供本学位在线验证报告查询…"
	// rather than "entity/xue-xin-wang".
	extractedPages := make([]types.WikiLogPageRef, 0, len(slugItems)+1)
	for slug, item := range slugItems {
		title := item.Name
		if title == "" {
			title = slug
		}
		extractedPages = append(extractedPages, types.WikiLogPageRef{Slug: slug, Title: title})
	}

	// Count total distinct chunks cited across all slugs for logging.
	citedChunkSet := make(map[string]bool)
	for _, ids := range citations {
		for _, id := range ids {
			citedChunkSet[id] = true
		}
	}

	var updates []SlugUpdate
	// docSummaryLine is the one-sentence headline used for terse log/audit
	// previews and for <document_added> blocks in retract prompts.
	// docSummary is the full summary body attached to each entity/concept
	// update so the editor model gets rich framing in <source_context>.
	var docSummaryLine string
	var docSummary string

	sumLine, sumBody := splitSummaryLine(summaryContent)
	if sumBody == "" {
		sumBody = summaryContent
	}
	if sumLine == "" {
		sumLine = docTitle
	}
	docSummaryLine = sumLine
	docSummary = sumBody
	if strings.TrimSpace(docSummary) == "" {
		docSummary = sumLine
	}
	updates = append(updates, SlugUpdate{
		Slug:        summarySlug,
		Type:        types.WikiPageTypeSummary,
		DocTitle:    docTitle,
		KnowledgeID: knowledgeID,
		SourceRef:   sourceRef,
		Language:    lang,
		SummaryLine: sumLine,
		SummaryBody: sumBody,
	})
	extractedPages = append(extractedPages, types.WikiLogPageRef{Slug: summarySlug, Title: docTitle})

	// Entities
	for _, item := range extractedEntities {
		if item.Slug != "" {
			updates = append(updates, SlugUpdate{
				Slug:         item.Slug,
				Type:         types.WikiPageTypeEntity,
				Item:         item,
				DocTitle:     docTitle,
				KnowledgeID:  knowledgeID,
				SourceRef:    sourceRef,
				Language:     lang,
				SourceChunks: item.SourceChunks,
				DocSummary:   docSummary,
			})
		}
	}

	// Concepts
	for _, item := range extractedConcepts {
		if item.Slug != "" {
			updates = append(updates, SlugUpdate{
				Slug:         item.Slug,
				Type:         types.WikiPageTypeConcept,
				Item:         item,
				DocTitle:     docTitle,
				KnowledgeID:  knowledgeID,
				SourceRef:    sourceRef,
				Language:     lang,
				SourceChunks: item.SourceChunks,
				DocSummary:   docSummary,
			})
		}
	}

	// Reconcile old page set against new extraction.
	//
	// Three cases:
	//
	//  (a) oldSlug ∉ new  → "retractStale": the doc no longer mentions this
	//      page's subject, so strip its ref (and possibly delete the page
	//      if this was the only source). Passes the NEW content as the
	//      retract context — if the LLM finds matching facts it trims
	//      them, otherwise the retract is a near no-op, which is fine.
	//
	//  (b) oldSlug ∈ new AND slug is an entity/concept page  → reparse
	//      swap: emit BOTH a "retract" (carrying the doc's PRIOR summary
	//      body as the old-version signal) AND the normal addition. The
	//      reduce stage sees HasAdditions=1 + HasRetractions=1 and the
	//      WikiPageModifyPrompt correctly tells the editor model to
	//      remove the old K section and add the new K section in one
	//      pass — giving us replace-not-append semantics that "append
	//      new K on top of old K" would otherwise violate.
	//
	//  (c) oldSlug ∈ new AND slug is a summary page (summary/...) →
	//      nothing to do here. reduceSlugUpdates' summary branch
	//      unconditionally overwrites the whole page from the new
	//      SummaryBody, so emitting an extra retract would just be
	//      dead weight that the summary branch discards anyway.
	//
	// priorContribution is the doc's LAST summary body, fetched lazily
	// at this point (rather than pre-loaded into the batch context).
	// Empty on first-ever ingest — in that case oldPageSlugs is also
	// empty, so we never consult it.
	priorContribution := batchCtx.SummaryContentByKnowledgeID(ctx, knowledgeID)

	newSlugSet := make(map[string]bool, len(extractedPages))
	for _, ns := range extractedPages {
		newSlugSet[ns.Slug] = true
	}

	var reparseOverlap, staleCount int
	updates, reparseOverlap, staleCount = appendWikiReparseReconciliation(
		updates,
		oldPageSlugs,
		newSlugSet,
		oldOwnedChunkRefs,
		priorContribution,
		content,
		docTitle,
		knowledgeID,
		lang,
	)

	logger.Infof(ctx,
		"wiki ingest: mapped knowledge %s title=%q candidates=%d chunks=%d batches=%d cited_chunks=%d uncited_slugs=%d new_slugs=%d updates=%d reparse_slugs=%d stale_slugs=%d pass0_fallback=%v elapsed=%s",
		knowledgeID, previewText(docTitle, 80),
		len(slugItems), len(chunks), batchCount, len(citedChunkSet), uncited, len(newSlugs),
		len(updates), reparseOverlap, staleCount, pass0Failed,
		time.Since(docStartedAt).Round(time.Millisecond),
	)

	// Map-phase metrics get attached to the postprocess.wiki span's
	// output, but we do NOT EndSpan here — the batch driver keeps the
	// span open through reduce + index rebuild + cross-link injection
	// + page publish, then closes it once this doc's pages have all
	// been written. That way the span's duration reflects the full
	// "wiki processing for this knowledge" time the user sees in the
	// trace viewer, not just the LLM extraction slice.
	mapStats := types.JSONMap{
		"doc_title":         previewText(docTitle, 120),
		"chunks":            len(chunks),
		"candidate_slugs":   len(slugItems),
		"cited_chunks":      len(citedChunkSet),
		"uncited_slugs":     uncited,
		"new_slugs":         len(newSlugs),
		"updates":           len(updates),
		"reparse_slugs":     reparseOverlap,
		"stale_slugs":       staleCount,
		"extracted_pages":   len(extractedPages),
		"summary_chars":     utf8.RuneCountInString(docSummary),
		"pass0_fallback":    pass0Failed,
		"classify_batches":  batchCount,
		"classify_failures": classificationFailures,
		"classify_degraded": classificationFailures > 0,
		"summary_preview":   previewText(docSummaryLine, 160),
	}

	for i := range updates {
		updates[i].ProcessingGeneration = op.ProcessingGeneration
		updates[i].SourceOpID = op.dbID
	}

	return &docIngestResult{
		KnowledgeID:          knowledgeID,
		ProcessingGeneration: op.ProcessingGeneration,
		DocTitle:             docTitle,
		Summary:              docSummaryLine,
		SourceOpID:           op.dbID,
		Pages:                extractedPages,
		MapStats:             mapStats,
		WikiSpan:             wikiSpan,
	}, updates, nil
}

func (s *wikiIngestService) extractEntitiesAndConceptsNoUpsert(
	ctx context.Context,
	chatModel chat.Chat,
	kbID string,
	content, lang string,
	oldPageSlugs map[string]bool,
	batchCtx *WikiBatchContext,
) ([]extractedItem, []extractedItem, map[string]extractedItem, error) {
	// Only entity/* and concept/* slugs are relevant for LLM slug-continuity —
	// summary slugs are code-generated from the knowledge ID and never appear
	// in the extraction output, so including them just wastes tokens and risks
	// confusing the model.
	var prevSlugsText string
	if len(oldPageSlugs) > 0 {
		var sb strings.Builder
		for slug := range oldPageSlugs {
			if !strings.HasPrefix(slug, "entity/") && !strings.HasPrefix(slug, "concept/") {
				continue
			}
			fmt.Fprintf(&sb, "- %s\n", slug)
		}
		prevSlugsText = sb.String()
	}
	if prevSlugsText == "" {
		prevSlugsText = "(none — this is a new document)"
	}

	extractionJSON, err := s.generateWithTemplate(ctx, chatModel, agent.WikiKnowledgeExtractPrompt, map[string]string{
		"Content":       content,
		"Language":      lang,
		"PreviousSlugs": prevSlugsText,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("combined extraction failed: %w", err)
	}

	extractionJSON = cleanLLMJSON(extractionJSON)

	var result combinedExtraction
	if err := json.Unmarshal([]byte(extractionJSON), &result); err != nil {
		logger.Warnf(ctx, "wiki ingest: failed to parse combined extraction JSON: %v\nRaw: %s", err, extractionJSON)
		return nil, nil, nil, fmt.Errorf("parse combined extraction JSON: %w", err)
	}

	// Dedup pre-filter is dispatched against the wiki page repo via
	// pg_trgm (see deduplicateExtractedBatch). Until the trgm path
	// lands the dedup pre-filter degrades to "no dedup" which is the
	// safe default — the LLM merge call simply doesn't get a candidate
	// list and the items pass through unchanged.
	result.Entities, result.Concepts = s.deduplicateExtractedBatch(
		ctx, chatModel, kbID, result.Entities, result.Concepts,
	)

	slugItems := make(map[string]extractedItem)
	for _, item := range result.Entities {
		if item.Slug != "" && item.Name != "" {
			slugItems[item.Slug] = item
		}
	}
	for _, item := range result.Concepts {
		if item.Slug != "" && item.Name != "" {
			slugItems[item.Slug] = item
		}
	}

	return result.Entities, result.Concepts, slugItems, nil
}

// reduceSlugUpdates returns:
//   - changed:          whether the wiki page was created or updated
//   - affectedType:     "ingest" or "retract" — drives downstream bookkeeping
//   - additionFailed:   true iff the slug had entity/concept additions queued
//     AND the WikiPageModifyPrompt LLM call failed, so no page exists/was
//     refreshed for it. Callers use this to sanitize dead [[slug]] links
//     elsewhere (e.g. in the doc's summary page) and to drop the slug from
//     the wiki log feed so users don't see a clickable entry that 404s.
//   - err:              transport / repo error from the persisted upsert.
func (s *wikiIngestService) reduceSlugUpdates(
	ctx context.Context,
	chatModel chat.Chat,
	kbID string,
	slug string,
	updates []SlugUpdate,
	tenantID uint64,
	batchCtx *WikiBatchContext,
	kidToWikiSpan map[string]*Span,
) (changed bool, affectedType string, additionFailed bool, err error) {
	// Final safety net for the ingest/delete race: between Map (which already
	// checks isKnowledgeGone) and Reduce there is a long LLM call where the
	// source document may be deleted. Drop any addition/summary updates whose
	// knowledge no longer exists so we don't resurrect a ghost source_ref.
	// Retract updates are kept — they actively remove refs, which is what we
	// want when the doc is gone.
	updates, err = s.filterLiveUpdates(ctx, tenantID, kbID, updates)
	if err != nil {
		return false, "", false, err
	}
	if len(updates) == 0 {
		return false, "", false, nil
	}

	// Per-slug page span attribution: a single slug can receive
	// contributions from multiple docs in the same batch (entity /
	// concept pages aggregate across sources). We attach the
	// postprocess.wiki.page[slug] subspan under whichever
	// contributing doc's wikiSpan is encountered first in the updates
	// list — span tree topology only allows one parent. Every
	// contributing knowledge id is recorded in the span's `contributors`
	// output so users can still see the full attribution. Pages whose
	// only contributors had no wikiSpan (e.g. their parse attempt
	// already closed and was archived) simply get a nil pageSpan,
	// which the tracker helpers no-op on.
	var (
		pageSpan     *Span
		contributors []string
	)
	{
		seen := make(map[string]bool, len(updates))
		for _, u := range updates {
			kid := u.KnowledgeID
			if kid == "" || seen[kid] {
				continue
			}
			seen[kid] = true
			contributors = append(contributors, kid)
			if pageSpan == nil {
				if sp, ok := kidToWikiSpan[kid]; ok && sp != nil {
					pageSpan = s.tracker().BeginSubSpan(ctx, sp, fmt.Sprintf("postprocess.wiki.page[%s]", slug), types.SpanKindSubSpan, types.JSONMap{
						"slug":         slug,
						"updates":      len(updates),
						"contributors": contributors,
					})
				}
			}
		}
	}
	var page *types.WikiPage
	// Deferred output captures `&page` so it observes the post-merge
	// state (title, page type, content snippet) at function return —
	// that's what's actually useful in the trace viewer, not the
	// stale pre-reduce shell that exists when the defer is registered.
	defer func() {
		if pageSpan == nil {
			return
		}
		if err != nil {
			s.tracker().FailSpan(ctx, pageSpan, "REDUCE_FAILED", err.Error(), err)
			return
		}
		if !changed {
			s.tracker().SkipSpan(ctx, pageSpan, "no_change")
			return
		}
		out := types.JSONMap{
			"affected_type":   affectedType,
			"addition_failed": additionFailed,
			"contributors":    contributors,
		}
		if page != nil {
			out["page_title"] = previewText(page.Title, 160)
			out["page_type"] = string(page.PageType)
			out["page_summary"] = previewText(page.Summary, 200)
			out["content_preview"] = previewText(page.Content, 320)
			out["source_refs"] = len(page.SourceRefs)
			out["chunk_refs"] = len(page.ChunkRefs)
			out["aliases"] = []string(page.Aliases)
		}
		s.tracker().EndSpan(ctx, pageSpan, out)
	}()

	page, err = s.wikiService.GetPageBySlug(ctx, kbID, slug)
	exists := false
	switch {
	case err == nil && page != nil:
		exists = true
	case errors.Is(err, repository.ErrWikiPageNotFound):
		// Explicit absence is the only state in which creating a new page
		// is safe. A transient read error must never be reinterpreted as a
		// miss, otherwise CreatePage can race an existing row and the caller
		// may still acknowledge its pending operation.
		err = nil
	case err != nil:
		return false, "", false, fmt.Errorf("get wiki page %s: %w", slug, err)
	default:
		return false, "", false, fmt.Errorf("get wiki page %s: service returned nil without error", slug)
	}

	// Page mutations are checkpointed in the owning task_pending_ops payload
	// inside the same transaction as the Wiki write. On a retry after a later
	// log/index/publication/settlement failure, remove the already-committed
	// contribution before any reducer LLM call. Returning changed=true for an
	// applied ingest keeps the slug in mandatory publish/post-processing; those
	// stages are independently idempotent and may be the reason for this retry.
	if exists {
		pendingUpdates := updates[:0]
		appliedIngest := false
		appliedRetract := false
		for _, update := range updates {
			if update.PageAlreadyApplied {
				switch update.Type {
				case types.WikiPageTypeEntity, types.WikiPageTypeConcept, "summary":
					appliedIngest = true
				case "retract", "retractStale":
					appliedRetract = true
				}
				continue
			}
			pendingUpdates = append(pendingUpdates, update)
		}
		updates = pendingUpdates
		if len(updates) == 0 {
			if appliedIngest {
				return true, "ingest", false, nil
			}
			if appliedRetract {
				return false, "retract", false, nil
			}
			return false, "", false, nil
		}
	}

	if !exists {
		hasAdditions := false
		for _, u := range updates {
			if u.Type == types.WikiPageTypeEntity || u.Type == types.WikiPageTypeConcept || u.Type == "summary" {
				hasAdditions = true
				break
			}
		}
		if !hasAdditions {
			return false, "", false, nil
		}

		page = &types.WikiPage{
			ID:              uuid.New().String(),
			TenantID:        tenantID,
			KnowledgeBaseID: kbID,
			Slug:            slug,
			Status:          types.WikiPageStatusDraft,
			SourceRefs:      types.StringArray{},
			Aliases:         types.StringArray{},
		}
	}

	affectedType = "ingest"

	var summaryUpdate *SlugUpdate
	var retracts []SlugUpdate
	var additions []SlugUpdate

	for i, u := range updates {
		if u.Type == "summary" {
			summaryUpdate = &updates[i]
		} else if u.Type == "retract" || u.Type == "retractStale" {
			retracts = append(retracts, u)
			affectedType = "retract"
		} else if u.Type == types.WikiPageTypeEntity || u.Type == types.WikiPageTypeConcept {
			additions = append(additions, u)
			affectedType = "ingest" // Additions override retracts type
		}
	}

	// A page write can succeed while a later log/index/queue-settlement step
	// fails. On retry, skip only retracts whose exact durable operation ID was
	// recorded atomically with that page write. Source-ref absence alone is not
	// proof: older immediate cleanup removed refs before cleaning shared prose.
	if exists && len(retracts) > 0 {
		pendingRetracts := retracts[:0]
		for _, retract := range retracts {
			applied, markerErr := wikidelete.IsApplied(page, retract.SourceOpID)
			if markerErr != nil {
				return false, affectedType, false,
					fmt.Errorf("read retract idempotency marker for %s: %w", slug, markerErr)
			}
			if applied {
				continue
			}
			pendingRetracts = append(pendingRetracts, retract)
		}
		retracts = pendingRetracts
		if len(retracts) == 0 && len(additions) == 0 && summaryUpdate == nil {
			return false, "retract", false, nil
		}
	}

	if summaryUpdate != nil {
		updates, err = s.requireAllLiveUpdates(ctx, tenantID, kbID, updates)
		if err != nil {
			return false, affectedType, false, fmt.Errorf("revalidate summary page %s before write: %w", slug, err)
		}
		if len(updates) == 0 {
			return false, affectedType, false, nil
		}
		// Rebind the pointer after the validation helper returned a fresh slice.
		summaryUpdate = nil
		for i := range updates {
			if updates[i].Type == "summary" {
				summaryUpdate = &updates[i]
				break
			}
		}
		if summaryUpdate == nil {
			return false, affectedType, false, nil
		}
		page.Title = summaryUpdate.DocTitle + " - Summary"
		page.Content = summaryUpdate.SummaryBody
		page.Summary = summaryUpdate.SummaryLine
		page.PageType = types.WikiPageTypeSummary
		page.SourceRefs = appendUnique(page.SourceRefs, summaryUpdate.SourceRef)
		// Summary pages don't carry chunk-level citations (they are document-
		// level synopses generated from the whole content). Clear any stale
		// chunk refs that may remain if this slug was once an entity page
		// and got converted to a summary page.
		page.ChunkRefs = types.StringArray{}
		changed = true

		writeCtx := wikiIngestPageApplicationContext(ctx, tenantID, kbID, slug, updates)
		if exists {
			_, err = s.wikiService.UpdatePage(writeCtx, page)
		} else {
			_, err = s.wikiService.CreatePage(writeCtx, page)
		}
		return changed, affectedType, false, err
	}

	var remainingSourcesContent strings.Builder
	var deletedContent strings.Builder
	var relatedSlugs strings.Builder
	var newContentBuilder strings.Builder
	var docTitles []string
	var language string
	var appliedRetractSourceIDs []string
	var appliedRetractOpIDs []int64
	var retractedSourceRefs types.StringArray
	var retractedChunkRefs types.StringArray

	if len(retracts) > 0 {
		language = retracts[0].Language

		for _, r := range retracts {
			fmt.Fprintf(&deletedContent, "<document>\n<title>%s</title>\n<content>\n%s\n</content>\n</document>\n\n", r.DocTitle, r.RetractDocContent)
			appliedRetractSourceIDs = append(appliedRetractSourceIDs, r.KnowledgeID)
			if r.SourceOpID > 0 {
				appliedRetractOpIDs = append(appliedRetractOpIDs, r.SourceOpID)
			}
		}

		retractKIDs := make(map[string]bool)
		for _, r := range retracts {
			retractKIDs[r.KnowledgeID] = true
		}

		for _, ref := range page.SourceRefs {
			pipeIdx := strings.Index(ref, "|")
			var refKnowledgeID, refTitle string
			if pipeIdx > 0 {
				refKnowledgeID = ref[:pipeIdx]
				refTitle = ref[pipeIdx+1:]
			} else {
				refKnowledgeID = ref
				refTitle = ref
			}

			if retractKIDs[refKnowledgeID] {
				continue
			}

			if content := batchCtx.SummaryContentByKnowledgeID(ctx, refKnowledgeID); content != "" {
				fmt.Fprintf(&remainingSourcesContent, "<document>\n<title>%s</title>\n<content>\n%s\n</content>\n</document>\n\n", refTitle, content)
			} else {
				fmt.Fprintf(&remainingSourcesContent, "<document>\n<title>%s</title>\n<content>\n(summary not available)\n</content>\n</document>\n\n", refTitle)
			}
		}
		if remainingSourcesContent.Len() == 0 {
			remainingSourcesContent.WriteString("(no remaining sources)")
		}

		newRefs := types.StringArray{}
		for _, ref := range page.SourceRefs {
			pipeIdx := strings.Index(ref, "|")
			refKnowledgeID := ref
			if pipeIdx > 0 {
				refKnowledgeID = ref[:pipeIdx]
			}
			if !retractKIDs[refKnowledgeID] {
				newRefs = append(newRefs, ref)
			}
		}
		retractedSourceRefs = newRefs
		retractedChunkRefs = removeChunkRefsFromRetracts(page.ChunkRefs, retracts)
		// A retract that removes the final owner is deterministic deletion,
		// not an LLM editing problem. Keeping an empty-source page would expose
		// orphaned content, especially when the synchronous delete fast path
		// previously failed and this durable retry is the last safety net.
		if len(retractedSourceRefs) == 0 && len(additions) == 0 {
			if _, err := s.requireAllLiveUpdates(ctx, tenantID, kbID, updates); err != nil {
				return false, affectedType, false, fmt.Errorf("revalidate sole-source page %s before delete: %w", slug, err)
			}
			writeCtx := wikiIngestPageApplicationContext(ctx, tenantID, kbID, slug, updates)
			if err := s.wikiService.DeletePage(writeCtx, kbID, slug); err != nil &&
				!errors.Is(err, repository.ErrWikiPageNotFound) {
				return false, affectedType, false, fmt.Errorf("delete sole-source wiki page %s: %w", slug, err)
			}
			return true, affectedType, false, nil
		}

		// Persist a visibility barrier before the model call. A timeout, crash,
		// or permanent model error leaves the shared page archived rather than
		// serving prose from the deleted source. Source refs remain in the DB
		// until the successful content write below.
		if err := wikidelete.Quarantine(page, appliedRetractSourceIDs...); err != nil {
			return false, affectedType, false, fmt.Errorf("quarantine wiki page %s: %w", slug, err)
		}
		if _, err := s.requireAllLiveUpdates(ctx, tenantID, kbID, updates); err != nil {
			return false, affectedType, false, fmt.Errorf("revalidate wiki page %s before quarantine: %w", slug, err)
		}
		quarantineCtx := wikiIngestValidationContextForUpdates(ctx, tenantID, kbID, updates)
		if err := s.wikiService.UpdatePageMeta(quarantineCtx, page); err != nil {
			return false, affectedType, false, fmt.Errorf("persist wiki page %s quarantine: %w", slug, err)
		}
	}

	if len(additions) > 0 {
		language = additions[0].Language

		// Resolve SourceChunks → chunk contents in a single batched query per
		// knowledge ID, so the <new_information> block can quote the chunks
		// verbatim instead of relying on the short Details paraphrase.
		chunkContentByID := s.resolveCitedChunks(ctx, tenantID, additions)

		for _, add := range additions {
			cited := collectCitedChunkContent(add.SourceChunks, chunkContentByID)
			// Frame the chunks with the document-level summary body so the
			// editor model knows BOTH what the document is about AND what
			// kind of document it is (resume vs announcement vs product
			// page vs schedule). The one-sentence headline alone was too
			// terse to keep the editor grounded on longer or multi-topic
			// source documents, and calibrating tone (self-reported vs
			// third-party authoritative) benefits from the richer context.
			sourceCtx := strings.TrimSpace(add.DocSummary)
			sourceCtxBlock := ""
			if sourceCtx != "" {
				sourceCtxBlock = fmt.Sprintf("<source_context>\n%s\n</source_context>\n", sourceCtx)
			}
			if cited != "" {
				fmt.Fprintf(&newContentBuilder,
					"<document>\n<title>%s</title>\n%s<content>\n**%s**: %s\n\n%s\n</content>\n</document>\n\n",
					add.DocTitle, sourceCtxBlock, add.Item.Name, add.Item.Description, cited)
			} else {
				// Fallback: no citations available (legacy path, citation pass
				// failed, or bad chunk IDs were filtered out) — stick with
				// the short Details summary so the page still gets real text.
				fmt.Fprintf(&newContentBuilder,
					"<document>\n<title>%s</title>\n%s<content>\n**%s**: %s\n\n%s\n</content>\n</document>\n\n",
					add.DocTitle, sourceCtxBlock, add.Item.Name, add.Item.Description, add.Item.Details)
			}
			docTitles = appendUnique(docTitles, add.DocTitle)

			for _, alias := range add.Item.Aliases {
				page.Aliases = appendUnique(page.Aliases, alias)
			}
			page.SourceRefs = appendUnique(page.SourceRefs, add.SourceRef)

			if page.Title == "" {
				page.Title = add.Item.Name
			}
			if page.PageType == "" {
				page.PageType = add.Type
			}
		}
	}

	if len(additions) > 0 || len(retracts) > 0 {
		titles := batchCtx.SlugTitleMany(ctx, []string(page.OutLinks))
		for _, outSlug := range page.OutLinks {
			if title := titles[outSlug]; title != "" {
				fmt.Fprintf(&relatedSlugs, "- %s (%s)\n", outSlug, title)
			}
		}

		existingContent := page.Content
		if !exists || existingContent == "" {
			existingContent = "(New page)"
		}

		hasAdditionsStr := ""
		if len(additions) > 0 {
			hasAdditionsStr = "1"
		}
		hasRetractionsStr := ""
		if len(retracts) > 0 {
			hasRetractionsStr = "1"
		}

		// Fall back gracefully if title/type are still unset (shouldn't happen
		// for well-formed updates — both get populated from `additions` above,
		// and retract-only paths require an existing page — but stay defensive
		// so we never feed the LLM an empty identity block).
		pageTitle := page.Title
		if pageTitle == "" {
			pageTitle = slug
		}
		pageType := string(page.PageType)
		if pageType == "" {
			pageType = "wiki page"
		}
		pageAliases := strings.Join(page.Aliases, ", ")

		var updatedContent string
		updatedContent, err = s.generateWithTemplate(ctx, chatModel, agent.WikiPageModifyPrompt, map[string]string{
			"HasAdditions":            hasAdditionsStr,
			"HasRetractions":          hasRetractionsStr,
			"PageSlug":                slug,
			"PageTitle":               pageTitle,
			"PageType":                pageType,
			"PageAliases":             pageAliases,
			"ExistingContent":         existingContent,
			"NewContent":              newContentBuilder.String(),
			"DeletedContent":          deletedContent.String(),
			"RemainingSourcesContent": remainingSourcesContent.String(),
			"AvailableSlugs":          relatedSlugs.String(),
			"Language":                language,
		})
		if err == nil {
			if _, fenceErr := s.requireAllLiveUpdates(ctx, tenantID, kbID, updates); fenceErr != nil {
				return false, affectedType, false,
					fmt.Errorf("revalidate wiki page %s after synthesis: %w", slug, fenceErr)
			}
		}

		if err == nil && strings.TrimSpace(updatedContent) == "" {
			return false, affectedType, additionFailed,
				fmt.Errorf("synthesize wiki page %s: model returned empty content", slug)
		}
		if err == nil {
			updatedSummary, updatedBody := splitSummaryLine(updatedContent)
			if updatedBody != "" {
				page.Content = updatedBody
			} else {
				page.Content = updatedContent
			}
			if updatedSummary != "" {
				page.Summary = updatedSummary
			}
			if len(appliedRetractSourceIDs) > 0 {
				page.SourceRefs = retractedSourceRefs
				page.ChunkRefs = retractedChunkRefs
				for _, addition := range additions {
					page.SourceRefs = appendUnique(page.SourceRefs, addition.SourceRef)
				}
				if markerErr := wikidelete.MarkApplied(page, appliedRetractOpIDs...); markerErr != nil {
					return false, affectedType, additionFailed,
						fmt.Errorf("mark retract applied for wiki page %s: %w", slug, markerErr)
				}
				if markerErr := wikidelete.Complete(page, appliedRetractSourceIDs...); markerErr != nil {
					return false, affectedType, additionFailed,
						fmt.Errorf("complete quarantine for wiki page %s: %w", slug, markerErr)
				}
			}
			changed = true
		} else if err != nil {
			logger.Warnf(ctx, "wiki ingest: update/retract failed for slug %s: %v", slug, err)
			// Flag addition failures so the batch can sanitize stale
			// [[slug]] references in the doc's summary page and prune
			// the slug from log entries — otherwise the wiki feed shows
			// a clickable entry whose target page doesn't exist.
			// Retract-only failures don't poison anything (they leave
			// the existing page unchanged), so don't flag those.
			if len(additions) > 0 {
				additionFailed = true
			}
			// Propagate the failure so every contributing durable op remains
			// pending and enters fail_count/dead-letter. Logging and then
			// clearing this error was the direct path to false acknowledgement.
			return false, affectedType, additionFailed, fmt.Errorf("synthesize wiki page %s: %w", slug, err)
		}
	}

	// Apply the batch taxonomy plan, but only to pages that aren't already
	// filed — so brand-new pages get a coherent folder while previously-filed
	// or user-moved pages keep their placement (manual edits are authoritative).
	// The page's category_path cache is derived from folder_id downstream by
	// CreatePage/UpdatePage, so assigning the folder id is sufficient here.
	if page.FolderID == "" && batchCtx != nil {
		if fid := batchCtx.PlannedFolderID[slug]; fid != "" {
			page.FolderID = fid
		}
	}

	if changed {
		if _, fenceErr := s.requireAllLiveUpdates(ctx, tenantID, kbID, updates); fenceErr != nil {
			return false, affectedType, additionFailed,
				fmt.Errorf("revalidate wiki page %s before commit: %w", slug, fenceErr)
		}
		// Refresh chunk refs in-place on the page so they persist alongside
		// the rest of the row. Retraction-owned IDs were removed above;
		// additions append their live citations, deduplicated.
		page.ChunkRefs = mergeChunkRefs(page.ChunkRefs, additions)
		writeCtx := ctx
		if len(appliedRetractSourceIDs) > 0 {
			writeCtx = wikidelete.WithQuarantineClear(ctx, appliedRetractSourceIDs...)
		}
		writeCtx = wikiIngestPageApplicationContext(writeCtx, tenantID, kbID, slug, updates)
		if exists {
			_, err = s.wikiService.UpdatePage(writeCtx, page)
		} else {
			_, err = s.wikiService.CreatePage(writeCtx, page)
		}
		return true, affectedType, additionFailed, err
	}

	return false, "", additionFailed, nil
}

// appendWikiReparseReconciliation gives every replacement/stale retract only
// the old citations proven to belong to this document. Keeping the projection
// per page avoids copying a large document's complete chunk set into every
// durable SlugUpdate.
func appendWikiReparseReconciliation(
	updates []SlugUpdate,
	oldPageSlugs map[string]bool,
	newSlugSet map[string]bool,
	oldOwnedChunkRefs map[string][]string,
	priorContribution string,
	currentContent string,
	docTitle string,
	knowledgeID string,
	language string,
) ([]SlugUpdate, int, int) {
	reparseOverlap := 0
	staleCount := 0
	for oldSlug := range oldPageSlugs {
		if newSlugSet[oldSlug] {
			// Summary pages are overwritten wholesale. Their chunk_refs are
			// always cleared by the summary branch, so a retract is redundant.
			if strings.HasPrefix(oldSlug, "summary/") {
				continue
			}
			reparseOverlap++
			updates = append(updates, SlugUpdate{
				Slug:              oldSlug,
				Type:              "retract",
				RetractDocContent: priorContribution,
				DocTitle:          docTitle,
				KnowledgeID:       knowledgeID,
				Language:          language,
				SourceChunks: append(
					[]string(nil), oldOwnedChunkRefs[oldSlug]...,
				),
			})
			continue
		}
		staleCount++
		updates = append(updates, SlugUpdate{
			Slug:              oldSlug,
			Type:              "retractStale",
			RetractDocContent: currentContent,
			DocTitle:          docTitle,
			KnowledgeID:       knowledgeID,
			Language:          language,
			SourceChunks: append(
				[]string(nil), oldOwnedChunkRefs[oldSlug]...,
			),
		})
	}
	return updates, reparseOverlap, staleCount
}

// mergeChunkRefs unions the chunk IDs currently on the page with the ones
// cited by this batch's additions, preserving insertion order and dropping
// duplicates. Empty strings are filtered out so a malformed source_chunks
// array can't leave junk in the column.
//
// Retraction-owned IDs are removed before this helper runs; additions then
// append their live citations on top of the surviving refs.
func mergeChunkRefs(current types.StringArray, additions []SlugUpdate) types.StringArray {
	seen := make(map[string]bool, len(current))
	out := make(types.StringArray, 0, len(current))
	for _, id := range current {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, add := range additions {
		for _, chunkID := range add.SourceChunks {
			if chunkID == "" || seen[chunkID] {
				continue
			}
			seen[chunkID] = true
			out = append(out, chunkID)
		}
	}
	return out
}

func removeChunkRefsByID(current types.StringArray, removed []string) types.StringArray {
	if len(current) == 0 || len(removed) == 0 {
		return current
	}
	removedSet := make(map[string]struct{}, len(removed))
	for _, id := range removed {
		if id != "" {
			removedSet[id] = struct{}{}
		}
	}
	out := make(types.StringArray, 0, len(current))
	for _, id := range current {
		if _, drop := removedSet[id]; !drop {
			out = append(out, id)
		}
	}
	return out
}

func removeChunkRefsFromRetracts(current types.StringArray, retracts []SlugUpdate) types.StringArray {
	if len(retracts) == 0 {
		return current
	}
	var removed []string
	for _, retract := range retracts {
		removed = append(removed, retract.SourceChunks...)
	}
	return removeChunkRefsByID(current, removed)
}
