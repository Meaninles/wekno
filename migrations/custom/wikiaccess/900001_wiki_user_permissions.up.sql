-- Per-user Wiki selection grants. Missing rows are denied by design.
CREATE TABLE IF NOT EXISTS custom_wiki_user_permissions (
    user_id VARCHAR(36) PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    granted_by VARCHAR(36) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

