// Package grepsearch owns the exact-text read model. It never classifies user
// questions, substitutes BM25/LIKE for regex, or falls back to a stale projection.
package grepsearch

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const CandidateLimit = 500

type Row struct {
	types.Chunk
	KnowledgeTitle  string `gorm:"column:knowledge_title"`
	TotalChunkCount int    `gorm:"-"`
}

// Search accepts only an internally constructed scope with bound arguments;
// callers must resolve file/tag/KB permissions before constructing that scope.
func Search(ctx context.Context, db *gorm.DB, scope string, scopeArgs []interface{}, patterns []string) ([]Row, error) {
	if db == nil || db.Dialector.Name() != "postgres" {
		return nil, fmt.Errorf("grep projection requires PostgreSQL")
	}
	if strings.TrimSpace(scope) == "" || len(patterns) == 0 {
		return nil, nil
	}
	conditions := make([]string, 0, len(patterns))
	args := append([]interface{}{}, scopeArgs...)
	for _, pattern := range patterns {
		conditions = append(conditions, "(chunks.content ~* ? OR chunks.knowledge_title ~* ?)")
		args = append(args, pattern, pattern)
	}
	args = append(args, CandidateLimit)
	query := `WITH selected AS MATERIALIZED (
        SELECT chunks.id FROM custom_grepsearch_chunks AS chunks
        WHERE (` + scope + `) AND (` + strings.Join(conditions, " OR ") + `)
        ORDER BY chunks.created_at DESC, chunks.id DESC LIMIT ?)
        SELECT chunks.id, chunks.content, chunks.chunk_index, chunks.knowledge_id,
            chunks.knowledge_base_id, chunks.chunk_type, chunks.metadata,
            chunks.source_locator, chunks.created_at, knowledges.title AS knowledge_title
        FROM selected JOIN chunks ON chunks.id=selected.id
        JOIN knowledges ON knowledges.id=chunks.knowledge_id
        ORDER BY chunks.created_at DESC, chunks.id DESC`
	var rows []Row
	started := time.Now()
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		acquired := time.Now()
		if err := tx.Raw(query, args...).Scan(&rows).Error; err != nil {
			return fmt.Errorf("grep candidates/payload: %w", err)
		}
		fetched := time.Now()
		ids := make([]string, 0, len(rows))
		seen := make(map[string]bool)
		for _, row := range rows {
			if !seen[row.KnowledgeID] {
				seen[row.KnowledgeID] = true
				ids = append(ids, row.KnowledgeID)
			}
		}
		if len(ids) > 0 {
			var counts []struct {
				KnowledgeID string
				Cnt         int
			}
			// Keep the original count's semantics: it includes summary chunks.
			if err := tx.Table("chunks").Select("knowledge_id,COUNT(*) AS cnt").Where("knowledge_id IN ? AND is_enabled = ? AND deleted_at IS NULL", ids, true).Group("knowledge_id").Scan(&counts).Error; err != nil {
				return fmt.Errorf("grep chunk counts: %w", err)
			}
			byID := make(map[string]int, len(counts))
			for _, count := range counts {
				byID[count.KnowledgeID] = count.Cnt
			}
			for i := range rows {
				rows[i].TotalChunkCount = byID[rows[i].KnowledgeID]
			}
		}
		logger.Infof(ctx, "[GrepProjection] candidates=%d acquire_ms=%d fetch_ms=%d count_ms=%d", len(rows), acquired.Sub(started).Milliseconds(), fetched.Sub(acquired).Milliseconds(), time.Since(fetched).Milliseconds())
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
