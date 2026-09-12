-- Executed inside one transaction by grepsearch.Migrate, with a pinned schema.
-- Block source writes before installing triggers/backfilling. No partial reads.
LOCK TABLE knowledges, chunks IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE custom_grepsearch_chunks (
    id varchar(36) PRIMARY KEY,
    knowledge_id varchar(36) NOT NULL,
    knowledge_base_id varchar(36) NOT NULL,
    tenant_id bigint NOT NULL,
    created_at timestamptz,
    content text,
    knowledge_title text
) WITH (fillfactor=90, autovacuum_vacuum_scale_factor=0.05,
        autovacuum_analyze_scale_factor=0.02);
CREATE INDEX custom_grepsearch_scope_order ON custom_grepsearch_chunks
    (knowledge_base_id, tenant_id, created_at DESC, id DESC);
CREATE INDEX custom_grepsearch_knowledge ON custom_grepsearch_chunks (knowledge_id);

-- Chunk and document changes serialize on the same document keys. Ordered keys
-- cover batch writes and moves between documents, without a global writer lock.
CREATE FUNCTION custom_grepsearch_lock_docs(docs text[]) RETURNS void
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE doc text;
BEGIN
    -- Trigger refreshes must see the preceding serialized writer's commit.
    -- A repeatable-read source writer could otherwise insert a new chunk using
    -- an old publication snapshot. Reject it explicitly, never build stale rows.
    IF current_setting('transaction_isolation') NOT IN ('read committed','read uncommitted') THEN
        RAISE EXCEPTION 'grep source writes require READ COMMITTED isolation' USING ERRCODE='0A000';
    END IF;
    FOR doc IN SELECT DISTINCT d FROM unnest(docs) d WHERE d IS NOT NULL ORDER BY d LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended(doc, 734819026));
    END LOOP;
END $$;

CREATE FUNCTION custom_grepsearch_sync_chunks() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE ids text[]; docs text[];
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT array_agg(id::text), array_agg(knowledge_id::text) INTO ids,docs FROM grep_new;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT array_agg(id::text), array_agg(knowledge_id::text) INTO ids,docs FROM grep_old;
    ELSE
        SELECT array_agg(id::text), array_agg(knowledge_id::text) INTO ids,docs FROM (
            SELECT o.id,o.knowledge_id FROM grep_old o FULL JOIN grep_new n USING (id)
            WHERE ROW(o.id,o.knowledge_id,o.knowledge_base_id,o.tenant_id,o.created_at,o.content,o.is_enabled,o.chunk_type,o.deleted_at)
              IS DISTINCT FROM ROW(n.id,n.knowledge_id,n.knowledge_base_id,n.tenant_id,n.created_at,n.content,n.is_enabled,n.chunk_type,n.deleted_at)
            UNION
            SELECT n.id,n.knowledge_id FROM grep_old o FULL JOIN grep_new n USING (id)
            WHERE ROW(o.id,o.knowledge_id,o.knowledge_base_id,o.tenant_id,o.created_at,o.content,o.is_enabled,o.chunk_type,o.deleted_at)
              IS DISTINCT FROM ROW(n.id,n.knowledge_id,n.knowledge_base_id,n.tenant_id,n.created_at,n.content,n.is_enabled,n.chunk_type,n.deleted_at)
        ) changed WHERE id IS NOT NULL;
    END IF;
    IF ids IS NULL THEN RETURN NULL; END IF;
    PERFORM custom_grepsearch_lock_docs(docs);
    DELETE FROM custom_grepsearch_chunks WHERE id = ANY(ids);
    IF TG_OP <> 'DELETE' THEN
        INSERT INTO custom_grepsearch_chunks
        SELECT c.id,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.created_at,c.content,k.title
        FROM chunks c JOIN knowledges k ON k.id=c.knowledge_id
        WHERE c.id = ANY(ids) AND c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type <> 'summary'
          AND k.deleted_at IS NULL AND k.publication_state='published'
        ORDER BY c.knowledge_base_id,c.tenant_id,c.created_at DESC,c.id DESC;
    END IF;
    RETURN NULL;
END $$;

CREATE FUNCTION custom_grepsearch_sync_knowledges() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE docs text[];
BEGIN
    IF TG_OP = 'INSERT' THEN SELECT array_agg(id::text) INTO docs FROM grep_new;
    ELSIF TG_OP = 'DELETE' THEN SELECT array_agg(id::text) INTO docs FROM grep_old;
    ELSE
        SELECT array_agg(id::text) INTO docs FROM (
            SELECT o.id FROM grep_old o FULL JOIN grep_new n USING (id)
            WHERE ROW(o.id,o.title,o.publication_state,o.deleted_at)
              IS DISTINCT FROM ROW(n.id,n.title,n.publication_state,n.deleted_at)
            UNION
            SELECT n.id FROM grep_old o FULL JOIN grep_new n USING (id)
            WHERE ROW(o.id,o.title,o.publication_state,o.deleted_at)
              IS DISTINCT FROM ROW(n.id,n.title,n.publication_state,n.deleted_at)
        ) changed WHERE id IS NOT NULL;
    END IF;
    IF docs IS NULL THEN RETURN NULL; END IF;
    PERFORM custom_grepsearch_lock_docs(docs);
    DELETE FROM custom_grepsearch_chunks WHERE knowledge_id = ANY(docs);
    IF TG_OP <> 'DELETE' THEN
        INSERT INTO custom_grepsearch_chunks
        SELECT c.id,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.created_at,c.content,k.title
        FROM chunks c JOIN knowledges k ON k.id=c.knowledge_id
        WHERE c.knowledge_id = ANY(docs) AND c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type <> 'summary'
          AND k.deleted_at IS NULL AND k.publication_state='published'
        ORDER BY c.knowledge_base_id,c.tenant_id,c.created_at DESC,c.id DESC;
    END IF;
    RETURN NULL;
END $$;

CREATE FUNCTION custom_grepsearch_truncate() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    TRUNCATE custom_grepsearch_chunks;
    RETURN NULL;
END $$;

CREATE TRIGGER custom_grepsearch_chunks_insert AFTER INSERT ON chunks
REFERENCING NEW TABLE AS grep_new FOR EACH STATEMENT EXECUTE FUNCTION custom_grepsearch_sync_chunks();
CREATE TRIGGER custom_grepsearch_chunks_update AFTER UPDATE ON chunks
REFERENCING OLD TABLE AS grep_old NEW TABLE AS grep_new FOR EACH STATEMENT EXECUTE FUNCTION custom_grepsearch_sync_chunks();
CREATE TRIGGER custom_grepsearch_chunks_delete AFTER DELETE ON chunks
REFERENCING OLD TABLE AS grep_old FOR EACH STATEMENT EXECUTE FUNCTION custom_grepsearch_sync_chunks();
CREATE TRIGGER custom_grepsearch_chunks_truncate AFTER TRUNCATE ON chunks
FOR EACH STATEMENT EXECUTE FUNCTION custom_grepsearch_truncate();
CREATE TRIGGER custom_grepsearch_knowledges_insert AFTER INSERT ON knowledges
REFERENCING NEW TABLE AS grep_new FOR EACH STATEMENT EXECUTE FUNCTION custom_grepsearch_sync_knowledges();
CREATE TRIGGER custom_grepsearch_knowledges_update AFTER UPDATE ON knowledges
REFERENCING OLD TABLE AS grep_old NEW TABLE AS grep_new FOR EACH STATEMENT EXECUTE FUNCTION custom_grepsearch_sync_knowledges();
CREATE TRIGGER custom_grepsearch_knowledges_delete AFTER DELETE ON knowledges
REFERENCING OLD TABLE AS grep_old FOR EACH STATEMENT EXECUTE FUNCTION custom_grepsearch_sync_knowledges();
CREATE TRIGGER custom_grepsearch_knowledges_truncate AFTER TRUNCATE ON knowledges
FOR EACH STATEMENT EXECUTE FUNCTION custom_grepsearch_truncate();

INSERT INTO custom_grepsearch_chunks
SELECT c.id,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.created_at,c.content,k.title
FROM chunks c JOIN knowledges k ON k.id=c.knowledge_id
WHERE c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type <> 'summary'
  AND k.deleted_at IS NULL AND k.publication_state='published'
ORDER BY c.knowledge_base_id,c.tenant_id,c.created_at DESC,c.id DESC;
ANALYZE custom_grepsearch_chunks;
CREATE TABLE custom_grepsearch_state (id integer PRIMARY KEY CHECK(id=1), version integer NOT NULL);
INSERT INTO custom_grepsearch_state VALUES(1,1);
