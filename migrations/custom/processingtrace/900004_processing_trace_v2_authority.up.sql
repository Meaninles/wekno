ALTER TABLE custom_processing_spans_v2
    ADD COLUMN IF NOT EXISTS metadata_summary text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_error_detail text NOT NULL DEFAULT '';

ALTER TABLE custom_processing_spans_v2
    ALTER COLUMN real_attempt_count SET DEFAULT 0;

-- Span V2 is the sole runtime authority. Historical versioned migrations are
-- intentionally left immutable; fresh and upgraded installations converge by
-- removing the physical-delivery table here.
DROP TABLE IF EXISTS knowledge_processing_spans;
