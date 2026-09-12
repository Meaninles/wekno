-- Prototype ONLY: all objects are confined to the local experiment schema.
SET search_path=grep_plan_lab_20260912,public;
CREATE FUNCTION sync_projection_chunk() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE doc text;
BEGIN
  doc:=CASE WHEN TG_OP='DELETE' THEN OLD.knowledge_id ELSE NEW.knowledge_id END;
  PERFORM pg_advisory_xact_lock(hashtextextended(doc,937));
  IF TG_OP<>'INSERT' THEN DELETE FROM search_projection WHERE id=OLD.id; END IF;
  IF TG_OP<>'DELETE' THEN
    INSERT INTO search_projection
      SELECT c.id,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.created_at,c.content,k.title
      FROM chunks c JOIN knowledges k ON k.id=c.knowledge_id
      WHERE c.id=NEW.id AND c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type<>'summary'
      AND k.deleted_at IS NULL AND k.publication_state='published';
  END IF;
  RETURN NULL;
END $$;
CREATE FUNCTION sync_projection_knowledge() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE doc text;
BEGIN
  doc:=CASE WHEN TG_OP='DELETE' THEN OLD.id ELSE NEW.id END;
  PERFORM pg_advisory_xact_lock(hashtextextended(doc,937));
  DELETE FROM search_projection WHERE knowledge_id=doc;
  IF TG_OP<>'DELETE' THEN
    INSERT INTO search_projection
      SELECT c.id,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.created_at,c.content,k.title
      FROM chunks c JOIN knowledges k ON k.id=c.knowledge_id
      WHERE c.knowledge_id=doc AND c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type<>'summary'
      AND k.deleted_at IS NULL AND k.publication_state='published';
  END IF;
  RETURN NULL;
END $$;
CREATE INDEX projection_knowledge ON search_projection(knowledge_id);
CREATE TRIGGER lab_projection_chunk AFTER INSERT OR UPDATE OR DELETE ON chunks
FOR EACH ROW EXECUTE FUNCTION sync_projection_chunk();
CREATE TRIGGER lab_projection_knowledge AFTER INSERT OR UPDATE OR DELETE ON knowledges
FOR EACH ROW EXECUTE FUNCTION sync_projection_knowledge();
