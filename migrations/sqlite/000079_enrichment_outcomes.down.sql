DROP TABLE IF EXISTS custom_enrichment_outcomes;
ALTER TABLE knowledges DROP COLUMN wiki_error_message;
ALTER TABLE knowledges DROP COLUMN wiki_status;
ALTER TABLE knowledges DROP COLUMN enrichment_status;
