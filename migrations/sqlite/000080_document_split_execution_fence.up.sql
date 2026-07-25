ALTER TABLE custom_document_split_parts
    ADD COLUMN lease_instance_id TEXT NOT NULL DEFAULT '';
ALTER TABLE custom_document_split_parts
    ADD COLUMN lease_boot_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_custom_document_split_part_owner_boot
    ON custom_document_split_parts (
        state, lease_instance_id, lease_boot_id, lease_until
    );
