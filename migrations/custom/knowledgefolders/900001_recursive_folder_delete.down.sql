DROP TABLE IF EXISTS custom_knowledge_folder_delete_items;
DROP TABLE IF EXISTS custom_knowledge_folder_delete_operations;

DROP INDEX IF EXISTS idx_custom_knowledge_folders_delete_operation_id;
DROP INDEX IF EXISTS idx_custom_knowledge_folders_delete_status;

ALTER TABLE custom_knowledge_folders
    DROP COLUMN IF EXISTS delete_operation_id;

ALTER TABLE custom_knowledge_folders
    DROP COLUMN IF EXISTS delete_status;
