CREATE TABLE IF NOT EXISTS knowledge_processing_spans (
    id bigserial PRIMARY KEY,
    knowledge_id varchar(36) NOT NULL,
    attempt integer NOT NULL DEFAULT 1,
    span_id varchar(64) NOT NULL,
    parent_span_id varchar(64) NOT NULL DEFAULT '',
    name varchar(64) NOT NULL,
    kind varchar(16) NOT NULL,
    status varchar(16) NOT NULL,
    input jsonb,
    output jsonb,
    metadata jsonb,
    error_code varchar(64) NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',
    error_detail text NOT NULL DEFAULT '',
    started_at timestamptz,
    finished_at timestamptz,
    duration_ms bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (knowledge_id, attempt, span_id)
);

ALTER TABLE custom_processing_spans_v2
    ALTER COLUMN real_attempt_count SET DEFAULT 1;
