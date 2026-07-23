DROP INDEX IF EXISTS idx_knowledges_processing_workflow;
ALTER TABLE knowledges DROP COLUMN IF EXISTS processing_workflow_id;
DROP TABLE IF EXISTS custom_document_queue_workflows;
DROP TABLE IF EXISTS custom_document_queue_instances;
