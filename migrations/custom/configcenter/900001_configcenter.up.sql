-- Custom extension migration reference for the default configuration center.
-- Runtime development environments apply these tables through GORM AutoMigrate
-- from internal/custom/bootstrap to keep native migration wiring untouched.

CREATE TABLE IF NOT EXISTS custom_config_default_grants (
    id VARCHAR(36) PRIMARY KEY,
    resource_type VARCHAR(64) NOT NULL,
    source_tenant_id BIGINT NOT NULL,
    source_resource_id VARCHAR(128) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS custom_config_user_grants (
    id VARCHAR(36) PRIMARY KEY,
    user_id VARCHAR(36) NOT NULL,
    resource_type VARCHAR(64) NOT NULL,
    source_tenant_id BIGINT NOT NULL,
    source_resource_id VARCHAR(128) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS custom_config_managed_copies (
    id VARCHAR(36) PRIMARY KEY,
    user_id VARCHAR(36) NOT NULL,
    target_tenant_id BIGINT NOT NULL,
    resource_type VARCHAR(64) NOT NULL,
    source_tenant_id BIGINT NOT NULL,
    source_resource_id VARCHAR(128) NOT NULL,
    target_resource_id VARCHAR(128) NOT NULL,
    source_hash VARCHAR(64) NOT NULL DEFAULT '',
    status VARCHAR(32) NOT NULL DEFAULT 'active',
    last_applied_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);
