DROP INDEX IF EXISTS idx_custom_document_split_part_owner_boot;

ALTER TABLE custom_document_split_parts
    DROP COLUMN IF EXISTS lease_boot_id;
ALTER TABLE custom_document_split_parts
    DROP COLUMN IF EXISTS lease_instance_id;
