CREATE TABLE IF NOT EXISTS custom_kbmanager_operations (
    id VARCHAR(36) PRIMARY KEY,
    agent_id VARCHAR(36) NOT NULL,
    agent_tenant_id BIGINT NOT NULL,
    session_id VARCHAR(36),
    run_id VARCHAR(80),
    caller_tenant_id BIGINT NOT NULL,
    source_tenant_id BIGINT NOT NULL,
    user_id VARCHAR(128) NOT NULL,
    caller_role VARCHAR(20) NOT NULL,
    type VARCHAR(20) NOT NULL,
    state VARCHAR(32) NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    old_knowledge_id VARCHAR(36),
    new_knowledge_id VARCHAR(36),
    old_file_hash VARCHAR(128),
    source_kind VARCHAR(24),
    source_id VARCHAR(255),
    source_sha256 VARCHAR(64),
    file_name VARCHAR(255),
    reason TEXT,
    result_message TEXT,
    error_message TEXT,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_agent_id ON custom_kbmanager_operations (agent_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_agent_tenant_id ON custom_kbmanager_operations (agent_tenant_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_session_id ON custom_kbmanager_operations (session_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_run_id ON custom_kbmanager_operations (run_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_caller_tenant_id ON custom_kbmanager_operations (caller_tenant_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_source_tenant_id ON custom_kbmanager_operations (source_tenant_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_user_id ON custom_kbmanager_operations (user_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_state ON custom_kbmanager_operations (state);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_knowledge_base_id ON custom_kbmanager_operations (knowledge_base_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_old_knowledge_id ON custom_kbmanager_operations (old_knowledge_id);
CREATE INDEX IF NOT EXISTS idx_custom_kbmanager_operations_new_knowledge_id ON custom_kbmanager_operations (new_knowledge_id);
