ALTER TABLE task_pending_ops
    ADD COLUMN IF NOT EXISTS next_attempt_at timestamptz,
    ADD COLUMN IF NOT EXISTS map_resource_pool_id varchar(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS map_dispatch_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS map_dispatch_task_id varchar(190) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS map_dispatch_lease_until timestamptz;

UPDATE task_pending_ops
SET next_attempt_at = COALESCE(claimed_at, enqueued_at, now())
WHERE task_type = 'wiki:ingest'
  AND scope = 'knowledge_base'
  AND op = 'ingest'
  AND next_attempt_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_task_pending_ops_wiki_map_dispatch_due
    ON task_pending_ops
       (next_attempt_at, map_dispatch_lease_until, id)
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base'
      AND op = 'ingest'
      AND map_ready_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_task_pending_ops_wiki_map_dispatch_pool
    ON task_pending_ops
       (map_resource_pool_id, map_dispatch_lease_until)
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base'
      AND op = 'ingest'
      AND map_ready_at IS NULL;
