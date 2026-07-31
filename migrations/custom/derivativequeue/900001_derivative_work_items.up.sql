CREATE TABLE IF NOT EXISTS custom_derivative_work_items (
    id uuid PRIMARY KEY,
    tenant_id bigint NOT NULL,
    knowledge_base_id varchar(36) NOT NULL,
    knowledge_id varchar(36) NOT NULL,
    processing_generation varchar(64) NOT NULL,
    processing_attempt integer NOT NULL DEFAULT 1,
    item_id varchar(160) NOT NULL,
    work_kind varchar(32) NOT NULL,
    payload jsonb NOT NULL,
    payload_hash varchar(64) NOT NULL,
    model_id varchar(64) NOT NULL DEFAULT '',
    model_tenant_id bigint NOT NULL DEFAULT 0,
    resource_pool_id varchar(64) NOT NULL DEFAULT '',
    quota_pool_id varchar(64) NOT NULL DEFAULT '',
    gateway_pool_id varchar(64) NOT NULL DEFAULT '',
    policy_version bigint NOT NULL DEFAULT 0,
    state varchar(32) NOT NULL,
    priority integer NOT NULL DEFAULT 0,
    queue_lane varchar(24) NOT NULL DEFAULT 'normal',
    dispatch_epoch bigint NOT NULL DEFAULT 0,
    dispatch_task_id varchar(190) NOT NULL DEFAULT '',
    owner_instance_id varchar(160) NOT NULL DEFAULT '',
    lease_token varchar(64) NOT NULL DEFAULT '',
    lease_until timestamptz,
    last_heartbeat_at timestamptz,
    provider_request_key varchar(160) NOT NULL DEFAULT '',
    provider_attempts integer NOT NULL DEFAULT 0,
    materialize_attempts integer NOT NULL DEFAULT 0,
    finalize_attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error_class varchar(32) NOT NULL DEFAULT '',
    last_error_code varchar(64) NOT NULL DEFAULT '',
    last_error_message text NOT NULL DEFAULT '',
    result_id uuid,
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CONSTRAINT uq_custom_derivative_item
        UNIQUE (tenant_id, knowledge_id, processing_generation, item_id),
    CONSTRAINT chk_custom_derivative_work_kind
        CHECK (work_kind IN ('summary', 'question_batch', 'graph_batch', 'finalizer')),
    CONSTRAINT chk_custom_derivative_state
        CHECK (state IN (
            'queued', 'leased', 'admitted', 'provider_running',
            'provider_succeeded', 'provider_unknown', 'retry_wait',
            'materializing', 'materialize_wait', 'materialized',
            'finalizing', 'finalize_wait', 'completed', 'cancelled', 'failed'
        ))
);

CREATE INDEX IF NOT EXISTS idx_custom_derivative_due
    ON custom_derivative_work_items
    (state, next_attempt_at, priority DESC, created_at, id);
CREATE INDEX IF NOT EXISTS idx_custom_derivative_pool_due
    ON custom_derivative_work_items
    (resource_pool_id, state, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_custom_derivative_scope
    ON custom_derivative_work_items
    (tenant_id, knowledge_base_id, state, created_at);
CREATE INDEX IF NOT EXISTS idx_custom_derivative_generation
    ON custom_derivative_work_items
    (knowledge_id, processing_generation, state);
CREATE INDEX IF NOT EXISTS idx_custom_derivative_lease
    ON custom_derivative_work_items
    (state, lease_until);
CREATE INDEX IF NOT EXISTS idx_custom_derivative_provider_request
    ON custom_derivative_work_items (provider_request_key)
    WHERE provider_request_key <> '';

CREATE TABLE IF NOT EXISTS custom_derivative_provider_calls (
    id uuid PRIMARY KEY,
    work_item_id uuid NOT NULL,
    request_hash varchar(64) NOT NULL,
    provider_request_key varchar(190) NOT NULL UNIQUE,
    model_id varchar(64) NOT NULL DEFAULT '',
    response jsonb NOT NULL,
    response_size bigint NOT NULL DEFAULT 0,
    content_checksum varchar(64) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_custom_derivative_provider_call
        UNIQUE (work_item_id, request_hash),
    CONSTRAINT fk_custom_derivative_provider_call_work_item
        FOREIGN KEY (work_item_id)
        REFERENCES custom_derivative_work_items (id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_custom_derivative_provider_calls_work_item
    ON custom_derivative_provider_calls (work_item_id, request_hash);

CREATE TABLE IF NOT EXISTS custom_derivative_results (
    id uuid PRIMARY KEY,
    work_item_id uuid NOT NULL UNIQUE,
    provider_request_key varchar(160) NOT NULL UNIQUE,
    model_id varchar(64) NOT NULL DEFAULT '',
    resource_pool_id varchar(64) NOT NULL DEFAULT '',
    response_content text NOT NULL DEFAULT '',
    response_uri text NOT NULL DEFAULT '',
    response_size bigint NOT NULL DEFAULT 0,
    response_usage jsonb NOT NULL DEFAULT '{}',
    response_metadata jsonb NOT NULL DEFAULT '{}',
    content_checksum varchar(64) NOT NULL,
    provider_request_id varchar(160) NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    materialized_at timestamptz,
    expires_at timestamptz NOT NULL,
    CONSTRAINT fk_custom_derivative_result_work_item
        FOREIGN KEY (work_item_id)
        REFERENCES custom_derivative_work_items (id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_custom_derivative_results_expiry
    ON custom_derivative_results (expires_at, id);
