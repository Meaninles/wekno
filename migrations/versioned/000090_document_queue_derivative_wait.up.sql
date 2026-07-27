-- Background derivative workflows are scanned in bounded updated_at order.
-- Keep that rotation index-backed even when a large upload creates thousands
-- of simultaneous graph/Wiki waits.
CREATE INDEX IF NOT EXISTS idx_custom_document_queue_external_wait
    ON custom_document_queue_workflows (state, updated_at, id);
