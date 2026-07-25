CREATE TABLE IF NOT EXISTS custom_document_queue_schedule_groups (
    tenant_id bigint NOT NULL,
    knowledge_base_id varchar(36) NOT NULL,
    last_admitted_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, knowledge_base_id)
);

CREATE INDEX IF NOT EXISTS idx_custom_document_queue_schedule_last_admitted
    ON custom_document_queue_schedule_groups (last_admitted_at);

-- Claim admission and fair queue-position queries repeatedly group live work
-- by tenant and knowledge base. Keep that access path independent of queue
-- depth so a thousand-document upload does not turn every claim into a table
-- scan.
CREATE INDEX IF NOT EXISTS idx_custom_document_queue_group_state
    ON custom_document_queue_workflows
    (tenant_id, knowledge_base_id, state, enqueued_at, id);
