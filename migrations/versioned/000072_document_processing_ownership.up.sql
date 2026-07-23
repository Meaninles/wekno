-- A content checksum cannot identify a processing attempt: reparsing the same
-- object keeps file_hash unchanged. These fields provide a durable generation,
-- an exclusive core-worker owner, and a replayable post-commit fan-out plan.
ALTER TABLE knowledges
    ADD COLUMN IF NOT EXISTS processing_generation VARCHAR(36) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS processing_owner VARCHAR(160) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS processing_fanout JSONB;

CREATE INDEX IF NOT EXISTS idx_knowledges_processing_generation
    ON knowledges (tenant_id, knowledge_base_id, id, processing_generation);

-- Rows already active when this migration starts have Redis tasks whose
-- payloads cannot prove a generation/owner. New workers must never ACK those
-- tasks as harmlessly stale while leaving the row Pending/Processing forever.
-- Install a deterministic non-empty repair generation and close each legacy
-- lifecycle explicitly. A core-committed row is usable only when processed_at
-- and at least one live chunk agree; optional enrichment is degraded. Every
-- other row is failed visibly and requires an operator/user reparse.
WITH legacy_active AS (
    SELECT
        k.id,
        k.tenant_id,
        (
            k.parse_status <> 'pending'
            AND k.processed_at IS NOT NULL
            AND EXISTS (
                SELECT 1
                FROM chunks AS c
                WHERE c.knowledge_id = k.id
                  AND c.tenant_id = k.tenant_id
                  AND c.deleted_at IS NULL
            )
        ) AS core_usable
    FROM knowledges AS k
    WHERE k.deleted_at IS NULL
      AND COALESCE(k.processing_generation, '') = ''
      AND COALESCE(k.processing_owner, '') = ''
      AND k.parse_status IN ('pending', 'processing', 'finalizing')
)
UPDATE knowledges AS k
SET processing_generation =
        SUBSTR(MD5('legacy-processing-repair:' || k.tenant_id::text || ':' || k.id), 1, 8) || '-' ||
        SUBSTR(MD5('legacy-processing-repair:' || k.tenant_id::text || ':' || k.id), 9, 4) || '-' ||
        SUBSTR(MD5('legacy-processing-repair:' || k.tenant_id::text || ':' || k.id), 13, 4) || '-' ||
        SUBSTR(MD5('legacy-processing-repair:' || k.tenant_id::text || ':' || k.id), 17, 4) || '-' ||
        SUBSTR(MD5('legacy-processing-repair:' || k.tenant_id::text || ':' || k.id), 21, 12),
    processing_owner = '',
    processing_fanout = NULL,
    parse_status = CASE WHEN legacy.core_usable THEN 'completed' ELSE 'failed' END,
    pending_subtasks_count = 0,
    error_message = CASE
        WHEN legacy.core_usable THEN ''
        ELSE 'legacy processing task lacked ownership after upgrade; reparse is required'
    END,
    summary_status = CASE
        WHEN k.summary_status IN ('pending', 'processing') THEN 'failed'
        ELSE k.summary_status
    END,
    updated_at = NOW()
FROM legacy_active AS legacy
WHERE k.id = legacy.id
  AND k.tenant_id = legacy.tenant_id
  AND k.deleted_at IS NULL
  AND COALESCE(k.processing_generation, '') = ''
  AND COALESCE(k.processing_owner, '') = ''
  AND k.parse_status IN ('pending', 'processing', 'finalizing');
