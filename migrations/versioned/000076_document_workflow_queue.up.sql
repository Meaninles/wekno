CREATE TABLE IF NOT EXISTS custom_document_queue_workflows (
    id varchar(36) PRIMARY KEY,
    tenant_id bigint NOT NULL,
    knowledge_base_id varchar(36) NOT NULL,
    knowledge_id varchar(36) NOT NULL,
    processing_generation varchar(64) NOT NULL,
    task_type varchar(64) NOT NULL,
    payload jsonb NOT NULL,
    plan_hash varchar(64) NOT NULL,
    state varchar(24) NOT NULL,
    stage varchar(32) NOT NULL DEFAULT 'preparing',
    dispatch_epoch bigint NOT NULL DEFAULT 1,
    dispatch_task_id varchar(160) NOT NULL DEFAULT '',
    dispatch_attempts integer NOT NULL DEFAULT 0,
    max_retry integer NOT NULL DEFAULT 3,
    delegate_timeout_nanos bigint NOT NULL DEFAULT 0,
    workflow_timeout_nanos bigint NOT NULL DEFAULT 0,
    deadline_at timestamptz NULL,
    retention_nanos bigint NOT NULL DEFAULT 0,
    owner_instance_id varchar(160) NOT NULL DEFAULT '',
    owner_boot_id varchar(36) NOT NULL DEFAULT '',
    lease_until timestamptz NULL,
    enqueued_at timestamptz NOT NULL,
    started_at timestamptz NULL,
    last_dispatched_at timestamptz NULL,
    last_heartbeat_at timestamptz NULL,
    last_progress_at timestamptz NULL,
    completed_at timestamptz NULL,
    last_error text NOT NULL DEFAULT '',
    version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT idx_custom_document_workflow_generation
        UNIQUE (tenant_id, knowledge_id, processing_generation)
);

CREATE INDEX IF NOT EXISTS idx_custom_document_queue_waiting
    ON custom_document_queue_workflows (state, enqueued_at, id);
CREATE INDEX IF NOT EXISTS idx_custom_document_queue_owner
    ON custom_document_queue_workflows (owner_instance_id, owner_boot_id, state);
CREATE INDEX IF NOT EXISTS idx_custom_document_queue_lease
    ON custom_document_queue_workflows (state, lease_until);

CREATE TABLE IF NOT EXISTS custom_document_queue_instances (
    instance_id varchar(160) PRIMARY KEY,
    boot_id varchar(36) NOT NULL,
    state varchar(24) NOT NULL,
    capacity integer NOT NULL,
    started_at timestamptz NOT NULL,
    last_heartbeat_at timestamptz NOT NULL,
    stopped_at timestamptz NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_custom_document_queue_instance_health
    ON custom_document_queue_instances (state, last_heartbeat_at);

-- The full workflow is prepared before the business transaction. That
-- transaction binds this immutable ID to the exact pending generation.
ALTER TABLE knowledges
    ADD COLUMN IF NOT EXISTS processing_workflow_id varchar(36) NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_knowledges_processing_workflow
    ON knowledges (processing_workflow_id)
    WHERE processing_workflow_id <> '';
