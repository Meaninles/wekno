package sourcerefs

import (
	"context"
	"fmt"
	"sort"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// DocumentSearchTargets gives an explicitly selected document precedence over
// an overlapping whole-KB target for document retrieval tools. Wiki tools keep
// the original whole-KB scope: selecting a file and its Wiki-capable KB in the
// same turn therefore means "this file for document evidence, this KB for
// Wiki evidence" instead of accidentally scanning every document in the KB.
//
// Non-overlapping KBs and tag scopes are preserved. The operation performs one
// small local metadata query only when both explicit document IDs and search
// targets are present; it never calls a model or retrieval backend.
func DocumentSearchTargets(
	ctx context.Context,
	db *gorm.DB,
	targets types.SearchTargets,
	explicitKnowledgeIDs []string,
) (types.SearchTargets, error) {
	knowledgeIDs := uniqueNonEmpty(explicitKnowledgeIDs)
	if len(knowledgeIDs) == 0 || len(targets) == 0 {
		return cloneSearchTargets(targets), nil
	}
	if db == nil {
		return nil, fmt.Errorf("resolve explicit document search scope: database is unavailable")
	}

	type knowledgeScopeRow struct {
		ID              string `gorm:"column:id"`
		KnowledgeBaseID string `gorm:"column:knowledge_base_id"`
	}
	var rows []knowledgeScopeRow
	if err := db.WithContext(ctx).
		Table("knowledges").
		Select("id", "knowledge_base_id").
		Where("id IN ?", knowledgeIDs).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("resolve explicit document search scope: %w", err)
	}

	byKB := make(map[string][]string)
	resolved := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.ID == "" || row.KnowledgeBaseID == "" {
			continue
		}
		resolved[row.ID] = true
		byKB[row.KnowledgeBaseID] = append(byKB[row.KnowledgeBaseID], row.ID)
	}
	if len(resolved) != len(knowledgeIDs) {
		missing := make([]string, 0, len(knowledgeIDs)-len(resolved))
		for _, id := range knowledgeIDs {
			if !resolved[id] {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("resolve explicit document search scope: unknown knowledge IDs %v", missing)
	}

	narrowed, covered := narrowDocumentSearchTargets(targets, byKB)
	for kbID := range byKB {
		if !covered[kbID] {
			return nil, fmt.Errorf("resolve explicit document search scope: knowledge base %s is outside the active scope", kbID)
		}
	}
	return narrowed, nil
}

func narrowDocumentSearchTargets(
	targets types.SearchTargets,
	explicitByKB map[string][]string,
) (types.SearchTargets, map[string]bool) {
	out := make(types.SearchTargets, 0, len(targets))
	covered := make(map[string]bool, len(explicitByKB))
	for _, target := range targets {
		if target == nil {
			continue
		}
		cloned := cloneSearchTarget(target)
		ids := uniqueNonEmpty(explicitByKB[target.KnowledgeBaseID])
		if len(ids) > 0 {
			// A tag target is an independently selected narrow scope and stays
			// alongside the explicit files. Only a whole-KB target is replaced.
			if target.Type == types.SearchTargetTypeKnowledgeBase && len(target.TagIDs) == 0 {
				cloned.Type = types.SearchTargetTypeKnowledge
				cloned.KnowledgeIDs = ids
				covered[target.KnowledgeBaseID] = true
			} else if target.Type == types.SearchTargetTypeKnowledge {
				cloned.KnowledgeIDs = uniqueNonEmpty(append(cloned.KnowledgeIDs, ids...))
				covered[target.KnowledgeBaseID] = true
			}
		}
		out = append(out, cloned)
	}
	return out, covered
}

func cloneSearchTargets(targets types.SearchTargets) types.SearchTargets {
	out := make(types.SearchTargets, 0, len(targets))
	for _, target := range targets {
		if target != nil {
			out = append(out, cloneSearchTarget(target))
		}
	}
	return out
}

func cloneSearchTarget(target *types.SearchTarget) *types.SearchTarget {
	cloned := *target
	cloned.KnowledgeIDs = append([]string(nil), target.KnowledgeIDs...)
	cloned.TagIDs = append([]string(nil), target.TagIDs...)
	return &cloned
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
