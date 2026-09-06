-- Existing local eval models only. Run after the gateway supports both modes.
BEGIN;
DO $$ BEGIN
 IF (SELECT count(*) FROM models WHERE tenant_id=10000
     AND id='prod-qwen36-27b-chat' AND deleted_at IS NULL
     AND name='Qwen3.8-27B-Agent'
     AND parameters->>'base_url' LIKE 'https://llmgateway.moutai.com.cn%') <> 1 THEN
  RAISE EXCEPTION 'Expected local eval Qwen models were not found';
 END IF;
END $$;
UPDATE models SET
 display_name='Qwen3.8-27B',
 parameters=jsonb_set(parameters::jsonb,'{extra_config}',
   coalesce(parameters::jsonb->'extra_config','{}'::jsonb) ||
   jsonb_build_object('reasoning_effort','xhigh','thinking_control','thinking_type','generation_policy','gateway')),
 updated_at=now()
WHERE tenant_id=10000 AND id='prod-qwen36-27b-chat' AND deleted_at IS NULL
  AND name='Qwen3.8-27B-Agent';
COMMIT;
