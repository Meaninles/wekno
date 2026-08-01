CREATE TABLE IF NOT EXISTS custom_model_scheduler_policies (
    id bigint PRIMARY KEY,
    prefetch_factor integer NOT NULL DEFAULT 2,
    derivative_weight integer NOT NULL DEFAULT 2,
    wiki_weight integer NOT NULL DEFAULT 1,
    background_max_wait_seconds integer NOT NULL DEFAULT 30,
    dispatch_lease_seconds integer NOT NULL DEFAULT 120,
    policy_version bigint NOT NULL DEFAULT 1,
    updated_by varchar(36) NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT chk_custom_model_scheduler_policy_singleton CHECK (id = 1),
    CONSTRAINT chk_custom_model_scheduler_prefetch CHECK (prefetch_factor BETWEEN 1 AND 8),
    CONSTRAINT chk_custom_model_scheduler_weights CHECK (
        derivative_weight BETWEEN 1 AND 100 AND wiki_weight BETWEEN 1 AND 100
    ),
    CONSTRAINT chk_custom_model_scheduler_wait CHECK (background_max_wait_seconds BETWEEN 1 AND 300),
    CONSTRAINT chk_custom_model_scheduler_dispatch CHECK (dispatch_lease_seconds BETWEEN 30 AND 900)
);

INSERT INTO custom_model_scheduler_policies (
    id, prefetch_factor, derivative_weight, wiki_weight,
    background_max_wait_seconds, dispatch_lease_seconds,
    policy_version, updated_by, created_at, updated_at
) VALUES (1, 2, 2, 1, 30, 120, 1, '', now(), now())
ON CONFLICT (id) DO NOTHING;
