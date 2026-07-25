-- A document generation is one logical Wiki ingest intent. Finalization,
-- recovery scanners, and restarted replicas can replay that intent
-- concurrently, so service-side read-before-insert cannot be the correctness
-- boundary. Consolidate historical duplicates without discarding the most
-- advanced durable checkpoint, then enforce the identity in PostgreSQL.

-- Keep rolling-version writers out between the de-duplication snapshot and
-- unique-index creation. Versioned migrations run transactionally, so this
-- lock is held until the index is visible.
LOCK TABLE task_pending_ops IN SHARE ROW EXCLUSIVE MODE;

CREATE TEMP TABLE wiki_ingest_duplicate_winners ON COMMIT DROP AS
WITH candidates AS (
    SELECT
        pending.id,
        pending.tenant_id,
        pending.task_type,
        pending.scope,
        pending.scope_id,
        pending.op,
        pending.dedup_key,
        ROW_NUMBER() OVER (
            PARTITION BY
                pending.tenant_id,
                pending.task_type,
                pending.scope,
                pending.scope_id,
                pending.op,
                pending.dedup_key
            ORDER BY
                EXISTS (
                    SELECT 1
                    FROM wiki_log_entries AS log_entry
                    WHERE log_entry.source_op_id = pending.id
                ) DESC,
                CASE
                    WHEN jsonb_typeof(pending.payload -> 'applied_page_slugs') = 'array'
                    THEN jsonb_array_length(pending.payload -> 'applied_page_slugs')
                    ELSE 0
                END DESC,
                (pending.payload ? 'prepared') DESC,
                (pending.payload ? 'map_checkpoint') DESC,
                length(pending.payload::text) DESC,
                pending.id ASC
        ) AS duplicate_rank,
        MAX(pending.fail_count) OVER (
            PARTITION BY
                pending.tenant_id,
                pending.task_type,
                pending.scope,
                pending.scope_id,
                pending.op,
                pending.dedup_key
        ) AS merged_fail_count,
        MIN(pending.enqueued_at) OVER (
            PARTITION BY
                pending.tenant_id,
                pending.task_type,
                pending.scope,
                pending.scope_id,
                pending.op,
                pending.dedup_key
        ) AS first_enqueued_at
    FROM task_pending_ops AS pending
    WHERE pending.task_type = 'wiki:ingest'
      AND pending.scope = 'knowledge_base'
      AND pending.op = 'ingest'
)
SELECT *
FROM candidates;

-- Retain the retry budget and original queue age even when the most advanced
-- checkpoint is not the oldest physical row.
UPDATE task_pending_ops AS pending
SET fail_count = winner.merged_fail_count,
    enqueued_at = winner.first_enqueued_at
FROM wiki_ingest_duplicate_winners AS winner
WHERE winner.duplicate_rank = 1
  AND pending.id = winner.id;

DELETE FROM task_pending_ops AS pending
USING wiki_ingest_duplicate_winners AS duplicate
WHERE duplicate.duplicate_rank > 1
  AND pending.id = duplicate.id;

CREATE UNIQUE INDEX IF NOT EXISTS uq_task_pending_ops_wiki_ingest
    ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base'
      AND op = 'ingest';

COMMENT ON INDEX uq_task_pending_ops_wiki_ingest IS
    'One durable Wiki ingest intent per tenant/KB/document processing generation.';
