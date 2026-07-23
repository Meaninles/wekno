CREATE TABLE IF NOT EXISTS custom_document_split_plans (
    id varchar(36) PRIMARY KEY,
    tenant_id bigint NOT NULL,
    knowledge_base_id varchar(36) NOT NULL,
    knowledge_id varchar(36) NOT NULL,
    processing_generation varchar(64) NOT NULL,
    processing_owner varchar(160) NOT NULL,
    source_path text NOT NULL,
    source_name text NOT NULL,
    source_type varchar(32) NOT NULL,
    source_size bigint NOT NULL,
    source_sha256 varchar(64) NOT NULL,
    planner_version varchar(64) NOT NULL DEFAULT '',
    rules_hash varchar(64) NOT NULL DEFAULT '',
    strategy varchar(64) NOT NULL DEFAULT '',
    state varchar(24) NOT NULL,
    part_count integer NOT NULL DEFAULT 0,
    completed_parts integer NOT NULL DEFAULT 0,
    failed_parts integer NOT NULL DEFAULT 0,
    total_part_bytes bigint NOT NULL DEFAULT 0,
    target_ratio double precision NOT NULL DEFAULT 0.75,
    attempt integer NOT NULL DEFAULT 1,
    last_error text NOT NULL DEFAULT '',
    last_progress_at timestamptz NOT NULL,
    finalizer_task_id varchar(180) NOT NULL DEFAULT '',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT idx_custom_document_split_generation
        UNIQUE (tenant_id, knowledge_id, processing_generation)
);

CREATE INDEX IF NOT EXISTS idx_custom_document_split_plan_state
    ON custom_document_split_plans (state, last_progress_at, id);
CREATE INDEX IF NOT EXISTS idx_custom_document_split_plan_knowledge
    ON custom_document_split_plans (tenant_id, knowledge_base_id, knowledge_id);

CREATE TABLE IF NOT EXISTS custom_document_split_parts (
    id varchar(36) PRIMARY KEY,
    plan_id varchar(36) NOT NULL,
    tenant_id bigint NOT NULL,
    knowledge_base_id varchar(36) NOT NULL,
    knowledge_id varchar(36) NOT NULL,
    processing_generation varchar(64) NOT NULL,
    part_index integer NOT NULL,
    file_name text NOT NULL,
    file_type varchar(32) NOT NULL,
    input_path text NOT NULL,
    input_size bigint NOT NULL,
    input_sha256 varchar(64) NOT NULL,
    locator jsonb NOT NULL,
    metrics jsonb NOT NULL,
    state varchar(24) NOT NULL,
    attempt integer NOT NULL DEFAULT 0,
    lease_epoch bigint NOT NULL DEFAULT 0,
    lease_owner varchar(160) NOT NULL DEFAULT '',
    lease_until timestamptz NULL,
    output_path text NOT NULL DEFAULT '',
    output_size bigint NOT NULL DEFAULT 0,
    output_sha256 varchar(64) NOT NULL DEFAULT '',
    markdown_chars bigint NOT NULL DEFAULT 0,
    draft_chunks integer NOT NULL DEFAULT 0,
    storage_bytes bigint NOT NULL DEFAULT 0,
    first_chunk_id varchar(36) NOT NULL DEFAULT '',
    last_chunk_id varchar(36) NOT NULL DEFAULT '',
    image_mappings jsonb NULL,
    last_error text NOT NULL DEFAULT '',
    last_progress_at timestamptz NOT NULL,
    completed_at timestamptz NULL,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT idx_custom_document_split_part UNIQUE (plan_id, part_index),
    CONSTRAINT fk_custom_document_split_part_plan
        FOREIGN KEY (plan_id) REFERENCES custom_document_split_plans(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_custom_document_split_part_state
    ON custom_document_split_parts (plan_id, state, part_index);
CREATE INDEX IF NOT EXISTS idx_custom_document_split_part_lease
    ON custom_document_split_parts (state, lease_until);
CREATE INDEX IF NOT EXISTS idx_custom_document_split_part_knowledge
    ON custom_document_split_parts (tenant_id, knowledge_base_id, knowledge_id, processing_generation);

ALTER TABLE chunks
    ADD COLUMN IF NOT EXISTS processing_generation varchar(36) NOT NULL DEFAULT '';
ALTER TABLE chunks
    ADD COLUMN IF NOT EXISTS split_part_index integer NOT NULL DEFAULT -1;
ALTER TABLE chunks
    ADD COLUMN IF NOT EXISTS source_locator jsonb NULL;

-- Physical parts retain a monotonic logical coordinate with deliberately
-- sparse strides. The original INTEGER columns overflow after only a few
-- large parts, while BIGINT safely covers the configured 10,000-part ceiling
-- and remains exactly representable by browser numbers at that range.
ALTER TABLE chunks
    ALTER COLUMN chunk_index TYPE bigint USING chunk_index::bigint,
    ALTER COLUMN start_at TYPE bigint USING start_at::bigint,
    ALTER COLUMN end_at TYPE bigint USING end_at::bigint;

CREATE INDEX IF NOT EXISTS idx_chunks_split_generation
    ON chunks (knowledge_id, processing_generation, split_part_index);
CREATE INDEX IF NOT EXISTS idx_chunks_split_logical_order
    ON chunks (tenant_id, knowledge_id, processing_generation, chunk_index, id);
CREATE INDEX IF NOT EXISTS idx_chunks_split_logical_type
    ON chunks (tenant_id, knowledge_id, processing_generation, chunk_type, chunk_index, id);
