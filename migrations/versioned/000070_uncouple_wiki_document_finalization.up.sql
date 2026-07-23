-- Migration: 000070_uncouple_wiki_document_finalization
--
-- Wiki ingestion is a durable, KB-scoped background pipeline. Migration
-- 000056 intentionally excluded it from knowledge.pending_subtasks_count,
-- but a later runtime change seeded one Wiki-owned slot per document. Under
-- a large Wiki backlog that slot kept otherwise searchable documents in
-- finalizing until housekeeping labelled them failed.
--
-- Runtime code from this version never seeds or drains a Wiki-owned slot
-- again. Existing finalizing counters are deliberately NOT decremented here:
-- the integer does not record per-owner provenance, so SQL cannot prove
-- whether Wiki has already drained its historical slot while a legitimate
-- summary/question/graph task still owns the remaining count. The production
-- housekeeping path safely converges legacy finalizing rows only after
-- confirming that no document-lifecycle task or fresh span remains.

CREATE INDEX IF NOT EXISTS idx_task_pending_ops_scope_op_dedup
    ON task_pending_ops (task_type, scope, scope_id, op, dedup_key);

-- Repair rows already mislabelled by the old housekeeping sweep. Requiring a
-- non-null processed_at, live chunks, and a completed postprocess span on the
-- latest attempt prove that the core parse reached the
-- usable-artifact boundary. The latest-attempt guard prevents a previously
-- successful document whose newer reparse genuinely stalled in core parsing
-- from being promoted. We intentionally do not depend on the KB's current
-- wiki_enabled flag: operators may have disabled Wiki as a workaround after
-- the false failure. Optional graph/question failures are not exclusions;
-- they degrade independently instead of poisoning the core document. Pending
-- Wiki ops are preserved so Wiki can continue asynchronously. Summary is an
-- optional enrichment and therefore is not a prerequisite for restoring the
-- core document. A summary that was still pending/processing when the old
-- housekeeping sweep fired is closed as failed/degraded instead of remaining
-- as a second, misleading in-progress state on a completed document.
WITH recoverable_false_failures AS (
    SELECT k.id
    FROM knowledges AS k
    WHERE k.deleted_at IS NULL
      AND k.parse_status = 'failed'
      AND k.processed_at IS NOT NULL
      AND k.error_message LIKE 'task stuck in processing > %, recovered by housekeeping'
      AND EXISTS (
          SELECT 1
          FROM chunks AS c
          WHERE c.knowledge_id = k.id
            AND c.tenant_id = k.tenant_id
            AND c.deleted_at IS NULL
            AND c.is_enabled = TRUE
      )
      AND EXISTS (
          SELECT 1
          FROM knowledge_processing_spans AS postprocess_span
          WHERE postprocess_span.knowledge_id = k.id
            AND postprocess_span.attempt = (
                SELECT MAX(latest_span.attempt)
                FROM knowledge_processing_spans AS latest_span
                WHERE latest_span.knowledge_id = k.id
            )
            AND postprocess_span.name = 'postprocess'
            AND postprocess_span.status = 'done'
      )
)
UPDATE knowledges AS k
SET parse_status = 'completed',
    pending_subtasks_count = 0,
    error_message = NULL,
    processed_at = COALESCE(k.processed_at, NOW()),
    summary_status = CASE
        WHEN k.summary_status IN ('pending', 'processing') THEN 'failed'
        ELSE k.summary_status
    END,
    updated_at = NOW()
FROM recoverable_false_failures AS r
WHERE k.id = r.id
  AND k.deleted_at IS NULL
  AND k.parse_status = 'failed'
  AND k.processed_at IS NOT NULL
  AND k.error_message LIKE 'task stuck in processing > %, recovered by housekeeping';
