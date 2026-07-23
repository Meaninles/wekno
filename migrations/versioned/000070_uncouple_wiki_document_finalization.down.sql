-- Runtime versions at and after 000070 never count Wiki ingestion as a
-- document-finalization subtask. Re-introducing historical Wiki slots would
-- strand documents, so rollback only removes the supporting lookup index.
-- Data changes are intentionally irreversible (project policy forbids
-- downgrading this development line).

DROP INDEX IF EXISTS idx_task_pending_ops_scope_op_dedup;
