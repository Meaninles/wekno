package knowledgeaux

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// Migrate runs in the one-shot custom migration role, including deployments
// where native AUTO_MIGRATE is disabled. Publication metadata and its backfill
// are one transaction; a restart cannot see a half-installed publication gate.
func Migrate(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("knowledge publication database is unavailable")
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(734819025)").Error; err != nil {
				return err
			}
		} else if tx.Dialector.Name() != "sqlite" {
			return fmt.Errorf("unsupported knowledge publication database %q", tx.Dialector.Name())
		}
		backfill := !tx.Migrator().HasColumn(&types.Knowledge{}, "PublishedGeneration")
		for _, field := range []struct{ name, ddl string }{
			{"PublicationState", "publication_state VARCHAR(16) NOT NULL DEFAULT 'published'"},
			{"PublishedGeneration", "published_generation VARCHAR(36) NOT NULL DEFAULT ''"},
			{"PublishedEmbeddingModelID", "published_embedding_model_id VARCHAR(64) NOT NULL DEFAULT ''"},
		} {
			if !tx.Migrator().HasColumn(&types.Knowledge{}, field.name) {
				if err := tx.Exec("ALTER TABLE knowledges ADD COLUMN " + field.ddl).Error; err != nil {
					return fmt.Errorf("install knowledge publication %s: %w", field.name, err)
				}
			}
		}
		if backfill {
			// The visible chunks identify the readable version even when a newer
			// processing generation has failed or is still in progress.
			if err := tx.Exec(`UPDATE knowledges SET
				published_generation = COALESCE((SELECT c.processing_generation FROM chunks c
				WHERE c.tenant_id = knowledges.tenant_id AND c.knowledge_id = knowledges.id
				AND c.is_enabled = true AND c.deleted_at IS NULL
				ORDER BY c.created_at DESC, c.id DESC LIMIT 1), ''),
				published_embedding_model_id = embedding_model_id`).Error; err != nil {
				return fmt.Errorf("backfill readable knowledge generation: %w", err)
			}
		}
		// Publication, retirement and FAQ status changes address vectors by
		// chunk identity. A full-text index does not serve equality updates;
		// without this B-tree each publication batch scans every document.
		if tx.Dialector.Name() == "postgres" && tx.Migrator().HasTable("embeddings") {
			if err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_embeddings_chunk_id
				ON embeddings (chunk_id)`).Error; err != nil {
				return fmt.Errorf("install vector chunk identity index: %w", err)
			}
		}
		return tx.Exec(`CREATE INDEX IF NOT EXISTS idx_knowledge_publication
			ON knowledges (tenant_id, knowledge_base_id, publication_state)`).Error
	})
}
