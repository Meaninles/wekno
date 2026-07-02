-- Custom extension migration reference for IAM organization/person sync.
-- Runtime development environments apply these tables through GORM AutoMigrate
-- from internal/custom/bootstrap to keep native migration wiring untouched.

CREATE TABLE IF NOT EXISTS custom_iam_sync_settings (
    id BIGINT PRIMARY KEY DEFAULT 1,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    base_url VARCHAR(512),
    login_client_id VARCHAR(255),
    login_client_secret TEXT,
    sync_client_id VARCHAR(255),
    sync_client_secret TEXT,
    schedule_mode VARCHAR(16) NOT NULL DEFAULT 'daily',
    weekdays VARCHAR(32) NOT NULL DEFAULT '',
    run_at VARCHAR(8) NOT NULL DEFAULT '03:10',
    last_run_at TIMESTAMPTZ,
    last_status VARCHAR(32),
    last_message TEXT,
    last_run_triggered_by VARCHAR(64),
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);

COMMENT ON COLUMN custom_iam_sync_settings.login_client_secret IS 'Stored as enc:v1 AES-GCM ciphertext when SYSTEM_AES_KEY is configured.';
COMMENT ON COLUMN custom_iam_sync_settings.sync_client_secret IS 'Stored as enc:v1 AES-GCM ciphertext when SYSTEM_AES_KEY is configured.';

CREATE TABLE IF NOT EXISTS custom_iam_organizations (
    id VARCHAR(36) PRIMARY KEY,
    external_id VARCHAR(128) NOT NULL UNIQUE,
    external_business_id VARCHAR(128),
    code VARCHAR(128),
    name VARCHAR(255) NOT NULL,
    parent_external_id VARCHAR(128),
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    sequence VARCHAR(128),
    external_updated_at VARCHAR(64),
    raw JSONB,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS custom_iam_users (
    id VARCHAR(36) PRIMARY KEY,
    external_id VARCHAR(128) NOT NULL UNIQUE,
    external_account_id VARCHAR(128),
    username VARCHAR(255),
    display_name VARCHAR(255),
    organization_external_id VARCHAR(128),
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    weknora_user_id VARCHAR(36),
    external_updated_at VARCHAR(64),
    raw JSONB,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS custom_iam_sync_runs (
    id VARCHAR(36) PRIMARY KEY,
    triggered_by VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL,
    message TEXT,
    org_count INTEGER NOT NULL DEFAULT 0,
    user_count INTEGER NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_users INTEGER NOT NULL DEFAULT 0,
    updated_users INTEGER NOT NULL DEFAULT 0,
    disabled_users INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);
