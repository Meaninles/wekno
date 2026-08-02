ALTER TABLE custom_document_split_parts
    ADD COLUMN backpressure_events INTEGER NOT NULL DEFAULT 0;

ALTER TABLE custom_document_split_parts
    ADD COLUMN dispatch_epoch INTEGER NOT NULL DEFAULT 0;

ALTER TABLE custom_document_split_parts
    ADD COLUMN dispatch_lease_until DATETIME;

CREATE INDEX IF NOT EXISTS idx_custom_document_split_parts_dispatch_recovery
    ON custom_document_split_parts (state, dispatch_lease_until, plan_id, part_index);
