DROP INDEX IF EXISTS idx_custom_derivative_provider_calls_generation;
DROP INDEX IF EXISTS idx_custom_derivative_provider_calls_replay;

ALTER TABLE custom_derivative_provider_calls
    DROP CONSTRAINT IF EXISTS chk_custom_derivative_provider_call_disposition;

ALTER TABLE custom_derivative_provider_calls
    DROP CONSTRAINT IF EXISTS uq_custom_derivative_provider_call;

ALTER TABLE custom_derivative_provider_calls
    ADD CONSTRAINT uq_custom_derivative_provider_call
    UNIQUE (work_item_id, request_hash);

ALTER TABLE custom_derivative_provider_calls
    DROP COLUMN IF EXISTS validated_at,
    DROP COLUMN IF EXISTS validation_error,
    DROP COLUMN IF EXISTS disposition,
    DROP COLUMN IF EXISTS processing_generation,
    DROP COLUMN IF EXISTS provider_request_id,
    DROP COLUMN IF EXISTS attempt;
