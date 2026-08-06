CREATE TABLE IF NOT EXISTS custom_dependency_capabilities (
    capability TEXT NOT NULL,
    scope TEXT NOT NULL,
    state TEXT NOT NULL,
    incident_id TEXT NOT NULL DEFAULT '',
    health_epoch INTEGER NOT NULL DEFAULT 0,
    observed_boot_epoch TEXT NOT NULL DEFAULT '',
    last_error_code TEXT NOT NULL DEFAULT '',
    last_error_message TEXT NOT NULL DEFAULT '',
    last_checked_at DATETIME NULL,
    last_healthy_at DATETIME NULL,
    blocked_at DATETIME NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (capability, scope)
);
CREATE INDEX IF NOT EXISTS idx_custom_dependency_capabilities_state
    ON custom_dependency_capabilities (state, updated_at);
