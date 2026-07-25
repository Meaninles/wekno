CREATE UNIQUE INDEX IF NOT EXISTS uq_kpspan_one_root_per_attempt
    ON knowledge_processing_spans (knowledge_id, attempt)
    WHERE kind = 'root';
