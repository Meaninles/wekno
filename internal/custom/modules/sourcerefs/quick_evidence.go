package sourcerefs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// ExactEvidenceChunkRepository is deliberately narrower than the application
// repository contract so evidence resolution can be tested as an isolated,
// deterministic boundary.
type ExactEvidenceChunkRepository interface {
	ListChunksByIDOnly(ctx context.Context, ids []string) ([]*types.Chunk, error)
	ListChunksByParentIDs(ctx context.Context, tenantID uint64, parentIDs []string) ([]*types.Chunk, error)
}

// ResolveQuickAnswerEvidence converts mutable quick-answer merge results into
// exact, independently citable evidence units. MergeResult.Content is model
// context and may contain a full parent, neighboring chunks or overlap-merged
// text; it is never accepted as document evidence when more than one physical
// chunk contributed to it.
func ResolveQuickAnswerEvidence(
	ctx context.Context,
	repo ExactEvidenceChunkRepository,
	tenantID uint64,
	merged []*types.SearchResult,
) ([]*types.SearchResult, error) {
	if len(merged) == 0 {
		return nil, nil
	}

	parentIDsByTenant := make(map[uint64][]string)
	seenParent := make(map[string]struct{})
	chunkIDs := make([]string, 0)
	seenChunk := make(map[string]struct{})
	addChunkID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seenChunk[id]; exists {
			return
		}
		seenChunk[id] = struct{}{}
		chunkIDs = append(chunkIDs, id)
	}

	for _, result := range merged {
		if result == nil || SourceTypeFromRef(result) != SourceTypeKnowledge {
			continue
		}
		// FAQ answer text is assembled from the authoritative FAQ metadata by the
		// merge stage. The stored chunk body may intentionally omit answers when
		// the FAQ index is configured as question-only, so replacing it with the
		// raw indexed body would silently remove the evidence used by the model.
		if result.ChunkType == string(types.ChunkTypeFAQ) {
			continue
		}
		sourceTenantID := result.SourceTenantID
		if sourceTenantID == 0 {
			sourceTenantID = tenantID
		}
		if result.ParentChunkID != "" && result.ChunkType == string(types.ChunkTypeText) {
			if _, exists := seenParent[result.ParentChunkID]; !exists {
				seenParent[result.ParentChunkID] = struct{}{}
				parentIDsByTenant[sourceTenantID] = append(parentIDsByTenant[sourceTenantID], result.ParentChunkID)
			}
			continue
		}
		if result.ParentChunkID != "" && isImageChunkType(result.ChunkType) {
			// Image retrieval is resolved to its text parent for answer context. The
			// text parent is the user-readable document fragment; the OCR/caption
			// vector remains retrieval provenance only.
			addChunkID(result.ParentChunkID)
			continue
		}
		// A direct, unmerged SearchResult.Content is already the DB chunk body.
		// Only overlap/neighbor membership needs a batched reread. This keeps the
		// common quick-answer path free of an unnecessary database round trip.
		if len(result.SubChunkID) > 0 {
			addChunkID(result.ID)
			for _, id := range result.SubChunkID {
				addChunkID(id)
			}
		}
	}

	childrenByParent := make(map[string][]*types.Chunk, len(seenParent))
	chunkMap := make(map[string]*types.Chunk, len(chunkIDs))
	if len(parentIDsByTenant) > 0 && repo == nil {
		return nil, fmt.Errorf("resolve exact parent evidence: chunk repository is unavailable")
	}
	for sourceTenantID, parentIDs := range parentIDsByTenant {
		if sourceTenantID == 0 {
			return nil, fmt.Errorf("resolve exact parent evidence: source tenant is unavailable")
		}
		children, err := repo.ListChunksByParentIDs(ctx, sourceTenantID, parentIDs)
		if err != nil {
			return nil, fmt.Errorf("resolve exact parent evidence: %w", err)
		}
		for _, chunk := range children {
			if chunk == nil || !isExactDocumentEvidenceChunk(chunk) {
				continue
			}
			childrenByParent[chunk.ParentChunkID] = append(childrenByParent[chunk.ParentChunkID], chunk)
			chunkMap[chunk.ID] = chunk
		}
	}
	for parentID := range seenParent {
		if len(childrenByParent[parentID]) == 0 {
			return nil, fmt.Errorf("resolve exact parent evidence: parent %s has no claim-bearing child chunks", parentID)
		}
		sortChunksBySourcePosition(childrenByParent[parentID])
	}

	missingIDs := make([]string, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		if _, exists := chunkMap[id]; !exists {
			missingIDs = append(missingIDs, id)
		}
	}
	if len(missingIDs) > 0 {
		if repo != nil {
			// IDs originate exclusively from the already-authorized retrieval result.
			// The cross-tenant batch method is required for shared knowledge bases,
			// whose source chunks are owned by a tenant different from the requester.
			chunks, err := repo.ListChunksByIDOnly(ctx, missingIDs)
			if err != nil {
				return nil, fmt.Errorf("resolve exact chunk evidence: %w", err)
			}
			for _, chunk := range chunks {
				if chunk != nil {
					chunkMap[chunk.ID] = chunk
				}
			}
		}
	}

	resolved := make([]*types.SearchResult, 0, len(merged))
	seenEvidence := make(map[string]struct{})
	appendEvidence := func(ref *types.SearchResult) {
		if ref == nil {
			return
		}
		key := CitationKey(ref)
		if key == "" {
			return
		}
		if _, exists := seenEvidence[key]; exists {
			return
		}
		seenEvidence[key] = struct{}{}
		resolved = append(resolved, ref)
	}

	for _, result := range merged {
		if result == nil {
			continue
		}
		sourceType := SourceTypeFromRef(result)
		if sourceType != SourceTypeKnowledge {
			// Wiki pages, fetched webpages and structured-data analysis already
			// carry their authoritative tool snapshot. Structured data is retained
			// for model context/statistics but will not receive a citation ID.
			appendEvidence(exactSnapshotFromResult(result))
			continue
		}
		if result.ChunkType == string(types.ChunkTypeFAQ) {
			appendEvidence(exactSnapshotFromResult(result))
			continue
		}

		var chunks []*types.Chunk
		switch {
		case result.ParentChunkID != "" && result.ChunkType == string(types.ChunkTypeText):
			chunks = childrenByParent[result.ParentChunkID]
		case result.ParentChunkID != "" && isImageChunkType(result.ChunkType):
			if chunk := chunkMap[result.ParentChunkID]; chunk != nil {
				chunks = []*types.Chunk{chunk}
			}
		default:
			ids := append([]string{result.ID}, result.SubChunkID...)
			localSeen := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				if _, exists := localSeen[id]; exists {
					continue
				}
				localSeen[id] = struct{}{}
				if chunk := chunkMap[id]; chunk != nil && isExactDocumentEvidenceChunk(chunk) {
					chunks = append(chunks, chunk)
				}
			}
			sortChunksBySourcePosition(chunks)
		}

		if len(chunks) == 0 {
			// A single unexpanded result is already the DB-backed body assembled by
			// knowledgebase_search_results. This path also keeps direct-load/FAQ
			// evidence working in isolated unit tests. Any parent/merged result was
			// handled above and is never allowed to fall back to aggregate content.
			if result.ParentChunkID == "" && len(result.SubChunkID) == 0 && strings.TrimSpace(result.Content) != "" {
				appendEvidence(exactSnapshotFromResult(result))
				continue
			}
			return nil, fmt.Errorf("resolve exact evidence for merged result %s: no source chunks found", result.ID)
		}

		for _, chunk := range chunks {
			ref := exactReferenceFromChunk(result, chunk)
			if ref.ImageInfo == "" && (chunk.ID == result.ID ||
				(isImageChunkType(result.ChunkType) && chunk.ID == result.ParentChunkID)) {
				// Parent expansion orders children by source position, not retrieval
				// relevance. Keep matched-child image context on the child that was
				// actually retrieved instead of leaking it onto the first sibling.
				ref.ImageInfo = result.ImageInfo
			}
			appendEvidence(ref)
		}
	}

	return resolved, nil
}

// CitableReferences returns exactly the three product-visible source types.
func CitableReferences(refs []*types.SearchResult) []*types.SearchResult {
	out := make([]*types.SearchResult, 0, len(refs))
	for _, ref := range refs {
		if IsSupportedCitationReference(ref) {
			out = append(out, ref)
		}
	}
	return out
}

func exactReferenceFromChunk(template *types.SearchResult, chunk *types.Chunk) *types.SearchResult {
	ref := cloneSearchResult(template)
	ref.ID = chunk.ID
	ref.Content = chunk.Content
	ref.EvidenceContent = chunk.Content
	ref.MatchedContent = ""
	ref.MatchedSourceID = ""
	ref.MatchOrigin = ""
	ref.ChunkIndex = chunk.ChunkIndex
	ref.Seq = chunk.ChunkIndex
	ref.StartAt = chunk.StartAt
	ref.EndAt = chunk.EndAt
	ref.SubChunkID = nil
	ref.ChunkType = string(chunk.ChunkType)
	ref.ParentChunkID = chunk.ParentChunkID
	ref.ImageInfo = chunk.ImageInfo
	ref.ChunkMetadata = append(types.JSON(nil), chunk.Metadata...)
	ref.SourceLocator = append(types.JSON(nil), chunk.SourceLocator...)
	if ref.Metadata != nil {
		delete(ref.Metadata, MetadataCitationID)
		delete(ref.Metadata, MetadataEvidenceHash)
		delete(ref.Metadata, MetadataObservedAt)
		delete(ref.Metadata, MetadataChunkID)
	}
	return ref
}

func exactSnapshotFromResult(result *types.SearchResult) *types.SearchResult {
	ref := cloneSearchResult(result)
	ref.EvidenceContent = result.EvidenceContent
	if strings.TrimSpace(ref.EvidenceContent) == "" {
		ref.EvidenceContent = result.Content
	}
	ref.Content = ref.EvidenceContent
	ref.MatchedContent = ""
	ref.MatchedSourceID = ""
	ref.MatchOrigin = ""
	ref.SubChunkID = nil
	if ref.Metadata != nil {
		delete(ref.Metadata, MetadataCitationID)
		delete(ref.Metadata, MetadataEvidenceHash)
		delete(ref.Metadata, MetadataObservedAt)
	}
	return ref
}

func sortChunksBySourcePosition(chunks []*types.Chunk) {
	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].StartAt == chunks[j].StartAt {
			if chunks[i].EndAt == chunks[j].EndAt {
				return chunks[i].ChunkIndex < chunks[j].ChunkIndex
			}
			return chunks[i].EndAt < chunks[j].EndAt
		}
		return chunks[i].StartAt < chunks[j].StartAt
	})
}

func isImageChunkType(chunkType string) bool {
	return chunkType == string(types.ChunkTypeImageOCR) || chunkType == string(types.ChunkTypeImageCaption)
}

func isExactDocumentEvidenceChunk(chunk *types.Chunk) bool {
	if chunk == nil || strings.TrimSpace(chunk.Content) == "" {
		return false
	}
	switch chunk.ChunkType {
	case types.ChunkTypeParentText, types.ChunkTypeImageOCR, types.ChunkTypeImageCaption:
		return false
	default:
		return true
	}
}
