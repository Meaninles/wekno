CREATE TABLE IF NOT EXISTS custom_processing_spans_v2 (
    knowledge_id varchar(36) NOT NULL,
    attempt integer NOT NULL,
    logical_key varchar(190) NOT NULL,
    span_id varchar(64) NOT NULL UNIQUE,
    parent_logical_key varchar(190) NOT NULL DEFAULT '',
    name varchar(160) NOT NULL,
    kind varchar(32) NOT NULL,
    status varchar(32) NOT NULL,
    real_attempt_count integer NOT NULL DEFAULT 1,
    input_summary text NOT NULL DEFAULT '',
    output_summary text NOT NULL DEFAULT '',
    last_error_code varchar(64) NOT NULL DEFAULT '',
    last_error_message text NOT NULL DEFAULT '',
    started_at timestamptz NOT NULL,
    last_business_progress_at timestamptz,
    finished_at timestamptz,
    duration_ms bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (knowledge_id, attempt, logical_key)
);

CREATE INDEX IF NOT EXISTS idx_custom_processing_spans_v2_status
    ON custom_processing_spans_v2 (status, updated_at);
CREATE INDEX IF NOT EXISTS idx_custom_processing_spans_v2_retention
    ON custom_processing_spans_v2 (finished_at, knowledge_id, attempt, logical_key)
    WHERE finished_at IS NOT NULL;
