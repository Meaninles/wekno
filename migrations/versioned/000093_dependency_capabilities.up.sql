CREATE TABLE IF NOT EXISTS custom_dependency_capabilities (
    capability varchar(64) NOT NULL,
    scope varchar(160) NOT NULL,
    state varchar(24) NOT NULL,
    incident_id varchar(64) NOT NULL DEFAULT '',
    health_epoch bigint NOT NULL DEFAULT 0,
    observed_boot_epoch varchar(128) NOT NULL DEFAULT '',
    last_error_code varchar(96) NOT NULL DEFAULT '',
    last_error_message text NOT NULL DEFAULT '',
    last_checked_at timestamptz NULL,
    last_healthy_at timestamptz NULL,
    blocked_at timestamptz NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (capability, scope)
);

CREATE INDEX IF NOT EXISTS idx_custom_dependency_capabilities_state
    ON custom_dependency_capabilities (state, updated_at);
CREATE INDEX IF NOT EXISTS idx_custom_dependency_capabilities_incident
    ON custom_dependency_capabilities (incident_id)
    WHERE incident_id <> '';
