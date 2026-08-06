DROP INDEX IF EXISTS idx_task_pending_ops_wiki_map_dispatch_pool;
DROP INDEX IF EXISTS idx_task_pending_ops_wiki_map_dispatch_due;

ALTER TABLE task_pending_ops
    DROP COLUMN IF EXISTS map_dispatch_lease_until,
    DROP COLUMN IF EXISTS map_dispatch_task_id,
    DROP COLUMN IF EXISTS map_dispatch_epoch,
    DROP COLUMN IF EXISTS map_resource_pool_id,
    DROP COLUMN IF EXISTS next_attempt_at;
