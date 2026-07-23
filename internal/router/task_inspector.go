package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// NewAsynqInspector constructs an *asynq.Inspector pointed at the same
// Redis used by the asynq client. Only registered in asynq mode.
func NewAsynqInspector() *asynq.Inspector {
	return asynq.NewInspector(getAsynqRedisClientOpt())
}

// asynqTaskInspector implements interfaces.TaskInspector across the Redis
// asynq queues and the durable PostgreSQL pending-op queue. Cancellation stays
// best-effort and logs Redis failures; read-side probes return backend errors
// so lifecycle callers never mistake unavailable state for an empty queue.
type asynqTaskInspector struct {
	inspector   *asynq.Inspector
	taskInfo    asynqTaskInfoReader
	snapshotter liveTaskSnapshotter
	activeKeys  *redis.Client
	pendingOps  interfaces.TaskPendingOpsRepository
	syncTasks   *SyncTaskExecutor
}

// asynqTaskInfoReader is deliberately narrower than *asynq.Inspector so the
// read-side failure semantics can be tested without a live Redis server. The
// concrete Inspector remains on asynqTaskInspector for cancellation methods.
type asynqTaskInfoReader interface {
	GetTaskInfo(queue, id string) (*asynq.TaskInfo, error)
}

type liveTaskSnapshotter interface {
	LiveTaskIDs(ctx context.Context, queue string) ([]string, error)
}

type redisLiveTaskSnapshotter struct {
	client *redis.Client
}

// NewAsynqTaskInspector returns a TaskInspector wrapping the given Inspector,
// direct Redis client, and durable PostgreSQL pending-op repository. The Redis
// client is required for an atomic view of all live states; the public Asynq
// Inspector is then used only to decode each snapshotted task and for the
// best-effort cancellation path.
func NewAsynqTaskInspector(
	inspector *asynq.Inspector,
	redisClient *redis.Client,
	pendingOps interfaces.TaskPendingOpsRepository,
) interfaces.TaskInspector {
	var taskInfo asynqTaskInfoReader
	if inspector != nil {
		taskInfo = inspector
	}
	var snapshotter liveTaskSnapshotter
	if redisClient != nil {
		snapshotter = &redisLiveTaskSnapshotter{client: redisClient}
	}
	return &asynqTaskInspector{
		inspector:   inspector,
		taskInfo:    taskInfo,
		snapshotter: snapshotter,
		activeKeys:  redisClient,
		pendingOps:  pendingOps,
	}
}

// knowledgeIDsProbe is the union of document identity fields carried by task
// payloads. Most workers own one document through knowledge_id; controller
// tasks such as move and list-reparse own every entry in knowledge_ids.
type knowledgeIDsProbe struct {
	KnowledgeID  string   `json:"knowledge_id,omitempty"`
	KnowledgeIDs []string `json:"knowledge_ids,omitempty"`
	DryRun       bool     `json:"dry_run,omitempty"`
}

type knowledgeBaseIDProbe struct {
	KnowledgeBaseID string `json:"knowledge_base_id"`
}

func wikiTaskKnowledgeBaseID(taskType string, payload []byte) (string, bool, error) {
	if taskType != types.TypeWikiIngest {
		return "", false, nil
	}
	var probe knowledgeBaseIDProbe
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "", true, fmt.Errorf("decode %s payload: %w", taskType, err)
	}
	if probe.KnowledgeBaseID == "" {
		return "", true, fmt.Errorf("%s payload has no knowledge_base_id", taskType)
	}
	return probe.KnowledgeBaseID, true, nil
}

// queuesScanned is the fixed set of queue names this codebase enqueues
// into. Kept tight on purpose — we never scan user-defined queues.
// MUST include every queue any cancelable task type can land in; the
// multimodal queue is required here so cancelling a knowledge also purges
// its (potentially hundreds of) pending image:multimodal tasks.
var queuesScanned = []string{
	types.QueueDefault,
	types.QueueCritical,
	types.QueueLow,
	types.QueueDocument,
	types.QueueDocumentHeavy,
	types.QueueMultimodal,
	types.QueueGraph,
	types.QueueQuestion,
}

// taskTypesForKnowledgeCancel contains only single-document workers. A batch
// controller is indivisible at the Asynq layer: cancelling it for document A
// would silently discard the requested work for B..Z. Batch controllers stay
// visible to the lifecycle barrier below but are allowed to finish; their
// per-item state fences skip a concurrently deleting document safely.
var taskTypesForKnowledgeCancel = map[string]struct{}{
	types.TypeDocumentProcess:      {},
	types.TypeManualProcess:        {},
	types.TypeImageMultimodal:      {},
	types.TypeKnowledgePostProcess: {},
	types.TypeQuestionGeneration:   {},
	types.TypeSummaryGeneration:    {},
	types.TypeChunkExtract:         {},
	types.TypeDataTableSummary:     {},
	types.TypeFAQImport:            {},
}

var multiKnowledgeTaskTypes = map[string]struct{}{
	types.TypeKnowledgeMove:        {},
	types.TypeKnowledgeListReparse: {},
}

var taskTypesForDocumentLifecycle = func() map[string]struct{} {
	result := make(map[string]struct{}, len(taskTypesForKnowledgeCancel)+len(multiKnowledgeTaskTypes))
	for taskType := range taskTypesForKnowledgeCancel {
		result[taskType] = struct{}{}
	}
	for taskType := range multiKnowledgeTaskTypes {
		result[taskType] = struct{}{}
	}
	return result
}()

var summaryTaskTypes = map[string]struct{}{
	types.TypeSummaryGeneration: {},
}

// listPageSize caps each Redis LIST call used by the best-effort destructive
// cancellation path. Lifecycle probes do not page: they take one atomic Redis
// snapshot per queue below.
const listPageSize = 100

// maxAtomicSnapshotTasks bounds the amount of work performed by one Redis Lua
// invocation. A snapshot larger than this is reported as an inspection error,
// which makes housekeeping fail closed. This trades automatic stale-row repair
// for Redis responsiveness during an exceptional backlog instead of running an
// unbounded LRANGE/ZRANGE script on Redis' single command thread.
const maxAtomicSnapshotTasks = 100_000

// liveTaskSnapshotScript captures every live Asynq task ID in one queue at a
// single Redis instant. All four keys use Asynq's {queue} hash tag, so the
// script is Redis Cluster safe. We intentionally duplicate the public key
// layout here instead of importing Asynq's internal/base package.
var liveTaskSnapshotScript = redis.NewScript(`
local total = redis.call("LLEN", KEYS[1])
    + redis.call("LLEN", KEYS[2])
    + redis.call("ZCARD", KEYS[3])
    + redis.call("ZCARD", KEYS[4])
local maximum = tonumber(ARGV[1])
if total > maximum then
    return redis.error_reply("live task snapshot exceeds safety limit")
end

local result = {}
for index = 1, 2 do
    local ids = redis.call("LRANGE", KEYS[index], 0, -1)
    for _, id in ipairs(ids) do
        table.insert(result, id)
    end
end
for index = 3, 4 do
    local ids = redis.call("ZRANGE", KEYS[index], 0, -1)
    for _, id in ipairs(ids) do
        table.insert(result, id)
    end
end
return result
`)

// CancelTasksForKnowledge removes queued tasks whose payload references the
// given knowledge ID and signals active workers running such tasks to stop.
// Multi-document controllers are deliberately not cancelled because Asynq
// cannot remove one ID from an already-published payload atomically.
func (a *asynqTaskInspector) CancelTasksForKnowledge(
	ctx context.Context, knowledgeID string,
) (int, int, error) {
	if a == nil || knowledgeID == "" {
		return 0, 0, nil
	}
	if a.syncTasks != nil {
		deleted, cancelled := a.syncTasks.cancelTasksForKnowledge(knowledgeID)
		logger.Infof(ctx,
			"[TaskInspector] Lite knowledge=%s cancel summary: pending_cancelled=%d active_cancel_signaled=%d",
			knowledgeID, deleted, cancelled,
		)
		return deleted, cancelled, nil
	}
	if a.inspector == nil {
		return 0, 0, errors.New("task inspector: cancellation backend unavailable")
	}
	deleted := 0
	cancelled := 0

	for _, queue := range queuesScanned {
		// Pending / Scheduled / Retry can all be deleted by task ID.
		// Archived tasks are NOT touched: dead-letter rows are
		// already final and should remain visible to operators.
		deleted += a.deletePendingMatches(ctx, queue, knowledgeID)
		deleted += a.deleteScheduledMatches(ctx, queue, knowledgeID)
		deleted += a.deleteRetryMatches(ctx, queue, knowledgeID)
		cancelled += a.cancelActiveMatches(ctx, queue, knowledgeID)
	}

	logger.Infof(ctx,
		"[TaskInspector] knowledge=%s cancel summary: deleted_from_queue=%d active_cancel_signaled=%d",
		knowledgeID, deleted, cancelled,
	)
	return deleted, cancelled, nil
}

// CancelWikiTasksForKnowledgeBase removes every disposable wiki:ingest
// wake-up for one KB and signals an active handler to stop. PostgreSQL pending
// ops are deliberately not touched here: the KB-delete coordinator purges
// them only after all document cleanup has committed.
//
// This method is an optional production extension (discovered by a narrow
// interface assertion in the KB delete service), so the broad public
// TaskInspector contract and unrelated test doubles remain unchanged.
func (a *asynqTaskInspector) CancelWikiTasksForKnowledgeBase(
	ctx context.Context, knowledgeBaseID string,
) (deleted int, cancelled int, retErr error) {
	if a == nil || knowledgeBaseID == "" {
		return 0, 0, errors.New("task inspector: Wiki KB cancellation requires an identity")
	}
	if a.syncTasks != nil {
		return a.syncTasks.cancelWikiTasksForKnowledgeBase(knowledgeBaseID)
	}
	if a.inspector == nil || a.taskInfo == nil || a.snapshotter == nil {
		return 0, 0, errors.New("task inspector: Wiki KB cancellation backend unavailable")
	}

	for _, queue := range queuesScanned {
		ids, err := a.snapshotter.LiveTaskIDs(ctx, queue)
		if err != nil {
			return deleted, cancelled, fmt.Errorf("snapshot Wiki tasks in queue %s: %w", queue, err)
		}
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return deleted, cancelled, err
			}
			if id == "" {
				return deleted, cancelled, fmt.Errorf("snapshot Wiki tasks in queue %s: empty task ID", queue)
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			info, err := a.taskInfo.GetTaskInfo(queue, id)
			if err != nil {
				return deleted, cancelled, fmt.Errorf("read Wiki task queue=%s id=%s: %w", queue, id, err)
			}
			kbID, tracked, err := wikiTaskKnowledgeBaseID(info.Type, info.Payload)
			if err != nil {
				return deleted, cancelled, fmt.Errorf("inspect Wiki task queue=%s id=%s: %w", queue, id, err)
			}
			if !tracked || kbID != knowledgeBaseID {
				continue
			}
			if info.State == asynq.TaskStateActive {
				if err := a.inspector.CancelProcessing(info.ID); err != nil {
					return deleted, cancelled, fmt.Errorf("cancel active Wiki task queue=%s id=%s: %w", queue, id, err)
				}
				cancelled++
				continue
			}
			if err := a.inspector.DeleteTask(queue, info.ID); err != nil {
				return deleted, cancelled, fmt.Errorf("delete Wiki task queue=%s id=%s state=%s: %w", queue, id, info.State, err)
			}
			deleted++
		}
	}
	return deleted, cancelled, nil
}

// HasWikiTasksForKnowledgeBase proves whether an Asynq/Lite Wiki trigger or
// active Redis owner marker still exists for the KB. Malformed matching task
// payloads and backend uncertainty fail closed.
func (a *asynqTaskInspector) HasWikiTasksForKnowledgeBase(
	ctx context.Context, knowledgeBaseID string,
) (bool, error) {
	if a == nil || knowledgeBaseID == "" {
		return false, errors.New("task inspector: Wiki KB liveness requires an identity")
	}
	if a.syncTasks != nil {
		return a.syncTasks.hasWikiTasksForKnowledgeBase(knowledgeBaseID)
	}
	if a.taskInfo == nil || a.snapshotter == nil || a.activeKeys == nil {
		return false, errors.New("task inspector: Wiki KB liveness backend unavailable")
	}
	for _, queue := range queuesScanned {
		ids, err := a.snapshotter.LiveTaskIDs(ctx, queue)
		if err != nil {
			return false, fmt.Errorf("snapshot Wiki tasks in queue %s: %w", queue, err)
		}
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if id == "" {
				return false, fmt.Errorf("snapshot Wiki tasks in queue %s: empty task ID", queue)
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			info, err := a.taskInfo.GetTaskInfo(queue, id)
			if err != nil {
				return false, fmt.Errorf("read Wiki task queue=%s id=%s: %w", queue, id, err)
			}
			kbID, tracked, err := wikiTaskKnowledgeBaseID(info.Type, info.Payload)
			if err != nil {
				return false, fmt.Errorf("inspect Wiki task queue=%s id=%s: %w", queue, id, err)
			}
			if tracked && kbID == knowledgeBaseID {
				return true, nil
			}
		}
	}
	active, err := a.activeKeys.Exists(ctx, "wiki:active:"+knowledgeBaseID).Result()
	if err != nil {
		return false, fmt.Errorf("inspect Wiki active marker for KB %s: %w", knowledgeBaseID, err)
	}
	return active > 0, nil
}

// HasQueuedTasksForKnowledge is the single-target convenience wrapper around
// QueuedKnowledgeIDs. It inspects both PostgreSQL and Redis and never deletes
// anything.
func (a *asynqTaskInspector) HasQueuedTasksForKnowledge(
	ctx context.Context, knowledgeBaseID, knowledgeID string,
) (bool, error) {
	queued, err := a.QueuedKnowledgeIDs(ctx, []interfaces.KnowledgeTaskTarget{{
		KnowledgeBaseID: knowledgeBaseID,
		KnowledgeID:     knowledgeID,
	}})
	if err != nil {
		return false, err
	}
	return queued[knowledgeID], nil
}

// QueuedKnowledgeIDs batch-inspects durable PostgreSQL wiki ops and all
// relevant asynq states. Redis is walked once for the whole target set: a
// large stale-candidate sweep therefore costs O(queue pages + candidates)
// rather than O(queue pages * candidates).
func (a *asynqTaskInspector) QueuedKnowledgeIDs(
	ctx context.Context,
	targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	queued := make(map[string]bool)
	if len(targets) == 0 {
		return queued, nil
	}
	if a == nil || a.pendingOps == nil {
		return nil, errors.New("task inspector: durable pending-op repository unavailable")
	}

	targetKBs, err := normalizeKnowledgeTaskTargets(targets)
	if err != nil {
		return nil, err
	}
	remaining := make(map[string]struct{}, len(targetKBs))
	for knowledgeID, knowledgeBaseID := range targetKBs {
		// Wiki trigger payloads are KB-scoped and deliberately omit
		// knowledge_id. The per-document identity lives durably in the
		// task_pending_ops.dedup_key prefix; its suffix is the exact processing
		// generation. A row remains present while a worker
		// processes the batch and is deleted only after terminal consume,
		// protecting both queued and active wiki enrichment.
		dedupPrefix, err := wikiqueue.IngestDedupPrefix(knowledgeID)
		if err != nil {
			return nil, fmt.Errorf("build durable wiki pending prefix for %s: %w", knowledgeID, err)
		}
		pending, err := a.pendingOps.ExistsByDedupKeyPrefix(
			ctx,
			types.TypeWikiIngest,
			types.TaskScopeKnowledgeBase,
			knowledgeBaseID,
			dedupPrefix,
			"ingest",
		)
		if err != nil {
			return nil, fmt.Errorf("probe durable wiki pending op for %s: %w", knowledgeID, err)
		}
		if pending {
			queued[knowledgeID] = true
			continue
		}
		remaining[knowledgeID] = struct{}{}
	}

	if err := a.addAsynqQueuedKnowledgeIDs(ctx, remaining, queued); err != nil {
		return nil, err
	}
	return queued, nil
}

// DocumentLifecycleTaskKnowledgeIDs batch-inspects only document-scoped
// asynq tasks that control core processing or finalization. PostgreSQL Wiki
// ops are omitted by design: Wiki runs independently after core parse
// completion and must not keep a document non-terminal during a large backlog.
func (a *asynqTaskInspector) DocumentLifecycleTaskKnowledgeIDs(
	ctx context.Context,
	targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	queued := make(map[string]bool)
	if len(targets) == 0 {
		return queued, nil
	}
	if a == nil {
		return nil, errors.New("task inspector unavailable")
	}

	targetKBs, err := normalizeKnowledgeTaskTargets(targets)
	if err != nil {
		return nil, err
	}
	remaining := make(map[string]struct{}, len(targetKBs))
	for knowledgeID := range targetKBs {
		remaining[knowledgeID] = struct{}{}
	}
	if err := a.addAsynqQueuedKnowledgeIDsForTypes(
		ctx, remaining, queued, taskTypesForDocumentLifecycle,
	); err != nil {
		return nil, err
	}
	return queued, nil
}

// SummaryTaskKnowledgeIDs batch-inspects just summary-generation tasks. It
// intentionally ignores unrelated document tasks: this probe owns only the
// summary_status repair decision and must neither hide nor infer the liveness
// of another pipeline stage.
func (a *asynqTaskInspector) SummaryTaskKnowledgeIDs(
	ctx context.Context,
	targets []interfaces.KnowledgeTaskTarget,
) (map[string]bool, error) {
	queued := make(map[string]bool)
	if len(targets) == 0 {
		return queued, nil
	}
	if a == nil {
		return nil, errors.New("task inspector unavailable")
	}

	targetKBs, err := normalizeKnowledgeTaskTargets(targets)
	if err != nil {
		return nil, err
	}
	remaining := make(map[string]struct{}, len(targetKBs))
	for knowledgeID := range targetKBs {
		remaining[knowledgeID] = struct{}{}
	}
	if err := a.addAsynqQueuedKnowledgeIDsForTypes(
		ctx,
		remaining,
		queued,
		summaryTaskTypes,
	); err != nil {
		return nil, err
	}
	return queued, nil
}

func normalizeKnowledgeTaskTargets(
	targets []interfaces.KnowledgeTaskTarget,
) (map[string]string, error) {
	targetKBs := make(map[string]string, len(targets))
	for _, target := range targets {
		if target.KnowledgeBaseID == "" || target.KnowledgeID == "" {
			return nil, errors.New("task inspector: knowledge base ID and knowledge ID are required")
		}
		if previousKB, duplicate := targetKBs[target.KnowledgeID]; duplicate {
			if previousKB != target.KnowledgeBaseID {
				return nil, fmt.Errorf(
					"task inspector: knowledge %s appears under multiple knowledge bases",
					target.KnowledgeID,
				)
			}
			continue
		}
		targetKBs[target.KnowledgeID] = target.KnowledgeBaseID
	}
	return targetKBs, nil
}

func (a *asynqTaskInspector) addAsynqQueuedKnowledgeIDs(
	ctx context.Context,
	remaining map[string]struct{},
	queued map[string]bool,
) error {
	return a.addAsynqQueuedKnowledgeIDsForTypes(
		ctx, remaining, queued, taskTypesForDocumentLifecycle,
	)
}

func (a *asynqTaskInspector) addAsynqQueuedKnowledgeIDsForTypes(
	ctx context.Context,
	remaining map[string]struct{},
	queued map[string]bool,
	taskTypes map[string]struct{},
) error {
	if len(remaining) == 0 {
		return nil
	}
	if a.syncTasks != nil {
		return a.addSyncQueuedKnowledgeIDsForTypes(remaining, queued, taskTypes)
	}
	// A missing or half-configured backend is unknown, never evidence that no
	// live work exists. Lite mode supplies syncTasks; Redis mode supplies both
	// taskInfo and snapshotter.
	if a.taskInfo == nil && a.snapshotter == nil {
		return errors.New("task inspector: no lifecycle task backend available")
	}
	if a.taskInfo == nil || a.snapshotter == nil {
		return errors.New("task inspector: Redis snapshot backend unavailable")
	}

	for _, queue := range queuesScanned {
		ids, err := a.snapshotter.LiveTaskIDs(ctx, queue)
		if err != nil {
			logger.Warnf(ctx, "[TaskInspector] atomic live-task snapshot queue=%s: %v", queue, err)
			return fmt.Errorf("snapshot live tasks in queue %s: %w", queue, err)
		}
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf("snapshot live tasks in queue %s: empty task ID", queue)
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}

			info, err := a.taskInfo.GetTaskInfo(queue, id)
			if err != nil {
				// The ID was live at snapshot time. If its task hash disappears
				// before we can decode the payload, absence cannot be proven for
				// any remaining candidate, so housekeeping must preserve them.
				return fmt.Errorf("read snapshotted task queue=%s id=%s: %w", queue, id, err)
			}
			knowledgeIDs, tracked, err := taskKnowledgeIDsForTypesStrict(
				info.Type, info.Payload, taskTypes,
			)
			if err != nil {
				return fmt.Errorf("decode snapshotted task queue=%s id=%s: %w", queue, id, err)
			}
			if !tracked {
				continue
			}
			for _, knowledgeID := range knowledgeIDs {
				if _, wanted := remaining[knowledgeID]; !wanted {
					continue
				}
				queued[knowledgeID] = true
				delete(remaining, knowledgeID)
			}
			if len(remaining) == 0 {
				return nil
			}
		}
	}
	return nil
}

func (a *asynqTaskInspector) addSyncQueuedKnowledgeIDsForTypes(
	remaining map[string]struct{},
	queued map[string]bool,
	taskTypes map[string]struct{},
) error {
	for _, task := range a.syncTasks.liveTaskSnapshots() {
		knowledgeIDs, tracked, err := taskKnowledgeIDsForTypesStrict(
			task.taskType, task.payload, taskTypes,
		)
		if err != nil {
			return fmt.Errorf("decode live Lite task type=%s: %w", task.taskType, err)
		}
		if !tracked {
			continue
		}
		for _, knowledgeID := range knowledgeIDs {
			if _, wanted := remaining[knowledgeID]; !wanted {
				continue
			}
			queued[knowledgeID] = true
			delete(remaining, knowledgeID)
		}
		if len(remaining) == 0 {
			return nil
		}
	}
	return nil
}

func asynqLiveStateKeys(queue string) []string {
	prefix := "asynq:{" + queue + "}:"
	return []string{
		prefix + "pending",
		prefix + "active",
		prefix + "scheduled",
		prefix + "retry",
	}
}

func (s *redisLiveTaskSnapshotter) LiveTaskIDs(
	ctx context.Context,
	queue string,
) ([]string, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("Redis client unavailable")
	}
	raw, err := liveTaskSnapshotScript.Run(
		ctx,
		s.client,
		asynqLiveStateKeys(queue),
		maxAtomicSnapshotTasks,
	).Result()
	if err != nil {
		return nil, err
	}
	return parseLiveTaskIDSnapshot(raw)
}

func parseLiveTaskIDSnapshot(raw any) ([]string, error) {
	values, ok := raw.([]interface{})
	if !ok {
		if strings, ok := raw.([]string); ok {
			return strings, nil
		}
		return nil, fmt.Errorf("unexpected Redis snapshot result %T", raw)
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			ids = append(ids, typed)
		case []byte:
			ids = append(ids, string(typed))
		default:
			return nil, fmt.Errorf("unexpected task ID type %T", value)
		}
	}
	return ids, nil
}

// matchesKnowledge returns true when the task type is one we cancel
// AND its payload references the target knowledge ID.
func matchesKnowledge(taskType string, payload []byte, knowledgeID string) bool {
	knowledgeIDs, ok := taskKnowledgeIDsForTypes(taskType, payload, taskTypesForKnowledgeCancel)
	if !ok {
		return false
	}
	for _, candidate := range knowledgeIDs {
		if candidate == knowledgeID {
			return true
		}
	}
	return false
}

func taskKnowledgeIDs(taskType string, payload []byte) ([]string, bool) {
	return taskKnowledgeIDsForTypes(taskType, payload, taskTypesForDocumentLifecycle)
}

func taskKnowledgeIDsForTypes(
	taskType string,
	payload []byte,
	taskTypes map[string]struct{},
) ([]string, bool) {
	knowledgeIDs, tracked, err := taskKnowledgeIDsForTypesStrict(taskType, payload, taskTypes)
	if err != nil {
		return nil, false
	}
	return knowledgeIDs, tracked
}

// taskKnowledgeIDsForTypesStrict is used by read-side liveness probes and the
// Lite executor registry. Cancellation remains best-effort and may skip a
// malformed payload, while a liveness probe must fail closed if a task type it
// owns cannot be attributed to all of its documents.
func taskKnowledgeIDsForTypesStrict(
	taskType string,
	payload []byte,
	taskTypes map[string]struct{},
) ([]string, bool, error) {
	if _, ok := taskTypes[taskType]; !ok {
		return nil, false, nil
	}
	var probe knowledgeIDsProbe
	if err := json.Unmarshal(payload, &probe); err != nil {
		return nil, true, fmt.Errorf("decode %s payload: %w", taskType, err)
	}
	if taskType == types.TypeFAQImport && probe.DryRun {
		// FAQ dry-runs validate an upload but do not own or mutate a knowledge
		// row. They intentionally have no knowledge_id and are not a lifecycle
		// blocker for any document.
		return nil, false, nil
	}

	_, expectsMany := multiKnowledgeTaskTypes[taskType]
	if expectsMany {
		if probe.KnowledgeID != "" {
			return nil, true, fmt.Errorf("%s payload unexpectedly carries knowledge_id", taskType)
		}
		if len(probe.KnowledgeIDs) == 0 {
			return nil, true, fmt.Errorf("%s payload has no knowledge_ids", taskType)
		}
		return normalizeStrictKnowledgeIDs(taskType, probe.KnowledgeIDs)
	}
	if len(probe.KnowledgeIDs) != 0 {
		return nil, true, fmt.Errorf("%s payload unexpectedly carries knowledge_ids", taskType)
	}
	if probe.KnowledgeID == "" {
		return nil, true, fmt.Errorf("%s payload has no knowledge_id", taskType)
	}
	return []string{probe.KnowledgeID}, true, nil
}

func normalizeStrictKnowledgeIDs(taskType string, values []string) ([]string, bool, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for index, knowledgeID := range values {
		if knowledgeID == "" {
			return nil, true, fmt.Errorf("%s payload knowledge_ids[%d] is empty", taskType, index)
		}
		if _, duplicate := seen[knowledgeID]; duplicate {
			continue
		}
		seen[knowledgeID] = struct{}{}
		result = append(result, knowledgeID)
	}
	return result, true, nil
}

func (a *asynqTaskInspector) deletePendingMatches(ctx context.Context, queue, knowledgeID string) int {
	deleted := 0
	page := 1
	for {
		tasks, err := a.inspector.ListPendingTasks(queue, asynq.PageSize(listPageSize), asynq.Page(page))
		if err != nil {
			if !errors.Is(err, asynq.ErrQueueNotFound) {
				logger.Warnf(ctx, "[TaskInspector] list pending queue=%s page=%d: %v", queue, page, err)
			}
			return deleted
		}
		if len(tasks) == 0 {
			return deleted
		}
		for _, t := range tasks {
			if !matchesKnowledge(t.Type, t.Payload, knowledgeID) {
				continue
			}
			if err := a.inspector.DeleteTask(queue, t.ID); err != nil {
				logger.Warnf(ctx, "[TaskInspector] delete pending type=%s id=%s: %v", t.Type, t.ID, err)
				continue
			}
			deleted++
		}
		if len(tasks) < listPageSize {
			return deleted
		}
		page++
	}
}

func (a *asynqTaskInspector) deleteScheduledMatches(ctx context.Context, queue, knowledgeID string) int {
	deleted := 0
	page := 1
	for {
		tasks, err := a.inspector.ListScheduledTasks(queue, asynq.PageSize(listPageSize), asynq.Page(page))
		if err != nil {
			if !errors.Is(err, asynq.ErrQueueNotFound) {
				logger.Warnf(ctx, "[TaskInspector] list scheduled queue=%s page=%d: %v", queue, page, err)
			}
			return deleted
		}
		if len(tasks) == 0 {
			return deleted
		}
		for _, t := range tasks {
			if !matchesKnowledge(t.Type, t.Payload, knowledgeID) {
				continue
			}
			if err := a.inspector.DeleteTask(queue, t.ID); err != nil {
				logger.Warnf(ctx, "[TaskInspector] delete scheduled type=%s id=%s: %v", t.Type, t.ID, err)
				continue
			}
			deleted++
		}
		if len(tasks) < listPageSize {
			return deleted
		}
		page++
	}
}

func (a *asynqTaskInspector) deleteRetryMatches(ctx context.Context, queue, knowledgeID string) int {
	deleted := 0
	page := 1
	for {
		tasks, err := a.inspector.ListRetryTasks(queue, asynq.PageSize(listPageSize), asynq.Page(page))
		if err != nil {
			if !errors.Is(err, asynq.ErrQueueNotFound) {
				logger.Warnf(ctx, "[TaskInspector] list retry queue=%s page=%d: %v", queue, page, err)
			}
			return deleted
		}
		if len(tasks) == 0 {
			return deleted
		}
		for _, t := range tasks {
			if !matchesKnowledge(t.Type, t.Payload, knowledgeID) {
				continue
			}
			if err := a.inspector.DeleteTask(queue, t.ID); err != nil {
				logger.Warnf(ctx, "[TaskInspector] delete retry type=%s id=%s: %v", t.Type, t.ID, err)
				continue
			}
			deleted++
		}
		if len(tasks) < listPageSize {
			return deleted
		}
		page++
	}
}

// cancelActiveMatches signals active workers to abort via
// Inspector.CancelProcessing. The worker's ctx becomes Done() so the
// next blocking call (or our checkpoint reads) bails. The DB-level
// abort flag (parse_status=cancelled) remains the durable signal —
// this is a latency optimization, not the correctness mechanism.
func (a *asynqTaskInspector) cancelActiveMatches(ctx context.Context, queue, knowledgeID string) int {
	cancelled := 0
	page := 1
	for {
		tasks, err := a.inspector.ListActiveTasks(queue, asynq.PageSize(listPageSize), asynq.Page(page))
		if err != nil {
			if !errors.Is(err, asynq.ErrQueueNotFound) {
				logger.Warnf(ctx, "[TaskInspector] list active queue=%s page=%d: %v", queue, page, err)
			}
			return cancelled
		}
		if len(tasks) == 0 {
			return cancelled
		}
		for _, t := range tasks {
			if !matchesKnowledge(t.Type, t.Payload, knowledgeID) {
				continue
			}
			if err := a.inspector.CancelProcessing(t.ID); err != nil {
				logger.Warnf(ctx, "[TaskInspector] cancel active type=%s id=%s: %v", t.Type, t.ID, err)
				continue
			}
			cancelled++
		}
		if len(tasks) < listPageSize {
			return cancelled
		}
		page++
	}
}

// NewNoopTaskInspector returns the Redis-free inspector used by Lite mode.
// SyncTaskExecutor dispatches goroutines, so its live registry is mandatory:
// absence of Redis must never be confused with absence of active workers.
func NewNoopTaskInspector(
	pendingOps interfaces.TaskPendingOpsRepository,
	syncTasks *SyncTaskExecutor,
) interfaces.TaskInspector {
	return &asynqTaskInspector{pendingOps: pendingOps, syncTasks: syncTasks}
}
