-- Durable fencing for the per-KB Wiki ingest coordinator. Redis/Lite locks
-- provide liveness only; this monotonically increasing epoch prevents a worker
-- that paused beyond the Redis TTL from committing after a replacement owner.
CREATE TABLE IF NOT EXISTS custom_wiki_ingest_leases (
    tenant_id          BIGINT      NOT NULL,
    knowledge_base_id VARCHAR(64) NOT NULL,
    epoch              BIGINT      NOT NULL,
    lease_token        VARCHAR(64) NOT NULL,
    acquired_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, knowledge_base_id),
    CONSTRAINT chk_custom_wiki_ingest_leases_epoch CHECK (epoch > 0),
    CONSTRAINT chk_custom_wiki_ingest_leases_token CHECK (length(lease_token) >= 32)
);

COMMENT ON TABLE custom_wiki_ingest_leases IS
    'Database fencing epoch/token for Wiki ingest coordination; Redis/Lite ownership alone is not authoritative.';
