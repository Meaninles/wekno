-- Migration: 000071_wiki_log_idempotency_and_dead_letter_time (rollback)
--
-- The repaired dead-letter and pending-op timestamps are intentionally not
-- reverted: their previous year-0001 values were invalid data, not meaningful
-- history.

DROP INDEX IF EXISTS uq_wiki_log_entries_source_op_id;

DROP INDEX IF EXISTS uq_task_pending_ops_wiki_retract;

ALTER TABLE wiki_log_entries
    DROP COLUMN IF EXISTS source_op_id;
