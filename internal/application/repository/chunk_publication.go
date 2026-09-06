package repository

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// Internal parsers can read their draft version; evidence reads explicitly
// enter this gate, including parent/neighbor/image expansion and paged tools.
func publishedChunkQuery(ctx context.Context, query *gorm.DB) *gorm.DB {
	if !types.PublishedChunksOnly(ctx) {
		return query
	}
	return query.Where(`chunks.is_enabled = true AND EXISTS (
		SELECT 1 FROM knowledges k WHERE k.id = chunks.knowledge_id
		AND k.tenant_id = chunks.tenant_id AND k.knowledge_base_id = chunks.knowledge_base_id
		AND k.deleted_at IS NULL AND k.enable_status = 'enabled'
		AND k.publication_state = 'published'
		AND ((k.type = 'faq' AND k.core_status = 'ready') OR
		(k.published_generation <> '' AND k.published_generation = chunks.processing_generation)))`)
}
