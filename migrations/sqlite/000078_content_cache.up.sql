CREATE TABLE IF NOT EXISTS custom_content_cache_entries (
    tenant_id INTEGER NOT NULL,
    cache_kind TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    version_hash TEXT NOT NULL,
    payload BLOB NOT NULL,
    payload_sha256 TEXT NOT NULL,
    payload_size INTEGER NOT NULL,
    ref_count INTEGER NOT NULL DEFAULT 0,
    hit_count INTEGER NOT NULL DEFAULT 0,
    expires_at DATETIME NULL,
    last_accessed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, cache_kind, content_hash, version_hash)
);

CREATE INDEX IF NOT EXISTS idx_custom_content_cache_gc
    ON custom_content_cache_entries (ref_count, last_accessed_at, expires_at);

CREATE TABLE IF NOT EXISTS custom_content_cache_refs (
    tenant_id INTEGER NOT NULL,
    knowledge_id TEXT NOT NULL,
    processing_generation TEXT NOT NULL,
    cache_kind TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    version_hash TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
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
