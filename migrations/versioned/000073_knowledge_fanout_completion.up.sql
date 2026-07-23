CREATE TABLE IF NOT EXISTS knowledge_fanout_completions (
    tenant_id BIGINT NOT NULL,
    knowledge_id VARCHAR(36) NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    processing_generation VARCHAR(36) NOT NULL,
    item_id VARCHAR(160) NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, knowledge_id, processing_generation, item_id),
    CONSTRAINT fk_knowledge_fanout_completion_knowledge
        FOREIGN KEY (knowledge_id) REFERENCES knowledges(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_knowledge_fanout_completion_generation
    ON knowledge_fanout_completions
    (tenant_id, knowledge_base_id, knowledge_id, processing_generation);
