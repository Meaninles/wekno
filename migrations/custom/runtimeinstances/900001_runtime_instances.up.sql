CREATE TABLE IF NOT EXISTS custom_runtime_instances (
    instance_id varchar(200) PRIMARY KEY,
    boot_id varchar(36) NOT NULL,
    role varchar(32) NOT NULL,
    state varchar(24) NOT NULL,
    derivative_concurrency integer NOT NULL DEFAULT 0,
    wiki_concurrency integer NOT NULL DEFAULT 0,
    parse_concurrency integer NOT NULL DEFAULT 0,
    started_at timestamptz NOT NULL,
    last_heartbeat_at timestamptz NOT NULL,
    stopped_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_custom_runtime_instances_live
    ON custom_runtime_instances (role, state, last_heartbeat_at);
