DROP TABLE IF EXISTS custom_enrichment_outcomes;
ALTER TABLE knowledges DROP COLUMN IF EXISTS wiki_error_message;
ALTER TABLE knowledges DROP COLUMN IF EXISTS wiki_status;
ALTER TABLE knowledges DROP COLUMN IF EXISTS enrichment_status;
