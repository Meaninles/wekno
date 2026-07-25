-- A document-processing attempt is represented by exactly one root span.
-- The application also serializes allocation with a PostgreSQL advisory
-- transaction lock; this partial unique index is the final database-level
-- fence against duplicate roots if a future caller bypasses that allocator.
CREATE UNIQUE INDEX IF NOT EXISTS uq_kpspan_one_root_per_attempt
    ON knowledge_processing_spans (knowledge_id, attempt)
    WHERE kind = 'root';
