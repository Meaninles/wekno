-- Preserve contract-invalid provider responses as audit evidence while
-- allowing a later provider attempt for the same deterministic request hash.
ALTER TABLE custom_derivative_provider_calls
    ADD COLUMN IF NOT EXISTS attempt integer NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS provider_request_id varchar(160) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS processing_generation varchar(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS disposition varchar(32) NOT NULL DEFAULT 'checkpointed',
    ADD COLUMN IF NOT EXISTS validation_error text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS validated_at timestamptz;

UPDATE custom_derivative_provider_calls AS calls
SET processing_generation = items.processing_generation
FROM custom_derivative_work_items AS items
WHERE calls.work_item_id = items.id
  AND calls.processing_generation = '';

-- GORM used this shorter name in an early schema. It may be a standalone
-- two-column unique index and can coexist with the upgraded constraint,
-- silently rejecting attempt N+1 unless it is removed explicitly.
ALTER TABLE custom_derivative_provider_calls
    DROP CONSTRAINT IF EXISTS uq_derivative_provider_call;

DROP INDEX IF EXISTS uq_derivative_provider_call;

ALTER TABLE custom_derivative_provider_calls
    DROP CONSTRAINT IF EXISTS uq_custom_derivative_provider_call;

ALTER TABLE custom_derivative_provider_calls
    ADD CONSTRAINT uq_custom_derivative_provider_call
    UNIQUE (work_item_id, request_hash, attempt);

CREATE INDEX IF NOT EXISTS idx_custom_derivative_provider_calls_replay
    ON custom_derivative_provider_calls
    (work_item_id, request_hash, disposition, attempt DESC);

CREATE INDEX IF NOT EXISTS idx_custom_derivative_provider_calls_generation
    ON custom_derivative_provider_calls
    (processing_generation, disposition, created_at);

ALTER TABLE custom_derivative_provider_calls
    DROP CONSTRAINT IF EXISTS chk_custom_derivative_provider_call_disposition;

ALTER TABLE custom_derivative_provider_calls
    ADD CONSTRAINT chk_custom_derivative_provider_call_disposition
    CHECK (disposition IN ('checkpointed', 'accepted', 'invalid_contract'));
