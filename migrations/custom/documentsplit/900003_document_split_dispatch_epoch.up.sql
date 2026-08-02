ALTER TABLE custom_document_split_parts
    ADD COLUMN IF NOT EXISTS backpressure_events integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dispatch_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dispatch_lease_until timestamptz;

CREATE INDEX IF NOT EXISTS idx_custom_document_split_parts_dispatch_recovery
    ON custom_document_split_parts (state, dispatch_lease_until, plan_id, part_index);
