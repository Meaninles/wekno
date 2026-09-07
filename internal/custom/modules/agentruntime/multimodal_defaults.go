package agentruntime

import (
	"context"

	"gorm.io/gorm"
)

// Enable both upload capabilities for existing agents once. Later explicit
// user choices remain intact across restarts; new agents use config defaults.
func enableAgentMultimodalDefaults(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(2070915)`).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Table("custom_agent_profile_cutovers").Where("version = 4").Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return nil
		}
		if err := tx.Exec(`UPDATE custom_agents a SET config = COALESCE(config::jsonb, '{}'::jsonb)
			|| jsonb_build_object('image_upload_enabled', true, 'audio_upload_enabled', true,
				'vlm_model_id', COALESCE(NULLIF(config->>'vlm_model_id', ''),
					(SELECT m.id FROM models m WHERE m.tenant_id = a.tenant_id AND m.type = 'VLLM'
					 AND m.deleted_at IS NULL ORDER BY m.is_default DESC, m.created_at, m.id LIMIT 1), ''),
				'asr_model_id', COALESCE(NULLIF(config->>'asr_model_id', ''),
					(SELECT m.id FROM models m WHERE m.tenant_id = a.tenant_id AND m.type = 'ASR'
					 AND m.deleted_at IS NULL ORDER BY m.is_default DESC, m.created_at, m.id LIMIT 1), '')),
			updated_at = NOW() WHERE a.deleted_at IS NULL`).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO custom_agent_profile_cutovers(version) VALUES (4)`).Error
	})
}
