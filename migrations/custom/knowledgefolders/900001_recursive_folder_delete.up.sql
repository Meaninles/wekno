ALTER TABLE custom_knowledge_folders
    ADD COLUMN IF NOT EXISTS delete_status VARCHAR(24) NOT NULL DEFAULT '';

ALTER TABLE custom_knowledge_folders
    ADD COLUMN IF NOT EXISTS delete_operation_id VARCHAR(36) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_custom_knowledge_folders_delete_status
    ON custom_knowledge_folders (delete_status);

CREATE INDEX IF NOT EXISTS idx_custom_knowledge_folders_delete_operation_id
    ON custom_knowledge_folders (delete_operation_id);

CREATE TABLE IF NOT EXISTS custom_knowledge_folder_delete_operations (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    root_folder_id VARCHAR(36) NOT NULL,
    root_folder_name VARCHAR(255) NOT NULL,
    parent_folder_id VARCHAR(36) NOT NULL DEFAULT '',
    requested_by VARCHAR(36) NOT NULL DEFAULT '',
    status VARCHAR(24) NOT NULL,
    total_document_count BIGINT NOT NULL DEFAULT 0,
    deleted_document_count BIGINT NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    dispatched_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_custom_folder_delete_scope
    ON custom_knowledge_folder_delete_operations (tenant_id, knowledge_base_id);

CREATE INDEX IF NOT EXISTS idx_custom_folder_delete_operations_root_folder_id
    ON custom_knowledge_folder_delete_operations (root_folder_id);

CREATE INDEX IF NOT EXISTS idx_custom_folder_delete_operations_status
    ON custom_knowledge_folder_delete_operations (status);

CREATE TABLE IF NOT EXISTS custom_knowledge_folder_delete_items (
    operation_id VARCHAR(36) NOT NULL,
    knowledge_id VARCHAR(36) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (operation_id, knowledge_id)
);

CREATE INDEX IF NOT EXISTS idx_custom_folder_delete_items_knowledge_id
    ON custom_knowledge_folder_delete_items (knowledge_id);
