ALTER TABLE custom_model_admission_templates
    DROP COLUMN IF EXISTS im_max_waiting,
    DROP COLUMN IF EXISTS im_max_concurrent;

ALTER TABLE custom_model_resource_pools
    DROP COLUMN IF EXISTS im_max_waiting,
    DROP COLUMN IF EXISTS im_max_concurrent;
