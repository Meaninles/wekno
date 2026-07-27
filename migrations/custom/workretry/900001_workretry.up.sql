-- Wiki pending operations remain durable in PostgreSQL while model work is
-- retried. The ordering matches applyWikiRetryRotation: lower real-provider
-- failure counts first, then never-attempted/oldest attempts, then stable ID.
-- This lets a poisoned row age behind untouched work without deleting it.
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
