DROP INDEX IF EXISTS idx_chunks_split_logical_type;
DROP INDEX IF EXISTS idx_chunks_split_logical_order;
DROP INDEX IF EXISTS idx_chunks_split_generation;
ALTER TABLE chunks DROP COLUMN IF EXISTS source_locator;
ALTER TABLE chunks DROP COLUMN IF EXISTS split_part_index;
ALTER TABLE chunks DROP COLUMN IF EXISTS processing_generation;
DROP TABLE IF EXISTS custom_document_split_parts;
DROP TABLE IF EXISTS custom_document_split_plans;
