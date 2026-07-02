-- Custom extension migration reference for MySQL/PostgreSQL database analytics.
-- Runtime development environments apply these tables through GORM AutoMigrate
-- from internal/custom/bootstrap to keep native migration wiring untouched.

CREATE TABLE IF NOT EXISTS custom_db_sources (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    type VARCHAR(32) NOT NULL,
    config JSONB,
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    error_message TEXT,
    query_mode VARCHAR(32) NOT NULL DEFAULT 'live',
    max_rows INTEGER NOT NULL DEFAULT 1000,
    max_scan_rows INTEGER NOT NULL DEFAULT 50000,
    timeout_seconds INTEGER NOT NULL DEFAULT 30,
    created_by VARCHAR(36),
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS custom_db_source_tables (
    id VARCHAR(36) PRIMARY KEY,
    source_id VARCHAR(36) NOT NULL,
    tenant_id BIGINT NOT NULL,
    schema_name VARCHAR(255) NOT NULL,
    table_name VARCHAR(255) NOT NULL,
    object_type VARCHAR(32) NOT NULL,
    virtual_name VARCHAR(255) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    row_estimate BIGINT,
    description TEXT,
    last_profiled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    UNIQUE (source_id, schema_name, table_name)
);

CREATE TABLE IF NOT EXISTS custom_db_source_columns (
    id VARCHAR(36) PRIMARY KEY,
    table_id VARCHAR(36) NOT NULL,
    source_id VARCHAR(36) NOT NULL,
    tenant_id BIGINT NOT NULL,
    column_name VARCHAR(255) NOT NULL,
    data_type VARCHAR(255) NOT NULL,
    nullable BOOLEAN,
    ordinal INTEGER,
    description TEXT,
    sample_values JSONB,
    semantic_type VARCHAR(32),
    sensitive_level VARCHAR(32) NOT NULL DEFAULT 'none',
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    UNIQUE (table_id, column_name)
);

CREATE TABLE IF NOT EXISTS custom_db_table_relations (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    source_id VARCHAR(36) NOT NULL,
    from_table_id VARCHAR(36) NOT NULL,
    from_column VARCHAR(255) NOT NULL,
    to_table_id VARCHAR(36) NOT NULL,
    to_column VARCHAR(255) NOT NULL,
    description TEXT,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS custom_db_agent_bindings (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    agent_id VARCHAR(36) NOT NULL,
    source_id VARCHAR(36) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    UNIQUE (tenant_id, agent_id, source_id)
);

CREATE TABLE IF NOT EXISTS custom_db_source_shares (
    id VARCHAR(36) PRIMARY KEY,
    source_id VARCHAR(36) NOT NULL,
    organization_id VARCHAR(36) NOT NULL,
    shared_by_user_id VARCHAR(36) NOT NULL,
    source_tenant_id BIGINT NOT NULL,
    permission VARCHAR(32) NOT NULL DEFAULT 'viewer',
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_custom_db_source_shares_source_id ON custom_db_source_shares (source_id);
CREATE INDEX IF NOT EXISTS idx_custom_db_source_shares_organization_id ON custom_db_source_shares (organization_id);
CREATE INDEX IF NOT EXISTS idx_custom_db_source_shares_source_tenant_id ON custom_db_source_shares (source_tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_custom_db_source_share_live
    ON custom_db_source_shares (source_id, source_tenant_id, organization_id)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS custom_db_query_audits (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    user_id VARCHAR(36),
    agent_id VARCHAR(36),
    source_id VARCHAR(36),
    original_sql TEXT,
    executed_sql TEXT,
    query_mode VARCHAR(32),
    chart_requested BOOLEAN,
    duration_ms BIGINT,
    row_count INTEGER,
    success BOOLEAN,
    error_message TEXT,
    created_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);
