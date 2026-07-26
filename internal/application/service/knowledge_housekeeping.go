// Package service: knowledge housekeeping.
//
// HousekeepingService periodically scans for knowledge rows that have been
// stuck in a non-terminal parse state longer than any reasonable execution
// window. Orphaned core parsing is marked failed; orphaned post-parse
// enrichment is degraded to completed because its chunks and embeddings are
// already usable. This is the safety net that catches anything the other
// defences (asynq retry, dead-letter callback, image_multimodal
// finalize-on-last-attempt) miss — for example:
//
//   - Worker process killed mid-handler before any defer could run.
//   - DocReader call genuinely exceeding DocReaderCallTimeout AND the
//     worker subsequently being lost before retry kicks in.
//   - Multimodal Redis counter set to N but ALL N image tasks failing in
//     ways that bypass finalize (extremely rare; defence-in-depth here).
//
// Without this sweep, a single unlucky failure mode can leave a knowledge
// row spinning forever. With this sweep the worst-case latency from stall to
// a truthful terminal status is bounded to ~1 stale-threshold + 1 interval.
package service

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/custom/modules/contentcache"
	"github.com/Tencent/WeKnora/internal/custom/modules/corefanout"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// HousekeepingService runs background sweeps to recover stuck rows.
type HousekeepingService struct {
	db   *gorm.DB
	cfg  *config.Config
	cron *cron.Cron

	// inspector lets the sweep distinguish a genuinely orphaned row from
	// one whose enrichment subtasks are merely backlogged behind a busy
	// queue (no span heartbeat yet because no worker has picked them up).
	// A missing inspector is treated as unknown queue state and preserves
	// candidates; housekeeping never guesses that unavailable means empty.
	inspector interfaces.TaskInspector
	cache     *contentcache.Store

	mu      sync.Mutex
	started bool
}

// NewHousekeepingService constructs a HousekeepingService. It does NOT start
// the cron — call Start in the application bootstrap so a misconfigured
// cron schedule cannot prevent the rest of the service from coming up.
func NewHousekeepingService(
	db *gorm.DB,
	cfg *config.Config,
	inspector interfaces.TaskInspector,
	cache *contentcache.Store,
) *HousekeepingService {
	return &HousekeepingService{
		db:        db,
		cfg:       cfg,
		inspector: inspector,
		cache:     cache,
		cron: cron.New(cron.WithSeconds(), cron.WithChain(
			cron.Recover(cron.DefaultLogger),
		)),
	}
}

// Start registers the sweep schedule and begins the background runner.
// Idempotent — repeated calls are a no-op so wiring code can call Start
// without coordinating ordering.
func (h *HousekeepingService) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return nil
	}
	if !housekeepingEnabled() {
		logger.Infof(ctx, "[Housekeeping] disabled via WEKNORA_HOUSEKEEPING_ENABLED=false")
		return nil
	}
	// Every 5 minutes — frequent enough that user-visible recovery latency
	// is acceptable, infrequent enough that the SQL sweep is invisible to
	// query load even on large knowledge tables.
	if _, err := h.cron.AddFunc("0 */5 * * * *", func() {
		// Use Background so a cancelled bootstrap ctx doesn't stop sweeps.
		h.runSweep(context.Background())
	}); err != nil {
		return err
	}
	h.cron.Start()
	h.started = true
	logger.Infof(ctx, "[Housekeeping] started with 5-minute sweep")
	return nil
}

// Stop halts the cron and waits for in-flight sweeps to finish.
func (h *HousekeepingService) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started {
		return
	}
	c := h.cron.Stop()
	<-c.Done()
	h.started = false
}

// runSweep is exported on the type for testability — tests can drive a
// single sweep without waiting for the cron tick.
func (h *HousekeepingService) runSweep(ctx context.Context) {
	threshold := h.staleThreshold()
	cutoff := time.Now().Add(-threshold)

	// Sweep A: knowledge stuck in "processing".
	//
	// Two-stage check is critical here: knowledge.updated_at advances
	// only at parse_status transitions, but a long stage (DocReader on
	// a 500MB PDF, embedding 5K chunks) can run for an hour with no
	// status change. Using updated_at alone would falsely kill that
	// run. So we OR-combine knowledge.updated_at with the most recent
	// span row's updated_at — every Begin/End/Fail/Skip from
	// SpanTracker bumps the span row, so an actively-progressing
	// pipeline always has a recent span heartbeat even when the
	// parent knowledge row is "frozen" mid-stage.
	//
	// Knowledge rows with no spans at all (lite mode, in-flight tasks
	// from before this code shipped) fall back to the simple
	// updated_at check — they have no heartbeat to consult.
	// Include 'finalizing' alongside 'processing': finalizing rows still
	// consume LLM compute via enrichment subtasks (summary/question/graph),
	// and the same stall modes (subtask worker dies, retry budget exhausted
	// without decrementing the counter) leave the row hanging just as
	// visibly. Recovery semantics differ below: an abandoned core parse is
	// failed, while abandoned optional enrichment degrades to completed.
	var candidates []types.Knowledge
	if err := h.db.WithContext(ctx).
		Where("parse_status IN ? AND updated_at < ?",
			[]string{types.ParseStatusProcessing, types.ParseStatusFinalizing}, cutoff).
		Where("COALESCE(error_message, '') NOT LIKE ?", knowledgeMoveRecoveryPrefix+"%").
		Where("COALESCE(error_message, '') NOT LIKE ?", knowledgeMoveAttemptEnvelope+"%"+knowledgeMoveAttemptDelimiter+knowledgeMoveRecoveryPrefix+"%").
		Find(&candidates).Error; err != nil {
		logger.Warnf(ctx, "[Housekeeping] knowledge candidate query failed: %v", err)
		return
	}

	// A core-committed processing_fanout is a durable recovery intent, even
	// when its JSON is malformed or its identity is inconsistent. Only the
	// corefanout recovery/operator may resolve it; housekeeping must never
	// erase the evidence and silently degrade the document to completed.
	candidates, fanoutSkipped := filterCommittedCoreFanout(candidates)

	stuck, spanSkipped := h.filterByLastSpanActivity(ctx, candidates, cutoff)

	// Second-stage gate: a row can have a stale span heartbeat yet still
	// be perfectly healthy when its document-lifecycle subtasks (summary /
	// question / graph) are merely backlogged behind a busy queue
	// — no worker has picked them up, so no span has been written since
	// post-process fanned them out. Killing such a row is the false-
	// positive users hit under heavy upload bursts. Drop any candidate
	// that still has a queued/active task referencing it; only rows with
	// nothing left in the queue are treated as genuinely orphaned.
	stuck, queueSkipped := h.filterOutQueued(ctx, stuck)

	if len(stuck) > 0 {
		h.recoverStuckKnowledge(ctx, stuck, cutoff, threshold)
	}
	if spanSkipped > 0 {
		// Visibility into "we considered killing N rows but their
		// span tree showed they're still progressing". Ops can grep
		// for this if they suspect housekeeping over- or under-fires.
		logger.Infof(ctx,
			"[Housekeeping] %d candidate(s) skipped — span heartbeat within threshold",
			spanSkipped)
	}
	if queueSkipped > 0 {
		// Visibility into "stale span heartbeat but liveness could not be
		// disproved". Usually this is queue backpressure; it can also mean
		// a transient backend probe failure, in which case preserving the
		// row is the intentional fail-closed behaviour.
		logger.Infof(ctx,
			"[Housekeeping] %d candidate(s) skipped — live work queued/running or queue state unavailable",
			queueSkipped)
	}

	if h.cache != nil {
		deleted, err := h.cache.Sweep(ctx, time.Now().Add(-30*24*time.Hour), 500)
		if err != nil {
			logger.Warnf(ctx, "[Housekeeping] content cache sweep failed: %v", err)
		} else if deleted > 0 {
			logger.Infof(ctx, "[Housekeeping] removed %d expired unreferenced content cache entries", deleted)
		}
	}
	if fanoutSkipped > 0 {
		logger.Infof(ctx,
			"[Housekeeping] %d candidate(s) preserved — committed core fanout awaits durable replay",
			fanoutSkipped)
	}

	// Sweep B: summary generation has no span heartbeat of its own, so queue
	// liveness is the authoritative signal. Use the same two-probe protocol as
	// the document sweep: one scan filters ordinary backlog, a second full scan
	// immediately before the guarded UPDATE closes active→retry and other
	// non-snapshot state transitions inside Redis inspection.
	h.runSummarySweep(ctx, time.Now().Add(-1*time.Hour))
}

func filterCommittedCoreFanout(candidates []types.Knowledge) (kept []types.Knowledge, skipped int) {
	kept = make([]types.Knowledge, 0, len(candidates))
	for i := range candidates {
		if corefanout.HasCommittedPlan(&candidates[i]) {
			skipped++
			continue
		}
		kept = append(kept, candidates[i])
	}
	return kept, skipped
}

func (h *HousekeepingService) runSummarySweep(ctx context.Context, cutoff time.Time) {
	var candidates []types.Knowledge
	if err := h.db.WithContext(ctx).
		Where("summary_status = ? AND updated_at < ?", types.SummaryStatusProcessing, cutoff).
		Find(&candidates).Error; err != nil {
		logger.Warnf(ctx, "[Housekeeping] summary candidate query failed: %v", err)
		return
	}

	candidates, firstSkipped := h.filterOutSummaryQueued(ctx, candidates)
	if len(candidates) == 0 {
		if firstSkipped > 0 {
			logger.Infof(ctx,
				"[Housekeeping] %d summary candidate(s) skipped — task queued/running or queue state unavailable",
				firstSkipped)
		}
		return
	}

	// Deliberately repeat the whole batch scan. Asynq exposes each state as a
	// separate paginated view, not one atomic snapshot; a task can move from
	// active to retry after that state's page was read. The second observation
	// makes such a transition visible before any terminal repair is attempted.
	candidates, secondSkipped := h.filterOutSummaryQueued(ctx, candidates)
	if firstSkipped+secondSkipped > 0 {
		logger.Infof(ctx,
			"[Housekeeping] %d summary candidate(s) skipped — task queued/running or queue state unavailable",
			firstSkipped+secondSkipped)
	}
	if len(candidates) == 0 {
		return
	}

	ids := make([]string, 0, len(candidates))
	for _, k := range candidates {
		ids = append(ids, k.ID)
	}
	res := h.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("id IN ?", ids).
		Where("summary_status = ?", types.SummaryStatusProcessing).
		Where("updated_at < ?", cutoff).
		Update("summary_status", types.SummaryStatusFailed)
	if res.Error != nil {
		logger.Warnf(ctx, "[Housekeeping] summary sweep failed: %v", res.Error)
	} else if res.RowsAffected > 0 {
		logger.Infof(ctx, "[Housekeeping] recovered %d stuck summary rows", res.RowsAffected)
	}
}

// filterByLastSpanActivity returns the subset of candidates whose most
// recent document-lifecycle span row predates `cutoff` — i.e. genuinely
// stuck. Wiki owns an independent durable queue and its postprocess.wiki*
// spans must never extend the core document lifecycle.
// with no span rows at all also pass through (they're lite-mode or
// pre-instrumentation tasks; the simple updated_at check already proved
// them stuck and we have no heartbeat to override that).
func (h *HousekeepingService) filterByLastSpanActivity(
	ctx context.Context,
	candidates []types.Knowledge,
	cutoff time.Time,
) (kept []types.Knowledge, skipped int) {
	if len(candidates) == 0 {
		return candidates, 0
	}
	ids := make([]string, 0, len(candidates))
	for _, k := range candidates {
		ids = append(ids, k.ID)
	}

	// We scan MAX(updated_at) as string then parse client-side. That
	// dodges the SQLite driver's well-known refusal to auto-convert
	// aggregate datetime values into time.Time on its own — Postgres
	// happily round-trips, but the same query shape must work in
	// Lite mode too. Since we only compare against a cutoff, the
	// parse layer below tries the formats both Postgres and SQLite
	// emit and takes the first that parses.
	type spanHeartbeat struct {
		KnowledgeID string `gorm:"column:knowledge_id"`
		LastSeen    string `gorm:"column:last_seen"`
	}
	var beats []spanHeartbeat
	err := h.db.WithContext(ctx).
		Table("knowledge_processing_spans").
		Select("knowledge_id, MAX(updated_at) AS last_seen").
		Where("knowledge_id IN ?", ids).
		Where("name NOT LIKE ?", "postprocess.wiki%").
		Group("knowledge_id").
		Find(&beats).Error
	if err != nil {
		// Recovery is destructive state repair. If activity cannot be
		// proven absent, preserve every candidate and retry next sweep;
		// treating an observability outage as evidence of abandonment is
		// exactly how healthy documents get misclassified under load.
		logger.Warnf(ctx,
			"[Housekeeping] span heartbeat query failed: %v (preserving %d candidate(s))",
			err, len(candidates))
		return nil, len(candidates)
	}
	heartbeat := make(map[string]time.Time, len(beats))
	unparseable := make(map[string]struct{})
	for _, b := range beats {
		if t, ok := parseHeartbeatTime(b.LastSeen); ok {
			heartbeat[b.KnowledgeID] = t
		} else {
			unparseable[b.KnowledgeID] = struct{}{}
			logger.Warnf(ctx,
				"[Housekeeping] unparseable span heartbeat for %s: %q (preserving row)",
				b.KnowledgeID, b.LastSeen)
		}
	}

	out := make([]types.Knowledge, 0, len(candidates))
	for _, k := range candidates {
		if _, unknown := unparseable[k.ID]; unknown {
			skipped++
			continue
		}
		if last, ok := heartbeat[k.ID]; ok && !last.Before(cutoff) {
			// Active span heartbeat — leave alone.
			skipped++
			continue
		}
		out = append(out, k)
	}
	return out, skipped
}

// filterOutQueued returns the subset of candidates that have NO task left
// in the queue backend, plus a count of how many were dropped because a
// task still references them. A dropped candidate is "backlogged, not
// orphaned" — its enrichment subtasks are waiting for a worker, so the
// missing span heartbeat is expected and recovering it would be a false
// positive. Missing inspector wiring or any backend error preserves the
// candidate: recovery may wait for the next sweep, but a transient Redis or
// PostgreSQL outage must never turn "unknown" into "orphaned".
func (h *HousekeepingService) filterOutQueued(
	ctx context.Context, candidates []types.Knowledge,
) (kept []types.Knowledge, skipped int) {
	if h.inspector == nil {
		return h.preserveWithoutInspector(ctx, candidates)
	}
	return h.filterOutTasks(
		ctx,
		candidates,
		"document lifecycle",
		h.inspector.DocumentLifecycleTaskKnowledgeIDs,
	)
}

func (h *HousekeepingService) filterOutSummaryQueued(
	ctx context.Context, candidates []types.Knowledge,
) (kept []types.Knowledge, skipped int) {
	if h.inspector == nil {
		return h.preserveWithoutInspector(ctx, candidates)
	}
	return h.filterOutTasks(
		ctx,
		candidates,
		"summary",
		h.inspector.SummaryTaskKnowledgeIDs,
	)
}

type housekeepingTaskProbe func(
	context.Context,
	[]interfaces.KnowledgeTaskTarget,
) (map[string]bool, error)

func (h *HousekeepingService) filterOutTasks(
	ctx context.Context,
	candidates []types.Knowledge,
	probeName string,
	probe housekeepingTaskProbe,
) (kept []types.Knowledge, skipped int) {
	if len(candidates) == 0 {
		return candidates, 0
	}
	targets := make([]interfaces.KnowledgeTaskTarget, 0, len(candidates))
	for _, k := range candidates {
		targets = append(targets, interfaces.KnowledgeTaskTarget{
			KnowledgeBaseID: k.KnowledgeBaseID,
			KnowledgeID:     k.ID,
		})
	}
	queued, err := probe(ctx, targets)
	if err != nil {
		logger.Warnf(ctx,
			"[Housekeeping] %s batch queue probe failed: %v (preserving %d candidate(s))",
			probeName, err, len(candidates))
		return nil, len(candidates)
	}

	out := make([]types.Knowledge, 0, len(candidates))
	for _, k := range candidates {
		if queued[k.ID] {
			skipped++
			continue
		}
		out = append(out, k)
	}
	return out, skipped
}

func (h *HousekeepingService) preserveWithoutInspector(
	ctx context.Context,
	candidates []types.Knowledge,
) ([]types.Knowledge, int) {
	if len(candidates) == 0 {
		return candidates, 0
	}
	logger.Warnf(ctx,
		"[Housekeeping] task inspector unavailable (preserving %d candidate(s))",
		len(candidates))
	return nil, len(candidates)
}

// recoverStuckKnowledge applies terminal state repair after all read-side
// probes found no live work. The two source states deliberately diverge:
//
//   - processing means the core DocReader/chunk/embed pipeline never reached
//     a usable result, so an orphan is a genuine parse failure;
//   - finalizing means core chunks and embeddings are already committed and
//     only optional enrichment remains, so an orphan whose Wiki lane is
//     already terminal is degraded to completed rather than poisoning an
//     otherwise searchable document.
//
// Every UPDATE repeats the cheap updated_at check and adds a correlated
// NOT-EXISTS guard for a new span heartbeat. That guard closes the race
// between the earlier probes and this write: if a worker makes progress in
// that window, zero rows are changed and the next sweep reevaluates from fresh
// state. A pending Wiki status is a final-completion guard even though Wiki
// owns no pending_subtasks_count slot; the durable Wiki/root-workflow recovery
// paths retain responsibility for settling that generation.
func (h *HousekeepingService) recoverStuckKnowledge(
	ctx context.Context,
	candidates []types.Knowledge,
	cutoff time.Time,
	threshold time.Duration,
) {
	// Re-probe immediately before writes. Redis queue states are independently
	// paginated and a task can transition (notably active→retry) while a scan is
	// in progress. runSweep already performed one batch scan; this second full
	// scan substantially narrows that non-snapshot/TOCTOU window. An error is
	// unknown liveness and therefore vetoes every affected repair.
	var secondProbeSkipped int
	candidates, secondProbeSkipped = h.filterOutQueued(ctx, candidates)
	if secondProbeSkipped > 0 {
		logger.Infof(ctx,
			"[Housekeeping] %d candidate(s) preserved by final document-task liveness probe",
			secondProbeSkipped)
	}
	if len(candidates) == 0 {
		return
	}

	processingCandidates := make([]types.Knowledge, 0, len(candidates))
	finalizingIDs := make([]string, 0, len(candidates))
	for _, k := range candidates {
		switch k.ParseStatus {
		case types.ParseStatusProcessing:
			processingCandidates = append(processingCandidates, k)
		case types.ParseStatusFinalizing:
			finalizingIDs = append(finalizingIDs, k.ID)
		}
	}

	// processing spans two materially different phases. processChunks writes
	// chunks/indexes, sets processed_at, and deliberately leaves the row in
	// processing until the post-process hand-off succeeds. If that enqueue (or
	// the process immediately after it) is lost, the document is already
	// searchable and must degrade to completed, not be relabelled as a parse
	// failure. Require a live enabled chunk in addition to processed_at so a
	// partially-written/corrupt row is never promoted on the timestamp alone.
	usableProcessing, artifactErr := h.usableProcessingKnowledgeIDs(ctx, processingCandidates)
	processingFailedIDs := make([]string, 0, len(processingCandidates))
	processingCompletedIDs := make([]string, 0, len(processingCandidates))
	if artifactErr != nil {
		// Artifact visibility is part of this destructive classification. An
		// unavailable chunk table/connection makes every processing candidate
		// unknown, so preserve them for the next sweep.
		logger.Warnf(ctx,
			"[Housekeeping] processing artifact query failed: %v (preserving %d candidate(s))",
			artifactErr, len(processingCandidates))
	} else {
		for _, k := range processingCandidates {
			if usableProcessing[k.ID] {
				processingCompletedIDs = append(processingCompletedIDs, k.ID)
			} else {
				processingFailedIDs = append(processingFailedIDs, k.ID)
			}
		}
	}

	if len(processingFailedIDs) > 0 {
		res := h.guardedRecoveryQuery(ctx, processingFailedIDs, types.ParseStatusProcessing, cutoff).
			Updates(map[string]interface{}{
				"parse_status":           types.ParseStatusFailed,
				"error_message":          "core parsing stuck > " + threshold.String() + ", recovered by housekeeping",
				"pending_subtasks_count": 0,
				"enrichment_status":      types.EnrichmentStatusNone,
				"wiki_status":            types.WikiStatusNone,
				"wiki_error_message":     "",
				"processing_owner":       "",
				"processing_fanout":      nil,
			})
		if res.Error != nil {
			logger.Warnf(ctx, "[Housekeeping] processing recovery update failed: %v", res.Error)
		} else if res.RowsAffected > 0 {
			logger.Infof(ctx,
				"[Housekeeping] marked %d orphaned core parse row(s) failed (threshold=%s)",
				res.RowsAffected, threshold)
		}
	}

	if len(processingCompletedIDs) > 0 {
		completedAt := time.Now()
		res := h.guardedRecoveryQuery(ctx, processingCompletedIDs, types.ParseStatusProcessing, cutoff).
			Where("COALESCE(wiki_status, '') <> ?", types.WikiStatusPending).
			Updates(map[string]interface{}{
				"parse_status":           types.ParseStatusCompleted,
				"pending_subtasks_count": 0,
				"enrichment_status":      types.EnrichmentStatusDegraded,
				"error_message":          "",
				"processing_owner":       "",
				"processing_fanout":      nil,
				"processed_at":           gorm.Expr("COALESCE(processed_at, ?)", completedAt),
				"summary_status": gorm.Expr(
					"CASE WHEN summary_status IN (?, ?) THEN ? ELSE summary_status END",
					types.SummaryStatusPending,
					types.SummaryStatusProcessing,
					types.SummaryStatusFailed,
				),
			})
		if res.Error != nil {
			logger.Warnf(ctx, "[Housekeeping] post-index processing recovery update failed: %v", res.Error)
		} else if res.RowsAffected > 0 {
			logger.Infof(ctx,
				"[Housekeeping] completed %d orphaned post-index processing row(s) with enrichment degraded (threshold=%s)",
				res.RowsAffected, threshold)
		}
	}

	if len(finalizingIDs) > 0 {
		completedAt := time.Now()
		res := h.guardedRecoveryQuery(ctx, finalizingIDs, types.ParseStatusFinalizing, cutoff).
			Where("COALESCE(wiki_status, '') <> ?", types.WikiStatusPending).
			Updates(map[string]interface{}{
				"parse_status":           types.ParseStatusCompleted,
				"pending_subtasks_count": 0,
				"enrichment_status":      types.EnrichmentStatusDegraded,
				"error_message":          "",
				"processing_owner":       "",
				"processing_fanout":      nil,
				"processed_at":           gorm.Expr("COALESCE(processed_at, ?)", completedAt),
				"summary_status": gorm.Expr(
					"CASE WHEN summary_status IN (?, ?) THEN ? ELSE summary_status END",
					types.SummaryStatusPending,
					types.SummaryStatusProcessing,
					types.SummaryStatusFailed,
				),
			})
		if res.Error != nil {
			logger.Warnf(ctx, "[Housekeeping] finalizing recovery update failed: %v", res.Error)
		} else if res.RowsAffected > 0 {
			logger.Infof(ctx,
				"[Housekeeping] completed %d orphaned finalizing row(s) with enrichment degraded (threshold=%s)",
				res.RowsAffected, threshold)
		}
	}
}

// usableProcessingKnowledgeIDs proves that a stale processing row crossed
// the core usable-artifact boundary. processed_at is written only after chunk
// persistence/indexing; the live enabled chunk check protects against legacy
// or corrupt rows that carry a timestamp without a searchable artifact.
func (h *HousekeepingService) usableProcessingKnowledgeIDs(
	ctx context.Context,
	candidates []types.Knowledge,
) (map[string]bool, error) {
	usable := make(map[string]bool)
	ids := make([]string, 0, len(candidates))
	for _, k := range candidates {
		if k.ProcessedAt != nil {
			ids = append(ids, k.ID)
		}
	}
	if len(ids) == 0 {
		return usable, nil
	}

	type chunkOwner struct {
		KnowledgeID string `gorm:"column:knowledge_id"`
	}
	var owners []chunkOwner
	if err := h.db.WithContext(ctx).
		Table("chunks").
		Distinct("knowledge_id").
		Where("knowledge_id IN ? AND deleted_at IS NULL AND is_enabled = ?", ids, true).
		Find(&owners).Error; err != nil {
		return nil, err
	}
	for _, owner := range owners {
		usable[owner.KnowledgeID] = true
	}
	return usable, nil
}

func (h *HousekeepingService) guardedRecoveryQuery(
	ctx context.Context,
	ids []string,
	status string,
	cutoff time.Time,
) *gorm.DB {
	return h.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where("knowledges.id IN ? AND knowledges.parse_status = ?", ids, status).
		Where("knowledges.updated_at < ?", cutoff).
		Where(`NOT (
			knowledges.parse_status = ?
			AND COALESCE(knowledges.processing_owner, '') = ''
			AND knowledges.processed_at IS NOT NULL
			AND knowledges.processing_fanout IS NOT NULL
			AND COALESCE(CAST(knowledges.processing_fanout AS TEXT), '') <> ''
		)`, types.ParseStatusProcessing).
		Where("COALESCE(knowledges.error_message, '') NOT LIKE ?", knowledgeMoveRecoveryPrefix+"%").
		Where("COALESCE(knowledges.error_message, '') NOT LIKE ?", knowledgeMoveAttemptEnvelope+"%"+knowledgeMoveAttemptDelimiter+knowledgeMoveRecoveryPrefix+"%").
		Where(`NOT EXISTS (
			SELECT 1
			FROM knowledge_processing_spans AS active_span
			WHERE active_span.knowledge_id = knowledges.id
			  AND active_span.name NOT LIKE 'postprocess.wiki%'
			  AND active_span.updated_at >= ?
		)`, cutoff)
}

// parseHeartbeatTime accepts the timestamp formats Postgres and SQLite
// emit for a TIMESTAMP column read back through MAX(). Returns false if
// none parse; callers preserve rows with an unparseable observed heartbeat
// because unknown activity must not be treated as proof of abandonment.
func parseHeartbeatTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// staleThreshold returns how long a "processing" row may sit untouched
// before housekeeping treats it as orphaned. The floor is 1 hour so that a
// genuinely slow large-PDF parse cannot be killed mid-flight; the ceiling
// scales with the operator-configured DocumentProcessTimeout plus 10 minute
// buffer to absorb scheduling jitter.
func (h *HousekeepingService) staleThreshold() time.Duration {
	base := 1 * time.Hour
	if h.cfg != nil && h.cfg.KnowledgeBase != nil && h.cfg.KnowledgeBase.DocumentProcessTimeout > base {
		base = h.cfg.KnowledgeBase.DocumentProcessTimeout
	}
	return base + 10*time.Minute
}

func housekeepingEnabled() bool {
	// Default-on: missing/empty env enables the sweep. Operators must
	// explicitly set "false" to opt out, matching the plan's commitment
	// that no env change is required for the safety net to engage.
	v := strings.TrimSpace(os.Getenv("WEKNORA_HOUSEKEEPING_ENABLED"))
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}
