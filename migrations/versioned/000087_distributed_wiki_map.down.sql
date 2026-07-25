DROP INDEX IF EXISTS idx_task_pending_ops_wiki_map_pending;
DROP INDEX IF EXISTS idx_task_pending_ops_wiki_commit_ready;

ALTER TABLE task_pending_ops
    DROP COLUMN IF EXISTS map_ready_at;
