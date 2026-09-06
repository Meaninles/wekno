package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var listKnowledgeChunksTool = BaseTool{
	name: ToolListKnowledgeChunks,
	description: `Retrieve full chunk content for a document or a single FAQ entry.

## Use to complete insufficient grep_chunks or knowledge_search evidence:
- Do not call this tool when a search result already contains complete claim-bearing content plus a current canonical citation handle.
- **Incomplete FAQ hit** (type faq): list_knowledge_chunks(faq_id="<chunk_id from search>") — reads that one FAQ entry with answers from metadata.
- **Incomplete document hit**: list_knowledge_chunks(chunk_id="<chunk_id from search>") — loads that exact text chunk plus one adjacent text chunk on each side when the search result is truncated, catalog-only, ambiguous, handle-less, or needs exact surrounding context.
- **Whole document (exhaustive review only)**: list_knowledge_chunks(knowledge_id="<document id>") — pages through chunks. For pinpoint questions or multiple named topics, first use a targeted grep_chunks or knowledge_search query, then load only evidence gaps; otherwise bounded tool output can hide later evidence.

## Parameters (provide exactly one id target):
- faq_id (optional): FAQ entry ID from grep_chunks / knowledge_search.
- chunk_id (optional): Single non-FAQ chunk ID (do not use for FAQ — use faq_id).
- knowledge_id (optional): Document/knowledge ID to page through all chunks.
- limit / offset: Only for knowledge_id paging (default limit 20, max 100).
- Never derive offset from chunk_index. chunk_index is a logical coordinate and
  can be sparse in historical documents; offset is a zero-based ordinal in the
  filtered document stream. Follow next_offset from the previous page.
- chunk_index is never a page number, spreadsheet row, source line, JSON item,
  image frame/tile, or audio timestamp. Cite source_locator and the record keys
  present in content; source_locator always refers to the original unsplit file.

## Output:
Full chunk content. FAQ entries include <faq> with <answer> from metadata.`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "faq_id": {
      "type": "string",
      "description": "FAQ entry ID (same as chunk_id). Use for FAQ hits instead of knowledge_id."
    },
    "chunk_id": {
      "type": "string",
      "description": "Single chunk ID (alias of faq_id)"
    },
    "knowledge_id": {
      "type": "string",
      "description": "Document/knowledge ID to list all chunks"
    },
    "limit": {
      "type": "integer",
      "description": "Chunks per page when using knowledge_id (default 20, max 100)",
      "default": 20,
      "minimum": 1,
      "maximum": 100
    },
    "offset": {
      "type": "integer",
      "description": "Zero-based ordinal start position for knowledge_id paging (default 0). Never copy chunk_index into offset; use next_offset returned by the previous page.",
      "default": 0,
      "minimum": 0
    }
  }
}`),
}

// ListKnowledgeChunksInput defines the input parameters for list knowledge chunks tool
type ListKnowledgeChunksInput struct {
	KnowledgeID string `json:"knowledge_id,omitempty"`
	FAQID       string `json:"faq_id,omitempty"`
	ChunkID     string `json:"chunk_id,omitempty"`
	Limit       int    `json:"limit"`
	Offset      int    `json:"offset"`
}

// ListKnowledgeChunksTool retrieves chunk snapshots for a specific knowledge document.
type ListKnowledgeChunksTool struct {
	BaseTool
	chunkService     interfaces.ChunkService
	knowledgeService interfaces.KnowledgeService
	searchTargets    types.SearchTargets // Pre-computed unified search targets with KB-tenant mapping
}

// NewListKnowledgeChunksTool creates a new tool instance.
func NewListKnowledgeChunksTool(
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	searchTargets types.SearchTargets,
) *ListKnowledgeChunksTool {
	return &ListKnowledgeChunksTool{
		BaseTool:         listKnowledgeChunksTool,
		chunkService:     chunkService,
		knowledgeService: knowledgeService,
		searchTargets:    searchTargets,
	}
}

// Execute performs the chunk fetch against the chunk service.
func (t *ListKnowledgeChunksTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	ctx = types.WithPublishedChunks(ctx)
	// Parse args from json.RawMessage
	var input ListKnowledgeChunksInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse args: %v", err),
		}, err
	}

	chunkID := strings.TrimSpace(input.FAQID)
	if chunkID == "" {
		chunkID = strings.TrimSpace(input.ChunkID)
	}
	if chunkID != "" {
		return t.executeByChunkID(ctx, chunkID)
	}

	knowledgeID := strings.TrimSpace(input.KnowledgeID)
	if knowledgeID == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "one of faq_id, chunk_id, or knowledge_id is required",
		}, fmt.Errorf("missing id parameter")
	}

	// Get knowledge info without tenant filter to support shared KB
	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Knowledge not found: %v", err),
		}, err
	}

	// Verify the knowledge's KB is in searchTargets (permission check)
	if !t.searchTargets.ContainsKB(knowledge.KnowledgeBaseID) {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Knowledge base %s is not accessible", knowledge.KnowledgeBaseID),
		}, fmt.Errorf("knowledge base not in search targets")
	}
	allowed, err := searchTargetsAllowKnowledgeID(ctx, t.searchTargets, knowledge.ID, knowledge.KnowledgeBaseID, t.knowledgeService)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("failed to validate knowledge scope: %v", err),
		}, err
	}
	if !allowed {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Knowledge %s is not within the current @mention scope", knowledge.ID),
		}, fmt.Errorf("knowledge not in search target scope")
	}

	// Use the knowledge's actual tenant_id for chunk query (supports cross-tenant shared KB)
	effectiveTenantID := knowledge.TenantID

	chunkLimit := 20
	if input.Limit > 0 {
		chunkLimit = input.Limit
	}
	offset := 0
	if input.Offset > 0 {
		offset = input.Offset
	}
	if offset < 0 {
		offset = 0
	}

	pagination := &types.Pagination{
		StartOffset: &offset,
		Page:        offset/chunkLimit + 1,
		PageSize:    chunkLimit,
	}

	chunks, total, err := t.chunkService.GetRepository().ListPagedChunksByKnowledgeID(ctx,
		effectiveTenantID, knowledgeID, pagination, []types.ChunkType{types.ChunkTypeText, types.ChunkTypeFAQ}, "", "", "", "", "")
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("failed to list chunks: %v", err),
		}, err
	}
	if chunks == nil {
		return &types.ToolResult{
			Success: false,
			Error:   "chunk query returned no data",
		}, fmt.Errorf("chunk query returned no data")
	}

	totalChunks := total
	fetched := len(chunks)

	// Explicit out-of-range guidance: when the caller paged past the end
	// (offset >= total with total > 0), silently returning fetched=0 is
	// confusing for LLMs that just saw the document in search results. Tell
	// them exactly what happened and what offset would be valid so the next
	// call lands on a real page.
	if fetched == 0 && totalChunks > 0 && int64(offset) >= totalChunks {
		suggestedOffset := totalChunks - int64(chunkLimit)
		if suggestedOffset < 0 {
			suggestedOffset = 0
		}
		return &types.ToolResult{
			Success: false,
			Error: fmt.Sprintf(
				"offset %d is out of range: document has only %d chunks (valid offset range: 0..%d). Retry with offset=%d (or any value < %d).",
				offset, totalChunks, totalChunks-1, suggestedOffset, totalChunks,
			),
			Data: map[string]interface{}{
				"knowledge_id":     knowledgeID,
				"total_chunks":     totalChunks,
				"requested_offset": offset,
				"requested_limit":  chunkLimit,
				"suggested_offset": suggestedOffset,
			},
		}, nil
	}

	// Enrich image info from child image chunks (lazy loading)
	if fetched > 0 {
		chunkIDs := make([]string, 0, fetched)
		for _, c := range chunks {
			chunkIDs = append(chunkIDs, c.ID)
		}
		infoMap := searchutil.CollectImageInfoByChunkIDs(ctx, t.chunkService.GetRepository(), effectiveTenantID, chunkIDs)
		for _, c := range chunks {
			if c.ImageInfo == "" {
				if merged, ok := infoMap[c.ID]; ok {
					c.ImageInfo = merged
				}
			}
		}
	}

	knowledgeTitle := t.lookupKnowledgeTitle(ctx, knowledgeID)

	output := t.buildOutput(knowledgeID, knowledgeTitle, totalChunks, fetched, offset, chunks)

	formattedChunks := make([]map[string]interface{}, 0, len(chunks))
	for idx, c := range chunks {
		chunkData := map[string]interface{}{
			"seq":               idx + 1,
			"chunk_id":          c.ID,
			"chunk_index":       c.ChunkIndex,
			"content":           c.Content,
			"chunk_type":        c.ChunkType,
			"knowledge_id":      c.KnowledgeID,
			"knowledge_base_id": c.KnowledgeBaseID,
			"start_at":          c.StartAt,
			"end_at":            c.EndAt,
			"parent_chunk_id":   c.ParentChunkID,
		}
		if len(c.SourceLocator) > 0 && json.Valid(c.SourceLocator) {
			chunkData["source_locator"] = json.RawMessage(append([]byte(nil), c.SourceLocator...))
		}

		appendFAQChunkData(chunkData, c)
		normalizeFAQChunkDataMap(chunkData, c)

		// 添加图片信息
		if c.ImageInfo != "" {
			var imageInfos []types.ImageInfo
			if err := json.Unmarshal([]byte(c.ImageInfo), &imageInfos); err == nil && len(imageInfos) > 0 {
				imageList := make([]map[string]string, 0, len(imageInfos))
				for _, img := range imageInfos {
					imgData := make(map[string]string)
					if img.URL != "" {
						imgData["url"] = img.URL
					}
					if img.Caption != "" {
						imgData["caption"] = img.Caption
					}
					if img.OCRText != "" {
						imgData["ocr_text"] = img.OCRText
					}
					if len(imgData) > 0 {
						imageList = append(imageList, imgData)
					}
				}
				if len(imageList) > 0 {
					chunkData["images"] = imageList
				}
			}
		}

		formattedChunks = append(formattedChunks, chunkData)
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"display_type":    "knowledge_chunks_list",
			"knowledge_id":    knowledgeID,
			"knowledge_title": knowledgeTitle,
			"total_chunks":    totalChunks,
			"fetched_chunks":  fetched,
			"page":            pagination.Page,
			"page_size":       pagination.PageSize,
			"chunks":          formattedChunks,
		},
	}, nil
}

// executeByChunkID loads one chunk by faq_id / chunk_id (FAQ entry or any chunk).
func (t *ListKnowledgeChunksTool) executeByChunkID(ctx context.Context, chunkID string) (*types.ToolResult, error) {
	chunk, err := t.chunkService.GetChunkByIDOnly(ctx, chunkID)
	if err != nil || chunk == nil || !chunk.IsEnabled {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("chunk not found: %v", err),
		}, err
	}
	if !t.searchTargets.ContainsKB(chunk.KnowledgeBaseID) {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("knowledge base %s is not accessible", chunk.KnowledgeBaseID),
		}, fmt.Errorf("knowledge base not in search targets")
	}
	allowed, scopeErr := searchTargetsAllowKnowledgeID(ctx, t.searchTargets, chunk.KnowledgeID, chunk.KnowledgeBaseID, t.knowledgeService)
	if scopeErr != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("failed to validate chunk scope: %v", scopeErr),
		}, scopeErr
	}
	if !allowed {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("chunk %s is not within the current @mention scope", chunk.ID),
		}, fmt.Errorf("chunk not in search target scope")
	}

	chunks := []*types.Chunk{chunk}
	if chunk.ChunkType == types.ChunkTypeText {
		effectiveTenantID := t.searchTargets.GetTenantIDForKB(chunk.KnowledgeBaseID)
		if effectiveTenantID > 0 {
			// Clause definitions and their numbered conditions frequently straddle
			// parser boundaries. Bounded ±1 expansion is enough to restore that
			// continuity without reading the whole document. Exact-target success
			// remains fail-open if an optional neighbour query fails.
			if neighbours, neighbourErr := t.chunkService.GetRepository().ListAdjacentTextChunks(
				ctx,
				effectiveTenantID,
				chunk.KnowledgeID,
				chunk.ChunkIndex,
				1,
			); neighbourErr == nil {
				chunks = mergeChunkNeighborhood(chunk, neighbours)
			}
		}
	}
	if len(chunks) > 0 {
		effectiveTenantID := t.searchTargets.GetTenantIDForKB(chunk.KnowledgeBaseID)
		if effectiveTenantID > 0 {
			chunkIDs := make([]string, 0, len(chunks))
			for _, item := range chunks {
				chunkIDs = append(chunkIDs, item.ID)
			}
			infoMap := searchutil.CollectImageInfoByChunkIDs(ctx, t.chunkService.GetRepository(), effectiveTenantID, chunkIDs)
			for _, item := range chunks {
				if item.ImageInfo == "" {
					if merged, ok := infoMap[item.ID]; ok {
						item.ImageInfo = merged
					}
				}
			}
		}
	}

	knowledgeTitle := t.lookupKnowledgeTitle(ctx, chunk.KnowledgeID)
	output := t.buildOutput(chunk.KnowledgeID, knowledgeTitle, int64(len(chunks)), len(chunks), 0, chunks)

	formattedChunks := make([]map[string]interface{}, 0, len(chunks))
	for index, item := range chunks {
		chunkData := map[string]interface{}{
			"seq":               index + 1,
			"chunk_id":          item.ID,
			"chunk_index":       item.ChunkIndex,
			"content":           item.Content,
			"chunk_type":        item.ChunkType,
			"knowledge_id":      item.KnowledgeID,
			"knowledge_base_id": item.KnowledgeBaseID,
			"start_at":          item.StartAt,
			"end_at":            item.EndAt,
			"parent_chunk_id":   item.ParentChunkID,
			"requested_chunk":   item.ID == chunk.ID,
		}
		if len(item.SourceLocator) > 0 && json.Valid(item.SourceLocator) {
			chunkData["source_locator"] = json.RawMessage(append([]byte(nil), item.SourceLocator...))
		}
		appendFAQChunkData(chunkData, item)
		normalizeFAQChunkDataMap(chunkData, item)
		formattedChunks = append(formattedChunks, chunkData)
	}

	data := map[string]interface{}{
		"display_type":      "knowledge_chunks_list",
		"knowledge_id":      chunk.KnowledgeID,
		"knowledge_title":   knowledgeTitle,
		"total_chunks":      int64(len(chunks)),
		"fetched_chunks":    len(chunks),
		"page":              1,
		"page_size":         1,
		"chunks":            formattedChunks,
		"faq_id":            chunk.ID,
		"single_chunk":      len(chunks) == 1,
		"neighbor_expanded": len(chunks) > 1,
	}
	if q := faqStandardQuestion(chunk); q != "" {
		data["faq_question"] = q
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data:    data,
	}, nil
}

func mergeChunkNeighborhood(target *types.Chunk, neighbours []*types.Chunk) []*types.Chunk {
	byID := map[string]*types.Chunk{target.ID: target}
	for _, item := range neighbours {
		if item == nil || item.ID == "" || item.KnowledgeID != target.KnowledgeID ||
			item.ChunkType != types.ChunkTypeText {
			continue
		}
		byID[item.ID] = item
	}
	out := make([]*types.Chunk, 0, len(byID))
	for _, item := range byID {
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ChunkIndex == out[j].ChunkIndex {
			return out[i].ID < out[j].ID
		}
		return out[i].ChunkIndex < out[j].ChunkIndex
	})
	return out
}

// lookupKnowledgeTitle looks up the title of a knowledge document
// Uses GetKnowledgeByIDOnly to support cross-tenant shared KB
func (t *ListKnowledgeChunksTool) lookupKnowledgeTitle(ctx context.Context, knowledgeID string) string {
	if t.knowledgeService == nil {
		return ""
	}
	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil || knowledge == nil {
		return ""
	}
	return strings.TrimSpace(knowledge.Title)
}

// buildOutput builds the output as XML for the list knowledge chunks tool
func (t *ListKnowledgeChunksTool) buildOutput(
	knowledgeID string,
	knowledgeTitle string,
	total int64,
	fetched int,
	offset int,
	chunks []*types.Chunk,
) string {
	var b strings.Builder

	titleAttr := ""
	if knowledgeTitle != "" {
		titleAttr = fmt.Sprintf(" title=\"%s\"", knowledgeTitle)
	}
	fmt.Fprintf(&b, "<knowledge_chunks knowledge_id=\"%s\"%s total=\"%d\" fetched=\"%d\" offset=\"%d\">\n",
		knowledgeID, titleAttr, total, fetched, offset)
	b.WriteString(
		"<coordinate_instruction>chunk_index is a logical chunk ordinal only and MUST NOT be reported as a page, " +
			"spreadsheet row, source line, JSON item, image frame/tile, or audio timestamp. " +
			"For citations use source_locator from the original unsplit file and record keys in content.</coordinate_instruction>\n",
	)

	if fetched == 0 {
		b.WriteString("</knowledge_chunks>")
		return b.String()
	}

	for _, c := range chunks {
		if c.ChunkType == types.ChunkTypeFAQ {
			writeFAQEntryXML(&b, c)
			writeChunkImagesXML(&b, c)
			continue
		}

		if q := faqStandardQuestion(c); q != "" {
			fmt.Fprintf(&b, "<chunk chunk_id=\"%s\" chunk_index=\"%d\" type=\"%s\" question=\"%s\">\n",
				c.ID, c.ChunkIndex, c.ChunkType, xmlEscape(q))
		} else {
			fmt.Fprintf(&b, "<chunk chunk_id=\"%s\" chunk_index=\"%d\" type=\"%s\">\n",
				c.ID, c.ChunkIndex, c.ChunkType)
		}
		if locator := sourcerefs.ModelSourceLocator(c.SourceLocator); locator != "" {
			fmt.Fprintf(&b, "<source_locator>%s</source_locator>\n", xmlEscape(locator))
		}
		fmt.Fprintf(&b, "<content>%s</content>\n", summarizeContent(c.Content))
		writeChunkImagesXML(&b, c)
		b.WriteString("</chunk>\n")
	}

	nextOffset := int64(offset + fetched)
	if nextOffset < total {
		fmt.Fprintf(
			&b,
			"<pagination next_offset=\"%d\" remaining=\"%d\" instruction=\"use next_offset exactly; never use chunk_index as offset\" />\n",
			nextOffset, total-nextOffset,
		)
	}

	b.WriteString("</knowledge_chunks>")
	return b.String()
}

func writeChunkImagesXML(b *strings.Builder, c *types.Chunk) {
	if c == nil || c.ImageInfo == "" {
		return
	}
	var imageInfos []types.ImageInfo
	if err := json.Unmarshal([]byte(c.ImageInfo), &imageInfos); err != nil || len(imageInfos) == 0 {
		return
	}
	for _, img := range imageInfos {
		if img.URL != "" {
			fmt.Fprintf(b, "<image url=\"%s\">\n", img.URL)
		} else {
			b.WriteString("<image>\n")
		}
		if img.Caption != "" {
			fmt.Fprintf(b, "<image_caption>%s</image_caption>\n", img.Caption)
		}
		if img.OCRText != "" {
			fmt.Fprintf(b, "<image_ocr>%s</image_ocr>\n", img.OCRText)
		}
		b.WriteString("</image>\n")
	}
}

// faqStandardQuestion returns the FAQ standard question for an FAQ-type chunk,
// or "" for non-FAQ chunks (or when metadata is missing/unparseable). All FAQ
// entries inside one knowledge share the same knowledge title, so surfacing the
// standard question gives each entry a distinct, human-readable identity in
// tool output that would otherwise look like duplicate same-titled chunks.
func faqStandardQuestion(c *types.Chunk) string {
	if c == nil || c.ChunkType != types.ChunkTypeFAQ {
		return ""
	}
	meta, err := c.FAQMetadata()
	if err != nil || meta == nil {
		return ""
	}
	return strings.TrimSpace(meta.StandardQuestion)
}

// summarizeContent summarizes the content of a chunk
func summarizeContent(content string) string {
	cleaned := strings.TrimSpace(content)
	if cleaned == "" {
		return "(empty)"
	}

	return strings.TrimSpace(string(cleaned))
}
