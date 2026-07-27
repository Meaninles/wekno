-- Prevent one repeatedly failing Wiki op from monopolizing the oldest rows.
-- claimed_at is a last-attempt timestamp, not an ownership claim; the
-- per-KB lease remains the write-serialization boundary.
CREATE INDEX IF NOT EXISTS idx_task_pending_ops_wiki_retry_rotation
    ON task_pending_ops (
        scope_id,
        tenant_id,
        fail_count,
        (CASE WHEN claimed_at IS NULL THEN 0 ELSE 1 END),
        claimed_at,
        id
    )
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base';

COMMENT ON COLUMN task_pending_ops.claimed_at IS
    'Wiki last-attempt timestamp used for fair retry rotation; it is not an ownership claim.';
