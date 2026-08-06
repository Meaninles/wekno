ALTER TABLE custom_model_resource_pools
    ADD COLUMN IF NOT EXISTS im_max_concurrent INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS im_max_waiting INTEGER NOT NULL DEFAULT 50;

ALTER TABLE custom_model_admission_templates
    ADD COLUMN IF NOT EXISTS im_max_concurrent INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS im_max_waiting INTEGER NOT NULL DEFAULT 50;

UPDATE custom_model_resource_pools
SET im_max_concurrent = 0, im_max_waiting = 0
WHERE resource_kind <> 'chat';

UPDATE custom_model_admission_templates
SET im_max_concurrent = 0, im_max_waiting = 0
WHERE kind <> 'chat';
