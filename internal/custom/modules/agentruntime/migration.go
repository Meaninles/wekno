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
	if err := db.WithContext(ctx).Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS error_code varchar(40) NOT NULL DEFAULT ''`).Error; err != nil {
		return err
	}
	if err := db.WithContext(ctx).AutoMigrate(&Artifact{}); err != nil {
		return err
	}
	return db.WithContext(ctx).Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_custom_general_agent_artifact_delivery
        ON custom_general_agent_artifacts (tenant_id, run_id, file_token) WHERE deleted_at IS NULL`).Error
}
