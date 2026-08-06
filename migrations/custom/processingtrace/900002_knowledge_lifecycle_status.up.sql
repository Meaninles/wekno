ALTER TABLE knowledges
    ADD COLUMN IF NOT EXISTS core_status varchar(32) NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS core_completed_at timestamptz NULL,
    ADD COLUMN IF NOT EXISTS enrichment_completed_at timestamptz NULL,
    ADD COLUMN IF NOT EXISTS enrichment_error_summary text NOT NULL DEFAULT '';

UPDATE knowledges
SET core_status = CASE
        WHEN parse_status IN ('completed', 'finalizing') THEN 'ready'
        WHEN parse_status = 'failed' THEN 'failed'
        WHEN parse_status = 'processing' THEN 'processing'
        ELSE 'pending'
    END,
    core_completed_at = CASE
        WHEN parse_status IN ('completed', 'finalizing')
            THEN COALESCE(processed_at, updated_at, created_at)
        ELSE NULL
    END,
    enrichment_completed_at = CASE
        WHEN enrichment_status IN ('completed', 'degraded', 'failed')
            THEN COALESCE(processed_at, updated_at, created_at)
        ELSE NULL
    END;

CREATE INDEX IF NOT EXISTS idx_knowledges_core_retrieval
    ON knowledges (tenant_id, knowledge_base_id, core_status, id)
    WHERE deleted_at IS NULL AND enable_status = 'enabled';
