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
