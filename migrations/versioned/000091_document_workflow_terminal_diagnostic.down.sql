ALTER TABLE custom_document_queue_workflows
    DROP CONSTRAINT IF EXISTS ck_document_workflow_terminal_diagnostic;

ALTER TABLE custom_document_queue_workflows
    DROP COLUMN IF EXISTS terminal_diagnostic;
