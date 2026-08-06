DROP INDEX IF EXISTS idx_custom_document_split_parts_dispatch_recovery;

ALTER TABLE custom_document_split_parts
    DROP COLUMN IF EXISTS dispatch_lease_until,
    DROP COLUMN IF EXISTS dispatch_epoch,
    DROP COLUMN IF EXISTS backpressure_events;
