-- Migration: 000069_knowledge_name_search_indexes
-- Accelerates case-insensitive substring search over large document sets.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS idx_knowledges_file_name_trgm_active
    ON knowledges USING GIN (LOWER(file_name) gin_trgm_ops)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_knowledges_title_trgm_active
    ON knowledges USING GIN (LOWER(title) gin_trgm_ops)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_knowledges_tenant_kb_created_active
    ON knowledges (tenant_id, knowledge_base_id, created_at DESC)
    WHERE deleted_at IS NULL;
