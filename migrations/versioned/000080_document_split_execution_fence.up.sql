ALTER TABLE custom_document_split_parts
    ADD COLUMN IF NOT EXISTS lease_instance_id VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE custom_document_split_parts
    ADD COLUMN IF NOT EXISTS lease_boot_id VARCHAR(64) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_custom_document_split_part_owner_boot
    ON custom_document_split_parts (
        state, lease_instance_id, lease_boot_id, lease_until
    );
