-- Knowledge-owned storage objects use task_pending_ops as a durable ownership
-- ledger. Their KB-scoped tuple must be unique so retries cannot create two
-- competing ownership proofs for the same (knowledge, provider path).

-- A rolling-version writer must not insert between the de-duplication
-- snapshot and unique-index creation. The migration runner executes this file
-- transactionally, so this lock is retained through CREATE INDEX.
LOCK TABLE task_pending_ops IN SHARE ROW EXCLUSIVE MODE;

WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY tenant_id, task_type, scope, scope_id, op, dedup_key
               ORDER BY id ASC
           ) AS rn
    FROM task_pending_ops
    WHERE task_type = 'knowledge:aux_object'
      AND scope = 'knowledge_base'
      AND op = 'owned'
)
DELETE FROM task_pending_ops AS pending
USING ranked
WHERE pending.id = ranked.id
  AND ranked.rn > 1;

CREATE UNIQUE INDEX IF NOT EXISTS uq_task_pending_ops_knowledge_aux_owned
    ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
    WHERE task_type = 'knowledge:aux_object'
      AND scope = 'knowledge_base'
      AND op = 'owned';
