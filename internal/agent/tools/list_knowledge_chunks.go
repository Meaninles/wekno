package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var listKnowledgeChunksTool = BaseTool{
	name: ToolListKnowledgeChunks,
	description: `检索文档或单条 FAQ 条目的完整分块内容。

## 在 grep_chunks 或 knowledge_search 之后使用：
- **FAQ 命中**（type 为 faq）：list_knowledge_chunks(faq_id="<搜索返回的 chunk_id>")，读取该 FAQ 条目及其元数据中的答案。
- **文档命中**：list_knowledge_chunks(knowledge_id="<document id>")，分页读取全部分块。

## 参数（只提供一个 ID 目标）：
- faq_id（可选）：来自 grep_chunks / knowledge_search 的 FAQ 条目 ID。
- chunk_id（可选）：单个非 FAQ 分块 ID（FAQ 不要用它，请用 faq_id）。
- knowledge_id（可选）：文档/知识 ID，用于分页读取全部分块。
- limit / offset：仅用于 knowledge_id 分页（默认 limit 20，最大 100）。

## 输出：
完整分块内容。FAQ 条目会包含 <faq>，其中 <answer> 来自元数据。`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "faq_id": {
      "type": "string",
      "description": "FAQ 条目 ID（与 chunk_id 相同）。FAQ 命中时使用它代替 knowledge_id。"
    },
    "chunk_id": {
      "type": "string",
      "description": "单个分块 ID（faq_id 的别名）"
    },
    "knowledge_id": {
      "type": "string",
      "description": "要列出全部分块的文档/知识 ID"
    },
    "limit": {
      "type": "integer",
      "description": "使用 knowledge_id 时每页分块数（默认 20，最大 100）",
      "default": 20,
      "minimum": 1,
      "maximum": 100
    },
    "offset": {
      "type": "integer",
      "description": "使用 knowledge_id 时的起始位置（默认 0）",
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
	// Parse args from json.RawMessage
	var input ListKnowledgeChunksInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("解析参数失败: %v", err),
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
			Error:   "需要提供 faq_id、chunk_id 或 knowledge_id 其中之一",
		}, fmt.Errorf("missing id parameter")
	}

	// Get knowledge info without tenant filter to support shared KB
	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("未找到知识: %v", err),
		}, err
	}

	// Verify the knowledge's KB is in searchTargets (permission check)
	if !t.searchTargets.ContainsKB(knowledge.KnowledgeBaseID) {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("无法访问知识库 %s", knowledge.KnowledgeBaseID),
		}, fmt.Errorf("knowledge base not in search targets")
	}
	allowed, err := searchTargetsAllowKnowledgeID(ctx, t.searchTargets, knowledge.ID, knowledge.KnowledgeBaseID, t.knowledgeService)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("校验知识范围失败: %v", err),
		}, err
	}
	if !allowed {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("知识 %s 不在当前 @mention 范围内", knowledge.ID),
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
		Page:     offset/chunkLimit + 1,
		PageSize: chunkLimit,
	}

	chunks, total, err := t.chunkService.GetRepository().ListPagedChunksByKnowledgeID(ctx,
		effectiveTenantID, knowledgeID, pagination, []types.ChunkType{types.ChunkTypeText, types.ChunkTypeFAQ}, "", "", "", "", "")
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("列出分块失败: %v", err),
		}, err
	}
	if chunks == nil {
		return &types.ToolResult{
			Success: false,
			Error:   "分块查询未返回数据",
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
				"offset %d 超出范围：文档只有 %d 个分块（有效 offset 范围：0..%d）。请用 offset=%d（或任何小于 %d 的值）重试。",
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

	output := t.buildOutput(knowledgeID, knowledgeTitle, totalChunks, fetched, chunks)

	formattedChunks := make([]map[string]interface{}, 0, len(chunks))
	for idx, c := range chunks {
		chunkData := map[string]interface{}{
			"seq":             idx + 1,
			"chunk_id":        c.ID,
			"chunk_index":     c.ChunkIndex,
			"content":         c.Content,
			"chunk_type":      c.ChunkType,
			"knowledge_id":    c.KnowledgeID,
			"knowledge_base":  c.KnowledgeBaseID,
			"start_at":        c.StartAt,
			"end_at":          c.EndAt,
			"parent_chunk_id": c.ParentChunkID,
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
	if err != nil || chunk == nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("未找到分块: %v", err),
		}, err
	}
	if !t.searchTargets.ContainsKB(chunk.KnowledgeBaseID) {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("无法访问知识库 %s", chunk.KnowledgeBaseID),
		}, fmt.Errorf("knowledge base not in search targets")
	}
	allowed, scopeErr := searchTargetsAllowKnowledgeID(ctx, t.searchTargets, chunk.KnowledgeID, chunk.KnowledgeBaseID, t.knowledgeService)
	if scopeErr != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("校验分块范围失败: %v", scopeErr),
		}, scopeErr
	}
	if !allowed {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("分块 %s 不在当前 @mention 范围内", chunk.ID),
		}, fmt.Errorf("chunk not in search target scope")
	}

	chunks := []*types.Chunk{chunk}
	if chunk.ImageInfo == "" {
		effectiveTenantID := t.searchTargets.GetTenantIDForKB(chunk.KnowledgeBaseID)
		if effectiveTenantID > 0 {
			infoMap := searchutil.CollectImageInfoByChunkIDs(ctx, t.chunkService.GetRepository(), effectiveTenantID, []string{chunk.ID})
			if merged, ok := infoMap[chunk.ID]; ok {
				chunk.ImageInfo = merged
			}
		}
	}

	knowledgeTitle := t.lookupKnowledgeTitle(ctx, chunk.KnowledgeID)
	output := t.buildOutput(chunk.KnowledgeID, knowledgeTitle, 1, 1, chunks)

	formattedChunks := []map[string]interface{}{
		{
			"seq":            1,
			"chunk_id":       chunk.ID,
			"chunk_index":    chunk.ChunkIndex,
			"content":        chunk.Content,
			"chunk_type":     chunk.ChunkType,
			"knowledge_id":   chunk.KnowledgeID,
			"knowledge_base": chunk.KnowledgeBaseID,
		},
	}
	appendFAQChunkData(formattedChunks[0], chunk)
	normalizeFAQChunkDataMap(formattedChunks[0], chunk)

	data := map[string]interface{}{
		"display_type":    "knowledge_chunks_list",
		"knowledge_id":    chunk.KnowledgeID,
		"knowledge_title": knowledgeTitle,
		"total_chunks":    int64(1),
		"fetched_chunks":  1,
		"page":            1,
		"page_size":       1,
		"chunks":          formattedChunks,
		"faq_id":          chunk.ID,
		"single_chunk":    true,
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
	chunks []*types.Chunk,
) string {
	var b strings.Builder

	titleAttr := ""
	if knowledgeTitle != "" {
		titleAttr = fmt.Sprintf(" title=\"%s\"", knowledgeTitle)
	}
	fmt.Fprintf(&b, "<knowledge_chunks knowledge_id=\"%s\"%s total=\"%d\" fetched=\"%d\">\n",
		knowledgeID, titleAttr, total, fetched)

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
		fmt.Fprintf(&b, "<content>%s</content>\n", summarizeContent(c.Content))
		writeChunkImagesXML(&b, c)
		b.WriteString("</chunk>\n")
	}

	if int64(fetched) < total {
		fmt.Fprintf(&b, "<pagination remaining=\"%d\" />\n", int64(total)-int64(fetched))
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
		return "（空）"
	}

	return strings.TrimSpace(string(cleaned))
}
