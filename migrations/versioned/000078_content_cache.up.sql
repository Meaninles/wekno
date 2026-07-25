CREATE TABLE IF NOT EXISTS custom_content_cache_entries (
    tenant_id BIGINT NOT NULL,
    cache_kind VARCHAR(32) NOT NULL,
    content_hash VARCHAR(64) NOT NULL,
    version_hash VARCHAR(64) NOT NULL,
    payload BYTEA NOT NULL,
    payload_sha256 VARCHAR(64) NOT NULL,
    payload_size BIGINT NOT NULL,
    ref_count BIGINT NOT NULL DEFAULT 0,
    hit_count BIGINT NOT NULL DEFAULT 0,
    expires_at TIMESTAMPTZ NULL,
    last_accessed_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, cache_kind, content_hash, version_hash)
);

CREATE INDEX IF NOT EXISTS idx_custom_content_cache_gc
    ON custom_content_cache_entries (ref_count, last_accessed_at, expires_at);

CREATE TABLE IF NOT EXISTS custom_content_cache_refs (
    tenant_id BIGINT NOT NULL,
    knowledge_id VARCHAR(36) NOT NULL,
    processing_generation VARCHAR(36) NOT NULL,
    cache_kind VARCHAR(32) NOT NULL,
    content_hash VARCHAR(64) NOT NULL,
    version_hash VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (
        tenant_id, knowledge_id, processing_generation,
        cache_kind, content_hash, version_hash
    ),
    FOREIGN KEY (tenant_id, cache_kind, content_hash, version_hash)
        REFERENCES custom_content_cache_entries (
            tenant_id, cache_kind, content_hash, version_hash
        ) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_custom_content_cache_refs_generation
    ON custom_content_cache_refs (tenant_id, knowledge_id, processing_generation);
