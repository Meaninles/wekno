DELETE FROM custom_generated_question_claims AS newer
USING custom_generated_question_claims AS older
WHERE newer.tenant_id = older.tenant_id
  AND newer.knowledge_id = older.knowledge_id
  AND newer.processing_generation = older.processing_generation
  AND newer.claim_id = older.claim_id
  AND (newer.created_at, newer.question_hash) > (older.created_at, older.question_hash);

CREATE UNIQUE INDEX IF NOT EXISTS idx_generated_question_claim_slot
    ON custom_generated_question_claims
    (tenant_id, knowledge_id, processing_generation, claim_id);
