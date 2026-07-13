-- Rollback: remove large-document search indexes.

DROP INDEX IF EXISTS idx_knowledges_tenant_kb_created_active;
DROP INDEX IF EXISTS idx_knowledges_title_trgm_active;
DROP INDEX IF EXISTS idx_knowledges_file_name_trgm_active;
