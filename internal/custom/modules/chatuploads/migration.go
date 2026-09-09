package chatuploads

import (
	"context"
	"fmt"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

func (s *Service) Migrate(ctx context.Context) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&OriginalUpload{}); err != nil {
			return fmt.Errorf("install chat original uploads: %w", err)
		}
		for _, f := range []struct{ name, ddl string }{
			{"ChatSessionID", "chat_session_id VARCHAR(36) NOT NULL DEFAULT ''"},
			{"ChatOwnerID", "chat_owner_id VARCHAR(512) NOT NULL DEFAULT ''"},
		} {
			if !tx.Migrator().HasColumn(&types.KnowledgeBase{}, f.name) {
				if err := tx.Exec("ALTER TABLE knowledge_bases ADD COLUMN " + f.ddl).Error; err != nil {
					return fmt.Errorf("install private chat source scope: %w", err)
				}
			}
		}
		return tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_source_session
			ON knowledge_bases (tenant_id, chat_session_id) WHERE chat_session_id <> '' AND deleted_at IS NULL`).Error
	})
}
