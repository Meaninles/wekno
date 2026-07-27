-- Platform derivative-model control plane.
-- Run on the single maintenance replica before serving replicas start.

ALTER TABLE models
    ADD COLUMN IF NOT EXISTS workload_scope varchar(32);

UPDATE models
SET workload_scope = 'interactive'
WHERE workload_scope IS NULL OR workload_scope = '';

ALTER TABLE models
    ALTER COLUMN workload_scope SET DEFAULT 'interactive',
    ALTER COLUMN workload_scope SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'chk_models_workload_scope'
    ) THEN
        ALTER TABLE models
            ADD CONSTRAINT chk_models_workload_scope
            CHECK (workload_scope IN ('interactive', 'derivative_only'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_models_workload_scope
    ON models (workload_scope)
    WHERE deleted_at IS NULL;

ALTER TABLE knowledge_bases
    ADD COLUMN IF NOT EXISTS derivative_model_id varchar(64) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_knowledge_bases_derivative_model
    ON knowledge_bases (derivative_model_id)
    WHERE deleted_at IS NULL AND derivative_model_id <> '';

CREATE TABLE IF NOT EXISTS custom_derivative_control_configs (
    id bigint PRIMARY KEY,
    default_model_id varchar(64) NOT NULL DEFAULT '',
    version bigint NOT NULL DEFAULT 0,
    updated_by varchar(36) NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_custom_derivative_config_singleton CHECK (id = 1)
);

INSERT INTO custom_derivative_control_configs (
    id, default_model_id, version, updated_by, created_at, updated_at
) VALUES (1, '', 0, '', now(), now())
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS custom_derivative_model_assignments (
    model_id varchar(64) PRIMARY KEY,
    model_tenant_id bigint NOT NULL,
    published_by varchar(36) NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_custom_derivative_assignments_tenant
    ON custom_derivative_model_assignments (model_tenant_id);
