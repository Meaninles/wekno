package agentruntime

import (
	"context"
	"gorm.io/gorm"
)

// Artifacts retain their existing business table and object identity. Replacing
// an execution engine must not discard the user's previously delivered files.
func applyAgentRuntimeMigrations(ctx context.Context, db *gorm.DB) error {
	if err := migrateAgentProfiles(ctx, db); err != nil {
		return err
	}
	if err := retireSimpleChat(ctx, db); err != nil {
		return err
	}
	// Remove only the Wiki Questioner profile. Keep Wiki data, tools, other
	// agents and historical conversation bindings unchanged.
	if err := db.WithContext(ctx).Exec(`DELETE FROM custom_agents WHERE id = 'builtin-wiki-researcher' AND is_builtin = true`).Error; err != nil {
		return err
	}
	if err := enableAgentMultimodalDefaults(ctx, db); err != nil {
		return err
	}
	// Keep persisted editor/API configuration aligned with the shared policy.
	// This changes only the budget, preserving each profile's identity and tools.
	if err := db.WithContext(ctx).Exec(`UPDATE custom_agents SET config = jsonb_set(config::jsonb, '{max_iterations}',
        to_jsonb(CASE WHEN config->>'agent_type' = 'knowledge-qa' THEN 15
                      WHEN config->>'enable_artifacts' = 'true' THEN 100 ELSE 50 END))
        WHERE deleted_at IS NULL AND (config->>'max_iterations')::integer IS DISTINCT FROM
            CASE WHEN config->>'agent_type' = 'knowledge-qa' THEN 15
                 WHEN config->>'enable_artifacts' = 'true' THEN 100 ELSE 50 END`).Error; err != nil {
		return err
	}
	if err := db.WithContext(ctx).Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS error_code varchar(40) NOT NULL DEFAULT ''`).Error; err != nil {
		return err
	}
	if err := db.WithContext(ctx).AutoMigrate(&Artifact{}); err != nil {
		return err
	}
	return db.WithContext(ctx).Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_custom_general_agent_artifact_delivery
        ON custom_general_agent_artifacts (tenant_id, run_id, file_token) WHERE deleted_at IS NULL`).Error
}
