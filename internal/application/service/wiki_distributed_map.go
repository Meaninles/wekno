package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/workretry"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

const (
	wikiMapDispatchLease = 45 * time.Second
	wikiMapDispatchRenew = 15 * time.Second
)

// newWikiBatchContext builds the lazy KB lookups shared by both a distributed
// single-document Map worker and the later KB materialization worker. Its
// error collector is mandatory: lookup failures return empty fallback values
// to the model-facing code, then fail the phase before any prepared result is
// published.
func (s *wikiIngestService) newWikiBatchContext(
	knowledgeBaseID string,
	granularity types.WikiExtractionGranularity,
) (*WikiBatchContext, func() error) {
	var (
		fetchMu         sync.Mutex
		fetchErrMu      sync.Mutex
		fetchErrs       []error
		slugTitleCache  = make(map[string]string)
		summaryKIDCache = make(map[string]string)
	)
	recordFetchError := func(err error) {
		if err == nil {
			return
		}
		fetchErrMu.Lock()
		fetchErrs = append(fetchErrs, err)
		fetchErrMu.Unlock()
	}
	fetchError := func() error {
		fetchErrMu.Lock()
		defer fetchErrMu.Unlock()
		return errors.Join(fetchErrs...)
	}

	resolveSlugs := func(ctx context.Context, slugs []string) map[string]string {
		need := make([]string, 0, len(slugs))
		fetchMu.Lock()
		for _, slug := range slugs {
			if _, ok := slugTitleCache[slug]; !ok {
				need = append(need, slug)
			}
		}
		fetchMu.Unlock()

		if len(need) > 0 {
			pages, err := s.wikiService.ListBySlugs(ctx, knowledgeBaseID, need)
			if err != nil {
				logger.Warnf(ctx, "wiki ingest: ListBySlugs(%d slugs) failed: %v", len(need), err)
				recordFetchError(fmt.Errorf("list wiki pages by slugs: %w", err))
			} else {
				fetchMu.Lock()
				for _, slug := range need {
					if page, ok := pages[slug]; ok && page != nil &&
						page.Status != types.WikiPageStatusArchived &&
						page.PageType != types.WikiPageTypeIndex &&
						page.PageType != types.WikiPageTypeLog {
						slugTitleCache[slug] = page.Title
					} else {
						slugTitleCache[slug] = ""
					}
				}
				fetchMu.Unlock()
			}
		}

		out := make(map[string]string, len(slugs))
		fetchMu.Lock()
		for _, slug := range slugs {
			if title := slugTitleCache[slug]; title != "" {
				out[slug] = title
			}
		}
		fetchMu.Unlock()
		return out
	}

	resolveSummaries := func(ctx context.Context, knowledgeIDs []string) map[string]string {
		need := make([]string, 0, len(knowledgeIDs))
		fetchMu.Lock()
		for _, knowledgeID := range knowledgeIDs {
			if _, ok := summaryKIDCache[knowledgeID]; !ok {
				need = append(need, knowledgeID)
			}
		}
		fetchMu.Unlock()

		if len(need) > 0 {
			contents, err := s.wikiService.ListSummariesByKnowledgeIDs(ctx, knowledgeBaseID, need)
			if err != nil {
				logger.Warnf(ctx, "wiki ingest: ListSummariesByKnowledgeIDs(%d ids) failed: %v", len(need), err)
				recordFetchError(fmt.Errorf("list wiki summaries by knowledge IDs: %w", err))
			} else {
				fetchMu.Lock()
				for _, knowledgeID := range need {
					summaryKIDCache[knowledgeID] = contents[knowledgeID]
				}
				fetchMu.Unlock()
			}
		}

		out := make(map[string]string, len(knowledgeIDs))
		fetchMu.Lock()
		for _, knowledgeID := range knowledgeIDs {
			if content := summaryKIDCache[knowledgeID]; content != "" {
				out[knowledgeID] = content
			}
		}
		fetchMu.Unlock()
		return out
	}

	return &WikiBatchContext{
		SlugTitle: func(ctx context.Context, slug string) string {
			return resolveSlugs(ctx, []string{slug})[slug]
		},
		SlugTitleMany: resolveSlugs,
		SummaryContentByKnowledgeID: func(ctx context.Context, knowledgeID string) string {
			return resolveSummaries(ctx, []string{knowledgeID})[knowledgeID]
		},
		ExtractionGranularity: granularity,
	}, fetchError
}

func wikiMapActiveKey(payload WikiIngestPayload) string {
	identity := fmt.Sprintf("%d\x00%s\x00%s", payload.TenantID, payload.KnowledgeBaseID, payload.MapDedupKey)
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s%x", wikiMapActiveKeyPrefix, sum[:16])
}

// withWikiMapLease is a renewable, token-owned document-generation lease.
// It is intentionally independent of the KB materialization lease: every
// replica can own different documents concurrently, while duplicate delivery
// of one exact Map is serialized and cancellation-safe.
func (s *wikiIngestService) withWikiMapLease(
	ctx context.Context,
	payload WikiIngestPayload,
	work func(context.Context) error,
) error {
	key := wikiMapActiveKey(payload)
	if s.redisClient == nil {
		liteKey := "map:" + key
		if _, loaded := s.liteLocks.LoadOrStore(liteKey, struct{}{}); loaded {
			return ErrWikiIngestConcurrent
		}
		defer s.liteLocks.Delete(liteKey)
		return work(ctx)
	}

	token := uuid.NewString()
	acquired, err := s.redisClient.SetNX(ctx, key, token, wikiActiveLockTTL).Result()
	if err != nil {
		return fmt.Errorf("wiki Map: acquire document lease: %w", err)
	}
	if !acquired {
		return ErrWikiIngestConcurrent
	}

	workCtx, cancelWork := context.WithCancelCause(ctx)
	renewCtx, cancelRenewLoop := context.WithCancel(context.Background())
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(wikiActiveLockRenew)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				commandCtx, cancel := context.WithTimeout(renewCtx, wikiActiveLockCommandTimeout)
				result, renewErr := wikiActiveLockRenewScript.Run(
					commandCtx,
					s.redisClient,
					[]string{key},
					token,
					wikiActiveLockTTL.Milliseconds(),
				).Int64()
				cancel()
				if renewErr == nil && result != 0 {
					continue
				}
				if renewErr != nil {
					cancelWork(fmt.Errorf("wiki Map: renew document lease: %w", renewErr))
				} else {
					cancelWork(errors.New("wiki Map: document lease ownership lost"))
				}
				return
			}
		}
	}()

	defer func() {
		cancelRenewLoop()
		<-renewDone
		cancelWork(nil)
		releaseCtx, cancel := context.WithTimeout(context.Background(), wikiActiveLockCommandTimeout)
		defer cancel()
		if _, releaseErr := wikiActiveLockReleaseScript.Run(
			releaseCtx, s.redisClient, []string{key}, token,
		).Int64(); releaseErr != nil {
			logger.Warnf(releaseCtx, "wiki Map: release document lease failed: %v", releaseErr)
		}
	}()
	return work(workCtx)
}

func decodeDistributedWikiMapOp(row *types.TaskPendingOp) (WikiPendingOp, error) {
	if row == nil {
		return WikiPendingOp{}, errors.New("wiki Map: pending row is nil")
	}
	op := WikiPendingOp{dbID: row.ID, tenantID: row.TenantID, Op: row.Op}
	if err := json.Unmarshal(row.Payload, &op); err != nil {
		return op, fmt.Errorf("wiki Map: decode pending row %d: %w", row.ID, err)
	}
	if op.Op == "" {
		op.Op = row.Op
	}
	op.dbID = row.ID
	op.tenantID = row.TenantID
	if op.Op != WikiOpIngest || strings.TrimSpace(op.KnowledgeID) == "" ||
		strings.TrimSpace(op.ProcessingGeneration) == "" {
		return op, fmt.Errorf("wiki Map: row %d has incomplete ingest identity", row.ID)
	}
	expected, err := wikiqueue.IngestDedupKey(op.KnowledgeID, op.ProcessingGeneration)
	if err != nil {
		return op, fmt.Errorf("wiki Map: row %d has invalid generation identity: %w", row.ID, err)
	}
	if expected != row.DedupKey {
		return op, fmt.Errorf("wiki Map: row %d dedup identity mismatch", row.ID)
	}
	return op, nil
}

func (s *wikiIngestService) scheduleDistributedMapRetry(
	ctx context.Context,
	payload WikiIngestPayload,
	delay time.Duration,
) error {
	payload.WakePhase ^= 1
	return enqueueWikiMapTask(s.task, payload, delay)
}

func (s *wikiIngestService) wakeWikiCommitFromMap(
	ctx context.Context,
	payload WikiIngestPayload,
) error {
	commit := WikiIngestPayload{
		TenantID:        payload.TenantID,
		KnowledgeBaseID: payload.KnowledgeBaseID,
		// Map workers converge on phase 1. A phase-0 upload/recovery signal
		// cannot suppress this hand-off, while simultaneous completions still
		// collapse into one commit wake-up.
		WakePhase: 1,
	}
	return enqueueWikiTriggerFenced(ctx, s.pendingRepo, s.task, commit, time.Second, true)
}

func (s *wikiIngestService) deferDistributedMapDelivery(
	ctx context.Context,
	payload WikiIngestPayload,
	rowID int64,
	delay time.Duration,
) (bool, error) {
	repo, ok := s.pendingRepo.(wikiMapDispatchRepository)
	if !ok || repo == nil || payload.MapDispatchEpoch == 0 || rowID <= 0 {
		return false, nil
	}
	return true, repo.DeferWikiMapDispatch(
		ctx, rowID, payload.MapDispatchEpoch, delay,
	)
}

func (s *wikiIngestService) recordDistributedMapFailure(
	ctx context.Context,
	payload WikiIngestPayload,
	op WikiPendingOp,
	mapErr error,
) error {
	if retryAfter, ok := modeladmission.CircuitRetryAfter(mapErr); ok &&
		!workretry.ConsumesBudget(mapErr) {
		// Provider outages are external backpressure, not bad document
		// attempts. Keep fail_count unchanged and replace this disposable
		// wake-up with one scheduled after the shared circuit may probe again.
		if retryAfter < wikiFollowUpDelay {
			retryAfter = wikiFollowUpDelay
		}
		retryAfter = modeladmission.SpreadProviderRetry(
			retryAfter,
			fmt.Sprintf(
				"wiki-map\x00%d\x00%s\x00%s",
				payload.TenantID,
				payload.KnowledgeBaseID,
				payload.MapDedupKey,
			),
		)
		settleCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			wikiQueueSettlementTimeout,
		)
		defer cancel()
		handled, err := s.deferDistributedMapDelivery(
			settleCtx, payload, op.dbID, retryAfter,
		)
		if err != nil {
			return fmt.Errorf("wiki Map: defer provider wait: %w", err)
		}
		if !handled {
			if err := s.rotateWikiAttempts(settleCtx, []WikiPendingOp{op}); err != nil {
				return fmt.Errorf("wiki Map: rotate provider-deferred row: %w", err)
			}
			if err := s.scheduleDistributedMapRetry(settleCtx, payload, retryAfter); err != nil {
				return fmt.Errorf("wiki Map: schedule provider retry: %w", err)
			}
		}
		logger.Infof(ctx,
			"wiki Map: provider work deferred for knowledge %s without consuming fail_count for %s",
			op.KnowledgeID, retryAfter)
		return nil
	}
	op.lastError = mapErr.Error()
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wikiQueueSettlementTimeout)
	defer cancel()
	if err := s.requeueFailedOps(settleCtx, payload, []WikiPendingOp{op}); err != nil {
		return err
	}
	repo, ok := s.pendingRepo.(wikiDistributedMapRepository)
	if !ok || repo == nil {
		return errors.New("wiki Map: distributed repository is unavailable during retry")
	}
	row, err := repo.GetWikiIngestByDedupKey(
		settleCtx, payload.TenantID, payload.KnowledgeBaseID, payload.MapDedupKey,
	)
	if err != nil {
		return fmt.Errorf("wiki Map: recheck failed row: %w", err)
	}
	if row == nil {
		return nil
	}
	handled, err := s.deferDistributedMapDelivery(
		settleCtx, payload, row.ID, wikiFollowUpDelay,
	)
	if err != nil {
		return fmt.Errorf("wiki Map: defer failed row: %w", err)
	}
	if !handled {
		if err := s.scheduleDistributedMapRetry(settleCtx, payload, wikiFollowUpDelay); err != nil {
			return fmt.Errorf("wiki Map: schedule failed-row retry: %w", err)
		}
	}
	return nil
}

// ProcessWikiMap performs only document-local, checkpointable model work.
// Shared Wiki page writes are deliberately absent; successful output is
// atomically marked ready and a KB commit wake-up performs materialization.
func (s *wikiIngestService) ProcessWikiMap(ctx context.Context, task *asynq.Task) error {
	ctx = modeladmission.WithWorkLane(ctx, modeladmission.WorkLaneWikiMap)
	var payload WikiIngestPayload
	if task == nil {
		return errors.New("wiki Map: task is nil")
	}
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("wiki Map: unmarshal payload: %w", err)
	}
	if payload.TaskMode != wikiTaskModeMap || payload.TenantID == 0 ||
		strings.TrimSpace(payload.KnowledgeBaseID) == "" ||
		strings.TrimSpace(payload.MapDedupKey) == "" {
		return errors.New("wiki Map: complete task identity is required")
	}
	// Epoch zero identifies the old eager-publication protocol. PostgreSQL has
	// the durable row and the bounded dispatcher will issue a fresh epoch; ACK
	// this obsolete Redis copy without rotating or republishing it.
	if payload.MapDispatchEpoch == 0 {
		return nil
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, payload.TenantID)
	if queueName, known := asynq.GetQueueName(ctx); known && queueName != types.QueueWikiMap {
		return nil
	}

	mapRepo, ok := s.pendingRepo.(wikiDistributedMapRepository)
	if !ok || mapRepo == nil {
		return errors.New("wiki Map: pending repository does not support distributed Map")
	}
	dispatchRepo, ok := s.pendingRepo.(wikiMapDispatchRepository)
	if !ok || dispatchRepo == nil {
		return errors.New("wiki Map: pending repository does not support durable dispatch fencing")
	}
	row, err := dispatchRepo.ClaimWikiMapDispatch(
		ctx, payload.TenantID, payload.KnowledgeBaseID, payload.MapDedupKey,
		payload.MapDispatchEpoch, wikiMapDispatchLease,
	)
	if err != nil {
		return fmt.Errorf("wiki Map: claim dispatch epoch: %w", err)
	}
	if row == nil {
		return nil
	}
	execCtx, cancelExecution := context.WithCancelCause(ctx)
	heartbeatDone := make(chan struct{})
	go s.heartbeatWikiMapDispatch(
		execCtx, cancelExecution, dispatchRepo, row.ID,
		payload.MapDispatchEpoch, heartbeatDone,
	)

	process := func(mapCtx context.Context) error {
		op, err := decodeDistributedWikiMapOp(row)
		if err != nil {
			if context.Cause(mapCtx) != nil {
				return context.Cause(mapCtx)
			}
			return s.recordDistributedMapFailure(mapCtx, payload, op, err)
		}
		if payload.KnowledgeID != "" && payload.KnowledgeID != op.KnowledgeID {
			return s.recordDistributedMapFailure(
				mapCtx, payload, op, errors.New("wiki Map: task/pending knowledge identity mismatch"),
			)
		}
		if payload.ProcessingGeneration != "" &&
			payload.ProcessingGeneration != op.ProcessingGeneration {
			return s.recordDistributedMapFailure(
				mapCtx, payload, op, errors.New("wiki Map: task/pending generation identity mismatch"),
			)
		}
		if op.Language != "" {
			mapCtx = context.WithValue(mapCtx, types.LanguageContextKey, op.Language)
		}

		if row.MapReadyAt != nil {
			return s.wakeWikiCommitFromMap(mapCtx, payload)
		}
		if op.Prepared != nil || op.MapFinished {
			if err := s.publishWikiMapReady(mapCtx, payload, op, "recovered completed map"); err != nil {
				return err
			}
			return s.wakeWikiCommitFromMap(mapCtx, payload)
		}

		stale, err := s.isWikiIngestGenerationStale(
			mapCtx, payload.TenantID, payload.KnowledgeBaseID, op,
		)
		if err != nil {
			if context.Cause(mapCtx) != nil {
				return context.Cause(mapCtx)
			}
			return s.recordDistributedMapFailure(mapCtx, payload, op, err)
		}
		if stale {
			// Exact generation key means this cannot delete a newer reparse.
			return mapRepo.DeleteWikiIngestByDedupKey(
				mapCtx,
				payload.TenantID,
				payload.KnowledgeBaseID,
				payload.MapDedupKey,
			)
		}

		kb, err := s.kbService.GetKnowledgeBaseByIDOnly(mapCtx, payload.KnowledgeBaseID)
		if errors.Is(err, repository.ErrKnowledgeBaseNotFound) {
			return s.wakeWikiCommitFromMap(mapCtx, payload)
		}
		if err != nil {
			return fmt.Errorf("wiki Map: load knowledge base: %w", err)
		}
		if kb == nil || kb.TenantID != payload.TenantID {
			return errors.New("wiki Map: knowledge-base identity mismatch")
		}
		if !kb.IsWikiEnabled() {
			return s.wakeWikiCommitFromMap(mapCtx, payload)
		}

		chatModel, err := GetDerivativeChatModel(
			mapCtx, s.modelService, kb.DerivativeModelID,
		)
		if err != nil {
			if context.Cause(mapCtx) != nil {
				return context.Cause(mapCtx)
			}
			return s.recordDistributedMapFailure(mapCtx, payload, op, fmt.Errorf("resolve synthesis model: %w", err))
		}

		granularity := types.WikiExtractionStandard
		if kb.WikiConfig != nil {
			granularity = kb.WikiConfig.ExtractionGranularity.Normalize()
		}
		batchCtx, fetchErr := s.newWikiBatchContext(payload.KnowledgeBaseID, granularity)
		result, updates, mapErr := s.mapOneDocument(mapCtx, chatModel, payload, op, batchCtx)
		if mapErr == nil {
			mapErr = fetchErr()
		}
		if mapErr != nil {
			if context.Cause(mapCtx) != nil {
				return context.Cause(mapCtx)
			}
			return s.recordDistributedMapFailure(mapCtx, payload, op, mapErr)
		}
		if result == nil {
			if err := s.checkpointWikiMapFinishedWithoutArtifacts(
				mapCtx, payload, op, "no Wiki artifacts were produced for this document",
			); err != nil {
				return err
			}
		} else if err := s.checkpointPreparedWikiIngest(mapCtx, payload, op, result, updates); err != nil {
			return err
		}
		return s.wakeWikiCommitFromMap(mapCtx, payload)
	}

	err = s.withWikiMapLease(execCtx, payload, process)
	cancelExecution(nil)
	<-heartbeatDone
	if errors.Is(err, ErrWikiIngestConcurrent) {
		if deferErr := dispatchRepo.DeferWikiMapDispatch(
			context.WithoutCancel(ctx), row.ID, payload.MapDispatchEpoch,
			wikiLockConflictDelay,
		); deferErr != nil {
			return fmt.Errorf("wiki Map: defer document-lock conflict: %w", deferErr)
		}
		return nil
	}
	return err
}

func (s *wikiIngestService) heartbeatWikiMapDispatch(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	repo wikiMapDispatchRepository,
	rowID int64,
	epoch uint64,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(wikiMapDispatchRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, renewCancel := context.WithTimeout(
				context.WithoutCancel(ctx), 5*time.Second,
			)
			renewed, err := repo.RenewWikiMapDispatch(
				renewCtx, rowID, epoch, wikiMapDispatchLease,
			)
			renewCancel()
			if err != nil {
				cancel(fmt.Errorf("wiki Map: renew dispatch lease: %w", err))
				return
			}
			if !renewed {
				cancel(errors.New("wiki Map: dispatch epoch ownership lost"))
				return
			}
		}
	}
}
