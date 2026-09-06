package sourcerefs

import (
	"context"
	"fmt"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ReuseEvidence revalidates exact snapshots in the current permission-checked
// search scope. It never promotes old assistant prose or a search summary.
// A changed, disabled, deleted or out-of-scope source requires a new read.
func ReuseEvidence(ctx context.Context, db *gorm.DB, targets types.SearchTargets, refs []*types.SearchResult) ([]*types.SearchResult, []string, error) {
	valid := make([]*types.SearchResult, 0, len(refs))
	invalid := make([]string, 0)
	sources, err := loadReusableSources(ctx, db, targets, refs)
	if err != nil {
		return nil, nil, err
	}
	for _, saved := range refs {
		if saved == nil {
			continue
		}
		ref := cloneSearchResult(saved)
		accepted := false
		for targetIndex, target := range targets {
			if target == nil || target.KnowledgeBaseID != ref.KnowledgeBaseID || target.TenantID == 0 {
				continue
			}
			if target.Type == types.SearchTargetTypeKnowledge && !slices.Contains(target.KnowledgeIDs, ref.KnowledgeID) {
				continue
			}
			switch SourceTypeFromRef(ref) {
			case SourceTypeKnowledge:
				chunk := sources.chunks[reusableKey(targetIndex, knowledgeChunkID(ref))]
				knowledge := sources.knowledges[reusableKey(targetIndex, ref.KnowledgeID)]
				if chunk == nil || knowledge == nil || chunk.KnowledgeID != ref.KnowledgeID || !knowledge.IsPublishedChunk(chunk) {
					continue
				}
				observed, err := time.Parse(time.RFC3339Nano, ref.Metadata[MetadataObservedAt])
				if err != nil || chunk.UpdatedAt.After(observed) {
					continue
				}
				// FAQ answers are drawn from structured metadata; timestamp + metadata
				// identity is required in addition to the evidence snapshot hash.
				if chunk.ChunkType == types.ChunkTypeFAQ {
					if len(ref.ChunkMetadata) == 0 || string(chunk.Metadata) != string(ref.ChunkMetadata) {
						continue
					}
				} else {
					ref.EvidenceContent = strings.TrimSpace(chunk.Content)
				}
				ref.Content = ref.EvidenceContent
				ref.SourceLocator = chunk.SourceLocator
				ref.StartAt = chunk.StartAt
				ref.EndAt = chunk.EndAt
				ref.ChunkIndex = chunk.ChunkIndex
				ref.KnowledgeTitle = knowledge.Title
				ref.KnowledgeFilename = knowledge.FileName
				ref.SourceTenantID = target.TenantID
				accepted = true
			case SourceTypeWiki:
				if target.Type == types.SearchTargetTypeKnowledge || len(target.TagIDs) > 0 {
					continue
				}
				page := sources.pages[reusableKey(targetIndex, ref.Metadata["page_id"])]
				if page == nil || page.Slug != ref.Metadata["slug"] || page.Status == types.WikiPageStatusArchived || strconv.Itoa(page.Version) != ref.Metadata["page_version"] || page.PageType == types.WikiPageTypeIndex || page.PageType == types.WikiPageTypeLog {
					continue
				}
				ref.EvidenceContent = strings.TrimSpace(strings.TrimSpace(page.Summary) + "\n\n" + strings.TrimSpace(page.Content))
				ref.Content = ref.EvidenceContent
				ref.KnowledgeTitle = page.Title
				accepted = true
			}
			if accepted {
				break
			}
		}
		oldHash := ref.Metadata[MetadataEvidenceHash]
		if accepted && oldHash != "" {
			delete(ref.Metadata, MetadataCitationID)
			delete(ref.Metadata, MetadataEvidenceHash)
			delete(ref.Metadata, MetadataObservedAt)
			ensureEvidenceSnapshotMetadata(ref)
			accepted = ref.Metadata[MetadataEvidenceHash] == oldHash
		} else {
			accepted = false
		}
		if accepted {
			valid = append(valid, ref)
		} else {
			invalid = append(invalid, fmt.Sprintf("%s:%s", SourceTypeFromRef(saved), saved.ID))
		}
	}
	return valid, invalid, nil
}

type reusableSources struct {
	chunks     map[string]*types.Chunk
	knowledges map[string]*types.Knowledge
	pages      map[string]*types.WikiPage
}

func reusableKey(targetIndex int, id string) string { return strconv.Itoa(targetIndex) + ":" + id }

// Fetch by authorized target, not by fragment. Each source kind needs at most
// two queries per target even when a prior answer contains many fragments.
// Version/hash validation remains per snapshot, with no cross-request cache.
func loadReusableSources(ctx context.Context, db *gorm.DB, targets types.SearchTargets, refs []*types.SearchResult) (*reusableSources, error) {
	out := &reusableSources{chunks: map[string]*types.Chunk{}, knowledges: map[string]*types.Knowledge{}, pages: map[string]*types.WikiPage{}}
	for index, target := range targets {
		if target == nil || target.TenantID == 0 {
			continue
		}
		var chunkIDs, pageIDs []string
		for _, ref := range refs {
			if ref == nil || ref.KnowledgeBaseID != target.KnowledgeBaseID {
				continue
			}
			if target.Type == types.SearchTargetTypeKnowledge && !slices.Contains(target.KnowledgeIDs, ref.KnowledgeID) {
				continue
			}
			switch SourceTypeFromRef(ref) {
			case SourceTypeKnowledge:
				chunkIDs = append(chunkIDs, knowledgeChunkID(ref))
			case SourceTypeWiki:
				if target.Type != types.SearchTargetTypeKnowledge && len(target.TagIDs) == 0 {
					pageIDs = append(pageIDs, ref.Metadata["page_id"])
				}
			}
		}
		if len(chunkIDs) > 0 {
			var chunks []*types.Chunk
			query := db.WithContext(ctx).Where("id IN ? AND tenant_id = ? AND knowledge_base_id = ? AND is_enabled = ?", chunkIDs, target.TenantID, target.KnowledgeBaseID, true)
			if len(target.TagIDs) > 0 {
				query = query.Where("tag_id IN ? OR EXISTS (SELECT 1 FROM knowledge_tag_relations r WHERE r.knowledge_id = chunks.knowledge_id AND r.tag_id IN ?)", target.TagIDs, target.TagIDs)
			}
			if err := query.Find(&chunks).Error; err != nil {
				return nil, err
			}
			var knowledgeIDs []string
			for _, chunk := range chunks {
				out.chunks[reusableKey(index, chunk.ID)] = chunk
				knowledgeIDs = append(knowledgeIDs, chunk.KnowledgeID)
			}
			if len(knowledgeIDs) > 0 {
				var knowledges []*types.Knowledge
				if err := db.WithContext(ctx).Where("id IN ? AND tenant_id = ? AND knowledge_base_id = ? AND enable_status = ?", knowledgeIDs, target.TenantID, target.KnowledgeBaseID, "enabled").Find(&knowledges).Error; err != nil {
					return nil, err
				}
				for _, knowledge := range knowledges {
					out.knowledges[reusableKey(index, knowledge.ID)] = knowledge
				}
			}
		}
		if len(pageIDs) > 0 {
			var pages []*types.WikiPage
			if err := db.WithContext(ctx).Where("id IN ? AND tenant_id = ? AND knowledge_base_id = ?", pageIDs, target.TenantID, target.KnowledgeBaseID).Find(&pages).Error; err != nil {
				return nil, err
			}
			for _, page := range pages {
				out.pages[reusableKey(index, page.ID)] = page
			}
		}
	}
	return out, nil
}
