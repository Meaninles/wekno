-- Split Wiki processing into a horizontally scalable document-local Map
-- phase and a KB-serialized materialization phase. The nullable marker is
-- updated atomically with the durable Map payload, so a commit worker can
-- never observe a half-written plan.

ALTER TABLE task_pending_ops
    ADD COLUMN IF NOT EXISTS map_ready_at TIMESTAMPTZ;

COMMENT ON COLUMN task_pending_ops.map_ready_at IS
    'Wiki ingest Map completion marker; set atomically with the prepared payload before KB materialization may consume the row.';

-- Preserve already-completed Map checkpoints across the rollout. Rows without
-- a prepared payload remain unready and are republished by Wiki recovery.
UPDATE task_pending_ops
SET map_ready_at = COALESCE(map_ready_at, enqueued_at)
WHERE task_type = 'wiki:ingest'
  AND scope = 'knowledge_base'
  AND op = 'ingest'
  AND payload ? 'prepared';

CREATE INDEX IF NOT EXISTS idx_task_pending_ops_wiki_commit_ready
    ON task_pending_ops (tenant_id, scope_id, id)
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base'
      AND (op <> 'ingest' OR map_ready_at IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_task_pending_ops_wiki_map_pending
    ON task_pending_ops (id)
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base'
      AND op = 'ingest'
      AND map_ready_at IS NULL;
