DROP TABLE IF EXISTS custom_derivative_model_assignments;
DROP TABLE IF EXISTS custom_derivative_control_configs;

DROP INDEX IF EXISTS idx_knowledge_bases_derivative_model;
ALTER TABLE knowledge_bases DROP COLUMN IF EXISTS derivative_model_id;

DROP INDEX IF EXISTS idx_models_workload_scope;
ALTER TABLE models DROP CONSTRAINT IF EXISTS chk_models_workload_scope;
ALTER TABLE models DROP COLUMN IF EXISTS workload_scope;
