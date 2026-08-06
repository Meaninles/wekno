-- Management-folder hierarchy for every live knowledge base. Folder IDs are
-- KB-local references and must be remapped by path when a hybrid KB is rebuilt
-- into a distinct pure-document target.
SELECT
    f.tenant_id,
    f.knowledge_base_id,
    f.id AS source_folder_id,
    f.parent_id AS source_parent_id,
    f.name,
    f.description,
    f.path,
    f.depth,
    f.sort_order
FROM custom_knowledge_folders f
INNER JOIN knowledge_bases kb
    ON kb.id=f.knowledge_base_id AND kb.deleted_at IS NULL
ORDER BY f.tenant_id, f.knowledge_base_id, f.depth, f.sort_order, f.path, f.id;
