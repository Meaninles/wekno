DROP INDEX IF EXISTS idx_knowledges_core_retrieval;
ALTER TABLE knowledges
    DROP COLUMN IF EXISTS enrichment_error_summary,
    DROP COLUMN IF EXISTS enrichment_completed_at,
    DROP COLUMN IF EXISTS core_completed_at,
    DROP COLUMN IF EXISTS core_status;
