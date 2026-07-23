-- Migration: 000071_wiki_log_idempotency_and_dead_letter_time
--
-- A Wiki pending operation may finish materialization and append its audit
-- event, then fail during index publication or queue settlement. Retrying the
-- same durable task_pending_ops row must not append the event again. The
-- nullable source_op_id records that durable provenance; NULL remains valid
-- for legacy and manually-created log entries.

ALTER TABLE wiki_log_entries
    ADD COLUMN IF NOT EXISTS source_op_id BIGINT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_wiki_log_entries_source_op_id
    ON wiki_log_entries (source_op_id);

COMMENT ON COLUMN wiki_log_entries.source_op_id IS
    'Durable task_pending_ops.id provenance; unique idempotency key for Wiki event retries.';

-- A delete retry represents the same cleanup intent. Retain the newest
-- payload from any historical duplicates, then enforce one retract per
-- document/KB. Ingest rows intentionally remain non-unique because reparses
-- may enqueue a later version while an older operation is still draining.
-- Block legacy/rolling-version writers before taking the de-duplication
-- snapshot. SHARE ROW EXCLUSIVE conflicts with INSERT/UPDATE/DELETE's ROW
-- EXCLUSIVE lock while still allowing ordinary queue reads. The lock is held
-- through unique-index creation because a PostgreSQL migration file executes
-- as one transaction in the default golang-migrate configuration.
LOCK TABLE task_pending_ops IN SHARE ROW EXCLUSIVE MODE;

WITH ranked_retracts AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY tenant_id, task_type, scope, scope_id, op, dedup_key
               ORDER BY id DESC
           ) AS duplicate_rank
    FROM task_pending_ops
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base'
      AND op = 'retract'
)
DELETE FROM task_pending_ops AS pending
USING ranked_retracts AS ranked
WHERE pending.id = ranked.id
  AND ranked.duplicate_rank > 1;

CREATE UNIQUE INDEX IF NOT EXISTS uq_task_pending_ops_wiki_retract
    ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base'
      AND op = 'retract';

-- Older Wiki dead-letter writers explicitly supplied Go's zero time, which
-- bypassed the database DEFAULT NOW() and produced year-0001 timestamps.
-- Runtime validation now supplies FailedAt; normalize the existing invalid
-- rows so ordering and retention queries are truthful.
UPDATE task_dead_letters
SET failed_at = NOW()
WHERE failed_at < TIMESTAMPTZ '2000-01-01 00:00:00+00';

-- TaskPendingOp.EnqueuedAt was affected by the same Go-zero-time behaviour as
-- dead letters. Runtime now stamps UTC before INSERT; normalize surviving
-- durable rows so backlog-age and operations queries are meaningful.
UPDATE task_pending_ops
SET enqueued_at = NOW()
WHERE enqueued_at < TIMESTAMPTZ '2000-01-01 00:00:00+00';
