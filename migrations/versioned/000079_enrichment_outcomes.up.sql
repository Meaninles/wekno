ALTER TABLE knowledges
    ADD COLUMN IF NOT EXISTS enrichment_status VARCHAR(32) NOT NULL DEFAULT 'none';
ALTER TABLE knowledges
    ADD COLUMN IF NOT EXISTS wiki_status VARCHAR(32) NOT NULL DEFAULT 'none';
ALTER TABLE knowledges
    ADD COLUMN IF NOT EXISTS wiki_error_message TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS custom_enrichment_outcomes (
    tenant_id BIGINT NOT NULL,
    knowledge_id VARCHAR(36) NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    processing_generation VARCHAR(36) NOT NULL,
    item_id VARCHAR(160) NOT NULL,
    status VARCHAR(32) NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    completed_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, knowledge_id, processing_generation, item_id),
    CONSTRAINT fk_custom_enrichment_outcome_knowledge
        FOREIGN KEY (knowledge_id) REFERENCES knowledges(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_custom_enrichment_outcomes_generation
    ON custom_enrichment_outcomes
    (tenant_id, knowledge_base_id, knowledge_id, processing_generation, status);
