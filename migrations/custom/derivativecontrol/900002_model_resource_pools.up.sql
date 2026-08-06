-- Model admission control-plane v2.
-- PostgreSQL is authoritative for policy; Redis only stores short-lived
-- admission leases, rate windows, and circuit state.

CREATE TABLE IF NOT EXISTS custom_model_resource_pools (
    id varchar(64) PRIMARY KEY,
    name varchar(128) NOT NULL,
    resource_kind varchar(32) NOT NULL,
    max_inflight integer NOT NULL,
    max_background_inflight integer NOT NULL,
    interactive_reserve integer NOT NULL,
    tenant_guaranteed integer NOT NULL DEFAULT 1,
    tenant_burst integer NOT NULL,
    document_guaranteed integer NOT NULL DEFAULT 1,
    document_burst integer NOT NULL DEFAULT 2,
    rpm integer NOT NULL DEFAULT 0,
    tpm bigint NOT NULL DEFAULT 0,
    token_burst bigint NOT NULL DEFAULT 0,
    request_timeout_seconds integer NOT NULL DEFAULT 900,
    circuit_threshold integer NOT NULL DEFAULT 3,
    circuit_window_seconds integer NOT NULL DEFAULT 600,
    circuit_open_seconds integer NOT NULL DEFAULT 60,
    state varchar(24) NOT NULL DEFAULT 'enabled',
    policy_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_custom_model_resource_pool_state
        CHECK (state IN ('enabled', 'draining', 'disabled')),
    CONSTRAINT chk_custom_model_resource_pool_limits
        CHECK (
            max_inflight > 0
            AND max_background_inflight BETWEEN 0 AND max_inflight
            AND interactive_reserve BETWEEN 0 AND max_inflight
            AND tenant_burst BETWEEN 1 AND max_inflight
            AND document_burst BETWEEN 1 AND max_inflight
        )
);

CREATE INDEX IF NOT EXISTS idx_custom_model_resource_pools_kind
    ON custom_model_resource_pools (resource_kind, state, id);

CREATE TABLE IF NOT EXISTS custom_model_quota_pools (
    id varchar(64) PRIMARY KEY,
    name varchar(128) NOT NULL,
    rpm integer NOT NULL DEFAULT 0,
    tpm bigint NOT NULL DEFAULT 0,
    token_burst bigint NOT NULL DEFAULT 0,
    state varchar(24) NOT NULL DEFAULT 'enabled',
    policy_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_custom_model_quota_pool_state
        CHECK (state IN ('enabled', 'draining', 'disabled'))
);

CREATE TABLE IF NOT EXISTS custom_model_gateway_pools (
    id varchar(64) PRIMARY KEY,
    name varchar(128) NOT NULL,
    max_inflight integer NOT NULL DEFAULT 32,
    state varchar(24) NOT NULL DEFAULT 'enabled',
    policy_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_custom_model_gateway_pool_state
        CHECK (state IN ('enabled', 'draining', 'disabled'))
);

CREATE TABLE IF NOT EXISTS custom_model_resource_bindings (
    model_id varchar(64) NOT NULL,
    model_tenant_id bigint NOT NULL,
    resource_pool_id varchar(64) NOT NULL,
    quota_pool_id varchar(64) NOT NULL DEFAULT '',
    gateway_pool_id varchar(64) NOT NULL DEFAULT '',
    route_fingerprint varchar(64) NOT NULL,
    binding_version bigint NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (model_id, model_tenant_id),
    CONSTRAINT fk_custom_model_binding_resource_pool
        FOREIGN KEY (resource_pool_id)
        REFERENCES custom_model_resource_pools (id)
);

CREATE INDEX IF NOT EXISTS idx_custom_model_bindings_resource
    ON custom_model_resource_bindings (resource_pool_id, model_tenant_id, model_id);
CREATE INDEX IF NOT EXISTS idx_custom_model_bindings_quota
    ON custom_model_resource_bindings (quota_pool_id)
    WHERE quota_pool_id <> '';
CREATE INDEX IF NOT EXISTS idx_custom_model_bindings_gateway
    ON custom_model_resource_bindings (gateway_pool_id)
    WHERE gateway_pool_id <> '';
CREATE INDEX IF NOT EXISTS idx_custom_model_bindings_route
    ON custom_model_resource_bindings (route_fingerprint);

CREATE TABLE IF NOT EXISTS custom_model_admission_templates (
    kind varchar(32) PRIMARY KEY,
    max_inflight integer NOT NULL,
    max_background_inflight integer NOT NULL,
    interactive_reserve integer NOT NULL,
    tenant_burst integer NOT NULL,
    document_burst integer NOT NULL,
    rpm integer NOT NULL DEFAULT 0,
    tpm bigint NOT NULL DEFAULT 0,
    request_timeout_seconds integer NOT NULL DEFAULT 900,
    circuit_threshold integer NOT NULL DEFAULT 3,
    circuit_window_seconds integer NOT NULL DEFAULT 600,
    circuit_open_seconds integer NOT NULL DEFAULT 60,
    policy_version bigint NOT NULL DEFAULT 1,
    updated_by varchar(36) NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS custom_model_admission_audits (
    id bigserial PRIMARY KEY,
    actor_id varchar(36) NOT NULL DEFAULT '',
    action varchar(48) NOT NULL,
    resource_type varchar(32) NOT NULL,
    resource_id varchar(64) NOT NULL,
    old_value text NOT NULL DEFAULT '{}',
    new_value text NOT NULL DEFAULT '{}',
    policy_version bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_custom_model_admission_audits_lookup
    ON custom_model_admission_audits (resource_type, resource_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_custom_model_admission_audits_created
    ON custom_model_admission_audits (created_at, id);
