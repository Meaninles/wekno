package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const knowledgeMoveWikiSettleTimeout = 30 * time.Second

const knowledgeMoveWikiPendingPrefix = knowledgeMoveRecoveryPrefix + "wiki_pending:"

func knowledgeMoveWikiPendingMarker(attemptID, sourceKBID, targetKBID string) string {
	return knowledgeMoveAttemptMarker(
		attemptID,
		knowledgeMoveWikiPendingPrefix+sourceKBID+":"+targetKBID,
	)
}

func hasKnowledgeMoveWikiPendingMarker(knowledge *types.Knowledge) bool {
	if knowledge == nil {
		return false
	}
	_, marker, ok := parseKnowledgeMoveAttemptMarker(knowledge.ErrorMessage)
	return ok && strings.HasPrefix(marker, knowledgeMoveWikiPendingPrefix)
}

// reconcileWikiAfterKnowledgeMove is the durable Wiki boundary after a
// document's cross-KB CAS. It is safe to call repeatedly from both the normal
// success path and the target-side idempotency path.
//
// Source cleanup is always queued, even when Wiki is currently disabled: the
// disabled worker drains retracts and removes pages left from an earlier
// enabled generation. A target ingest is queued only when Wiki is enabled and
// the authoritative target document is Completed. Reparse moves therefore
// retract the old source immediately and rely on normal post-processing to
// enqueue the new target generation once parsing completes.
func (s *knowledgeService) reconcileWikiAfterKnowledgeMove(
	ctx context.Context,
	knowledge *types.Knowledge,
	sourceKB, targetKB *types.KnowledgeBase,
) error {
	if knowledge == nil || sourceKB == nil || targetKB == nil ||
		knowledge.ID == "" || sourceKB.ID == "" || targetKB.ID == "" ||
		sourceKB.ID == targetKB.ID {
		return errors.New("knowledge move Wiki reconciliation requires complete, distinct identities")
	}
	if s.wikiDeleteCoord == nil {
		return errors.New("knowledge move Wiki reconciliation is unavailable")
	}

	// The row CAS has already committed. Finish the durable queue transition
	// during task cancellation too; any error is still returned so Asynq retries
	// and the target-side idempotency branch invokes this helper again.
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), knowledgeMoveWikiSettleTimeout)
	defer cancel()
	_, legacyMarker, marked := parseKnowledgeMoveAttemptMarker(knowledge.ErrorMessage)
	if !marked || legacyMarker != knowledgeMoveWikiPendingPrefix+sourceKB.ID+":"+targetKB.ID {
		return nil
	}
	expectedMarker := knowledge.ErrorMessage
	pending, err := s.wikiDeleteCoord.IsMovePending(
		settleCtx, knowledge.TenantID, knowledge.ID, sourceKB.ID, expectedMarker,
	)
	if err != nil {
		return fmt.Errorf("knowledge move Wiki: inspect pending generation: %w", err)
	}
	if !pending {
		return nil
	}

	var pages []*types.WikiPage
	var chunkIDs []string
	if !sourceKB.DeletedAt.Valid {
		if s.wikiRepo == nil {
			return errors.New("knowledge move Wiki reconciliation page repository is unavailable")
		}
		chunkIDRepo, ok := s.chunkRepo.(unscopedChunkIDRepository)
		if !ok || chunkIDRepo == nil {
			return errors.New("knowledge move Wiki reconciliation chunk repository is unavailable")
		}
		pages, err = s.wikiRepo.ListBySourceRef(settleCtx, sourceKB.ID, knowledge.ID)
		if err != nil {
			return fmt.Errorf("knowledge move Wiki: snapshot source pages: %w", err)
		}
		chunkIDs, err = chunkIDRepo.ListChunkIDsByKnowledgeIDUnscoped(
			settleCtx, knowledge.TenantID, knowledge.ID,
		)
		if err != nil {
			return fmt.Errorf("knowledge move Wiki: snapshot source chunks: %w", err)
		}
	}

	docTitle := knowledge.Title
	if docTitle == "" {
		docTitle = knowledge.FileName
	}
	if docTitle == "" {
		docTitle = knowledge.ID
	}
	docSummary := knowledge.Description
	pageSlugs := make([]string, 0, len(pages))
	for _, page := range pages {
		if page == nil || page.PageType == types.WikiPageTypeIndex || page.PageType == types.WikiPageTypeLog {
			continue
		}
		if page.PageType == types.WikiPageTypeSummary && page.Summary != "" {
			docSummary = page.Summary
		}
		if page.Slug != "" {
			pageSlugs = append(pageSlugs, page.Slug)
		}
	}
	lang, _ := types.LanguageFromContext(ctx)
	retractPayload := WikiPendingOp{
		Op:                        WikiOpRetract,
		KnowledgeID:               knowledge.ID,
		DocTitle:                  docTitle,
		DocSummary:                docSummary,
		Language:                  lang,
		PageSlugs:                 stableStringUnion(pageSlugs),
		SourceChunks:              stableStringUnion(chunkIDs),
		MoveTargetKnowledgeBaseID: targetKB.ID,
	}
	ingestPayload := WikiPendingOp{
		Op:                   WikiOpIngest,
		KnowledgeID:          knowledge.ID,
		ProcessingGeneration: knowledge.ProcessingGeneration,
		Language:             lang,
	}
	ingestDedupKey, err := wikiqueue.IngestDedupKey(knowledge.ID, knowledge.ProcessingGeneration)
	if err != nil {
		return fmt.Errorf("knowledge move Wiki: build target ingest generation key: %w", err)
	}
	retractBytes, err := json.Marshal(retractPayload)
	if err != nil {
		return fmt.Errorf("knowledge move Wiki: marshal source retract: %w", err)
	}
	ingestBytes, err := json.Marshal(ingestPayload)
	if err != nil {
		return fmt.Errorf("knowledge move Wiki: marshal target ingest: %w", err)
	}

	// Hide every known source page, then the source index, before publishing
	// the durable retract. The marker remains set on any failure, so a task
	// retry repeats this idempotently and no visible ghost page is accepted as
	// a completed move.
	sourceTombstonedDuringQuarantine := sourceKB.DeletedAt.Valid
	for _, page := range pages {
		if sourceTombstonedDuringQuarantine {
			break
		}
		if page == nil || page.Slug == "" || page.PageType == types.WikiPageTypeIndex || page.PageType == types.WikiPageTypeLog {
			continue
		}
		if err := s.wikiRepo.QuarantineForDelete(
			settleCtx, sourceKB.ID, page.Slug, knowledge.ID,
		); err != nil {
			if errors.Is(err, kbwritefence.ErrKnowledgeBaseUnavailable) {
				sourceTombstonedDuringQuarantine = true
				break
			}
			if !errors.Is(err, repository.ErrWikiPageNotFound) {
				return fmt.Errorf("knowledge move Wiki: quarantine source page %s: %w", page.Slug, err)
			}
		}
	}
	if !sourceTombstonedDuringQuarantine {
		indexPage, indexErr := s.wikiRepo.GetBySlug(settleCtx, sourceKB.ID, "index")
		if indexErr != nil && !errors.Is(indexErr, repository.ErrWikiPageNotFound) {
			return fmt.Errorf("knowledge move Wiki: load source index: %w", indexErr)
		}
		if indexErr == nil && indexPage != nil {
			if err := s.wikiRepo.QuarantineForDelete(
				settleCtx, sourceKB.ID, indexPage.Slug, knowledge.ID,
			); err != nil && !errors.Is(err, repository.ErrWikiPageNotFound) &&
				!errors.Is(err, kbwritefence.ErrKnowledgeBaseUnavailable) {
				return fmt.Errorf("knowledge move Wiki: quarantine source index: %w", err)
			}
		}
	}

	// A pending target is a reparse move. Prepare its immutable root plan only
	// after all destructive/source-side work above succeeded, but before the
	// transaction that clears the Wiki marker. Prepare is intentionally
	// invisible; PrepareMove atomically installs its ID on the exact Pending
	// generation together with source-Wiki settlement.
	var prepared *preparedDocumentWorkflow
	workflowID := ""
	if knowledge.KnowledgeBaseID == targetKB.ID && knowledge.ParseStatus == types.ParseStatusPending {
		workflowID = strings.TrimSpace(knowledge.ProcessingWorkflowID)
		if !targetKB.DeletedAt.Valid {
			if workflowID == "" {
				prepared, err = s.prepareMovedKnowledgeReparse(settleCtx, targetKB, knowledge)
				if err != nil {
					return fmt.Errorf("knowledge move Wiki: prepare target reparse workflow: %w", err)
				}
				workflowID = prepared.binding.WorkflowID
			} else {
				binding, bindingErr := documentWorkflowBindingForKnowledge(knowledge)
				if bindingErr != nil {
					return fmt.Errorf("knowledge move Wiki: load target reparse workflow binding: %w", bindingErr)
				}
				prepared = &preparedDocumentWorkflow{binding: binding}
			}
		}
	}
	var bindTargetWorkflowTx func(*gorm.DB, func(*gorm.DB) error) error
	if prepared != nil {
		bindTargetWorkflowTx = func(tx *gorm.DB, transition func(*gorm.DB) error) error {
			return s.bindPreparedDocumentWorkflowTransitionTx(tx, prepared, transition)
		}
	}

	result, err := s.wikiDeleteCoord.PrepareMove(settleCtx, wikidelete.MoveRequest{
		TenantID:                   knowledge.TenantID,
		KnowledgeID:                knowledge.ID,
		SourceKnowledgeBaseID:      sourceKB.ID,
		TargetKnowledgeBaseID:      targetKB.ID,
		TargetWikiEnabled:          targetKB.IsWikiEnabled(),
		ExpectedMarker:             expectedMarker,
		TargetProcessingWorkflowID: workflowID,
		ExpectedProcessingGeneration: func() string {
			if workflowID == "" {
				return ""
			}
			return knowledge.ProcessingGeneration
		}(),
		ExpectedProcessingOwner: func() string {
			if workflowID == "" {
				return ""
			}
			return knowledge.ProcessingOwner
		}(),
		BindTargetWorkflowTx: bindTargetWorkflowTx,
		SourceRetractPendingOp: &types.TaskPendingOp{
			TenantID: knowledge.TenantID, TaskType: wikiTaskType, Scope: wikiTaskScope,
			ScopeID: sourceKB.ID, Op: WikiOpRetract, DedupKey: knowledge.ID, Payload: retractBytes,
		},
		TargetIngestPendingOp: &types.TaskPendingOp{
			TenantID: knowledge.TenantID, TaskType: wikiTaskType, Scope: wikiTaskScope,
			ScopeID: targetKB.ID, Op: WikiOpIngest, DedupKey: ingestDedupKey, Payload: ingestBytes,
		},
	})
	if err != nil {
		return fmt.Errorf("knowledge move Wiki: persist queue transition: %w", err)
	}
	// Preserve the coordinator's locked tombstone observation for the immediate
	// post-settlement handoff decision. The caller's KB snapshots may have been
	// loaded before a concurrent delete committed.
	if result.SourceKnowledgeBaseDeleted {
		sourceKB.DeletedAt.Valid = true
	}
	if result.TargetKnowledgeBaseDeleted {
		targetKB.DeletedAt.Valid = true
	}
	if result.TargetWorkflowBound {
		knowledge.ProcessingWorkflowID = workflowID
		knowledge.ErrorMessage = ""
	} else if !result.AlreadySettled {
		knowledge.ErrorMessage = ""
	}

	var triggerErrs []error
	if result.SourceRetractPersisted {
		if err := enqueueWikiTrigger(s.task, WikiIngestPayload{
			TenantID: knowledge.TenantID, KnowledgeBaseID: sourceKB.ID,
		}, wikiFollowUpDelay, true); err != nil {
			triggerErrs = append(triggerErrs, fmt.Errorf("wake source retract: %w", err))
		}
	}
	if result.TargetIngestPersisted {
		if err := enqueueWikiTrigger(s.task, WikiIngestPayload{
			TenantID: knowledge.TenantID, KnowledgeBaseID: targetKB.ID,
		}, wikiIngestDelay, true); err != nil {
			triggerErrs = append(triggerErrs, fmt.Errorf("wake target ingest: %w", err))
		}
	}
	if joined := errors.Join(triggerErrs...); joined != nil {
		logger.Warnf(settleCtx,
			"knowledge move Wiki durable rows persisted for %s but trigger degraded: %v",
			knowledge.ID, joined)
	}
	// Wake the source retract before publishing the target parser. Both are
	// already durable, but this ordering minimizes the interval in which old
	// source Wiki pages remain visible while the new target parse advances.
	if result.TargetWorkflowBound {
		if prepared != nil {
			s.dispatchPreparedDocumentWorkflow(settleCtx, prepared)
		} else if err := s.resumeBoundDocumentWorkflow(settleCtx, knowledge); err != nil {
			return fmt.Errorf("knowledge move Wiki: resume bound target reparse: %w", err)
		}
	}
	return nil
}
