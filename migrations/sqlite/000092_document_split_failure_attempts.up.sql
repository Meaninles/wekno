ALTER TABLE custom_document_split_parts
    ADD COLUMN failure_attempts INTEGER NOT NULL DEFAULT 0;

UPDATE custom_document_split_parts
SET failure_attempts = attempt
WHERE state = 'failed' AND failure_attempts = 0;

CREATE INDEX IF NOT EXISTS idx_custom_document_split_part_failure_budget
    ON custom_document_split_parts (plan_id, state, failure_attempts, part_index);
