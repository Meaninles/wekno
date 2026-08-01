package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/custom/modules/contentcache"
	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/workretry"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// ErrWikiIngestConcurrent is returned by the wiki ingest handler when another
// batch is already running for the same KB (i.e. the `wiki:active:<kbID>`
// Redis lock is held). The asynq server's RetryDelayFunc uses errors.Is on
// this sentinel to apply a short, fixed retry delay instead of asynq's default
// exponential backoff — otherwise a freshly orphaned lock (e.g. from a crash
// or restart) would force newcomers to wait minutes even after the lock
// naturally expires.
var ErrWikiIngestConcurrent = errors.New("concurrent wiki task active")

const (
	// maxContentForWiki limits the document content sent to LLM for wiki generation
	maxContentForWiki = 32768

	// wikiActiveKeyPrefix is the Redis key for the "batch in progress" flag.
	// Key format: wiki:active:{kbID} → "1" with TTL. Prevents concurrent batches.
	wikiActiveKeyPrefix = "wiki:active:"

	// wikiMapActiveKeyPrefix owns only one document-generation Map. Unlike the
	// KB materialization lock, different keys may execute concurrently on
	// different replicas.
	wikiMapActiveKeyPrefix = "wiki:map:active:"

	wikiTaskModeMap = "map"

	// A Map wake-up starts promptly once the durable ingest row exists. The
	// document's core parse/post-process generation is already committed at
	// this boundary, so the old 30-second KB debounce is unnecessary here.
	wikiMapInitialDelay = time.Second

	// Map tasks are disposable wake-ups over PostgreSQL state. A long Unique
	// lease coalesces recovery scans; the renewable per-document lock still
	// protects correctness if this lease expires during an unusually long Map.
	wikiMapUniqueTTL = 30 * time.Minute

	// wikiIngestDelay is how long to wait after a document is added before
	// the batch task fires. Debounces rapid uploads.
	wikiIngestDelay = 30 * time.Second

	// wikiMaxDocsPerBatch limits how many documents a single batch processes.
	// Prevents unbounded execution time. Remaining ops stay in
	// task_pending_ops and are picked up by the follow-up task.
	wikiMaxDocsPerBatch = 5

	// wikiIngestMaxRetry controls the Asynq retry budget for genuine
	// wiki:ingest failures. Per-KB lock conflicts do not return a handler error:
	// they publish a fresh delayed wake-up and complete the current signal, so
	// Asynq's "retry exhausted" branch can never archive a healthy contention
	// event before consulting its IsFailure hook.
	wikiIngestMaxRetry = 10

	// wikiIngestTaskTimeout is the hard execution ceiling for a whole batch.
	// Individual model calls are bounded much more tightly by
	// CUSTOM_WORK_RETRY_WIKI_CALL_TIMEOUT_SECONDS, so this is capacity for a
	// legitimately large batch, not permission for one upstream request to
	// hang for hours.
	wikiIngestTaskTimeout = 2 * time.Hour

	// wikiTriggerUniqueTTL coalesces the burst of identical per-KB trigger
	// tasks produced by bulk uploads. The durable document operations live in
	// task_pending_ops; the asynq task is only a wake-up signal, so suppressing
	// duplicate signals within this window cannot lose work.
	wikiTriggerUniqueTTL = 2 * time.Minute

	// wikiFollowUpDelay gives the current handler time to release its per-KB
	// active lock before the next queue-draining pass starts.
	wikiFollowUpDelay = 5 * time.Second

	// wikiLockConflictDelay is the polling cadence while another worker owns
	// the renewable per-KB lease. Contenders publish an alternating, Unique
	// wake-up so a bulk-upload burst collapses to at most one successor instead
	// of preserving a thundering herd forever.
	wikiLockConflictDelay = 15 * time.Second

	// wikiQueueSettlementTimeout bounds the independent context used for
	// acknowledging rows, recording failed attempts, and scheduling the next
	// trigger. It must remain short: this is durable queue bookkeeping, not a
	// second business-processing window after asynq has cancelled the task.
	wikiQueueSettlementTimeout = 30 * time.Second

	// wikiDeletedKeyPrefix is the Redis key prefix for "recently deleted
	// knowledge" tombstones. Key: wiki:deleted:{kbID}:{knowledgeID}. Written
	// by cleanupWikiOnKnowledgeDelete so that any wiki_ingest task still in
	// flight (or queued) for this knowledge can fast-path skip without
	// hitting the DB. TTL > wikiIngestDelay so it's guaranteed to outlast
	// any in-flight ingest.
	wikiDeletedKeyPrefix = "wiki:deleted:"

	// wikiDeletedTTL bounds how long we remember a deletion. Must comfortably
	// exceed the longest plausible ingest run (LLM extraction + reduce).
	wikiDeletedTTL = 1 * time.Hour

	// wikiActiveLockTTL is the TTL for the per-KB "batch in progress" flag.
	// Kept short (relative to total batch runtime) so that if the owning
	// process crashes without running its `defer Del`, the orphaned lock
	// expires quickly and newcomers aren't blocked. A periodic renew
	// (wikiActiveLockRenew) keeps the lock alive while the handler is
	// genuinely still running.
	wikiActiveLockTTL = 60 * time.Second

	// wikiActiveLockRenew is how often the in-flight handler bumps the TTL.
	// Must be comfortably shorter than wikiActiveLockTTL so a single missed
	// tick (GC pause, Redis blip) doesn't let the lock slip out from under a
	// live handler.
	wikiActiveLockRenew = 20 * time.Second

	// wikiActiveLockCommandTimeout bounds Redis lease renew/release commands.
	// A stalled Redis connection must cancel business work well before the
	// 60-second lease can expire and admit another worker.
	wikiActiveLockCommandTimeout = 5 * time.Second

	// wikiTaskType is the task_type stamp used in task_pending_ops and
	// task_dead_letters rows for this pipeline. Stable across the lifetime
	// of any pending op so the follow-up consumer can pull it back.
	wikiTaskType = "wiki:ingest"

	// wikiTaskScope is the scope used by both pending ops and dead letters.
	// Wiki ingest is per-KB, so every op is scoped to a knowledge_base.
	wikiTaskScope = types.TaskScopeKnowledgeBase
)

var (
	// wikiActiveLockRenewScript renews only the lease owned by this task. A
	// plain EXPIRE can accidentally extend a replacement worker's lease after
	// the original token expired.
	wikiActiveLockRenewScript = redis.NewScript(`
		if redis.call('GET', KEYS[1]) == ARGV[1] then
			return redis.call('PEXPIRE', KEYS[1], ARGV[2])
		end
		return 0
	`)

	// wikiActiveLockReleaseScript releases only the lease owned by this task.
	// A plain DEL in defer can otherwise erase a new worker's lock after an
	// expiry/reacquire race.
	wikiActiveLockReleaseScript = redis.NewScript(`
		if redis.call('GET', KEYS[1]) == ARGV[1] then
			return redis.call('DEL', KEYS[1])
		end
		return 0
	`)
)

// WikiDeletedTombstoneKey returns the Redis key used to mark a knowledge as
// recently deleted, so wiki_ingest tasks in flight can short-circuit. Exposed
// so knowledgeService.cleanupWikiOnKnowledgeDelete can write the same key
// without duplicating the format string.
func WikiDeletedTombstoneKey(kbID, knowledgeID string) string {
	return wikiDeletedKeyPrefix + kbID + ":" + knowledgeID
}

// WikiIngestPayload is the asynq task payload for wiki ingest batch trigger.
// The actual document IDs are stored in the task_pending_ops table; this
// payload only carries the trigger metadata so the worker can resolve
// the queue tuple (task_type, scope, scope_id) and process whatever rows
// are queued under it.
type WikiIngestPayload struct {
	types.TracingContext
	TenantID        uint64 `json:"tenant_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	Language        string `json:"language,omitempty"`
	// TaskMode="map" turns this otherwise KB-scoped wake-up into an exact
	// document-generation Map wake-up. MapDedupKey is the canonical
	// task_pending_ops key and is always resolved again from PostgreSQL.
	TaskMode    string `json:"task_mode,omitempty"`
	MapDedupKey string `json:"map_dedup_key,omitempty"`
	KnowledgeID string `json:"knowledge_id,omitempty"`
	// ProcessingGeneration is diagnostic/observability metadata. The pending
	// row remains authoritative and must match before any model work.
	ProcessingGeneration string `json:"processing_generation,omitempty"`
	// WakePhase alternates between 0 and 1 for internally-produced successor
	// signals. The next payload therefore has a different Asynq uniqueness
	// digest from the currently-running task while all concurrent contenders
	// still converge on the same successor digest.
	WakePhase uint8 `json:"wake_phase,omitempty"`
}

// WikiRetractPayload is the asynq task payload for wiki content retraction
type WikiRetractPayload struct {
	types.TracingContext
	TenantID        uint64   `json:"tenant_id"`
	KnowledgeBaseID string   `json:"knowledge_base_id"`
	KnowledgeID     string   `json:"knowledge_id"`
	DocTitle        string   `json:"doc_title"`
	DocSummary      string   `json:"doc_summary,omitempty"` // one-line summary of the deleted document
	Language        string   `json:"language,omitempty"`
	PageSlugs       []string `json:"page_slugs"`
	SourceChunks    []string `json:"source_chunks,omitempty"`
	// MoveTargetKnowledgeBaseID marks a retract created by a successful
	// cross-KB move. The worker authorizes it only while the knowledge row no
	// longer belongs to this payload's source KnowledgeBaseID.
	MoveTargetKnowledgeBaseID string `json:"move_target_knowledge_base_id,omitempty"`
}

const (
	WikiOpIngest  = "ingest"
	WikiOpRetract = "retract"

	// A delete is claimed before active document writers are quiesced.  The
	// intent row is the crash-recovery proof, but it must not be consumed by a
	// Wiki worker until the post-quiescence page/chunk snapshot has replaced
	// it with a ready plan.  Empty remains ready for rows written by older
	// binaries and for move/manual retract producers.
	wikiRetractPlanIntent = "intent"
	wikiRetractPlanReady  = "ready"
)

// WikiPendingOp represents a single operation queued in task_pending_ops
// under task_type="wiki:ingest". The struct is the JSON payload of the
// task_pending_ops row; the surrounding (task_type, scope, scope_id,
// dedup_key) fields live as separate columns and are not serialized
// here.
//
// dbID is the auto-increment primary key of the task_pending_ops row
// the op was loaded from. PeekBatch fills it; consumers carry it
// through Map/Reduce so DeleteByIDs (after consume) and IncrFailCount
// (after failure) can address the right row. It is intentionally
// unexported and excluded from JSON so the persisted payload does not
// duplicate the column.
type WikiPendingOp struct {
	Op                   string `json:"op"`
	KnowledgeID          string `json:"knowledge_id"`
	ProcessingGeneration string `json:"processing_generation,omitempty"`
	// Ingest fields
	Language string `json:"language,omitempty"`
	// Retract fields
	DocTitle   string   `json:"doc_title,omitempty"`
	DocSummary string   `json:"doc_summary,omitempty"`
	PageSlugs  []string `json:"page_slugs,omitempty"`
	// SourceChunks snapshots the deleted document's chunk IDs. Workers union
	// it with an unscoped read so chunk_refs remain removable after soft delete.
	SourceChunks []string `json:"source_chunks,omitempty"`
	// MoveTargetKnowledgeBaseID distinguishes move cleanup from delete
	// cleanup. It is intentionally durable in the pending-op payload so a
	// restart can re-check the authoritative knowledge location.
	MoveTargetKnowledgeBaseID string `json:"move_target_knowledge_base_id,omitempty"`
	// RetractPlanState is "intent" only during the delete claim/quiescence
	// window. Such a row is deliberately invisible to normal Wiki consumers
	// and is atomically refreshed to "ready" before the first model-backed
	// retract. Empty is treated as ready for backward compatibility.
	RetractPlanState string `json:"retract_plan_state,omitempty"`
	// Prepared checkpoints the expensive Map phase before any page mutation.
	// Retries after page/log/index/publication/settlement failures restore this
	// plan instead of repeating extraction/summary/classification LLM calls.
	Prepared *wikiPreparedIngest `json:"prepared,omitempty"`
	// MapCheckpoint persists successful sub-stages inside Map. Extraction,
	// summary and citation classification are independently resumable, so a
	// timeout in one branch does not replay every earlier model call.
	MapCheckpoint *wikiMapCheckpoint `json:"map_checkpoint,omitempty"`
	// MapFinished is the durable hand-off from the horizontally scalable Map
	// lane to the KB-serialized materialization lane. Prepared is non-nil when
	// artifacts exist; a true MapFinished with nil Prepared represents a
	// deliberate degraded/no-artifact outcome.
	MapFinished      bool   `json:"map_finished,omitempty"`
	MapOutcomeDetail string `json:"map_outcome_detail,omitempty"`
	// AppliedPageSlugs is transactionally patched by the Wiki page repository
	// together with each page mutation. It is scoped to this pending row and
	// disappears naturally when queue settlement deletes the row.
	AppliedPageSlugs []string `json:"applied_page_slugs,omitempty"`

	// dbID is set by peekPendingList from task_pending_ops.id. Zero in
	// constructions made outside the queue (e.g. legacy tests).
	dbID int64 `json:"-"`
	// tenantID is copied from the normalized queue row and checked against the
	// KB-scoped trigger before any document lookup or LLM work.
	tenantID uint64 `json:"-"`
	// lastError carries the batch-local failure reason into the durable
	// dead-letter row after retries are exhausted. It is never serialized as
	// part of the operation payload itself.
	lastError string `json:"-"`
}

// wikiIngestService handles the LLM-powered wiki generation pipeline.
//
// Durable state lives in two places:
//   - task_pending_ops (rows tagged task_type="wiki:ingest", scope=
//     "knowledge_base"): the per-document op queue. Replaces the
//     legacy Redis wiki:pending:<kbID> list, which was vulnerable to
//     24h TTL eviction at 4w-document scale.
//   - task_dead_letters: in-batch failures that exhausted the configured
//     Wiki attempt budget land here. The asynq dead-letter middleware
//     also writes asynq-level archived rows here uniformly across
//     every task type.
//
// Redis is still used for the per-KB active-batch liveness lock
// (wiki:active:<kbID>) and the delete tombstone (wiki:deleted:<...>). The
// database wikilease epoch/token is the authoritative correctness fence, so a
// worker that resumes after Redis TTL loss cannot commit durable side effects.
type wikiIngestService struct {
	wikiService    interfaces.WikiPageService
	kbService      interfaces.KnowledgeBaseService
	knowledgeSvc   interfaces.KnowledgeService
	knowledgeRepo  interfaces.KnowledgeRepository
	chunkRepo      interfaces.ChunkRepository
	modelService   interfaces.ModelService
	task           interfaces.TaskEnqueuer
	logEntrySvc    interfaces.WikiLogEntryService
	pendingRepo    interfaces.TaskPendingOpsRepository
	deadLetterRepo interfaces.TaskDeadLetterRepository
	redisClient    *redis.Client // nil in Lite mode (no Redis)
	// spanTracker lets per-document map work surface as a
	// postprocess.wiki subspan in the knowledge trace tree. Async
	// batch design means we look up the parent attempt by knowledge
	// id at run-time (LatestAttempt) rather than carrying it in the
	// asynq payload, which is per-KB and would otherwise be ambiguous
	// for the 5-docs-per-batch fan-out.
	spanTracker  SpanTracker
	splitManager *documentsplit.Manager
	contentCache *contentcache.Store
	// llmRetryPolicy is an optional test seam. Production uses the policy from
	// the custom Wiki queue module; tests inject deterministic jitter/waits and
	// never spend real time sleeping.
	llmRetryPolicy *wikiqueue.RetryPolicy
	// liteLocks provides per-KB mutual exclusion in Lite mode (no Redis).
	// Keys are kbID strings; values are unused (presence = locked).
	liteLocks sync.Map
}

// unscopedChunkIDRepository is an optional production extension implemented
// by the GORM chunk repository. Keeping it separate from the broad public
// ChunkRepository interface avoids forcing unrelated test/search adapters to
// expose soft-deleted data.
type unscopedChunkIDRepository interface {
	ListChunkIDsByKnowledgeIDUnscoped(ctx context.Context, tenantID uint64, knowledgeID string) ([]string, error)
}

// NewWikiIngestService creates a new wiki ingest service
func NewWikiIngestService(
	wikiService interfaces.WikiPageService,
	kbService interfaces.KnowledgeBaseService,
	knowledgeSvc interfaces.KnowledgeService,
	knowledgeRepo interfaces.KnowledgeRepository,
	chunkRepo interfaces.ChunkRepository,
	modelService interfaces.ModelService,
	task interfaces.TaskEnqueuer,
	logEntrySvc interfaces.WikiLogEntryService,
	pendingRepo interfaces.TaskPendingOpsRepository,
	deadLetterRepo interfaces.TaskDeadLetterRepository,
	redisClient *redis.Client,
	spanTracker SpanTracker,
	splitManager *documentsplit.Manager,
	contentCache *contentcache.Store,
) interfaces.TaskHandler {
	svc := &wikiIngestService{
		wikiService:    wikiService,
		kbService:      kbService,
		knowledgeSvc:   knowledgeSvc,
		knowledgeRepo:  knowledgeRepo,
		chunkRepo:      chunkRepo,
		modelService:   modelService,
		task:           task,
		logEntrySvc:    logEntrySvc,
		pendingRepo:    pendingRepo,
		deadLetterRepo: deadLetterRepo,
		redisClient:    redisClient,
		spanTracker:    spanTracker,
		splitManager:   splitManager,
		contentCache:   contentCache,
	}
	return svc
}

// tracker returns a non-nil span tracker so callers don't have to
// nil-check on every Begin/End. Matches the noopSpanTracker pattern
// used elsewhere (see knowledgeService.tracker, KnowledgePostProcessService.tracker).
func (s *wikiIngestService) tracker() SpanTracker {
	if s.spanTracker == nil {
		return noopSpanTracker{}
	}
	return s.spanTracker
}

// beginWikiSubspan opens a postprocess.wiki subspan for this document
// under the knowledge's most recent attempt. Returns nil when there is
// no parse attempt to attach to (e.g. a wiki ingest fired from a manual
// reparse path that never went through the tracker) — callers must
// pair every begin with a tolerant end / fail / skip below.
//
// Lookups are by `LatestAttempt(knowledgeID)` because the asynq task
// payload (WikiIngestPayload) is KB-scoped and carries no per-doc
// attempt — see the type's comment for the batch architecture.
func (s *wikiIngestService) beginWikiSubspan(ctx context.Context, knowledgeID string, input types.JSONMap) *Span {
	if knowledgeID == "" {
		return nil
	}
	attempt := s.tracker().LatestAttempt(ctx, knowledgeID)
	if attempt <= 0 {
		return nil
	}
	parent := s.tracker().LookupStage(ctx, knowledgeID, attempt, types.StagePostProcess)
	if parent == nil {
		return nil
	}
	return s.tracker().BeginSubSpan(ctx, parent, "postprocess.wiki", types.SpanKindSubSpan, input)
}

// WikiEnqueueResult separates the durability boundary from the best-effort
// wake-up. Callers that own a document lifecycle must stop before changing
// state or fanning out other work when PendingPersisted is false. A persisted
// row remains recoverable even when TriggerScheduled is false because the
// queue recovery scanner can wake it later.
type WikiEnqueueResult struct {
	PendingPersisted bool
	TriggerScheduled bool
	// MapScheduled is reported separately because the KB trigger and the
	// document-local Map wake-up have independent recovery paths.
	MapScheduled bool
	// AlreadySettled means the exact generation is stale or already has a
	// terminal Wiki outcome. The durable intent is satisfied without creating
	// another queue row or trigger.
	AlreadySettled bool
}

// EnqueueWikiIngest queues a document for wiki ingestion.
//
// Architecture: each upload inserts one row into task_pending_ops
// (task_type="wiki:ingest", scope="knowledge_base", scope_id=kbID,
// dedup_key="knowledgeID:processingGeneration"), then schedules a debounced
// asynq trigger task.
// When the trigger fires, the worker peeks a batch from
// task_pending_ops, processes it, deletes consumed rows, and (if more
// remain) schedules a follow-up. Initial triggers carry a stable per-KB
// payload and asynq.Unique, so bulk-upload wake-ups coalesce. Triggers beyond
// that uniqueness window are still serialized by the owner-token active lock.
//
// Lite mode (no Redis) still works as long as Postgres is reachable —
// the queue lives in PG, only the active-batch lock is Redis-only and
// has a process-local fallback (liteLocks) inside the worker.
func EnqueueWikiIngest(
	ctx context.Context,
	task interfaces.TaskEnqueuer,
	pendingRepo interfaces.TaskPendingOpsRepository,
	tenantID uint64,
	kbID, knowledgeID, processingGeneration string,
) (WikiEnqueueResult, error) {
	var result WikiEnqueueResult
	dedupKey, err := wikiqueue.IngestDedupKey(knowledgeID, processingGeneration)
	if err != nil || tenantID == 0 || strings.TrimSpace(kbID) == "" {
		if err == nil {
			err = errors.New("tenant ID and knowledge base ID are required")
		}
		return result, fmt.Errorf("wiki ingest: invalid generation identity: %w", err)
	}
	lang, _ := types.LanguageFromContext(ctx)

	// Persist the pending op. Duplicate producers for the same generation
	// collapse at the database unique index; a different generation has a
	// different key and remains independently visible so the consumer can
	// terminally reject it without suppressing the authoritative row.
	op := WikiPendingOp{
		Op:                   WikiOpIngest,
		KnowledgeID:          knowledgeID,
		ProcessingGeneration: processingGeneration,
		Language:             lang,
	}
	payloadBytes, err := json.Marshal(op)
	if err != nil {
		return result, fmt.Errorf("wiki ingest: marshal pending op for %s: %w", knowledgeID, err)
	}
	var persistErr error
	if pendingRepo == nil {
		persistErr = errors.New("wiki ingest: pending-op repository is nil")
	} else {
		identity := wikiIngestIdentity(tenantID, kbID, knowledgeID, processingGeneration)
		enqueueCtx := wikiingestguard.WithValidation(ctx, identity)
		if err := pendingRepo.Enqueue(enqueueCtx, &types.TaskPendingOp{
			TenantID: tenantID,
			TaskType: wikiTaskType,
			Scope:    wikiTaskScope,
			ScopeID:  kbID,
			Op:       WikiOpIngest,
			DedupKey: dedupKey,
			Payload:  payloadBytes,
		}); err != nil {
			if len(wikiingestguard.StaleIdentities(err)) > 0 {
				// A completed/degraded/failed exact generation, or a producer
				// delayed past reparse/delete, is an idempotent success. Do not
				// wake the shared KB consumer: there is no new durable row.
				result.PendingPersisted = true
				result.AlreadySettled = true
				return result, nil
			}
			logger.Warnf(ctx, "wiki ingest: failed to enqueue pending op for %s: %v", knowledgeID, err)
			persistErr = fmt.Errorf("wiki ingest: persist pending op for %s: %w", knowledgeID, err)
		} else {
			result.PendingPersisted = true
		}
	}

	var mapErr error
	if result.PendingPersisted {
		if _, ok := pendingRepo.(wikiDistributedMapRepository); ok {
			mapPayload := WikiIngestPayload{
				TenantID:             tenantID,
				KnowledgeBaseID:      kbID,
				TaskMode:             wikiTaskModeMap,
				MapDedupKey:          dedupKey,
				KnowledgeID:          knowledgeID,
				ProcessingGeneration: processingGeneration,
			}
			mapErr = enqueueWikiMapTask(task, mapPayload, wikiMapInitialDelay)
			if mapErr != nil {
				logger.Warnf(ctx, "wiki ingest: failed to enqueue distributed Map for %s: %v", knowledgeID, mapErr)
			} else {
				result.MapScheduled = true
			}
		}
	}

	// Keep the trigger payload stable for a given tenant/KB. asynq.Unique
	// hashes task type + payload + queue; request-scoped tracing ids or locale
	// would make every upload look unique and defeat coalescing. Locale remains
	// durable on each WikiPendingOp, and the worker resolves its batch locale
	// from those rows. Per-document persisted spans are resolved by knowledge
	// id at run time (beginWikiSubspan); Langfuse still creates a standalone
	// worker trace, but deliberately does not attach this shared KB trigger to
	// one arbitrary upload request's trace.
	trigger := WikiIngestPayload{
		TenantID:        tenantID,
		KnowledgeBaseID: kbID,
	}
	triggerErr := enqueueWikiTriggerFenced(ctx, pendingRepo, task, trigger, wikiIngestDelay, true)
	if triggerErr != nil {
		logger.Warnf(ctx, "wiki ingest: failed to enqueue trigger task: %v", triggerErr)
	} else {
		result.TriggerScheduled = true
	}

	// Always try to wake an already-existing queue even if this specific row
	// failed to persist, but return every failure so the caller can retry. If
	// the row persisted and only the trigger failed, the durable row remains
	// untouched and the caller's retry (or another trigger) can recover it.
	return result, errors.Join(persistErr, mapErr, triggerErr)
}

// EnqueueWikiRetract queues a wiki retraction op (a delete cleanup).
// Identical persistence model as EnqueueWikiIngest — the op rides in
// task_pending_ops and an asynq trigger fires shortly after to
// process the batch. Retracts use a slightly shorter ProcessIn delay
// because there is no "user upload arriving in waves" pattern to
// debounce against — a deletion fires once and we want the cleanup
// to land promptly.
func EnqueueWikiRetract(
	ctx context.Context,
	task interfaces.TaskEnqueuer,
	pendingRepo interfaces.TaskPendingOpsRepository,
	payload WikiRetractPayload,
) (WikiEnqueueResult, error) {
	var result WikiEnqueueResult
	op := WikiPendingOp{
		Op:                        WikiOpRetract,
		KnowledgeID:               payload.KnowledgeID,
		DocTitle:                  payload.DocTitle,
		DocSummary:                payload.DocSummary,
		PageSlugs:                 payload.PageSlugs,
		Language:                  payload.Language,
		SourceChunks:              payload.SourceChunks,
		MoveTargetKnowledgeBaseID: payload.MoveTargetKnowledgeBaseID,
		RetractPlanState:          wikiRetractPlanReady,
	}
	payloadBytes, err := json.Marshal(op)
	if err != nil {
		return result, fmt.Errorf("wiki retract: marshal pending op: %w", err)
	}
	var persistErr error
	if pendingRepo == nil {
		persistErr = errors.New("wiki retract: pending-op repository is nil")
	} else {
		if err := pendingRepo.Enqueue(ctx, &types.TaskPendingOp{
			TenantID: payload.TenantID,
			TaskType: wikiTaskType,
			Scope:    wikiTaskScope,
			ScopeID:  payload.KnowledgeBaseID,
			Op:       WikiOpRetract,
			DedupKey: payload.KnowledgeID,
			Payload:  payloadBytes,
		}); err != nil {
			logger.Warnf(ctx, "wiki retract: failed to enqueue pending op: %v", err)
			persistErr = fmt.Errorf("wiki retract: persist pending op for %s: %w", payload.KnowledgeID, err)
		} else {
			result.PendingPersisted = true
		}
	}

	trigger := WikiIngestPayload{
		TenantID:        payload.TenantID,
		KnowledgeBaseID: payload.KnowledgeBaseID,
	}
	triggerErr := enqueueWikiTriggerFenced(ctx, pendingRepo, task, trigger, wikiFollowUpDelay, true)
	if triggerErr != nil {
		logger.Warnf(ctx, "wiki retract: failed to enqueue trigger task: %v", triggerErr)
	} else {
		result.TriggerScheduled = true
	}
	return result, errors.Join(persistErr, triggerErr)
}

// enqueueWikiTrigger publishes a KB-scoped wake-up signal. Initial ingest and
// retract signals use asynq.Unique because task_pending_ops is the source of
// truth; a duplicate signal carries no additional work. Internal successors
// alternate WikiIngestPayload.WakePhase, so they can also use Unique without
// colliding with the currently-running signal.
//
// ErrDuplicateTask is therefore success for a unique trigger: it proves an
// equivalent wake-up signal already exists. Every other enqueue error is
// returned to the caller so a durable pending row can never become silently
// orphaned.
func enqueueWikiTrigger(
	task interfaces.TaskEnqueuer,
	payload WikiIngestPayload,
	delay time.Duration,
	unique bool,
) error {
	if task == nil {
		return errors.New("wiki ingest: task enqueuer is nil")
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("wiki ingest: marshal trigger payload: %w", err)
	}
	t := asynq.NewTask(types.TypeWikiIngest, payloadBytes)
	opts := []asynq.Option{
		asynq.Queue(types.QueueWikiControl),
		asynq.MaxRetry(wikiIngestMaxRetry),
		asynq.Timeout(wikiIngestTaskTimeout),
		asynq.ProcessIn(delay),
	}
	if unique {
		opts = append(opts, asynq.Unique(wikiTriggerUniqueTTL))
	}
	if _, err := task.Enqueue(t, opts...); err != nil {
		if unique && errors.Is(err, asynq.ErrDuplicateTask) {
			return nil
		}
		return fmt.Errorf("wiki ingest: enqueue trigger: %w", err)
	}
	return nil
}

func enqueueWikiMapTask(
	task interfaces.TaskEnqueuer,
	payload WikiIngestPayload,
	delay time.Duration,
) error {
	if task == nil {
		return errors.New("wiki ingest: task enqueuer is nil")
	}
	if payload.TenantID == 0 || strings.TrimSpace(payload.KnowledgeBaseID) == "" ||
		strings.TrimSpace(payload.MapDedupKey) == "" {
		return errors.New("wiki ingest: complete distributed Map identity is required")
	}
	payload.TaskMode = wikiTaskModeMap
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("wiki ingest: marshal distributed Map payload: %w", err)
	}
	_, err = task.Enqueue(
		asynq.NewTask(types.TypeWikiIngest, payloadBytes),
		asynq.Queue(types.QueueWikiMap),
		asynq.MaxRetry(wikiIngestMaxRetry),
		asynq.Timeout(wikiIngestTaskTimeout),
		asynq.ProcessIn(delay),
		asynq.Unique(wikiMapUniqueTTL),
	)
	if err == nil || errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return fmt.Errorf("wiki ingest: enqueue distributed Map: %w", err)
}

// activeWikiKnowledgeBasePublisher is a mandatory durable-ingest capability of
// the GORM pending-op repository. It keeps the external wake-up publication
// inside the same parent-KB serialization boundary used by durable Wiki
// writes and whole-KB deletion. Ordinary non-ingest callers retain the direct
// compatibility path; a durable ingest context fails closed if its repository
// does not implement this extension.
type activeWikiKnowledgeBasePublisher interface {
	WithActiveWikiKnowledgeBase(
		ctx context.Context,
		tenantID uint64,
		knowledgeBaseID string,
		publish func() error,
	) error
}

func enqueueWikiTriggerFenced(
	ctx context.Context,
	pendingRepo interfaces.TaskPendingOpsRepository,
	task interfaces.TaskEnqueuer,
	payload WikiIngestPayload,
	delay time.Duration,
	unique bool,
) error {
	publish := func() error { return enqueueWikiTrigger(task, payload, delay, unique) }
	if fence, ok := pendingRepo.(activeWikiKnowledgeBasePublisher); ok && fence != nil {
		return fence.WithActiveWikiKnowledgeBase(
			ctx, payload.TenantID, payload.KnowledgeBaseID, publish,
		)
	}
	if wikilease.Required(ctx) {
		return errors.New("wiki ingest: pending repository does not support lease-fenced trigger publication")
	}
	return publish()
}

// Handle implements interfaces.TaskHandler for asynq task processing.
// Initial Wiki ingest tasks are debounced via asynq.Unique + ProcessIn.
// The per-KB Redis/process-local active lock in ProcessWikiIngest remains the
// correctness boundary for follow-ups and for triggers whose uniqueness TTL
// has expired while a long batch is still running.
func (s *wikiIngestService) Handle(ctx context.Context, t *asynq.Task) error {
	var payload WikiIngestPayload
	if t != nil && json.Unmarshal(t.Payload(), &payload) == nil && payload.TaskMode == wikiTaskModeMap {
		return s.ProcessWikiMap(ctx, t)
	}
	return s.ProcessWikiIngest(ctx, t)
}

// peekPendingList loads up to `limit` ops from task_pending_ops for
// this KB, ordered FIFO. Rows are NOT removed; callers must
// DeleteByIDs once they have been consumed (or IncrFailCount + leave
// them in place for the next pass).
//
// peekedIDs returns the DB ids of every row included in the peek
// (NOT just the ones that survived dedup) so trimPendingList can
// delete them all in one statement at the end of the batch — this
// matches the legacy "LTrim peekedCount entries" semantics, where
// duplicates collapsed by the consumer were also drained from the
// list once their canonical sibling had been processed.
func (s *wikiIngestService) peekPendingList(ctx context.Context, kbID string, limit int) (ops []WikiPendingOp, peekedIDs []int64, retErr error) {
	return s.peekPendingListIncludingRetractIntents(ctx, kbID, limit, false)
}

// peekPendingListIncludingRetractIntents has one terminal-cleanup escape
// hatch. Normal workers exclude delete-intent retracts from both the returned
// operations and acknowledgement IDs: the same PostgreSQL row will be
// refreshed to a ready plan after active document writers quiesce. Terminal
// KB deletion may include the rows because no target Wiki can survive.
func (s *wikiIngestService) peekPendingListIncludingRetractIntents(
	ctx context.Context,
	kbID string,
	limit int,
	includeRetractIntents bool,
) (ops []WikiPendingOp, peekedIDs []int64, retErr error) {
	if s.pendingRepo == nil {
		return nil, nil, errors.New("wiki ingest: pending-op repository is nil")
	}
	if limit <= 0 {
		limit = wikiMaxDocsPerBatch
	}
	rows, err := s.pendingRepo.PeekBatch(ctx, wikiTaskType, wikiTaskScope, kbID, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("wiki ingest: peek pending list for KB %s: %w", kbID, err)
	}
	return decodeWikiPendingRows(ctx, rows, includeRetractIntents)
}

// peekWikiCommitPendingList bypasses unprepared ingest rows in production.
// Focused service tests use repositories without the distributed extension
// and retain the historical in-process Map+Reduce behavior.
func (s *wikiIngestService) peekWikiCommitPendingList(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	limit int,
) (ops []WikiPendingOp, peekedIDs []int64, distributed bool, retErr error) {
	repo, ok := s.pendingRepo.(wikiDistributedMapRepository)
	if !ok || repo == nil {
		ops, peekedIDs, retErr = s.peekPendingList(ctx, kbID, limit)
		return ops, peekedIDs, false, retErr
	}
	rows, err := repo.PeekWikiCommitBatch(ctx, tenantID, kbID, limit)
	if err != nil {
		return nil, nil, true, fmt.Errorf("wiki ingest: peek commit-ready list for KB %s: %w", kbID, err)
	}
	ops, peekedIDs, err = decodeWikiPendingRows(ctx, rows, false)
	return ops, peekedIDs, true, err
}

func decodeWikiPendingRows(
	ctx context.Context,
	rows []*types.TaskPendingOp,
	includeRetractIntents bool,
) (ops []WikiPendingOp, peekedIDs []int64, retErr error) {
	if len(rows) == 0 {
		return nil, nil, nil
	}

	all := make([]WikiPendingOp, 0, len(rows))
	peekedIDs = make([]int64, 0, len(rows))
	for _, r := range rows {
		var op WikiPendingOp
		if len(r.Payload) > 0 {
			if err := json.Unmarshal(r.Payload, &op); err != nil {
				// The normalized columns are deliberately redundant with the
				// JSON payload. Fall back to them so a corrupt payload cannot
				// become an immortal FIFO head (or hide a retract while Wiki is
				// disabled). Retracts can resolve their authoritative page set
				// from KnowledgeID at execution time, so this remains useful even
				// without the optional title/summary/slug snapshot.
				logger.Warnf(ctx, "wiki ingest: failed to unmarshal pending op id=%d, falling back to durable columns: %v", r.ID, err)
				op = WikiPendingOp{Op: r.Op}
			}
		} else {
			// Defensive: if payload was lost, fall back to column data
			// so the row is still drainable (otherwise it would loop
			// on every batch as un-deletable).
			op = WikiPendingOp{Op: r.Op}
		}
		// Old rows and partially-written payloads may omit one of these
		// fields even when the normalized queue columns are intact.
		if op.Op == "" {
			op.Op = r.Op
		}
		if op.KnowledgeID == "" && r.Op == WikiOpRetract {
			op.KnowledgeID = r.DedupKey
		}
		op.dbID = r.ID
		op.tenantID = r.TenantID
		if op.Op == WikiOpRetract &&
			op.RetractPlanState == wikiRetractPlanIntent &&
			!includeRetractIntents {
			// Do not acknowledge or retry this row. A normal delete path will
			// UPSERT the complete ready payload into the same identity; a
			// crashed path is resumed from knowledges.parse_status=deleting.
			continue
		}
		peekedIDs = append(peekedIDs, r.ID)
		all = append(all, op)
	}

	// Retraction is authoritative for a document/KB and suppresses every ingest
	// in the same peek. Otherwise de-duplicate ingest only within one processing
	// generation. Keeping distinct generations lets preflight reject a late old
	// producer without allowing it to replace a newer queued generation.
	retracted := make(map[string]bool)
	for _, op := range all {
		if op.Op == WikiOpRetract && op.KnowledgeID != "" {
			retracted[op.KnowledgeID] = true
		}
	}
	seen := make(map[string]bool)
	reversedUnique := make([]WikiPendingOp, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		op := all[i]
		if op.KnowledgeID == "" {
			// No dedup key — keep verbatim (rare; edge case for
			// future ops without a knowledge anchor).
			reversedUnique = append(reversedUnique, op)
			continue
		}
		if op.Op == WikiOpIngest && retracted[op.KnowledgeID] {
			continue
		}
		logicalKey := op.Op + "\x00" + op.KnowledgeID
		if op.Op == WikiOpIngest {
			logicalKey += "\x00" + op.ProcessingGeneration
		}
		if seen[logicalKey] {
			continue
		}
		seen[logicalKey] = true
		reversedUnique = append(reversedUnique, op)
	}

	ops = make([]WikiPendingOp, 0, len(reversedUnique))
	for i := len(reversedUnique) - 1; i >= 0; i-- {
		ops = append(ops, reversedUnique[i])
	}
	return ops, peekedIDs, nil
}

// trimPendingList deletes consumed rows from task_pending_ops. Empty
// input is a no-op so callers can invoke unconditionally at the end
// of a batch.
func (s *wikiIngestService) trimPendingList(ctx context.Context, ids []int64) error {
	if s.pendingRepo == nil || len(ids) == 0 {
		if s.pendingRepo == nil && len(ids) > 0 {
			return errors.New("wiki ingest: pending-op repository is nil")
		}
		return nil
	}
	if err := s.pendingRepo.DeleteByIDs(ctx, ids); err != nil {
		return fmt.Errorf("wiki ingest: trim %d pending rows: %w", len(ids), err)
	}
	return nil
}

// requeueFailedOps records in-batch failures.
//
// For each failed op:
//
//   - IncrFailCount on the source row. The repo returns the new total,
//     so a single round trip handles both bookkeeping and retry-budget
//     check.
//   - If the count is below the configured maximum total attempts: leave the
//     row in place. Retry selection rotates it behind lower-attempt and
//     never-attempted rows, so it cannot monopolize the queue head.
//   - If the count reaches the retry cap: atomically move the op into
//     task_dead_letters via ArchiveToDeadLetter. The repository transaction
//     rolls the pending-row delete back if archive insertion fails. Any DB
//     failure is returned so the asynq trigger retries; silently swallowing
//     it would leave a poisoned row at the queue head while reporting success.
func (s *wikiIngestService) requeueFailedOps(ctx context.Context, payload WikiIngestPayload, ops []WikiPendingOp) error {
	if s.pendingRepo == nil || len(ops) == 0 {
		if s.pendingRepo == nil && len(ops) > 0 {
			return errors.New("wiki ingest: pending-op repository is nil")
		}
		return nil
	}
	maxAttempts := workretry.ConfigFromEnv().WikiMaxAttempts
	var errs []error
	for _, op := range ops {
		if op.dbID == 0 {
			// Op was never persisted (synthetic / test) — nothing to
			// retry against.
			continue
		}
		count, err := s.pendingRepo.IncrFailCount(ctx, op.dbID)
		if err != nil {
			logger.Warnf(ctx, "wiki ingest: failed to increment fail count for %s (id=%d): %v", op.KnowledgeID, op.dbID, err)
			errs = append(errs, fmt.Errorf("increment fail count for pending row %d: %w", op.dbID, err))
			continue
		}
		if count < maxAttempts {
			logger.Infof(ctx, "wiki ingest: re-queued failed op %s (%s) for retry (attempt %d/%d)", op.KnowledgeID, op.DocTitle, count, maxAttempts)
			continue
		}

		// Exhausted in-batch retries — archive and remove. Wiki enrichment
		// is KB-scoped optional work and does not participate in a knowledge
		// row's pending_subtasks_count.
		logger.Warnf(ctx, "wiki ingest: dropping op %s (%s) after %d failures (limit %d)", op.KnowledgeID, op.DocTitle, count, maxAttempts)
		payloadBytes, marshalErr := json.Marshal(op)
		if marshalErr != nil {
			errs = append(errs, fmt.Errorf("marshal dead letter for pending row %d: %w", op.dbID, marshalErr))
			continue
		}
		lastError := op.lastError
		if lastError == "" {
			lastError = fmt.Sprintf("reached Wiki max attempts=%d", maxAttempts)
		} else {
			lastError = fmt.Sprintf("%s (reached Wiki max attempts=%d)", lastError, maxAttempts)
		}
		// Record the terminal document-level outcome before removing the only
		// durable queue row that carries this processing generation.  A
		// transient status-write failure deliberately keeps the pending row in
		// place so the next wake-up can retry both operations; archiving first
		// would make a failed Wiki lane look permanently pending.
		if statusErr := s.recordWikiGenerationStatus(
			ctx,
			payload,
			op,
			types.WikiStatusFailed,
			lastError,
		); statusErr != nil {
			logger.Warnf(ctx, "wiki ingest: failed to record terminal status for op %s: %v", op.KnowledgeID, statusErr)
			errs = append(errs, fmt.Errorf("record terminal Wiki status for pending row %d: %w", op.dbID, statusErr))
			continue
		}
		deadLetter := &types.TaskDeadLetter{
			TenantID:  payload.TenantID,
			TaskType:  wikiTaskType,
			Scope:     wikiTaskScope,
			ScopeID:   payload.KnowledgeBaseID,
			RelatedID: op.KnowledgeID,
			Payload:   payloadBytes,
			LastError: lastError,
			FailCount: count,
		}
		if archiveErr := s.pendingRepo.ArchiveToDeadLetter(ctx, op.dbID, deadLetter); archiveErr != nil {
			logger.Warnf(ctx, "wiki ingest: failed to archive op %s to dead letters: %v", op.KnowledgeID, archiveErr)
			errs = append(errs, fmt.Errorf("archive pending row %d: %w", op.dbID, archiveErr))
		}
	}
	return errors.Join(errs...)
}

// docIngestResult captures per-document info for batch post-processing.
type docIngestResult struct {
	KnowledgeID          string
	ProcessingGeneration string
	DocTitle             string
	Summary              string // one-line summary of the document (from summary page)
	// SourceOpID is task_pending_ops.id for the canonical ingest row. It is
	// carried from map to the deferred batch log write so retries use the same
	// durable idempotency key even when logging succeeded but a later index,
	// publication, or queue-settlement step failed.
	SourceOpID int64
	// Pages records the wiki pages this document touched, carrying both
	// the slug (for navigation / retract lookups) and the human-readable
	// title captured at ingest time (for the log feed's display layer).
	Pages []types.WikiLogPageRef
	// MapStats are the per-doc map-phase metrics captured at the moment
	// mapOneDocument finishes. Surfaced into the postprocess.wiki span's
	// output so the trace viewer can show "what the map phase produced"
	// even though the span itself stays open until the batch's reduce +
	// cleanup phases complete (so the user-visible duration covers the
	// whole pipeline for this doc, not just LLM extraction).
	MapStats types.JSONMap
	// WikiSpan is the postprocess.wiki subspan opened at the start of
	// mapOneDocument. ProcessWikiIngest holds it open across the reduce
	// + cleanup phases and closes it once this doc's pages have all
	// been materialised — see the EndSpan call near the end of
	// ProcessWikiIngest. nil when no parent attempt was found, in which
	// case the tracker helpers are all no-ops anyway.
	WikiSpan *Span
}

// WikiBatchContext holds shared data across Map and Reduce phases.
//
// Historically this carried a fully materialized `AllPages` slice plus
// pre-built SlugTitleMap / SummaryContentByKnowledgeID lookup tables.
// At 4w-document scale that meant the very first thing every batch
// did was load 100K+ wiki_pages rows (content TEXT included) into Go
// memory — and then walk them several more times for cleanDeadLinks /
// injectCrossLinks / getExistingPageSlugsForKnowledge.
//
// We now lazy-load via fetchers backed by lightweight projections
// (ListBySlugs / ListSummariesByKnowledgeIDs). Each fetcher caches
// results keyed by its input so repeat lookups within a batch are
// free; the cache is per-batch and goroutine-local-via-mutex (sync.Map
// would also work but mutex keeps the surface small).
type WikiBatchContext struct {
	// SlugTitle resolves a slug to its current title (or "" if missing).
	// Backed by ListBySlugs; cache is populated as callers ask, so we
	// only pay for the slugs we actually look at.
	SlugTitle func(ctx context.Context, slug string) string

	// SlugTitleMany batches a slug-set into a single ListBySlugs query
	// and returns the resolved titles map. Convenient when a caller
	// already has the full slug list; results are still cached.
	SlugTitleMany func(ctx context.Context, slugs []string) map[string]string

	// SummaryContentByKnowledgeID returns the surviving summary page's
	// content for the given knowledge id (or "" if no summary page
	// exists / was archived). Backed by ListSummariesByKnowledgeIDs;
	// cache is populated lazily as well.
	SummaryContentByKnowledgeID func(ctx context.Context, kid string) string

	// ExtractionGranularity drives Pass 0 (candidate slug extraction)
	// aggressiveness. Resolved once per batch from the KnowledgeBase's
	// WikiConfig so every doc in the batch sees the same scope rules.
	// Already Normalize()'d — consumers can assume it is one of the
	// three valid values.
	ExtractionGranularity types.WikiExtractionGranularity

	// PlannedFolderID holds the per-slug wiki_folders.id assigned by the batch
	// taxonomy planning pass (planBatchTaxonomy + folder resolution), keyed by
	// page slug. Reduce applies it only to pages that aren't already filed
	// (FolderID == ""), so the whole batch lands on one coherent tree without
	// churning user-curated placements. The folders themselves are created
	// sequentially before reduce, so the parallel reduce phase only assigns
	// pre-resolved ids and never races on folder creation. Read-only during
	// reduce.
	PlannedFolderID map[string]string
}

// SlugUpdate represents a single update operation for a specific slug
type SlugUpdate struct {
	Slug        string
	Type        string        // "entity", "concept", "summary", "retract", "retractStale"
	Item        extractedItem // For entity/concept
	DocTitle    string
	KnowledgeID string
	// ProcessingGeneration fences additions and reparse replacements against a
	// newer parse that may start while Wiki map/reduce LLM calls are in flight.
	// Delete/move retract updates deliberately leave this empty.
	ProcessingGeneration string
	SourceRef            string
	Language             string
	SummaryBody          string // For summary
	SummaryLine          string // For summary
	RetractDocContent    string // For retract / retractStale
	// SourceOpID identifies the durable task_pending_ops row that owns a
	// retract. Wiki page metadata records successful application so a retry
	// after log/index/settlement failure never edits the same body twice.
	SourceOpID int64
	// PageAlreadyApplied is derived from the durable pending-row checkpoint on
	// load. It is never serialized inside Prepared because AppliedPageSlugs is
	// the canonical source of truth.
	PageAlreadyApplied bool `json:"-"`
	// SourceChunks lists the chunk IDs (within KnowledgeID) that substantively
	// support this update. Mirrors Item.SourceChunks for convenience — the
	// Reduce phase reads from here to avoid an extra field hop.
	SourceChunks []string
	// DocSummary is the document-level summary body produced by
	// WikiSummaryPrompt (everything after the SUMMARY: ... headline, falling
	// back to the raw output if no headline could be parsed out). Carried
	// here so the Reduce phase can frame cited chunks with a rich
	// <source_context> block that tells the editor model what the document
	// is about AND what kind of document it is (resume vs announcement vs
	// product page). The one-line headline alone was too terse to keep the
	// editor grounded on longer / multi-topic source documents.
	DocSummary string
}

// wikiPreparedIngest is the durable Map output stored inside the owning
// task_pending_ops payload. Keeping it with the queue row makes retries cheap
// and exact without a second lifecycle table or permanent per-page markers.
type wikiPreparedIngest struct {
	DocTitle string                 `json:"doc_title"`
	Summary  string                 `json:"summary"`
	Pages    []types.WikiLogPageRef `json:"pages"`
	MapStats types.JSONMap          `json:"map_stats,omitempty"`
	Updates  []SlugUpdate           `json:"updates"`
	// WikiSpan lets a Map worker on one replica hand the exact open span to a
	// materialization worker on another replica. The latter closes the same
	// row instead of creating a duplicate/cancelling the distributed Map span.
	WikiSpan *wikiPreparedSpan `json:"wiki_span,omitempty"`
}

type wikiPreparedSpan struct {
	KnowledgeID  string    `json:"knowledge_id"`
	Attempt      int       `json:"attempt"`
	SpanID       string    `json:"span_id"`
	ParentSpanID string    `json:"parent_span_id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	StartedAt    time.Time `json:"started_at"`
}

const wikiMapCheckpointVersion = 1

type wikiMapCheckpoint struct {
	Version                int                   `json:"version"`
	ContentHash            string                `json:"content_hash"`
	ExtractedEntities      []extractedItem       `json:"extracted_entities,omitempty"`
	ExtractedConcepts      []extractedItem       `json:"extracted_concepts,omitempty"`
	Pass0Failed            bool                  `json:"pass0_failed,omitempty"`
	ExtractionDone         bool                  `json:"extraction_done,omitempty"`
	SummaryContent         string                `json:"summary_content,omitempty"`
	SummaryDone            bool                  `json:"summary_done,omitempty"`
	Citations              map[string][]string   `json:"citations,omitempty"`
	NewSlugs               []newSlugFromCitation `json:"new_slugs,omitempty"`
	ClassificationBatches  int                   `json:"classification_batches,omitempty"`
	ClassificationFailures int                   `json:"classification_failures,omitempty"`
	ClassificationDegraded bool                  `json:"classification_degraded,omitempty"`
	ClassificationDone     bool                  `json:"classification_done,omitempty"`
}

type wikiPendingPayloadCheckpointer interface {
	UpdateWikiPayload(
		ctx context.Context,
		id int64,
		tenantID uint64,
		knowledgeBaseID string,
		payload []byte,
	) (bool, error)
}

type wikiAttemptRotator interface {
	TouchWikiAttempt(context.Context, int64) error
}

// rotateWikiAttempts moves budget-free circuit/admission rejects behind
// untouched rows without changing their real-provider failure count. The
// production repository implements this with task_pending_ops.claimed_at;
// focused interface-only tests may omit the optional capability.
func (s *wikiIngestService) rotateWikiAttempts(
	ctx context.Context,
	ops []WikiPendingOp,
) error {
	rotator, ok := s.pendingRepo.(wikiAttemptRotator)
	if !ok || rotator == nil || len(ops) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ops))
	var errs []error
	for _, op := range ops {
		if op.dbID <= 0 {
			continue
		}
		if _, duplicate := seen[op.dbID]; duplicate {
			continue
		}
		seen[op.dbID] = struct{}{}
		if err := rotator.TouchWikiAttempt(ctx, op.dbID); err != nil {
			errs = append(errs, fmt.Errorf("rotate Wiki pending row %d: %w", op.dbID, err))
		}
	}
	return errors.Join(errs...)
}

// wikiDistributedMapRepository is a production-only extension of the generic
// pending-op repository. Keeping these Wiki-specific selectors structural
// avoids coupling unrelated queue consumers while allowing focused service
// tests to retain their small in-memory repositories.
type wikiDistributedMapRepository interface {
	GetWikiIngestByDedupKey(
		context.Context, uint64, string, string,
	) (*types.TaskPendingOp, error)
	DeleteWikiIngestByDedupKey(context.Context, uint64, string, string) error
	PeekWikiCommitBatch(
		context.Context, uint64, string, int,
	) ([]*types.TaskPendingOp, error)
	CountWikiCommitReady(context.Context, uint64, string) (int64, error)
	MarkWikiMapReady(
		context.Context, int64, uint64, string, []byte,
	) (bool, error)
}

type wikiGenerationStatusRepository interface {
	UpdateWikiStatusGeneration(
		context.Context, uint64, string, string, string, string, string,
	) (bool, error)
}

func (s *wikiIngestService) recordWikiGenerationStatus(
	ctx context.Context,
	payload WikiIngestPayload,
	op WikiPendingOp,
	status string,
	detail string,
) error {
	if op.Op != WikiOpIngest || op.KnowledgeID == "" || op.ProcessingGeneration == "" {
		return nil
	}
	// Focused Wiki unit tests use interface-only stubs. Production wiring always
	// provides the GORM knowledge repository; when it is present, lack of the
	// generation-scoped extension is a hard configuration error.
	if s.knowledgeRepo == nil {
		return nil
	}
	statusRepo, ok := s.knowledgeRepo.(wikiGenerationStatusRepository)
	if !ok || statusRepo == nil {
		return errors.New("wiki ingest: knowledge repository does not support generation-scoped Wiki status")
	}
	updated, err := statusRepo.UpdateWikiStatusGeneration(
		ctx,
		payload.TenantID,
		op.KnowledgeID,
		payload.KnowledgeBaseID,
		op.ProcessingGeneration,
		status,
		detail,
	)
	if err != nil {
		return fmt.Errorf("wiki ingest: record status for knowledge %s: %w", op.KnowledgeID, err)
	}
	if !updated {
		logger.Infof(
			ctx,
			"wiki ingest: status update skipped for stale generation knowledge=%s generation=%s",
			op.KnowledgeID,
			op.ProcessingGeneration,
		)
	} else {
		pipelineobs.ObserveWikiOutcome(status)
	}
	return nil
}

// wikiDatabaseLeaseAcquirer is mandatory for the production durable ingest
// path. A repository that cannot advance the DB fencing epoch must fail closed
// rather than silently relying on the expiring Redis/Lite lock.
type wikiDatabaseLeaseAcquirer interface {
	AcquireWikiIngestLease(
		ctx context.Context,
		tenantID uint64,
		knowledgeBaseID string,
	) (wikilease.Identity, error)
}

func wikiIngestIdentity(tenantID uint64, kbID, knowledgeID, generation string) wikiingestguard.Identity {
	return wikiingestguard.Identity{
		TenantID:             tenantID,
		KnowledgeBaseID:      kbID,
		KnowledgeID:          knowledgeID,
		ProcessingGeneration: generation,
	}
}

func wikiIngestOperationsForUpdates(tenantID uint64, kbID string, updates []SlugUpdate) []wikiingestguard.Operation {
	operations := make([]wikiingestguard.Operation, 0, len(updates))
	for _, update := range updates {
		if strings.TrimSpace(update.ProcessingGeneration) == "" {
			continue
		}
		operations = append(operations, wikiingestguard.Operation{
			PendingOpID: update.SourceOpID,
			Identity: wikiIngestIdentity(
				tenantID, kbID, update.KnowledgeID, update.ProcessingGeneration,
			),
		})
	}
	return operations
}

func wikiIngestValidationContextForUpdates(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	updates []SlugUpdate,
) context.Context {
	operations := wikiIngestOperationsForUpdates(tenantID, kbID, updates)
	identities := make([]wikiingestguard.Identity, 0, len(operations))
	for _, operation := range operations {
		identities = append(identities, operation.Identity)
	}
	return wikiingestguard.WithValidation(ctx, identities...)
}

func wikiIngestPageApplicationContext(
	ctx context.Context,
	tenantID uint64,
	kbID, slug string,
	updates []SlugUpdate,
) context.Context {
	return wikiingestguard.WithPageApplication(
		ctx, slug, wikiIngestOperationsForUpdates(tenantID, kbID, updates)...,
	)
}

func wikiIngestIdentitiesForResults(tenantID uint64, kbID string, results []*docIngestResult) []wikiingestguard.Identity {
	identities := make([]wikiingestguard.Identity, 0, len(results))
	for _, result := range results {
		if result == nil {
			continue
		}
		identities = append(identities, wikiIngestIdentity(
			tenantID, kbID, result.KnowledgeID, result.ProcessingGeneration,
		))
	}
	return identities
}

func (s *wikiIngestService) checkpointPreparedWikiIngest(
	ctx context.Context,
	payload WikiIngestPayload,
	op WikiPendingOp,
	result *docIngestResult,
	updates []SlugUpdate,
) error {
	if result == nil || op.dbID <= 0 {
		return nil
	}
	preparedUpdates := append([]SlugUpdate(nil), updates...)
	for i := range preparedUpdates {
		preparedUpdates[i].PageAlreadyApplied = false
	}
	op.MapCheckpoint = nil
	op.MapFinished = true
	op.MapOutcomeDetail = ""
	op.Prepared = &wikiPreparedIngest{
		DocTitle: result.DocTitle,
		Summary:  result.Summary,
		Pages:    append([]types.WikiLogPageRef(nil), result.Pages...),
		MapStats: result.MapStats,
		Updates:  preparedUpdates,
		WikiSpan: preparedWikiSpan(result.WikiSpan),
	}
	return s.publishWikiMapReady(ctx, payload, op, "prepared map")
}

func preparedWikiSpan(span *Span) *wikiPreparedSpan {
	if span == nil {
		return nil
	}
	return &wikiPreparedSpan{
		KnowledgeID:  span.KnowledgeID,
		Attempt:      span.Attempt,
		SpanID:       span.SpanID,
		ParentSpanID: span.ParentSpanID,
		Name:         span.Name,
		Kind:         span.Kind,
		StartedAt:    span.StartedAt,
	}
}

func restoreWikiSpan(prepared *wikiPreparedSpan) *Span {
	if prepared == nil || strings.TrimSpace(prepared.SpanID) == "" {
		return nil
	}
	return &Span{
		KnowledgeID:  prepared.KnowledgeID,
		Attempt:      prepared.Attempt,
		SpanID:       prepared.SpanID,
		ParentSpanID: prepared.ParentSpanID,
		Name:         prepared.Name,
		Kind:         prepared.Kind,
		StartedAt:    prepared.StartedAt,
	}
}

func (s *wikiIngestService) checkpointWikiMapFinishedWithoutArtifacts(
	ctx context.Context,
	payload WikiIngestPayload,
	op WikiPendingOp,
	detail string,
) error {
	if op.dbID <= 0 {
		return nil
	}
	op.MapCheckpoint = nil
	op.Prepared = nil
	op.MapFinished = true
	op.MapOutcomeDetail = strings.TrimSpace(detail)
	return s.publishWikiMapReady(ctx, payload, op, "no-artifact map")
}

func (s *wikiIngestService) checkpointWikiMapProgress(
	ctx context.Context,
	payload WikiIngestPayload,
	op WikiPendingOp,
	checkpoint *wikiMapCheckpoint,
) error {
	if op.dbID <= 0 {
		return nil
	}
	if checkpoint == nil || checkpoint.Version != wikiMapCheckpointVersion ||
		strings.TrimSpace(checkpoint.ContentHash) == "" {
		return errors.New("wiki ingest: invalid partial map checkpoint")
	}
	op.Prepared = nil
	op.MapCheckpoint = checkpoint
	return s.persistWikiPendingOpPayload(ctx, payload, op, "partial map")
}

func (s *wikiIngestService) persistWikiPendingOpPayload(
	ctx context.Context,
	payload WikiIngestPayload,
	op WikiPendingOp,
	stage string,
) error {
	if op.dbID <= 0 {
		return nil
	}
	checkpointer, ok := s.pendingRepo.(wikiPendingPayloadCheckpointer)
	if !ok || checkpointer == nil {
		return errors.New("wiki ingest: pending repository does not support durable payload checkpoints")
	}
	encoded, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("wiki ingest: encode %s checkpoint: %w", stage, err)
	}
	identity := wikiIngestIdentity(
		payload.TenantID, payload.KnowledgeBaseID, op.KnowledgeID, op.ProcessingGeneration,
	)
	checkpointCtx := wikiingestguard.WithValidation(ctx, identity)
	updated, err := checkpointer.UpdateWikiPayload(
		checkpointCtx, op.dbID, payload.TenantID, payload.KnowledgeBaseID, encoded,
	)
	if err != nil {
		return fmt.Errorf("wiki ingest: persist %s checkpoint: %w", stage, err)
	}
	if !updated {
		return wikiingestguard.NewStaleIdentityError(identity)
	}
	return nil
}

func (s *wikiIngestService) publishWikiMapReady(
	ctx context.Context,
	payload WikiIngestPayload,
	op WikiPendingOp,
	stage string,
) error {
	if op.dbID <= 0 {
		return nil
	}
	publisher, ok := s.pendingRepo.(wikiDistributedMapRepository)
	if !ok || publisher == nil {
		// Focused legacy tests intentionally use a narrow fake repository.
		// Their in-process batch still consumes the row immediately, so the
		// separate readiness selector is not involved.
		return s.persistWikiPendingOpPayload(ctx, payload, op, stage)
	}
	encoded, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("wiki ingest: encode %s checkpoint: %w", stage, err)
	}
	identity := wikiIngestIdentity(
		payload.TenantID, payload.KnowledgeBaseID, op.KnowledgeID, op.ProcessingGeneration,
	)
	checkpointCtx := wikiingestguard.WithValidation(ctx, identity)
	updated, err := publisher.MarkWikiMapReady(
		checkpointCtx, op.dbID, payload.TenantID, payload.KnowledgeBaseID, encoded,
	)
	if err != nil {
		return fmt.Errorf("wiki ingest: publish %s checkpoint: %w", stage, err)
	}
	if !updated {
		return wikiingestguard.NewStaleIdentityError(identity)
	}
	return nil
}

func (s *wikiIngestService) restorePreparedWikiIngest(
	ctx context.Context,
	payload WikiIngestPayload,
	op WikiPendingOp,
) (*docIngestResult, []SlugUpdate, bool) {
	if op.Prepared == nil {
		return nil, nil, false
	}
	applied := make(map[string]struct{}, len(op.AppliedPageSlugs))
	for _, slug := range op.AppliedPageSlugs {
		if slug = strings.TrimSpace(slug); slug != "" {
			applied[slug] = struct{}{}
		}
	}
	updates := append([]SlugUpdate(nil), op.Prepared.Updates...)
	for i := range updates {
		updates[i].KnowledgeID = op.KnowledgeID
		updates[i].ProcessingGeneration = op.ProcessingGeneration
		updates[i].SourceOpID = op.dbID
		_, updates[i].PageAlreadyApplied = applied[updates[i].Slug]
	}
	wikiSpan := restoreWikiSpan(op.Prepared.WikiSpan)
	if wikiSpan == nil {
		wikiSpan = s.beginWikiSubspan(ctx, op.KnowledgeID, types.JSONMap{
			"knowledge_base_id": payload.KnowledgeBaseID,
			"durable_resume":    true,
		})
	}
	return &docIngestResult{
		KnowledgeID:          op.KnowledgeID,
		ProcessingGeneration: op.ProcessingGeneration,
		DocTitle:             op.Prepared.DocTitle,
		Summary:              op.Prepared.Summary,
		SourceOpID:           op.dbID,
		Pages:                append([]types.WikiLogPageRef(nil), op.Prepared.Pages...),
		MapStats:             op.Prepared.MapStats,
		WikiSpan:             wikiSpan,
	}, updates, true
}

func previewText(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	r := []rune(s)
	if maxRunes <= 0 || len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "...(truncated)"
}

func previewStringSlice(items []string, limit int) string {
	if len(items) == 0 {
		return "[]"
	}
	if limit <= 0 {
		limit = 1
	}
	n := len(items)
	if n > limit {
		items = items[:limit]
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, previewText(it, 48))
	}
	if n > limit {
		return fmt.Sprintf("[%s ...(+%d)]", strings.Join(out, ", "), n-limit)
	}
	return fmt.Sprintf("[%s]", strings.Join(out, ", "))
}

// previewExtractedItems returns a JSON-friendly preview of the first
// `limit` extracted entities or concepts so the trace viewer's
// postprocess.wiki.extract span shows actual names/slugs/descriptions
// instead of bare counts. Each item is trimmed to a small fixed
// budget — these end up serialised into the spans table's JSONB
// output column, so the cumulative size matters more than per-item
// fidelity.
func previewExtractedItems(items []extractedItem, limit int) []map[string]string {
	if limit <= 0 {
		limit = 1
	}
	n := len(items)
	if n > limit {
		items = items[:limit]
	}
	out := make([]map[string]string, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]string{
			"name":        previewText(it.Name, 60),
			"slug":        it.Slug,
			"description": previewText(it.Description, 120),
		})
	}
	return out
}

// topCitedSlugs returns the top `limit` slugs by chunk-citation count.
// Used by postprocess.wiki.classify so the trace surfaces which
// candidate slugs the citation pass attached the most chunks to —
// useful when triaging "this LLM run extracted weird things" without
// having to open and diff full chunk lists.
func topCitedSlugs(citations map[string][]string, limit int) []map[string]any {
	if len(citations) == 0 {
		return nil
	}
	type entry struct {
		slug  string
		count int
	}
	entries := make([]entry, 0, len(citations))
	for slug, ids := range citations {
		entries = append(entries, entry{slug: slug, count: len(ids)})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].slug < entries[j].slug
	})
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"slug":   e.slug,
			"chunks": e.count,
		})
	}
	return out
}

// previewNewSlugs returns a JSON-friendly preview of the first
// `limit` slugs that the citation pass discovered (i.e. did not appear
// in pass-0's candidate list). Surfacing these makes "the citation
// LLM kept inventing entries" trivially diagnosable from the trace
// viewer.
func previewNewSlugs(items []newSlugFromCitation, limit int) []map[string]string {
	if limit <= 0 {
		limit = 1
	}
	n := len(items)
	if n > limit {
		items = items[:limit]
	}
	out := make([]map[string]string, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]string{
			"name":   previewText(it.Name, 60),
			"slug":   it.Slug,
			"type":   it.Type,
			"chunks": fmt.Sprintf("%d", len(it.SourceChunks)),
		})
	}
	return out
}

// wikiLinkRE matches `[[slug]]` and `[[slug|display text]]` references
// inside wiki page content. The slug capture group rejects whitespace and
// the closing-bracket / pipe characters so we don't accidentally swallow
// adjacent text. Display text (group 2) is optional.
var wikiLinkRE = regexp.MustCompile(`\[\[([^\[\]\|\s]+)(?:\|([^\]]+))?\]\]`)

// sanitizeDeadSummaryLinks rewrites the summary pages produced by THIS
// batch to fix `[[slug]]` / `[[slug|display]]` references that point
// at slugs whose entity/concept page generation failed in reduce.
//
// Background: WikiSummaryPrompt instructs the LLM to embed wiki links
// for every extracted slug it knows about, but slug extraction happens
// during map (parallel with summary generation) and the actual page
// creation happens later in reduce. When reduce's WikiPageModifyPrompt
// fails on an entity/concept slug the page never gets written — and
// the already-persisted summary is left holding a `[[entity/foo|name]]`
// link that 404s.
//
// We pass the batch's affected-slug set + the SlugTitleMany fetcher
// to the resolver so that LLM-mangled slugs (e.g. extra pinyin hyphens
// in "shang-hai-tower" vs "shanghai-tower") are healed in place rather
// than stripped to plain text — preserving cross-link information
// whenever the display text or surface form unambiguously identifies a
// live page.
//
// Pure text replacement, no LLM call. Scoped to the doc-summary slugs
// in this batch (`summary/<slugify(knowledgeID)>`), keeping the work
// proportional to batch size.
func (s *wikiIngestService) sanitizeDeadSummaryLinks(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	docResults []*docIngestResult,
	failedSlugs map[string]struct{},
	batchCtx *WikiBatchContext,
) map[string]error {
	failures := make(map[string]error)
	if len(failedSlugs) == 0 || len(docResults) == 0 {
		return failures
	}
	// Build a (live-slug-set, title->slug) pair the resolver can consult.
	// We seed liveSlugs from batchCtx (the slugs that DID make it into
	// pages this batch) and expand it lazily as needed via SlugTitleMany.
	// titleToSlug is filled with the same successful pages' titles so the
	// display-text reverse lookup works on first try.
	for _, r := range docResults {
		if r == nil || r.KnowledgeID == "" {
			continue
		}
		summarySlug := "summary/" + slugify(r.KnowledgeID)
		page, err := s.wikiService.GetPageBySlug(ctx, kbID, summarySlug)
		if err != nil || page == nil {
			continue
		}

		// Collect the slugs this summary actually links to (so the
		// resolver has a non-empty pool of candidates), plus all the
		// successfully-written sibling pages from the same doc. These
		// two sets together cover the LLM-vs-actual mismatch cases
		// without paying for a full ListAll scan.
		candidateSlugs := make(map[string]struct{}, len(page.OutLinks)+len(r.Pages))
		for _, slug := range page.OutLinks {
			candidateSlugs[slug] = struct{}{}
		}
		for _, ref := range r.Pages {
			if _, bad := failedSlugs[ref.Slug]; bad {
				continue
			}
			candidateSlugs[ref.Slug] = struct{}{}
		}
		liveSlugs, titleToSlug := s.resolveLiveSlugs(ctx, batchCtx, candidateSlugs)

		newContent, changed := stripDeadWikiLinks(page.Content, failedSlugs, liveSlugs, titleToSlug)
		if !changed {
			continue
		}
		_, stale, fenceErr := s.partitionLiveDocIngestResults(
			ctx, tenantID, kbID, []*docIngestResult{r},
		)
		if fenceErr != nil {
			failures[r.KnowledgeID] = fmt.Errorf("revalidate summary link cleanup: %w", fenceErr)
			continue
		}
		if len(stale) > 0 {
			failures[r.KnowledgeID] = wikiingestguard.NewStaleIdentityError(stale...)
			continue
		}
		page.Content = newContent
		writeCtx := wikiingestguard.WithValidation(ctx, wikiIngestIdentity(
			tenantID, kbID, r.KnowledgeID, r.ProcessingGeneration,
		))
		if err := s.wikiService.UpdateAutoLinkedContent(writeCtx, page); err != nil {
			logger.Warnf(ctx, "wiki ingest: failed to sanitize dead links in summary %s: %v", summarySlug, err)
			failures[r.KnowledgeID] = err
			continue
		}
		logger.Infof(ctx, "wiki ingest: sanitized dead [[slug]] refs in summary %s", summarySlug)
	}
	return failures
}

// resolveLiveSlugs builds the (liveSlugs, titleToSlug) pair that
// stripDeadWikiLinks / cleanDeadLinks pass into resolveDeadSlug.
//
// We start from a caller-supplied candidate set (typically the page's
// own out-links + this batch's freshly-written slugs) and ask the
// batch's SlugTitleMany fetcher to resolve them in one batched query.
// The fetcher already filters out archived / system pages, so missing
// entries naturally translate to "not live" without an extra check.
//
// titleToSlug is keyed by the page's exact title only — we don't have
// aliases in the lite projection. That's an acceptable trade-off: the
// reported breakage pattern is "slug munged, display = title", not
// "slug munged, display = alias", so display-by-title carries the
// majority of the rescue value at a fraction of the storage cost.
func (s *wikiIngestService) resolveLiveSlugs(
	ctx context.Context,
	batchCtx *WikiBatchContext,
	candidates map[string]struct{},
) (map[string]struct{}, map[string]string) {
	if len(candidates) == 0 || batchCtx == nil || batchCtx.SlugTitleMany == nil {
		return nil, nil
	}
	slugList := make([]string, 0, len(candidates))
	for s := range candidates {
		slugList = append(slugList, s)
	}
	titles := batchCtx.SlugTitleMany(ctx, slugList)
	live := make(map[string]struct{}, len(titles))
	titleToSlug := make(map[string]string, len(titles))
	for slug, title := range titles {
		live[slug] = struct{}{}
		if title != "" {
			titleToSlug[title] = slug
		}
	}
	return live, titleToSlug
}

// stripDeadWikiLinks rewrites `[[slug]]` / `[[slug|display]]` references
// whose `slug` falls into the dead set. The handling depends on whether
// the dead slug can be repaired:
//
//   - If the resolver maps the dead slug to a live one (typically via
//     display-text reverse lookup or hyphen-normalized equality —
//     see resolveDeadSlug), the link is REWRITTEN with the corrected
//     slug. Display text is preserved.
//   - If no live candidate is close enough, the link is STRIPPED to
//     plain text (display text when present; otherwise a humanized
//     last-segment of the slug). This is the original behaviour.
//
// The resolver is optional: when liveSlugs / titleToSlug are nil or
// empty, every dead slug falls through to the strip path. This keeps
// backward compatibility for tests / call sites that don't yet wire
// the resolution data.
func stripDeadWikiLinks(
	content string,
	deadSlugs map[string]struct{},
	liveSlugs map[string]struct{},
	titleToSlug map[string]string,
) (string, bool) {
	if len(deadSlugs) == 0 || content == "" {
		return content, false
	}
	changed := false
	out := wikiLinkRE.ReplaceAllStringFunc(content, func(match string) string {
		sub := wikiLinkRE.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		slug := sub[1]
		if _, dead := deadSlugs[slug]; !dead {
			return match
		}
		display := ""
		if len(sub) >= 3 {
			display = strings.TrimSpace(sub[2])
		}

		// (1) Try fuzzy resolve before falling back to strip. The
		// resolver consults display-text reverse lookup, hyphen-
		// normalized equality, and bigram similarity in that order;
		// returns "" only when no candidate is safe.
		if resolved, ok := resolveDeadSlug(slug, display, liveSlugs, titleToSlug); ok && resolved != slug {
			changed = true
			if display != "" {
				return "[[" + resolved + "|" + display + "]]"
			}
			return "[[" + resolved + "]]"
		}

		// (2) Strip — best-effort plain text. Prefer the LLM-supplied
		// display text; otherwise humanize the slug's last path segment
		// so the prose stays readable.
		changed = true
		if display != "" {
			return display
		}
		parts := strings.Split(slug, "/")
		label := parts[len(parts)-1]
		label = strings.ReplaceAll(label, "-", " ")
		return label
	})
	return out, changed
}

// cleanDeadLinks rewrites `[[slug]]` references in the batch's affected
// pages whose targets no longer exist (or were archived). Pure text
// cleanup — no LLM call.
//
// Scope is intentionally limited to the slugs touched by this batch:
// at 4w-document scale the legacy "scan every page in the KB" path was
// the dominant tail in the post-batch phase, and the long-tail
// historical dead links are better handled by the lint AutoFix pipeline
// (which runs out-of-band and can afford a full table walk).
//
// For each affected page:
//
//  1. Pull its lite projection (out_links + status) via the batch's
//     SlugTitle fetcher (one IN query for the whole affected set,
//     amortized via the batchCtx cache).
//  2. Probe the union of out-link targets through ExistsSlugs to
//     classify them as live vs dead.
//  3. For each dead link, try resolveDeadSlug first; rewrite if a
//     safe candidate exists, otherwise strip to plain text.
//  4. Persist the rewritten content via UpdateAutoLinkedContent so
//     the version counter stays unchanged (this is a maintenance
//     pass, not a user-visible edit).
func (s *wikiIngestService) cleanDeadLinks(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	affectedSlugs []string,
	updatesBySlug map[string][]SlugUpdate,
	batchCtx *WikiBatchContext,
) map[string]error {
	failures := make(map[string]error)
	if len(affectedSlugs) == 0 {
		return failures
	}

	// (1) Load the affected pages' content + out-links in one go.
	// We need the full WikiPage rows here (not just lite projections)
	// because we're going to rewrite content; the lite path saves
	// nothing once we're touching content anyway.
	cleaned := 0
	for _, slug := range affectedSlugs {
		page, err := s.wikiService.GetPageBySlug(ctx, kbID, slug)
		if err != nil || page == nil {
			continue
		}
		if page.Status == types.WikiPageStatusArchived {
			continue
		}
		if page.PageType == types.WikiPageTypeIndex || page.PageType == types.WikiPageTypeLog {
			continue
		}
		if len(page.OutLinks) == 0 {
			continue
		}

		// (2) Classify out-links as live vs dead via one batched
		// ExistsSlugs query. Empty slug list → no-op.
		liveMap, err := s.wikiService.ExistsSlugs(ctx, kbID, []string(page.OutLinks))
		if err != nil {
			logger.Warnf(ctx, "wiki: ExistsSlugs failed during dead-link cleanup for %s: %v", slug, err)
			continue
		}
		deadSlugs := make(map[string]struct{})
		liveSlugs := make(map[string]struct{}, len(liveMap))
		for outSlug, alive := range liveMap {
			if alive {
				liveSlugs[outSlug] = struct{}{}
			} else {
				deadSlugs[outSlug] = struct{}{}
			}
		}
		if len(deadSlugs) == 0 {
			continue
		}

		// (3) Build the title->slug reverse-lookup map for fuzzy
		// resolve. We pull titles for the live slugs only — those
		// are the candidates a dead reference could be remapped to.
		titles := batchCtx.SlugTitleMany(ctx, []string(page.OutLinks))
		titleToSlug := make(map[string]string, len(titles))
		for s, t := range titles {
			if t != "" {
				titleToSlug[t] = s
			}
		}

		newContent, changed := stripDeadWikiLinks(page.Content, deadSlugs, liveSlugs, titleToSlug)
		if !changed {
			continue
		}

		// (4) Persist. UpdateAutoLinkedContent skips the version bump
		// because dead-link cleanup is a machine-only edit.
		updates, fenceErr := s.requireAllLiveUpdates(ctx, tenantID, kbID, updatesBySlug[slug])
		if fenceErr != nil {
			failures[slug] = fmt.Errorf("revalidate dead-link cleanup: %w", fenceErr)
			continue
		}
		page.Content = newContent
		writeCtx := wikiIngestValidationContextForUpdates(ctx, tenantID, kbID, updates)
		if err := s.wikiService.UpdateAutoLinkedContent(writeCtx, page); err != nil {
			logger.Warnf(ctx, "wiki: failed to clean dead links in page %s: %v", page.Slug, err)
			failures[slug] = err
			continue
		}
		cleaned++
	}

	if cleaned > 0 {
		logger.Infof(ctx, "wiki: cleaned dead links in %d pages", cleaned)
	}
	return failures
}

// injectCrossLinks scans the batch's affected pages and injects
// `[[wiki-links]]` for mentions of other wiki page titles / aliases
// in the content. Pure text replacement, no LLM call.
//
// Scope is intentionally limited to two slug sets:
//
//  1. The affected pages themselves — we only rewrite their content.
//  2. The candidate refs come from (a) the affected pages' existing
//     out-links (already known to be relevant via prior linkification
//     or manual edits) plus (b) the batch's freshly-written sibling
//     slugs supplied via `linkRefs` from the caller.
//
// At 4w-document scale this is the difference between loading 100K+
// pages just to find link candidates vs O(batch-size) lookups. We
// trade off some long-tail recall (a brand new entity in this batch
// won't be linkified into pages from previous batches until they get
// re-edited), but lint AutoFix is the right place for that.
//
// linkifyContent does the actual matching work, including code-block /
// existing-link / word-boundary exclusions.
func (s *wikiIngestService) injectCrossLinks(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	affectedSlugs []string,
	freshRefs []linkRef,
	updatesBySlug map[string][]SlugUpdate,
	batchCtx *WikiBatchContext,
) map[string]error {
	failures := make(map[string]error)
	if len(affectedSlugs) == 0 {
		return failures
	}

	updated := 0
	for _, slug := range affectedSlugs {
		page, err := s.wikiService.GetPageBySlug(ctx, kbID, slug)
		if err != nil || page == nil {
			continue
		}
		if page.PageType == types.WikiPageTypeIndex || page.PageType == types.WikiPageTypeLog {
			continue
		}

		// Build the per-page candidate ref set: the existing out-links
		// (resolved via the batch's title fetcher to skip archived /
		// system pages) plus the freshly-written sibling slugs from
		// this batch.
		var refs []linkRef
		if len(page.OutLinks) > 0 {
			titles := batchCtx.SlugTitleMany(ctx, []string(page.OutLinks))
			for outSlug, title := range titles {
				if title == "" || outSlug == slug {
					continue
				}
				refs = append(refs, linkRef{slug: outSlug, matchText: title})
			}
		}
		for _, fr := range freshRefs {
			if fr.slug == slug {
				continue
			}
			refs = append(refs, fr)
		}
		if len(refs) == 0 {
			continue
		}

		newContent, changed := linkifyContent(page.Content, refs, page.Slug)
		if !changed {
			continue
		}
		updates, fenceErr := s.requireAllLiveUpdates(ctx, tenantID, kbID, updatesBySlug[slug])
		if fenceErr != nil {
			failures[slug] = fmt.Errorf("revalidate cross-link injection: %w", fenceErr)
			continue
		}
		page.Content = newContent
		writeCtx := wikiIngestValidationContextForUpdates(ctx, tenantID, kbID, updates)
		if err := s.wikiService.UpdateAutoLinkedContent(writeCtx, page); err != nil {
			logger.Warnf(ctx, "wiki ingest: cross-link injection failed for %s: %v", page.Slug, err)
			failures[slug] = err
			continue
		}
		updated++
	}

	if updated > 0 {
		logger.Infof(ctx, "wiki ingest: injected cross-links in %d pages", updated)
	}
	return failures
}

// collectLinkRefs flattens (title + aliases) of all non-system pages into a
// single linkRef slice suitable for linkifyContent.
func collectLinkRefs(pages []*types.WikiPage) []linkRef {
	refs := make([]linkRef, 0, len(pages)*2)
	for _, p := range pages {
		if p.PageType == types.WikiPageTypeIndex || p.PageType == types.WikiPageTypeLog {
			continue
		}
		if p.Title != "" {
			refs = append(refs, linkRef{slug: p.Slug, matchText: p.Title})
		}
		for _, alias := range p.Aliases {
			if alias != "" {
				refs = append(refs, linkRef{slug: p.Slug, matchText: alias})
			}
		}
	}
	return refs
}

// wikiTaxonomyPromptMaxPaths caps how many existing folders are rendered into a
// planning prompt as the set to reuse. Reached only for pathologically large
// taxonomies; the similarity preprocessing keeps the fed set well under it.
const wikiTaxonomyPromptMaxPaths = 150

// wikiTaxonomyFolderPoolMax bounds the existing folders pulled from the DB as the
// candidate pool for similarity selection. Distinct folders are few even for
// large KBs, so this only guards against a degenerate taxonomy.
const wikiTaxonomyFolderPoolMax = 400

// wikiTaxonomyFeedAllMaxFolders is the folder count at or below which the whole
// folder set is fed to the planner as-is: a healthy navigation directory is
// small, so feeding everything gives perfect reuse recall with no embedding cost
// (similarity preprocessing only earns its keep once folders are numerous).
const wikiTaxonomyFeedAllMaxFolders = 60

// wikiTaxonomyRelevantTopK is how many nearest existing deeper folders each item
// contributes to the reuse set when similarity preprocessing kicks in.
const wikiTaxonomyRelevantTopK = 3

// wikiTaxonomyPlanChunkSize caps how many items go into a single planning call.
// Larger batches are split into chunks; folders assigned by earlier chunks are
// fed forward as "existing folders" so later chunks converge onto the same tree.
const wikiTaxonomyPlanChunkSize = 60

const wikiTaxonomyEmptyTreeHint = "(none yet — this knowledge base has no folders, design a fresh directory)"

type wikiTaxonomyNode struct {
	children map[string]*wikiTaxonomyNode
}

func insertWikiTaxonomyPath(root *wikiTaxonomyNode, path []string) {
	if root == nil || len(path) == 0 {
		return
	}
	cur := root
	for _, part := range path {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if cur.children == nil {
			cur.children = make(map[string]*wikiTaxonomyNode)
		}
		child := cur.children[part]
		if child == nil {
			child = &wikiTaxonomyNode{}
			cur.children[part] = child
		}
		cur = child
	}
}

func appendWikiTaxonomyNode(buf *strings.Builder, label string, node *wikiTaxonomyNode, depth int) {
	if label != "" {
		fmt.Fprintf(buf, "%s%s\n", strings.Repeat("  ", depth), label)
	}
	if node == nil || len(node.children) == 0 {
		return
	}
	keys := make([]string, 0, len(node.children))
	for k := range node.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		appendWikiTaxonomyNode(buf, k, node.children[k], depth+1)
	}
}

// formatExistingTaxonomyForPrompt renders distinct category_path values as an
// indented folder tree for LLM extraction prompts.
func formatExistingTaxonomyForPrompt(paths [][]string) string {
	if len(paths) == 0 {
		return ""
	}
	root := &wikiTaxonomyNode{}
	for _, path := range paths {
		insertWikiTaxonomyPath(root, path)
	}
	if len(root.children) == 0 {
		return ""
	}
	var buf strings.Builder
	keys := make([]string, 0, len(root.children))
	for k := range root.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		appendWikiTaxonomyNode(&buf, k, root.children[k], 0)
	}
	return strings.TrimSpace(buf.String())
}

// getExistingPageSlugsForKnowledge returns all page slugs that currently
// reference a given knowledge ID in their source_refs. Used to snapshot
// state before re-ingest so the reduce phase can reconcile additions vs
// retractions.
//
// Backed by idx_wiki_pages_source_refs (GIN jsonb_path_ops, migration
// 000041) and the legacy text-index fallback for "kid|title" entries.
// We project to slugs only — no need to load full row content for a
// per-doc snapshot.
//
// Index/log slugs (wiki-intrinsic system pages) never carry real
// source_refs in practice, but we filter them out explicitly here as
// a defense-in-depth measure: an old buggy ingest that mistakenly
// stamped a system page with a knowledge ref would otherwise show up
// in the reparse "old set" and confuse the reduce stage.
func (s *wikiIngestService) getExistingPageSlugsForKnowledge(ctx context.Context, kbID, knowledgeID string) (map[string]bool, error) {
	slugs, err := s.wikiService.ListSlugsBySourceRef(ctx, kbID, knowledgeID)
	if err != nil {
		return nil, fmt.Errorf("wiki ingest: list existing page slugs for knowledge %s: %w", knowledgeID, err)
	}
	if len(slugs) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(slugs))
	for _, slug := range slugs {
		// Defense-in-depth: skip wiki-intrinsic slugs that never have
		// real source refs.
		if slug == "index" || slug == "log" {
			continue
		}
		out[slug] = true
	}
	return out, nil
}

// getExistingPageProvenanceForKnowledge is the reparse-specific "before"
// snapshot. Unlike ListPagesBySourceRef it never loads content; unlike the
// slug-only lookup it retains old chunk citations so Reduce can replace this
// document's exact contribution instead of accumulating stale generation IDs.
func (s *wikiIngestService) getExistingPageProvenanceForKnowledge(
	ctx context.Context,
	kbID string,
	knowledgeID string,
) (map[string]bool, map[string]types.StringArray, error) {
	rows, err := s.wikiService.ListSourceProvenanceBySourceRef(ctx, kbID, knowledgeID)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"wiki ingest: list existing page provenance for knowledge %s: %w",
			knowledgeID, err,
		)
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	slugs := make(map[string]bool, len(rows))
	chunkRefs := make(map[string]types.StringArray, len(rows))
	for _, row := range rows {
		if row.Slug == "" || row.Slug == "index" || row.Slug == "log" {
			continue
		}
		slugs[row.Slug] = true
		if len(row.ChunkRefs) > 0 {
			chunkRefs[row.Slug] = append(types.StringArray(nil), row.ChunkRefs...)
		}
	}
	return slugs, chunkRefs, nil
}

func intersectWikiChunkRefsBySlug(
	pageChunkRefs map[string]types.StringArray,
	ownerChunkIDs []string,
) map[string][]string {
	if len(pageChunkRefs) == 0 || len(ownerChunkIDs) == 0 {
		return nil
	}
	owned := make(map[string]struct{}, len(ownerChunkIDs))
	for _, id := range ownerChunkIDs {
		if id != "" {
			owned[id] = struct{}{}
		}
	}
	out := make(map[string][]string, len(pageChunkRefs))
	for slug, refs := range pageChunkRefs {
		seen := make(map[string]struct{}, len(refs))
		for _, id := range refs {
			if _, ok := owned[id]; !ok || id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			out[slug] = append(out[slug], id)
		}
	}
	return out
}

// retractStalePages handles pages that were previously linked to this document
// but are no longer produced by the updated extraction.
// - Single-source stale pages → deleted
// - Multi-source stale pages → LLM retract to clean content synchronously

// Build set of newly affected slugs (including summary)

// Stale = was in old set but not in new set

// Remove this doc's source ref

// No other sources → delete the page

// Multi-source → remove ref, queue retract

// extractedItem represents a single extracted entity or concept.
//
// SourceChunks holds the stable chunk IDs (from the source document) that
// substantively discuss this item. Populated by the chunk-citation pass; when
// non-empty the Reduce phase uses these chunks verbatim as the item's
// evidence instead of the shorter Description/Details fields.
type extractedItem struct {
	Name         string   `json:"name"`
	Slug         string   `json:"slug"`
	Aliases      []string `json:"aliases"`
	Description  string   `json:"description"`
	Details      string   `json:"details"`
	SourceChunks []string `json:"source_chunks,omitempty"`
}

// combinedExtraction represents the parsed result of the combined entity+concept extraction
type combinedExtraction struct {
	Entities []extractedItem `json:"entities"`
	Concepts []extractedItem `json:"concepts"`
}

// rebuildIndexPage refreshes the LLM-generated intro that sits on the
// index wiki_pages row.
//
// History: the index page used to store "intro + full directory listing" as
// a single multi-MB markdown blob in content. Every ingest batch rewrote
// the whole column, which on KBs with tens of thousands of pages caused
// O(N) TOAST writes per batch. The directory was lifted out into the
// structured GET /wiki/index endpoint (see wikiPageService.GetIndexView),
// and this method now only maintains the intro.
//
// Intro lifecycle:
//   - First time (empty or legacy placeholder): generate from all document
//     summaries via WikiIndexIntroPrompt.
//   - Subsequent calls with a change description: incremental update via
//     WikiIndexIntroUpdatePrompt so the intro reflects what just landed.
//   - No change description: keep the existing intro untouched.
//
// The new intro is written to both Content and Summary so readers that
// still fall back to Summary (older clients, legacy migrations) stay in
// sync with the column the view actually renders.
// indexIntroSummaryCap caps how many summary pages we feed into the
// LLM when generating the wiki index intro from scratch. A 4w-document
// KB would otherwise blow the context window every batch, and the
// intro is a "set the scene" artifact where the most-recently-touched
// documents carry disproportionately more signal anyway. We pick the
// top-N most-recently-updated summaries and add a "showing N of M"
// hint to the prompt so the LLM can be honest about its sample.
const indexIntroSummaryCap = 200

type wikiIndexChange struct {
	SourceOpID           int64
	KnowledgeID          string
	ProcessingGeneration string
	Description          string
	// Retract distinguishes a safety-critical removal from a best-effort
	// ingest intro refresh. An incremental intro that still contains deleted
	// source prose must never be acknowledged after an LLM failure.
	Retract bool
}

func (s *wikiIngestService) requireAllLiveIndexChanges(
	ctx context.Context,
	payload WikiIngestPayload,
	changes []wikiIndexChange,
) ([]wikiIndexChange, []wikiingestguard.Identity, error) {
	identities := make([]wikiingestguard.Identity, 0, len(changes))
	stale := make([]wikiingestguard.Identity, 0)
	for _, change := range changes {
		if strings.TrimSpace(change.ProcessingGeneration) == "" {
			continue
		}
		identity := wikiIngestIdentity(
			payload.TenantID, payload.KnowledgeBaseID,
			change.KnowledgeID, change.ProcessingGeneration,
		)
		isStale, err := s.isWikiIngestGenerationStale(ctx, payload.TenantID, payload.KnowledgeBaseID, WikiPendingOp{
			KnowledgeID:          change.KnowledgeID,
			ProcessingGeneration: change.ProcessingGeneration,
		})
		if err != nil {
			return nil, nil, err
		}
		if isStale {
			stale = append(stale, identity)
			continue
		}
		identities = append(identities, identity)
	}
	if len(stale) > 0 {
		return nil, nil, wikiingestguard.NewStaleIdentityError(stale...)
	}
	return changes, identities, nil
}

// rebuildIndexPage refreshes the LLM-generated intro on the index
// page. Two paths:
//
//   - First-time generation (no existing intro, or only the legacy
//     placeholder): the LLM gets a CAPPED window of the most recent
//     summary pages (most-recently-updated wins). Compare with the
//     legacy path which loaded ALL summaries — at 4w-document scale
//     that produced multi-MB prompts that simply broke the context
//     window and silently fell back to a hardcoded intro.
//   - Incremental update: the LLM gets only the existing intro plus
//     the change description for THIS batch. Document summaries are
//     intentionally NOT included — at scale the change-description
//     alone is enough signal for "what landed?", and excluding the
//     full summary set keeps the prompt size bounded regardless of
//     KB size.
//
// The intro is written to both Content and Summary so legacy readers
// that fall through to Summary stay in sync.
func (s *wikiIngestService) rebuildIndexPage(
	ctx context.Context,
	chatModel chat.Chat,
	payload WikiIngestPayload,
	changes []wikiIndexChange,
	lang string,
) error {
	changes, activeIdentities, err := s.requireAllLiveIndexChanges(ctx, payload, changes)
	if err != nil {
		return fmt.Errorf("revalidate wiki index changes before load: %w", err)
	}
	guardedCtx := wikiingestguard.WithValidation(ctx, activeIdentities...)
	indexPage, err := s.wikiService.GetIndex(guardedCtx, payload.KnowledgeBaseID)
	if err != nil {
		return fmt.Errorf("load wiki index page: %w", err)
	}
	if indexPage == nil {
		return errors.New("load wiki index page: service returned nil page")
	}

	var changeDesc strings.Builder
	appliedOpIDs := make([]int64, 0, len(changes))
	appliedKnowledgeIDs := make([]string, 0, len(changes))
	hasAppliedRetract := false
	for _, change := range changes {
		alreadyApplied, markerErr := wikidelete.IsApplied(indexPage, change.SourceOpID)
		if markerErr != nil {
			return fmt.Errorf("read wiki index idempotency marker: %w", markerErr)
		}
		if alreadyApplied {
			continue
		}
		changeDesc.WriteString(change.Description)
		appliedKnowledgeIDs = append(appliedKnowledgeIDs, change.KnowledgeID)
		hasAppliedRetract = hasAppliedRetract || change.Retract
		if change.SourceOpID > 0 {
			appliedOpIDs = append(appliedOpIDs, change.SourceOpID)
		}
	}
	if changeDesc.Len() == 0 {
		return nil
	}

	// The intro lives on both Content and Summary. Prefer Content since
	// that's what the new index view returns; fall back to Summary for
	// rows written before this refactor so the incremental-update prompt
	// has something to work with.
	existingIntro := strings.TrimSpace(indexPage.Content)
	if existingIntro == "" {
		existingIntro = strings.TrimSpace(indexPage.Summary)
	}
	// Detect the legacy "intro + directory" payload. Such rows embed the
	// fence-separated "## Summary" sections right after the intro, so we
	// clip everything from the first directory heading onward to keep the
	// intro length bounded when we feed it back into the update prompt.
	if idx := strings.Index(existingIntro, "\n## "); idx >= 0 {
		existingIntro = strings.TrimSpace(existingIntro[:idx])
	}

	var intro string
	switch {
	case existingIntro == "" || existingIntro == "Wiki index - table of contents":
		// First-time generation: pull the top-N most-recent summary
		// pages via the lite projection. CountByType lets us tell the
		// LLM "showing N of M" so it can frame the intro honestly when
		// the KB is bigger than what we're sampling.
		recentSummaries, listErr := s.wikiService.ListByTypeRecent(ctx, payload.KnowledgeBaseID, types.WikiPageTypeSummary, indexIntroSummaryCap)
		if listErr != nil {
			return listErr
		}
		var docSummaries strings.Builder
		for _, e := range recentSummaries {
			fmt.Fprintf(&docSummaries, "<document>\n<title>%s</title>\n<summary>%s</summary>\n</document>\n\n", e.Title, e.Summary)
		}
		// Best-effort total count for the framing hint. CountByType
		// counts every page type; we need just summary, so we read
		// directly. A failure here doesn't block intro generation.
		totalSummaries := int64(len(recentSummaries))
		if counts, cntErr := s.wikiService.CountByType(ctx, payload.KnowledgeBaseID); cntErr == nil {
			if t, ok := counts[types.WikiPageTypeSummary]; ok {
				totalSummaries = t
			}
		}
		framing := ""
		if int(totalSummaries) > len(recentSummaries) && len(recentSummaries) > 0 {
			framing = fmt.Sprintf("(showing %d most recent of %d total documents)\n\n", len(recentSummaries), totalSummaries)
		}
		if docSummaries.Len() == 0 {
			docSummaries.WriteString("(no documents yet)")
		}
		generatedIntro, genErr := s.generateWithTemplate(ctx, chatModel, agent.WikiIndexIntroPrompt, map[string]string{
			"DocumentSummaries": framing + docSummaries.String(),
			"Language":          lang,
		})
		if genErr != nil {
			logger.Warnf(ctx, "wiki ingest: index intro generation degraded to static fallback for KB %s: %v",
				payload.KnowledgeBaseID, genErr)
			intro = "# Wiki Index\n\nThis wiki contains knowledge extracted from uploaded documents.\n"
		} else {
			intro = strings.TrimSpace(generatedIntro)
		}
	case changeDesc.Len() > 0:
		// Incremental update: only the existing intro + this batch's
		// change description go into the prompt. We deliberately stop
		// passing the full DocumentSummaries set here — at 4w docs it
		// would re-flood the context every batch, and the
		// change-description block already encodes the "what just
		// changed" signal the prompt is asking for.
		updatedIntro, genErr := s.generateWithTemplate(ctx, chatModel, agent.WikiIndexIntroUpdatePrompt, map[string]string{
			"ExistingIntro":     existingIntro,
			"ChangeDescription": changeDesc.String(),
			"DocumentSummaries": "",
			"Language":          lang,
		})
		if genErr != nil {
			if hasAppliedRetract {
				// Keeping the old intro is not a safe degradation for deletion:
				// it may still mention the removed document. Return the error before
				// MarkApplied/Complete so the durable retract remains pending and
				// the index quarantine continues hiding stale prose.
				return fmt.Errorf("update wiki index intro for retract: %w", genErr)
			}
			logger.Warnf(ctx, "wiki ingest: incremental index intro update degraded to existing content for KB %s: %v",
				payload.KnowledgeBaseID, genErr)
			intro = existingIntro // keep existing on error
		} else {
			intro = strings.TrimSpace(updatedIntro)
		}
	default:
		// No change description and an existing intro: leave it as-is so
		// we don't bump the version for a no-op.
		intro = existingIntro
	}

	// Defensive: some LLM outputs occasionally bleed into a directory-
	// like section even when the intro prompt doesn't ask for one. If
	// the freshly-generated intro starts to look like a legacy payload,
	// clip it at the first "\n## " just like we did on the read path
	// above. This keeps indexPage.Content a bounded intro-only blob.
	if idx := strings.Index(intro, "\n## "); idx >= 0 {
		intro = strings.TrimSpace(intro[:idx])
	}
	_, activeIdentities, err = s.requireAllLiveIndexChanges(ctx, payload, changes)
	if err != nil {
		return fmt.Errorf("revalidate wiki index changes before write: %w", err)
	}

	indexPage.Content = intro
	indexPage.Summary = intro
	if err := wikidelete.MarkApplied(indexPage, appliedOpIDs...); err != nil {
		return fmt.Errorf("mark wiki index changes applied: %w", err)
	}
	if err := wikidelete.Complete(indexPage, appliedKnowledgeIDs...); err != nil {
		return fmt.Errorf("complete wiki index quarantine: %w", err)
	}
	writeCtx := wikidelete.WithQuarantineClear(ctx, appliedKnowledgeIDs...)
	writeCtx = wikiingestguard.WithValidation(writeCtx, activeIdentities...)
	_, err = s.wikiService.UpdatePage(writeCtx, indexPage)
	return err
}

// splitSummaryLine extracts the "SUMMARY: ..." line from LLM output.
// Returns (summary, content). If no SUMMARY line found, summary is empty.
func splitSummaryLine(raw string) (summary string, content string) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "SUMMARY:") || strings.HasPrefix(raw, "SUMMARY：") {
		idx := strings.IndexByte(raw, '\n')
		if idx < 0 {
			// Only one line
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(raw, "SUMMARY:"), "SUMMARY：")), ""
		}
		summaryLine := raw[:idx]
		summaryLine = strings.TrimPrefix(summaryLine, "SUMMARY:")
		summaryLine = strings.TrimPrefix(summaryLine, "SUMMARY：")
		return strings.TrimSpace(summaryLine), strings.TrimSpace(raw[idx+1:])
	}
	return "", raw
}

// buildLogEntry builds a WikiLogEntry struct for the current batch. It is
// pure (no DB access) so callers can accumulate entries cheaply under their
// lock and flush them in a single AppendBatch call at the end of the batch.
//
// Historically this was a per-event `GetLog + UpdatePage` round trip, which
// rewrote the entire log page's TEXT column on every ingest/retract op —
// O(n^2) write amplification as the log grew. The batch writer now uses
// wikiLogEntryService.AppendBatch instead; see ProcessWikiIngest.
func (s *wikiIngestService) buildLogEntry(tenantID uint64, kbID, action, knowledgeID, docTitle, summary string, pagesAffected []types.WikiLogPageRef, pendingOpID int64) *types.WikiLogEntry {
	// Copy pagesAffected so the entry does not alias caller-owned slices.
	// The batch accumulates SlugUpdate results that may be reused downstream.
	var pages types.WikiLogPageRefs
	if len(pagesAffected) > 0 {
		pages = make(types.WikiLogPageRefs, len(pagesAffected))
		copy(pages, pagesAffected)
	}
	var sourceOpID *int64
	if pendingOpID > 0 {
		id := pendingOpID
		sourceOpID = &id
	}
	return &types.WikiLogEntry{
		TenantID:        tenantID,
		KnowledgeBaseID: kbID,
		SourceOpID:      sourceOpID,
		Action:          action,
		KnowledgeID:     knowledgeID,
		DocTitle:        docTitle,
		Summary:         summary,
		PagesAffected:   pages,
		CreatedAt:       time.Now(),
	}
}

// publishDraftPages transitions draft pages to published status after ingest
// completes and returns per-slug failures. Publication is part of successful
// materialization: callers must keep every contributing pending op when a
// page cannot be read or published.
func (s *wikiIngestService) publishDraftPages(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	slugs []string,
	updatesBySlug map[string][]SlugUpdate,
) map[string]error {
	failures := make(map[string]error)
	for _, slug := range slugs {
		updates, err := s.requireAllLiveUpdates(ctx, tenantID, kbID, updatesBySlug[slug])
		if err != nil {
			failures[slug] = fmt.Errorf("revalidate page for publication: %w", err)
			continue
		}
		page, err := s.wikiService.GetPageBySlug(ctx, kbID, slug)
		if err != nil {
			// A retract-only reduce may intentionally delete the final-owner page.
			// The slug still participates in the publication barrier so its owning
			// pending operation is settled only after all downstream work completes.
			// In that desired end state there is no draft page left to publish, and
			// treating the authoritative not-found as a retryable publication error
			// makes a successful delete look like a failed Wiki task.
			if errors.Is(err, repository.ErrWikiPageNotFound) && wikiUpdatesAreRetractOnly(updates) {
				logger.Infof(ctx, "wiki ingest: page %s already absent after retract; publication settled", slug)
				continue
			}
			failures[slug] = fmt.Errorf("get page for publication: %w", err)
			continue
		}
		if page == nil {
			failures[slug] = errors.New("get page for publication: service returned nil without error")
			continue
		}
		if page.Status == types.WikiPageStatusDraft {
			if _, err := s.requireAllLiveUpdates(ctx, tenantID, kbID, updates); err != nil {
				failures[slug] = fmt.Errorf("revalidate page before publication: %w", err)
				continue
			}
			page.Status = types.WikiPageStatusPublished
			publishCtx := wikiIngestValidationContextForUpdates(ctx, tenantID, kbID, updates)
			if err := s.wikiService.UpdatePageMeta(publishCtx, page); err != nil {
				logger.Warnf(ctx, "wiki ingest: failed to publish page %s: %v", slug, err)
				failures[slug] = fmt.Errorf("publish page: %w", err)
			}
		}
	}
	return failures
}

func wikiUpdatesAreRetractOnly(updates []SlugUpdate) bool {
	if len(updates) == 0 {
		return false
	}
	for _, update := range updates {
		if update.Type != "retract" && update.Type != "retractStale" {
			return false
		}
	}
	return true
}

// writeDedupItemXML renders a single entity/concept entry as a structured XML
// block for the deduplication prompt. Structured form (versus a single
// pipe-separated line) helps the LLM reliably tell name / aliases / type apart
// and reduces nonsensical merges like "居民身份证" → "工作居住证".
func writeDedupItemXML(buf *strings.Builder, slug, name, itemType string, aliases []string) {
	fmt.Fprintf(buf, "  <item slug=%q type=%q>\n", slug, itemType)
	fmt.Fprintf(buf, "    <name>%s</name>\n", xmlEscape(name))
	for _, alias := range aliases {
		if alias == "" {
			continue
		}
		fmt.Fprintf(buf, "    <alias>%s</alias>\n", xmlEscape(alias))
	}
	buf.WriteString("  </item>\n")
}

// xmlEscape escapes the minimal set of characters that can break XML text
// content. Slugs are ASCII-only so they don't need escaping when used as
// attribute values.
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// deduplicateExtractedBatch deduplicates both entities and concepts against
// existing wiki pages in a single LLM call. Uses pre-loaded allPages to avoid
// redundant DB queries. This replaces the two separate deduplicateItems calls
// that each queried ListAllPages + made a separate LLM call.
// deduplicateExtractedBatch deduplicates both entities and concepts against
// existing wiki pages in a single LLM call. Pre-filters candidates via the
// pg_trgm trigram index on lower(title) — every new item issues a
// FindSimilarPages probe and the union of top-K hits across all items is
// the candidate set. This replaces the legacy "ListAllPages + Go-side
// surface-form Jaccard" path that scaled O(P × N) on large KBs.
//
// The KB-id-keyed query relies on idx_wiki_pages_title_trgm (added in
// migration 000041); pg_search environments load pg_trgm in the same
// init step (see migrations/paradedb/00-init-db.sql).
func (s *wikiIngestService) deduplicateExtractedBatch(
	ctx context.Context,
	chatModel chat.Chat,
	kbID string,
	entities, concepts []extractedItem,
) ([]extractedItem, []extractedItem) {
	if len(entities) == 0 && len(concepts) == 0 {
		return entities, concepts
	}
	if s.wikiService == nil {
		return entities, concepts
	}

	// Build the candidate set: for each new item, ask the repo for
	// the top-K trigram-similar pages and union the results. Dedup by
	// slug as we go so the prompt only carries each candidate once.
	candidatePages := make(map[string]*types.WikiPageLite)
	probe := func(item extractedItem) {
		queries := make([]string, 0, 1+len(item.Aliases))
		if item.Name != "" {
			queries = append(queries, item.Name)
		}
		for _, alias := range item.Aliases {
			if alias != "" {
				queries = append(queries, alias)
			}
		}
		for _, q := range queries {
			pages, err := s.wikiService.FindSimilarPages(ctx, kbID, q,
				[]string{types.WikiPageTypeEntity, types.WikiPageTypeConcept},
				dedupCandidateTopK)
			if err != nil {
				logger.Warnf(ctx, "wiki ingest: dedup FindSimilarPages(%q) failed: %v", q, err)
				continue
			}
			for _, p := range pages {
				if p == nil || p.Slug == "" {
					continue
				}
				if _, ok := candidatePages[p.Slug]; !ok {
					candidatePages[p.Slug] = p
				}
			}
		}
	}
	for _, e := range entities {
		probe(e)
	}
	for _, c := range concepts {
		probe(c)
	}
	if len(candidatePages) == 0 {
		// No similar existing pages — nothing to merge against. The
		// items pass through unchanged.
		logger.Infof(ctx, "wiki ingest: no similar existing pages found for %d new items", len(entities)+len(concepts))
		return entities, concepts
	}
	logger.Infof(ctx, "wiki ingest: %d similar existing pages selected for %d new items",
		len(candidatePages), len(entities)+len(concepts))

	var existingBuf strings.Builder
	for _, p := range candidatePages {
		writeDedupItemXML(&existingBuf, p.Slug, p.Title, p.PageType, []string(p.Aliases))
	}
	if existingBuf.Len() == 0 {
		return entities, concepts
	}

	var newBuf strings.Builder
	for _, item := range entities {
		writeDedupItemXML(&newBuf, item.Slug, item.Name, "entity", item.Aliases)
	}
	for _, item := range concepts {
		writeDedupItemXML(&newBuf, item.Slug, item.Name, "concept", item.Aliases)
	}

	dedupeJSON, err := s.generateWithTemplate(ctx, chatModel, agent.WikiDeduplicationPrompt, map[string]string{
		"NewItems":      newBuf.String(),
		"ExistingPages": existingBuf.String(),
	})
	if err != nil {
		logger.Warnf(ctx, "wiki ingest: deduplication LLM call failed: %v", err)
		return entities, concepts
	}

	dedupeJSON = cleanLLMJSON(dedupeJSON)

	var dedupeResult struct {
		Merges map[string]string `json:"merges"`
	}
	if err := json.Unmarshal([]byte(dedupeJSON), &dedupeResult); err != nil {
		logger.Warnf(ctx, "wiki ingest: failed to parse dedup JSON: %v\nRaw: %s", err, dedupeJSON)
		return entities, concepts
	}

	if len(dedupeResult.Merges) == 0 {
		return entities, concepts
	}

	// Build the existing-slug set from the candidate map: anything not
	// in candidates is rejected as an LLM hallucination, since by
	// construction the model only ever saw those slugs as merge
	// targets. Compare with the legacy "look up against allPages"
	// path which had a wider acceptance window.
	existingSlugs := make(map[string]bool, len(candidatePages))
	for slug := range candidatePages {
		existingSlugs[slug] = true
	}

	validMerge := func(srcSlug, dstSlug string) bool {
		if !existingSlugs[dstSlug] {
			logger.Warnf(ctx, "wiki ingest: dedup rejected %s → %s (target slug does not exist in candidate set)", srcSlug, dstSlug)
			return false
		}
		srcSlash := strings.Index(srcSlug, "/")
		dstSlash := strings.Index(dstSlug, "/")
		if srcSlash <= 0 || dstSlash <= 0 {
			// A type-prefixed slug must look like "entity/foo" or
			// "concept/bar". An LLM that emits an un-prefixed slug
			// here is hallucinating; reject rather than fall through
			// the prefix-equality check (which would treat both empty
			// prefixes as a match).
			logger.Warnf(ctx, "wiki ingest: dedup rejected %s → %s (missing type prefix)", srcSlug, dstSlug)
			return false
		}
		srcPrefix := srcSlug[:srcSlash+1]
		dstPrefix := dstSlug[:dstSlash+1]
		if srcPrefix != dstPrefix {
			logger.Warnf(ctx, "wiki ingest: dedup rejected %s → %s (type mismatch: %s vs %s)", srcSlug, dstSlug, srcPrefix, dstPrefix)
			return false
		}
		return true
	}

	for i, item := range entities {
		if existingSlug, ok := dedupeResult.Merges[item.Slug]; ok && validMerge(item.Slug, existingSlug) {
			logger.Infof(ctx, "wiki ingest: dedup merge %s → %s", item.Slug, existingSlug)
			entities[i].Slug = existingSlug
		}
	}
	for i, item := range concepts {
		if existingSlug, ok := dedupeResult.Merges[item.Slug]; ok && validMerge(item.Slug, existingSlug) {
			logger.Infof(ctx, "wiki ingest: dedup merge %s → %s", item.Slug, existingSlug)
			concepts[i].Slug = existingSlug
		}
	}

	return entities, concepts
}

// generateWithTemplate executes a prompt template and calls the LLM with
// bounded exponential-backoff retries for transient infrastructure errors.
//
// Retry policy:
//   - Inline attempts are configurable but default to one. Durable per-op
//     retries provide the larger outer budget and queue fairness.
//   - Only retry errors classified as transient by isTransientLLMError:
//     HTTP 408/429/5xx, context deadline exceeded (when the parent ctx is
//     still alive), or generic "timeout"/"connection reset" wording.
//     4xx (except 408/429) is a caller-side fault and fails fast.
//   - A valid Retry-After takes precedence as a lower bound (never capped
//     downward), with a small positive jitter so workers released by the
//     provider together do not immediately synchronize again.
//   - Without Retry-After, backoff is 15s, 30s, ... plus up to 50% jitter,
//     capped at two minutes. Waiting always honors ctx cancellation.
//
// This exists because wiki ingest makes several independent LLM calls per
// document (extraction, summary, dedup, citations, intro) and a single
// transient 504 from the upstream gateway used to drop the document's
// summary page permanently. Retries plus failedOps requeuing (see
// mapOneDocument) turn those events into at-most-a-few-minute hiccups.
func (s *wikiIngestService) generateWithTemplate(ctx context.Context, chatModel chat.Chat, promptTpl string, data map[string]string) (string, error) {
	tmpl, err := template.New("wiki").Parse(promptTpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	maskedData, urlMap := maskTemplateDataImageURLs(data)

	var buf strings.Builder
	if err := tmpl.Execute(&buf, maskedData); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}

	prompt := buf.String()
	thinking := false
	retryPolicy := wikiqueue.DefaultRetryPolicy()
	if s != nil && s.llmRetryPolicy != nil {
		retryPolicy = *s.llmRetryPolicy
	}
	retryConfig := workretry.ConfigFromEnv()

	var lastErr error
	sawProviderCallFailure := false
	finishFailure := func(err error) error {
		if err == nil || ctx.Err() != nil || !sawProviderCallFailure {
			return err
		}
		return workretry.Consume(err)
	}
	for attempt := 1; attempt <= retryConfig.WikiInlineAttempts; attempt++ {
		callCtx, cancelCall := context.WithTimeout(ctx, retryConfig.WikiCallTimeout)
		response, err := chatModel.Chat(callCtx, []chat.Message{
			{Role: "user", Content: prompt},
		}, &chat.ChatOptions{
			Temperature: 0.3,
			Thinking:    &thinking,
		})
		cancelCall()
		if err == nil {
			return unmaskImageURLs(response.Content, urlMap), nil
		}
		lastErr = err
		if modeladmission.IsProviderCallFailure(err) {
			sawProviderCallFailure = true
		}

		// Abort immediately on non-retryable errors (4xx except 408/429,
		// parse/marshal failures, tool-side bugs, etc.). Retrying a
		// hard "invalid arguments" error just wastes the model's budget.
		if !isTransientLLMError(ctx, err) {
			return "", finishFailure(fmt.Errorf("LLM call failed: %w", err))
		}
		if attempt == retryConfig.WikiInlineAttempts {
			break
		}

		backoff, providerDirected := retryPolicy.Delay(err, attempt)
		backoffSource := "exponential backoff"
		if providerDirected {
			backoffSource = "Retry-After"
		}
		logger.Warnf(ctx, "wiki ingest: LLM call failed (attempt %d/%d), retrying in %s (%s): %v",
			attempt, retryConfig.WikiInlineAttempts, backoff, backoffSource, err)
		if waitErr := retryPolicy.WaitForRetry(ctx, backoff); waitErr != nil {
			return "", fmt.Errorf("LLM call aborted during backoff: %w", waitErr)
		}
	}
	return "", finishFailure(fmt.Errorf(
		"LLM call failed after %d attempts: %w",
		retryConfig.WikiInlineAttempts,
		lastErr,
	))
}

// isTransientLLMError reports whether an error from the chat provider
// looks like an infrastructure hiccup worth retrying. Classification is
// intentionally conservative: the truthful "could not tell, assume
// permanent" choice keeps retries cheap and avoids masking real bugs.
//
// We treat the following as transient:
//   - HTTP 408 (client request timeout — upstream usually didn't process),
//     429 (rate-limited — retry after backoff may succeed), 5xx (any
//     server-side fault, including the 504 "Remote error, timeout with
//     60" we see from the gateway in front of several LLM providers).
//   - Wrapped context.DeadlineExceeded when the parent ctx is still alive
//     (nested per-call timeouts).
//   - Substring matches on the error text for common transport failures
//     ("timeout", "connection reset", "EOF") that providers surface
//     without a structured status code.
func isTransientLLMError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	// Never retry after the parent ctx itself expired — the task is
	// being cancelled and the next attempt would just fail again.
	if ctx.Err() != nil {
		return false
	}

	if status, ok := chat.HTTPStatusCode(err); ok {
		return status == 408 || status == 429 || status >= 500
	}

	msg := err.Error()
	// Providers that bubble HTTP status up formatted as
	// "API request failed with status NNN: ..." — match that first.
	for _, s := range []string{
		"status 408", "status 429",
		"status 500", "status 501", "status 502", "status 503", "status 504",
		"status 520", "status 521", "status 522", "status 523", "status 524",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}

	lower := strings.ToLower(msg)
	for _, s := range []string{
		"timeout",
		"timed out",
		"connection reset",
		"connection refused",
		"broken pipe",
		"no such host", // DNS hiccup
		"i/o timeout",
		"unexpected eof",
		"tls handshake",
		"context deadline exceeded", // nested per-call deadline
	} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// --- Helpers ---

// isKnowledgeGone reports whether the given knowledge has been deleted or is
// in the middle of being deleted. It first consults the Redis tombstone
// (written by cleanupWikiOnKnowledgeDelete) as a fast path, then falls back
// to the DB. Only the repository's explicit not-found sentinel is terminal.
// Treating every lookup error as "gone" used to turn a transient Postgres
// outage into a successful map/filter skip followed by queue acknowledgement.
func (s *wikiIngestService) isKnowledgeGone(ctx context.Context, kbID, knowledgeID string) (bool, error) {
	if knowledgeID == "" {
		return true, nil
	}
	if s.redisClient != nil {
		if exists, err := s.redisClient.Exists(ctx, WikiDeletedTombstoneKey(kbID, knowledgeID)).Result(); err == nil && exists > 0 {
			return true, nil
		} else if err != nil {
			// Redis is only the fast path here. The authoritative DB lookup
			// below can still classify the knowledge safely.
			logger.Warnf(ctx, "wiki ingest: tombstone lookup failed for knowledge %s, falling back to DB: %v", knowledgeID, err)
		}
	}
	if s.knowledgeSvc == nil {
		return false, errors.New("wiki ingest: knowledge service is nil")
	}
	kn, err := s.knowledgeSvc.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil {
		if errors.Is(err, repository.ErrKnowledgeNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("wiki ingest: look up knowledge %s: %w", knowledgeID, err)
	}
	if kn == nil {
		return false, fmt.Errorf("wiki ingest: knowledge service returned nil without error for %s", knowledgeID)
	}
	if kn.KnowledgeBaseID != kbID {
		// A cross-KB move makes every ingest still scoped to the old KB stale.
		// Treat it exactly like deletion so a worker that mapped before the move
		// cannot publish source pages after the authoritative row moved away.
		return true, nil
	}
	switch kn.ParseStatus {
	case types.ParseStatusDeleting, types.ParseStatusCancelled:
		return true, nil
	}
	return false, nil
}

// isWikiIngestGenerationStale validates the complete durable ingest identity.
// A stale/missing/moved/terminal document is a terminal queue no-op; transient
// lookup failures and a same-generation row whose core commit is not yet
// visible are retried instead of being acknowledged as success.
func (s *wikiIngestService) isWikiIngestGenerationStale(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	op WikiPendingOp,
) (bool, error) {
	if tenantID == 0 || strings.TrimSpace(kbID) == "" || strings.TrimSpace(op.KnowledgeID) == "" ||
		strings.TrimSpace(op.ProcessingGeneration) == "" {
		return false, errors.New("wiki ingest: complete tenant/KB/knowledge/generation identity is required")
	}
	if op.tenantID != 0 && op.tenantID != tenantID {
		return false, fmt.Errorf(
			"wiki ingest: pending row tenant mismatch for %s (row=%d trigger=%d)",
			op.KnowledgeID, op.tenantID, tenantID,
		)
	}
	if s.redisClient != nil {
		if exists, err := s.redisClient.Exists(ctx, WikiDeletedTombstoneKey(kbID, op.KnowledgeID)).Result(); err == nil && exists > 0 {
			return true, nil
		} else if err != nil {
			logger.Warnf(ctx, "wiki ingest: generation tombstone lookup failed for knowledge %s, falling back to DB: %v", op.KnowledgeID, err)
		}
	}
	if s.knowledgeSvc == nil {
		return false, errors.New("wiki ingest: knowledge service is nil")
	}
	kn, err := s.knowledgeSvc.GetKnowledgeByIDOnly(ctx, op.KnowledgeID)
	if err != nil {
		if errors.Is(err, repository.ErrKnowledgeNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("wiki ingest: look up generation for knowledge %s: %w", op.KnowledgeID, err)
	}
	if kn == nil {
		return false, fmt.Errorf("wiki ingest: knowledge service returned nil without error for %s", op.KnowledgeID)
	}
	if kn.TenantID != tenantID {
		return false, fmt.Errorf(
			"wiki ingest: knowledge %s tenant mismatch (row=%d trigger=%d)",
			op.KnowledgeID, kn.TenantID, tenantID,
		)
	}
	if kn.KnowledgeBaseID != kbID || kn.ProcessingGeneration != op.ProcessingGeneration {
		return true, nil
	}
	if kn.ProcessedAt == nil {
		return false, fmt.Errorf(
			"wiki ingest: core generation %s for knowledge %s is not committed",
			op.ProcessingGeneration, op.KnowledgeID,
		)
	}
	switch kn.ParseStatus {
	case types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCompleted:
		switch kn.WikiStatus {
		case types.WikiStatusCompleted, types.WikiStatusDegraded, types.WikiStatusFailed:
			return true, nil
		default:
			return false, nil
		}
	case types.ParseStatusPending:
		return false, fmt.Errorf(
			"wiki ingest: committed generation %s unexpectedly returned to pending for knowledge %s",
			op.ProcessingGeneration, op.KnowledgeID,
		)
	default:
		return true, nil
	}
}

func (s *wikiIngestService) preflightWikiPendingOps(
	ctx context.Context,
	payload WikiIngestPayload,
	ops []WikiPendingOp,
) (processable, failed []WikiPendingOp, staleCount int) {
	processable = make([]WikiPendingOp, 0, len(ops))
	failed = make([]WikiPendingOp, 0)
	for _, op := range ops {
		switch {
		case op.Op != WikiOpIngest && op.Op != WikiOpRetract:
			op.lastError = fmt.Sprintf("unsupported wiki pending op %q", op.Op)
			failed = append(failed, op)
		case op.KnowledgeID == "":
			op.lastError = fmt.Sprintf("wiki pending op %q has empty knowledge ID", op.Op)
			failed = append(failed, op)
		case op.tenantID != 0 && op.tenantID != payload.TenantID:
			op.lastError = fmt.Sprintf(
				"wiki pending op tenant mismatch (row=%d trigger=%d)", op.tenantID, payload.TenantID,
			)
			failed = append(failed, op)
		case op.Op == WikiOpIngest && strings.TrimSpace(op.ProcessingGeneration) == "":
			op.lastError = "wiki ingest pending op has empty processing generation"
			failed = append(failed, op)
		case op.Op == WikiOpIngest:
			stale, err := s.isWikiIngestGenerationStale(ctx, payload.TenantID, payload.KnowledgeBaseID, op)
			if err != nil {
				op.lastError = err.Error()
				failed = append(failed, op)
				continue
			}
			if stale {
				staleCount++
				continue
			}
			processable = append(processable, op)
		default:
			processable = append(processable, op)
		}
	}
	return processable, failed, staleCount
}

func (s *wikiIngestService) filterLiveDocIngestResults(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	results []*docIngestResult,
) ([]*docIngestResult, error) {
	live, _, err := s.partitionLiveDocIngestResults(ctx, tenantID, kbID, results)
	return live, err
}

func (s *wikiIngestService) partitionLiveDocIngestResults(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	results []*docIngestResult,
) ([]*docIngestResult, []wikiingestguard.Identity, error) {
	live := make([]*docIngestResult, 0, len(results))
	staleIdentities := make([]wikiingestguard.Identity, 0)
	for _, result := range results {
		if result == nil {
			continue
		}
		isStale, err := s.isWikiIngestGenerationStale(ctx, tenantID, kbID, WikiPendingOp{
			KnowledgeID:          result.KnowledgeID,
			ProcessingGeneration: result.ProcessingGeneration,
		})
		if err != nil {
			return nil, nil, err
		}
		if isStale {
			s.tracker().SkipSpan(ctx, result.WikiSpan, "stale_processing_generation")
			staleIdentities = append(staleIdentities, wikiIngestIdentity(
				tenantID, kbID, result.KnowledgeID, result.ProcessingGeneration,
			))
			continue
		}
		live = append(live, result)
	}
	return live, staleIdentities, nil
}

// prepareWikiRetract validates the durable delete intent against PostgreSQL
// before touching any page. Redis tombstones are deliberately insufficient:
// an ambiguous commit response can leave a retract row even though the source
// transaction rolled back and the document is still active. Active sources
// therefore terminally cancel the stale retract; only deleting/not-found is
// authorized. The chunk-ID snapshot is refreshed with an unscoped read so
// soft-deleted chunks remain removable from wiki page metadata.
func (s *wikiIngestService) prepareWikiRetract(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	op WikiPendingOp,
) (WikiPendingOp, bool, error) {
	if op.KnowledgeID == "" {
		return op, false, errors.New("wiki retract: knowledge ID is required")
	}
	var (
		knowledge *types.Knowledge
		err       error
	)
	if s.knowledgeRepo != nil {
		knowledge, err = s.knowledgeRepo.GetKnowledgeByIDOnly(ctx, op.KnowledgeID)
	} else if s.knowledgeSvc != nil {
		knowledge, err = s.knowledgeSvc.GetKnowledgeByIDOnly(ctx, op.KnowledgeID)
	} else {
		return op, false, errors.New("wiki retract: knowledge repository is nil")
	}
	if err != nil {
		if !errors.Is(err, repository.ErrKnowledgeNotFound) {
			return op, false, fmt.Errorf("wiki retract: load source knowledge %s: %w", op.KnowledgeID, err)
		}
	} else {
		if knowledge == nil {
			return op, false, fmt.Errorf("wiki retract: source lookup returned nil for %s", op.KnowledgeID)
		}
		if tenantID != 0 && knowledge.TenantID != tenantID {
			return op, false, fmt.Errorf(
				"wiki retract: source identity mismatch for %s (tenant=%d kb=%s)",
				op.KnowledgeID, knowledge.TenantID, knowledge.KnowledgeBaseID,
			)
		}
		if op.MoveTargetKnowledgeBaseID != "" {
			// The move coordinator only persists this intent after the final
			// document CAS. If compensation kept/returned the row to the source,
			// cancel without touching pages. Any different current KB authorizes
			// cleanup of this old source, including a later chained move.
			if knowledge.KnowledgeBaseID == kbID {
				return op, false, nil
			}
		} else {
			if knowledge.KnowledgeBaseID != kbID {
				return op, false, fmt.Errorf(
					"wiki retract: source identity mismatch for %s (tenant=%d kb=%s)",
					op.KnowledgeID, knowledge.TenantID, knowledge.KnowledgeBaseID,
				)
			}
			if knowledge.ParseStatus != types.ParseStatusDeleting {
				return op, false, nil
			}
		}
	}

	chunkIDRepo, ok := s.chunkRepo.(unscopedChunkIDRepository)
	if !ok || chunkIDRepo == nil {
		return op, false, errors.New("wiki retract: chunk repository is nil")
	}
	liveAndDeletedChunkIDs, err := chunkIDRepo.ListChunkIDsByKnowledgeIDUnscoped(ctx, tenantID, op.KnowledgeID)
	if err != nil {
		return op, false, fmt.Errorf("wiki retract: list source chunk IDs for %s: %w", op.KnowledgeID, err)
	}
	op.SourceChunks = stableStringUnion(op.SourceChunks, liveAndDeletedChunkIDs)
	return op, true, nil
}

func stableStringUnion(groups ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, group := range groups {
		for _, value := range group {
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

// filterLiveUpdates drops additions/summaries whose source knowledge has been
// deleted since the Map phase finished. Retract updates are preserved so
// pages still get cleaned up. Caches per-knowledge results to avoid DB
// hammering when a single reduce slug carries many updates for the same doc.
func (s *wikiIngestService) filterLiveUpdates(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	updates []SlugUpdate,
) ([]SlugUpdate, error) {
	filtered, _, err := s.partitionLiveUpdates(ctx, tenantID, kbID, updates)
	return filtered, err
}

func (s *wikiIngestService) partitionLiveUpdates(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	updates []SlugUpdate,
) ([]SlugUpdate, []wikiingestguard.Identity, error) {
	if len(updates) == 0 {
		return updates, nil, nil
	}
	type goneResult struct {
		gone bool
		err  error
	}
	goneCache := make(map[string]goneResult)
	isGone := func(kid, generation string) (bool, error) {
		if kid == "" || generation == "" {
			return false, errors.New("wiki ingest update is missing knowledge generation identity")
		}
		key := kid + "\x00" + generation
		if v, ok := goneCache[key]; ok {
			return v.gone, v.err
		}
		gone, err := s.isWikiIngestGenerationStale(ctx, tenantID, kbID, WikiPendingOp{
			KnowledgeID:          kid,
			ProcessingGeneration: generation,
		})
		goneCache[key] = goneResult{gone: gone, err: err}
		return gone, err
	}
	filtered := make([]SlugUpdate, 0, len(updates))
	staleIdentities := make([]wikiingestguard.Identity, 0)
	staleSeen := make(map[string]struct{})
	dropped := 0
	for _, u := range updates {
		switch u.Type {
		case "retract", "retractStale":
			// Delete/move retract operations have no processing generation and
			// remain authoritative even after the source row disappears. A
			// reparse-generated retract carries the ingest generation and must be
			// fenced exactly like an addition.
			if u.ProcessingGeneration == "" {
				filtered = append(filtered, u)
				continue
			}
		}
		gone, err := isGone(u.KnowledgeID, u.ProcessingGeneration)
		if err != nil {
			return nil, nil, fmt.Errorf("wiki ingest: classify source knowledge for slug %s: %w", u.Slug, err)
		}
		if gone {
			dropped++
			key := u.KnowledgeID + "\x00" + u.ProcessingGeneration
			if _, exists := staleSeen[key]; !exists {
				staleSeen[key] = struct{}{}
				staleIdentities = append(staleIdentities, wikiIngestIdentity(
					tenantID, kbID, u.KnowledgeID, u.ProcessingGeneration,
				))
			}
			continue
		}
		filtered = append(filtered, u)
	}
	if dropped > 0 {
		logger.Infof(ctx, "wiki ingest: reduce dropped %d updates for deleted knowledge(s)", dropped)
	}
	return filtered, staleIdentities, nil
}

// requireAllLiveUpdates is used after a slow external call and immediately
// before a side effect. If any contributor advanced while the call was in
// flight, the generated aggregate is no longer valid; abort the whole write so
// live contributors can be recomputed without the stale input on retry.
func (s *wikiIngestService) requireAllLiveUpdates(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	updates []SlugUpdate,
) ([]SlugUpdate, error) {
	live, stale, err := s.partitionLiveUpdates(ctx, tenantID, kbID, updates)
	if err != nil {
		return nil, err
	}
	if len(stale) > 0 {
		return nil, wikiingestguard.NewStaleIdentityError(stale...)
	}
	return live, nil
}

// reconstructContent rebuilds document text from chunks.
//
// This only concatenates text-type chunks — image OCR / caption information is
// stored on image_ocr / image_caption child chunks (see image_multimodal.go),
// not on the parent text chunk's ImageInfo field. Callers that need the full
// enriched content (with OCR / captions inlined) should call
// reconstructEnrichedContent instead so image info is fetched from child
// chunks and embedded alongside Markdown image links.
func reconstructContent(chunks []*types.Chunk) string {
	var textChunks []*types.Chunk
	for _, c := range chunks {
		if c.ChunkType == types.ChunkTypeText || c.ChunkType == "" {
			textChunks = append(textChunks, c)
		}
	}

	// 重叠去重与排序统一交给公共逻辑（按文本匹配，兼容补写表头 / HTML 实体）。
	return searchutil.MergeTextChunks(textChunks, "\n")
}

// reconstructEnrichedContent rebuilds document text and inlines image_info
// (OCR text + caption) pulled from image_ocr / image_caption child chunks.
//
// Without this enrichment, image-heavy documents (e.g. a scanned PDF or a
// standalone .jpg) reach the LLM as bare Markdown image links, causing
// extraction / summarization to produce empty or "no textual content" output.
func reconstructEnrichedContent(
	ctx context.Context,
	chunkRepo interfaces.ChunkRepository,
	tenantID uint64,
	chunks []*types.Chunk,
) string {
	content := reconstructContent(chunks)

	var textChunkIDs []string
	for _, c := range chunks {
		if c.ChunkType == types.ChunkTypeText || c.ChunkType == "" {
			if c.ID != "" {
				textChunkIDs = append(textChunkIDs, c.ID)
			}
		}
	}
	if len(textChunkIDs) == 0 || chunkRepo == nil {
		return content
	}

	imageInfoMap := searchutil.CollectImageInfoByChunkIDs(ctx, chunkRepo, tenantID, textChunkIDs)
	mergedImageInfo := searchutil.MergeImageInfoJSON(imageInfoMap)
	if mergedImageInfo == "" {
		return content
	}
	return searchutil.EnrichContentWithImageInfo(content, mergedImageInfo)
}

// slugify creates a URL-friendly slug from a string
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '/' {
			return r
		}
		if r == ' ' || r == '_' {
			return '-'
		}
		// Keep CJK characters
		if r >= 0x4E00 && r <= 0x9FFF {
			return r
		}
		return -1
	}, s)
	// Collapse multiple hyphens
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// truncateString truncates a string to maxLen runes
func truncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// appendUnique appends a string to a StringArray if not already present
func appendUnique(arr types.StringArray, s string) types.StringArray {
	for _, v := range arr {
		if v == s {
			return arr
		}
	}
	return append(arr, s)
}

// minTextContentRunes is the minimum number of non-whitespace, non-image-reference
// runes required for content to be considered substantive enough for LLM
// summarization or wiki extraction. Documents below this threshold (e.g. a
// scanned PDF where OCR yielded nothing AND no caption either) are routed to
// a deterministic empty-content fallback instead of being passed to the LLM,
// which would otherwise hallucinate based on metadata alone.
//
// The threshold is intentionally low: legitimate short documents (brief
// memos, single-line notes) must still pass. The goal is only to catch
// the empty-image-only case.
//
// Declared as a var (not const) so tests can override it and future config
// plumbing can adjust it at runtime without a rebuild.
var minTextContentRunes = 10

var (
	// Markdown image references like ![alt](path) — pure visual placeholders
	// with no extractable text, so the whole reference is removed.
	mdImageRefRE = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)

	// <image_original>...</image_original> blocks wrap the verbatim Markdown
	// image reference inside an enriched <image> block (see
	// searchutil.EnrichContentWithImageInfo). The content is just a redundant
	// copy of an already-stripped image link, so the whole block (tags +
	// content) is removed.
	imageOriginalBlockRE = regexp.MustCompile(`(?is)<image_original\b[^>]*>.*?</image_original>`)

	// Self-closing or attribute-only HTML <img> tags.
	htmlImgTagRE = regexp.MustCompile(`(?i)<img\b[^>]*/?>`)

	// Wrapper-style <image>, <images>, <image_caption>, <image_ocr> tags
	// (opening or closing). Matches ONLY the tag; the text content between
	// open and close tags is preserved. This is critical: VLM-generated OCR
	// and caption text live inside <image_ocr>...</image_ocr> and
	// <image_caption>...</image_caption> blocks, and stripping the content
	// would silently destroy the very text we want to keep.
	imageWrapperTagRE = regexp.MustCompile(`(?i)</?image[a-z_]*\b[^>]*/?>`)

	// Markdown image references with the URL captured separately so LLM-bound
	// image URLs can be frozen while captions remain editable.
	mdImageURLRE = regexp.MustCompile(`!\[[^\]]*\]\(([^)]*)\)`)

	// Enriched image blocks store the original object URL as an attribute,
	// e.g. <image url="...">. Capture both double- and single-quoted forms.
	imageURLAttrRE = regexp.MustCompile(`(?i)<image\b[^>]*\surl\s*=\s*(?:"([^"]*)"|'([^']*)')`)

	imagePlaceholderTokenRE = regexp.MustCompile(`wkimg:[A-Za-z0-9_-]+`)
)

func maskTemplateDataImageURLs(data map[string]string) (map[string]string, map[string]string) {
	if len(data) == 0 {
		return data, nil
	}

	masked := make(map[string]string, len(data))
	urlToToken := make(map[string]string)
	tokenToURL := make(map[string]string)

	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		masked[key] = maskImageURLsWithState(data[key], urlToToken, tokenToURL)
	}

	return masked, tokenToURL
}

// maskImageURLs replaces image URLs with low-entropy placeholders. It only
// freezes URLs; alt/caption text remains in place for the LLM to edit.
func maskImageURLs(s string) (string, map[string]string) {
	urlToToken := make(map[string]string)
	tokenToURL := make(map[string]string)
	return maskImageURLsWithState(s, urlToToken, tokenToURL), tokenToURL
}

func maskImageURLsWithState(s string, urlToToken, tokenToURL map[string]string) string {
	urls := collectMaskableImageURLs(s)
	if len(urls) == 0 {
		return s
	}

	for _, url := range urls {
		if _, ok := urlToToken[url]; ok {
			continue
		}
		token := fmt.Sprintf("wkimg:%04d", len(tokenToURL)+1)
		urlToToken[url] = token
		tokenToURL[token] = url
	}

	replaceURLs := append([]string(nil), urls...)
	sort.SliceStable(replaceURLs, func(i, j int) bool {
		return len(replaceURLs[i]) > len(replaceURLs[j])
	})

	masked := s
	for _, url := range replaceURLs {
		masked = strings.ReplaceAll(masked, url, urlToToken[url])
	}
	return masked
}

func collectMaskableImageURLs(s string) []string {
	seen := make(map[string]struct{})
	var urls []string

	addURL := func(url string) {
		url = strings.TrimSpace(url)
		if url == "" {
			return
		}
		if _, ok := seen[url]; ok {
			return
		}
		seen[url] = struct{}{}
		urls = append(urls, url)
	}

	for _, match := range mdImageURLRE.FindAllStringSubmatch(s, -1) {
		addURL(match[1])
	}
	for _, match := range imageURLAttrRE.FindAllStringSubmatch(s, -1) {
		if match[1] != "" {
			addURL(match[1])
			continue
		}
		addURL(match[2])
	}

	return urls
}

// unmaskImageURLs restores known placeholders and drops any corrupted or
// invented image placeholders so broken image links never reach storage.
func unmaskImageURLs(out string, urlMap map[string]string) string {
	out = mdImageURLRE.ReplaceAllStringFunc(out, func(match string) string {
		parts := mdImageURLRE.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		url := strings.TrimSpace(parts[1])
		if realURL, ok := urlMap[url]; ok {
			idx := strings.LastIndex(match, "(")
			if idx < 0 {
				return match
			}
			return match[:idx+1] + realURL + ")"
		}
		if strings.HasPrefix(url, "wkimg:") {
			return ""
		}
		return match
	})

	return replaceImagePlaceholderTokensOutsideMarkdown(out, urlMap)
}

func replaceImagePlaceholderTokensOutsideMarkdown(s string, urlMap map[string]string) string {
	matches := mdImageURLRE.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return replaceImagePlaceholderTokens(s, urlMap)
	}

	var b strings.Builder
	last := 0
	for _, match := range matches {
		if match[0] > last {
			b.WriteString(replaceImagePlaceholderTokens(s[last:match[0]], urlMap))
		}
		b.WriteString(s[match[0]:match[1]])
		last = match[1]
	}
	if last < len(s) {
		b.WriteString(replaceImagePlaceholderTokens(s[last:], urlMap))
	}
	return b.String()
}

func replaceImagePlaceholderTokens(s string, urlMap map[string]string) string {
	return imagePlaceholderTokenRE.ReplaceAllStringFunc(s, func(token string) string {
		if realURL, ok := urlMap[token]; ok {
			return realURL
		}
		return ""
	})
}

// stripImageMarkup removes image-only placeholders (Markdown image refs,
// <img> tags, <image_original> redundancy blocks) and unwraps the
// <image>/<image_caption>/<image_ocr> XML wrappers produced by the search
// enrichment layer, leaving any OCR or caption text as plain inline text.
//
// This shape matters: when VLM OCR succeeds on a scanned PDF page, the
// extracted text reaches downstream code wrapped in <image_ocr> tags inside
// an <image> block. A naive "strip the whole <image>...</image> block"
// approach would discard the OCR text — the exact opposite of what we want.
func stripImageMarkup(s string) string {
	s = imageOriginalBlockRE.ReplaceAllString(s, "")
	s = mdImageRefRE.ReplaceAllString(s, "")
	s = htmlImgTagRE.ReplaceAllString(s, "")
	s = imageWrapperTagRE.ReplaceAllString(s, "")
	return s
}

// extractRealText returns the trimmed content with image markup stripped.
// Cached at the call site for use both in the threshold check and in any
// subsequent log message, avoiding redundant regex passes over large docs.
func extractRealText(content string) string {
	return strings.TrimSpace(stripImageMarkup(content))
}

// hasSufficientTextContent reports whether the given content carries enough
// real text (after image markup is stripped, with OCR/caption text retained)
// to warrant an LLM call. It is the primary defence against filename-driven
// hallucinations on scanned PDFs that have NO usable text at all.
func hasSufficientTextContent(content string) bool {
	return realTextRuneCount(content) >= minTextContentRunes
}

// realTextRuneCount returns the rune length of the content after image
// markup is stripped. Uses utf8.RuneCountInString to avoid allocating a
// rune slice for the count.
func realTextRuneCount(content string) int {
	return utf8.RuneCountInString(extractRealText(content))
}

// cleanLLMJSON strips markdown code-fence wrappers and sanitizes control characters
// from LLM-generated JSON output so it can be safely unmarshalled.
func cleanLLMJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	return sanitizeJSONString(s)
}

// sanitizeJSONString sanitizes a string that is intended to be parsed as JSON,
// by properly escaping unescaped control characters (like newlines) inside string literals.
func sanitizeJSONString(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	inString := false
	escape := false
	for _, r := range s {
		if escape {
			if r == '\n' {
				buf.WriteString(`n`)
			} else if r == '\r' {
				buf.WriteString(`r`)
			} else if r == '\t' {
				buf.WriteString(`t`)
			} else {
				buf.WriteRune(r)
			}
			escape = false
			continue
		}
		if r == '\\' {
			escape = true
			buf.WriteRune(r)
			continue
		}
		if r == '"' {
			inString = !inString
			buf.WriteRune(r)
			continue
		}
		if inString {
			if r == '\n' {
				buf.WriteString(`\n`)
				continue
			}
			if r == '\r' {
				buf.WriteString(`\r`)
				continue
			}
			if r == '\t' {
				buf.WriteString(`\t`)
				continue
			}
		}
		buf.WriteRune(r)
	}
	return buf.String()
}
