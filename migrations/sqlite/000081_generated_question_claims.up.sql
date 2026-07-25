CREATE TABLE IF NOT EXISTS custom_generated_question_claims (
    tenant_id INTEGER NOT NULL,
    knowledge_id VARCHAR(36) NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    processing_generation VARCHAR(36) NOT NULL,
    question_hash VARCHAR(64) NOT NULL,
    claim_id VARCHAR(256) NOT NULL,
    question TEXT NOT NULL,
    normalized_question TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, knowledge_id, processing_generation, question_hash),
    FOREIGN KEY (knowledge_id) REFERENCES knowledges(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_custom_generated_question_claims_generation
    ON custom_generated_question_claims
    (tenant_id, knowledge_base_id, knowledge_id, processing_generation);

CREATE UNIQUE INDEX IF NOT EXISTS uq_custom_generated_question_claims_slot
    ON custom_generated_question_claims
    (tenant_id, knowledge_id, processing_generation, claim_id);
