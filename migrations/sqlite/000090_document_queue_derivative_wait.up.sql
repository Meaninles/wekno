CREATE INDEX IF NOT EXISTS idx_custom_document_queue_external_wait
    ON custom_document_queue_workflows (state, updated_at, id);
