DROP INDEX IF EXISTS idx_task_pending_ops_wiki_retry_rotation;

COMMENT ON COLUMN task_pending_ops.claimed_at IS
    'Reserved timestamp for consumer-specific retry or claim workflows.';
