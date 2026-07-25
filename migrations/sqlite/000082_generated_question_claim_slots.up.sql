DELETE FROM custom_generated_question_claims
WHERE rowid NOT IN (
    SELECT MIN(rowid)
    FROM custom_generated_question_claims
    GROUP BY tenant_id, knowledge_id, processing_generation, claim_id
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_generated_question_claim_slot
    ON custom_generated_question_claims
    (tenant_id, knowledge_id, processing_generation, claim_id);
