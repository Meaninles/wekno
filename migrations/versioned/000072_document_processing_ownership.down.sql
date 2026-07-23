DROP INDEX IF EXISTS idx_knowledges_processing_generation;

ALTER TABLE knowledges
    DROP COLUMN IF EXISTS processing_fanout,
    DROP COLUMN IF EXISTS processing_owner,
    DROP COLUMN IF EXISTS processing_generation;
