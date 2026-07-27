package interfaces

import "context"

// KnowledgeTaskTarget is the compound identity needed to inspect all task
// backends for one document. Asynq payloads use KnowledgeID, while durable
// wiki pending ops are scoped by KnowledgeBaseID and keyed by the
// KnowledgeID plus a processing-generation suffix.
type KnowledgeTaskTarget struct {
	KnowledgeBaseID string
	KnowledgeID     string
}

// TaskInspector abstracts queue inspection / cancellation against the
// task backend. It is best-effort: implementations may scan a finite
// number of tasks per call and return whatever count they could affect. Lite
// mode (no Redis) inspects and cancels SyncTaskExecutor's in-process live-task
// registry; an active goroutine remains visible until its handler returns.
//
// Use cases today: user-initiated cancel of an in-progress knowledge
// parse, which must remove downstream multimodal / post-process /
// question / summary tasks already enqueued against the same
// knowledge_id, plus signal active workers to stop at their next
// checkpoint.
type TaskInspector interface {
	// CancelTasksForKnowledge removes pending/scheduled/retry tasks
	// whose payload references the given knowledge ID, and signals
	// active workers running such tasks to stop. Returns rough
	// counts of (deletedFromQueue, activeCancelled) for observability.
	// A multi-document controller is one indivisible backend task: matching
	// any knowledge ID cancels that whole controller, not only one array item.
	// Errors are returned but callers should treat the operation as
	// best-effort: the row-level abort flag remains the source of
	// truth, this just prevents wasted work.
	CancelTasksForKnowledge(ctx context.Context, knowledgeID string) (deleted int, cancelled int, err error)

	// HasQueuedTasksForKnowledge reports whether any pending / scheduled /
	// retry / active task referencing the knowledge still lives in either
	// task backend. knowledgeBaseID is required because durable wiki ops
	// are KB-scoped in PostgreSQL and identify the document by a dedup_key
	// prefix ("<knowledge_id>:") across processing generations;
	// ordinary asynq tasks continue to match their payload knowledge_id.
	// It is the broad read-only counterpart of CancelTasksForKnowledge;
	// lifecycle code that must exclude independent Wiki work uses the
	// narrower DocumentLifecycleTaskKnowledgeIDs method below.
	//
	// Short-circuiting: it returns true as soon as the first match is seen.
	// A backend error is returned rather than being collapsed into "not
	// queued"; lifecycle callers must preserve the row when queue state
	// cannot be proven. Lite mode still checks PostgreSQL pending ops even
	// though its inline executor has no Redis queue.
	HasQueuedTasksForKnowledge(ctx context.Context, knowledgeBaseID, knowledgeID string) (bool, error)

	// QueuedKnowledgeIDs is the broad batch liveness probe across Redis and
	// durable PostgreSQL ops. It scans each Redis queue/state once for the
	// whole candidate set instead of repeating a full queue walk per document,
	// while still probing the indexed PostgreSQL pending-op identity for each
	// target. The returned map contains only knowledge IDs with proven live
	// work. Any backend error makes the whole result unknown and is returned.
	QueuedKnowledgeIDs(ctx context.Context, targets []KnowledgeTaskTarget) (map[string]bool, error)

	// DocumentLifecycleTaskKnowledgeIDs reports document-scoped asynq tasks
	// that control core processing or own a pending_subtasks_count slot during
	// finalizing. Durable Wiki ops are intentionally excluded: Wiki is an
	// independent KB-scoped pipeline and, by the 000056 lifecycle contract,
	// must never hold parse_status in processing/finalizing.
	DocumentLifecycleTaskKnowledgeIDs(ctx context.Context, targets []KnowledgeTaskTarget) (map[string]bool, error)

	// SummaryTaskKnowledgeIDs reports only summary-generation tasks in any
	// live asynq state. Housekeeping uses this narrower batch probe because a
	// document may already be parse-complete while its summary is legitimately
	// waiting behind queue backpressure. As with the other liveness probes, an
	// inspection error means unknown and must be handled fail-closed.
	SummaryTaskKnowledgeIDs(ctx context.Context, targets []KnowledgeTaskTarget) (map[string]bool, error)
}

// TaskHistoryPurger is the deletion-only extension implemented by production
// task inspectors. Keeping it separate from TaskInspector prevents ordinary
// parse cancellation and read-only housekeeping implementations from being
// forced to expose a destructive terminal-history operation.
type TaskHistoryPurger interface {
	// PurgeTaskHistoryForKnowledge removes completed / archived backend task
	// records owned exclusively by one document. Callers must first prove live
	// task quiescence. The durable PostgreSQL document workflow is outside this
	// cleanup boundary and remains available as the audit trail.
	PurgeTaskHistoryForKnowledge(ctx context.Context, knowledgeID string) (deleted int, err error)
}

// KnowledgeDeletionTaskInspector is the deletion-only liveness extension.
// Normal document lifecycle probes intentionally ignore independent Wiki
// work, while destructive deletion must additionally wait for the exact
// document-generation Wiki Map worker so it cannot write artifacts after the
// final purge.
type KnowledgeDeletionTaskInspector interface {
	// DeletionTaskKnowledgeIDs reports every live document-owned task that can
	// still mutate a deletion target. KB-scoped Wiki triggers and durable
	// retract work are excluded; exact task_mode="map" Wiki tasks are included.
	DeletionTaskKnowledgeIDs(ctx context.Context, targets []KnowledgeTaskTarget) (map[string]bool, error)
}
