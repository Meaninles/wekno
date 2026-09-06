-- LOCAL EVAL ONLY: execute against WeKnora-agent-eval-postgres-dev.
-- Historical messages, answer/provider records, embeddings and files stay intact.
\set ON_ERROR_STOP on
BEGIN;
DO $$ BEGIN
 IF (SELECT count(*) FROM models WHERE tenant_id=10000 AND deleted_at IS NULL
     AND id IN ('prod-deepseek-v4-flash-int8-chat','prod-qwen36-27b-chat')
     AND parameters->>'base_url' LIKE 'https://llmgateway.moutai.com.cn%') <> 2 THEN
  RAISE EXCEPTION 'Expected local eval chat routes were not found';
 END IF;
END $$;
CREATE TEMP TABLE target_ids(old_id text PRIMARY KEY,new_id text NOT NULL) ON COMMIT DROP;
INSERT INTO target_ids VALUES
 ('agent-policy-ds-sdk','prod-deepseek-v4-flash-int8-chat'),
 ('agent-policy-qwen-sdk','prod-qwen36-27b-chat'),
 ('prod-qwen36-35b-chat','prod-qwen36-27b-chat'),
 ('prod-qwen36-35b-derivative','prod-qwen36-27b-chat'),
 ('b80433e8-6221-4ef1-a58a-26b9bbde2427','prod-deepseek-v4-flash-int8-chat');
CREATE TEMP TABLE retired_eval_agents ON COMMIT DROP AS
 SELECT id, CASE config->>'agent_type'
  WHEN 'document-processing-agent' THEN 'builtin-document-processing'
  WHEN 'data-analysis' THEN 'builtin-data-analyst'
  WHEN 'wiki-qa' THEN 'builtin-wiki-researcher'
  WHEN 'rag-qa' THEN CASE WHEN config->>'agent_mode'='quick-answer' THEN 'builtin-quick-answer' ELSE 'builtin-smart-reasoning' END
  ELSE 'builtin-general-agent' END AS replacement
 FROM custom_agents WHERE tenant_id=10000 AND NOT is_builtin AND deleted_at IS NULL
 AND id IN ('261169d4-61f2-42ed-a914-ea834cfa6f34','80b1587f-8995-4804-9b90-970152798293',
 '776836fc-f1ab-4f16-bcf9-697fd6e0ad9b','268c14ac-1822-4cd8-b772-7ae72141e093',
 '3cedfb93-294c-4f74-b8a1-7cb4d2561f38','dcf03cec-0a06-4268-a90a-28392335ce34',
 'de847298-5927-467b-b62d-f1a158b4b1ad','22674b6b-0213-4b35-93a0-bf9c7a69ff21',
 '06a999f2-f3ba-4a92-8d06-4aa1daadc607','e09f98f1-0f6d-404e-b3c5-8de266a3191b',
 '4f4905cf-6a9d-49ef-a1ea-5ca886027839','b275071f-3a11-4ae9-b082-a1c981a9c920',
 '7a418bb0-4e6a-4495-931c-5a19154df1d0','32d8688d-9e90-4dd5-8bb1-82d02a454d77',
 'd7b1d21f-dc26-4ab0-8bd1-2a27db5de7e2');
INSERT INTO target_ids SELECT id,replacement FROM retired_eval_agents;
-- Replace exact identifier values, never substrings of a prompt or document.
CREATE FUNCTION pg_temp.retarget(value jsonb) RETURNS jsonb LANGUAGE plpgsql AS $$
DECLARE result jsonb; replacement text;
BEGIN
 CASE jsonb_typeof(value)
 WHEN 'string' THEN
  SELECT new_id INTO replacement FROM target_ids WHERE old_id=value#>>'{}';
  RETURN coalesce(to_jsonb(replacement),value);
 WHEN 'array' THEN
  SELECT coalesce(jsonb_agg(pg_temp.retarget(item) ORDER BY ordinal),'[]'::jsonb) INTO result
   FROM jsonb_array_elements(value) WITH ORDINALITY AS elements(item,ordinal);
 WHEN 'object' THEN
  SELECT coalesce(jsonb_object_agg(key,pg_temp.retarget(val)),'{}'::jsonb) INTO result FROM jsonb_each(value) AS fields(key,val);
 ELSE RETURN value;
 END CASE;
 RETURN result;
END $$;
UPDATE models SET
 name=CASE id WHEN 'prod-deepseek-v4-flash-int8-chat' THEN 'DeepSeek-V4-Flash-Agent' ELSE 'Qwen3.8-27B-Agent' END,
 display_name=CASE id WHEN 'prod-deepseek-v4-flash-int8-chat' THEN 'DeepSeek V4 Flash' ELSE 'Qwen3.8-27B' END,
 is_default=(id='prod-qwen36-27b-chat'),
 parameters=jsonb_set(parameters::jsonb,'{extra_config}',
   (coalesce(parameters::jsonb->'extra_config','{}'::jsonb)-'general_agent_claude_base_url'-'sampling_profile') ||
   jsonb_build_object('thinking_control','thinking_type','generation_policy','gateway','agent_runtime_adapter','platform',
     'reasoning_effort',CASE id WHEN 'prod-deepseek-v4-flash-int8-chat' THEN 'high' ELSE 'xhigh' END)),
 updated_at=now()
WHERE tenant_id=10000 AND id IN ('prod-deepseek-v4-flash-int8-chat','prod-qwen36-27b-chat');
UPDATE custom_agents SET config=pg_temp.retarget(config),updated_at=now()
 WHERE tenant_id=10000 AND config IS DISTINCT FROM pg_temp.retarget(config);
UPDATE sessions SET agent_id=coalesce((SELECT new_id FROM target_ids WHERE old_id=agent_id),agent_id),
 summary_model_id=coalesce((SELECT new_id FROM target_ids WHERE old_id=summary_model_id),summary_model_id),
 agent_config=pg_temp.retarget(agent_config),context_config=pg_temp.retarget(context_config),summary_parameters=pg_temp.retarget(summary_parameters)
 WHERE tenant_id=10000;
UPDATE tenants SET agent_config=pg_temp.retarget(agent_config),context_config=pg_temp.retarget(context_config),
 conversation_config=pg_temp.retarget(conversation_config),chat_history_config=pg_temp.retarget(chat_history_config) WHERE id=10000;
UPDATE knowledge_bases SET
 summary_model_id=coalesce((SELECT new_id FROM target_ids WHERE old_id=summary_model_id),summary_model_id),
 derivative_model_id=coalesce((SELECT new_id FROM target_ids WHERE old_id=derivative_model_id),derivative_model_id),
 wiki_config=pg_temp.retarget(wiki_config),extract_config=pg_temp.retarget(extract_config),
 faq_config=pg_temp.retarget(faq_config),question_generation_config=pg_temp.retarget(question_generation_config)
 WHERE tenant_id=10000;
UPDATE custom_browser_agent_system_profiles SET navigation_model=pg_temp.retarget(navigation_model),
 component_model=pg_temp.retarget(component_model) WHERE tenant_id=10000;
UPDATE custom_scheduled_chat_tasks SET agent_id=coalesce((SELECT new_id FROM target_ids WHERE old_id=agent_id),agent_id),
 request_context=pg_temp.retarget(request_context) WHERE tenant_id=10000;
UPDATE custom_derivative_control_configs SET default_model_id=t.new_id,updated_at=now()
 FROM target_ids t WHERE default_model_id=t.old_id;
DELETE FROM custom_derivative_model_assignments old USING target_ids t
 WHERE old.model_id=t.old_id AND EXISTS(SELECT 1 FROM custom_derivative_model_assignments current WHERE current.model_id=t.new_id);
UPDATE custom_derivative_model_assignments SET model_id=t.new_id,updated_at=now()
 FROM target_ids t WHERE model_id=t.old_id AND model_tenant_id=10000;
-- Queued retries need a live route; completed work retains its provenance.
UPDATE custom_derivative_work_items SET model_id=t.new_id
 FROM target_ids t WHERE model_id=t.old_id AND model_tenant_id=10000 AND state NOT IN ('completed','failed','canceled','cancelled');
DELETE FROM custom_model_resource_bindings b USING target_ids t WHERE b.model_id=t.old_id AND b.model_tenant_id=10000;
UPDATE models SET deleted_at=now(),updated_at=now(),is_default=false
 WHERE tenant_id=10000 AND id IN (SELECT old_id FROM target_ids) AND deleted_at IS NULL;
UPDATE custom_agents SET deleted_at=now(),updated_at=now()
 WHERE tenant_id=10000 AND id IN (SELECT id FROM retired_eval_agents);
DO $$ BEGIN
 IF (SELECT count(*) FROM models WHERE tenant_id=10000 AND type='KnowledgeQA' AND deleted_at IS NULL) <> 2 THEN
  RAISE EXCEPTION 'Expected exactly two active local chat models';
 END IF;
END $$;
COMMIT;
