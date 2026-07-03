package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var getDocumentInfoTool = BaseTool{
	name: ToolGetDocumentInfo,
	description: `检索文档的详细元数据信息。

## 何时使用

在以下场景使用此工具：
- 需要了解文档基本信息（标题、类型、大小等）
- 检查文档是否存在且可用
- 批量查询多个文档的元数据
- 了解文档处理状态

不要在以下场景使用：
- 需要文档内容（使用 knowledge_search）
- 需要特定文本分块（搜索结果已经包含完整内容）


## 返回信息

- 基本信息：标题、描述、来源类型
- 文件信息：文件名、类型、大小
- 处理状态：是否已处理、分块数量
- 元数据：自定义标签和属性


## 注意事项

- 并发查询多个文档可获得更好性能
- 返回完整文档元数据，而不只是标题
- 可检查文档处理状态（parse_status）

## IDs
- knowledge_ids：普通文档 knowledges
- faq_ids：单条 FAQ 条目。返回标准问题和答案，而不是容器标题。`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "knowledge_ids": {
      "type": "array",
      "items": { "type": "string" },
      "description": "普通文档的 Document/knowledge ID"
    },
    "faq_ids": {
      "type": "array",
      "items": { "type": "string" },
      "description": "FAQ 条目 ID（等于 grep_chunks 返回的 chunk_id）。查询单条 FAQ 问答时用它代替 knowledge_ids。"
    }
  }
}`),
}

// GetDocumentInfoInput defines the input parameters for get document info tool.
// Either knowledge_ids or faq_ids may be provided (at least one); both are optional in the schema.
type GetDocumentInfoInput struct {
	KnowledgeIDs []string `json:"knowledge_ids,omitempty"`
	FAQIDs       []string `json:"faq_ids,omitempty"`
}

// GetDocumentInfoTool retrieves detailed information about a document/knowledge
type GetDocumentInfoTool struct {
	BaseTool
	knowledgeService interfaces.KnowledgeService
	chunkService     interfaces.ChunkService
	searchTargets    types.SearchTargets // Pre-computed unified search targets with KB-tenant mapping
}

// NewGetDocumentInfoTool creates a new get document info tool
func NewGetDocumentInfoTool(
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	searchTargets types.SearchTargets,
) *GetDocumentInfoTool {
	return &GetDocumentInfoTool{
		BaseTool:         getDocumentInfoTool,
		knowledgeService: knowledgeService,
		chunkService:     chunkService,
		searchTargets:    searchTargets,
	}
}

// Execute retrieves document information with concurrent processing
func (t *GetDocumentInfoTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	// Parse args from json.RawMessage
	var input GetDocumentInfoInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("解析参数失败: %v", err),
		}, err
	}

	knowledgeIDs := input.KnowledgeIDs
	faqIDs := input.FAQIDs
	if len(knowledgeIDs) == 0 && len(faqIDs) == 0 {
		return &types.ToolResult{
			Success: false,
			Error:   "需要提供 knowledge_ids 或 faq_ids（非空数组）",
		}, fmt.Errorf("missing ids")
	}

	type docInfo struct {
		knowledge  *types.Knowledge
		chunk      *types.Chunk
		faqMeta    *types.FAQChunkMetadata
		chunkCount int
		err        error
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make(map[string]*docInfo)

	for _, faqID := range faqIDs {
		faqID = strings.TrimSpace(faqID)
		if faqID == "" {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			chunk, err := t.chunkService.GetChunkByIDOnly(ctx, id)
			if err != nil || chunk == nil {
				mu.Lock()
				results["faq:"+id] = &docInfo{err: fmt.Errorf("未找到 FAQ 条目: %v", err)}
				mu.Unlock()
				return
			}
			if !t.searchTargets.ContainsKB(chunk.KnowledgeBaseID) {
				mu.Lock()
				results["faq:"+id] = &docInfo{err: fmt.Errorf("无法访问知识库 %s", chunk.KnowledgeBaseID)}
				mu.Unlock()
				return
			}
			allowed, scopeErr := searchTargetsAllowKnowledgeID(ctx, t.searchTargets, chunk.KnowledgeID, chunk.KnowledgeBaseID, t.knowledgeService)
			if scopeErr != nil || !allowed {
				mu.Lock()
				if scopeErr != nil {
					results["faq:"+id] = &docInfo{err: fmt.Errorf("校验 FAQ 范围失败: %v", scopeErr)}
				} else {
					results["faq:"+id] = &docInfo{err: fmt.Errorf("FAQ 条目 %s 不在当前 @mention 范围内", id)}
				}
				mu.Unlock()
				return
			}
			var meta *types.FAQChunkMetadata
			if chunk.ChunkType == types.ChunkTypeFAQ {
				meta, _ = chunk.FAQMetadata()
			}
			mu.Lock()
			results["faq:"+id] = &docInfo{chunk: chunk, faqMeta: meta, chunkCount: 1}
			mu.Unlock()
		}(faqID)
	}

	for _, knowledgeID := range knowledgeIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()

			// Get knowledge metadata without tenant filter to support shared KB
			knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, id)
			if err != nil {
				mu.Lock()
				results[id] = &docInfo{
					err: fmt.Errorf("获取文档信息失败: %v", err),
				}
				mu.Unlock()
				return
			}

			// Verify the knowledge's KB is in searchTargets (permission check)
			if !t.searchTargets.ContainsKB(knowledge.KnowledgeBaseID) {
				mu.Lock()
				results[id] = &docInfo{
					err: fmt.Errorf("无法访问知识库 %s", knowledge.KnowledgeBaseID),
				}
				mu.Unlock()
				return
			}
			allowed, scopeErr := searchTargetsAllowKnowledgeID(ctx, t.searchTargets, knowledge.ID, knowledge.KnowledgeBaseID, t.knowledgeService)
			if scopeErr != nil || !allowed {
				mu.Lock()
				if scopeErr != nil {
					results[id] = &docInfo{err: fmt.Errorf("校验文档范围失败: %v", scopeErr)}
				} else {
					results[id] = &docInfo{err: fmt.Errorf("文档 %s 不在当前 @mention 范围内", knowledge.ID)}
				}
				mu.Unlock()
				return
			}

			// Use knowledge's actual tenant_id for chunk query (supports cross-tenant shared KB).
			// Keep chunk-type filter aligned with list_knowledge_chunks so the
			// "chunk_count" reported here matches what that tool can page over.
			_, total, err := t.chunkService.GetRepository().
				ListPagedChunksByKnowledgeID(ctx, knowledge.TenantID, id, &types.Pagination{
					Page:     1,
					PageSize: 1,
				}, []types.ChunkType{types.ChunkTypeText, types.ChunkTypeFAQ}, "", "", "", "", "")
			if err != nil {
				mu.Lock()
				results[id] = &docInfo{
					err: fmt.Errorf("获取文档信息失败: %v", err),
				}
				mu.Unlock()
				return
			}
			chunkCount := int(total)

			mu.Lock()
			results[id] = &docInfo{
				knowledge:  knowledge,
				chunkCount: chunkCount,
			}
			mu.Unlock()
		}(knowledgeID)
	}

	wg.Wait()

	requested := len(knowledgeIDs) + len(faqIDs)
	successDocs := make([]*docInfo, 0)
	var errors []string

	for _, knowledgeID := range knowledgeIDs {
		result := results[knowledgeID]
		if result == nil {
			errors = append(errors, fmt.Sprintf("%s: 未找到", knowledgeID))
			continue
		}
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", knowledgeID, result.err))
		} else if result.knowledge != nil {
			successDocs = append(successDocs, result)
		}
	}
	for _, faqID := range faqIDs {
		faqID = strings.TrimSpace(faqID)
		if faqID == "" {
			continue
		}
		result := results["faq:"+faqID]
		if result == nil {
			errors = append(errors, fmt.Sprintf("faq:%s: 未找到", faqID))
			continue
		}
		if result.err != nil {
			errors = append(errors, fmt.Sprintf("faq:%s: %v", faqID, result.err))
		} else if result.chunk != nil {
			successDocs = append(successDocs, result)
		}
	}

	if len(successDocs) == 0 {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("未能检索到任何文档信息。错误: %v", errors),
		}, fmt.Errorf("all document retrievals failed")
	}

	output := "=== 文档信息 ===\n\n"
	output += fmt.Sprintf("成功检索 %d / %d 个条目\n\n", len(successDocs), requested)

	if len(errors) > 0 {
		output += "=== 部分失败 ===\n"
		for _, errMsg := range errors {
			output += fmt.Sprintf("  - %s\n", errMsg)
		}
		output += "\n"
	}

	formattedDocs := make([]map[string]interface{}, 0, len(successDocs))
	for i, doc := range successDocs {
		output += fmt.Sprintf("[条目 #%d]\n", i+1)

		if doc.chunk != nil {
			formatted := formatFAQEntryInfo(&output, doc.chunk, doc.faqMeta)
			formattedDocs = append(formattedDocs, formatted)
			continue
		}

		k := doc.knowledge
		output += fmt.Sprintf("  ID:           %s\n", k.ID)
		output += fmt.Sprintf("  标题:         %s\n", k.Title)

		if k.Description != "" {
			output += fmt.Sprintf("  描述:         %s\n", k.Description)
		}

		output += fmt.Sprintf("  来源:         %s\n", formatSource(k.Type, k.Source))

		if k.FileName != "" {
			output += fmt.Sprintf("  文件名:       %s\n", k.FileName)
			output += fmt.Sprintf("  文件类型:     %s\n", k.FileType)
			output += fmt.Sprintf("  文件大小:     %s\n", formatFileSize(k.FileSize))
		}

		output += fmt.Sprintf("  解析状态:     %s\n", formatParseStatus(k.ParseStatus))
		output += fmt.Sprintf("  分块数量:     %d\n", doc.chunkCount)

		if k.Metadata != nil {
			if metadata, err := k.Metadata.Map(); err == nil && len(metadata) > 0 {
				output += "  元数据:\n"
				for key, value := range metadata {
					output += fmt.Sprintf("    - %s: %v\n", key, value)
				}
			}
		}

		output += "\n"

		formattedDocs = append(formattedDocs, map[string]interface{}{
			"knowledge_id": k.ID,
			"title":        k.Title,
			"description":  k.Description,
			"type":         k.Type,
			"source":       k.Source,
			"file_name":    k.FileName,
			"file_type":    k.FileType,
			"file_size":    k.FileSize,
			"parse_status": k.ParseStatus,
			"chunk_count":  doc.chunkCount,
			"metadata":     k.GetMetadata(),
			"is_faq":       false,
		})
	}

	var firstTitle string
	if len(formattedDocs) > 0 {
		if t, ok := formattedDocs[0]["title"].(string); ok {
			firstTitle = t
		}
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"documents":    formattedDocs,
			"total_docs":   len(successDocs),
			"requested":    requested,
			"errors":       errors,
			"display_type": "document_info",
			"title":        firstTitle,
		},
	}, nil
}

func formatFAQEntryInfo(output *string, chunk *types.Chunk, meta *types.FAQChunkMetadata) map[string]interface{} {
	title := faqStandardQuestion(chunk)
	if title == "" && meta != nil {
		title = strings.TrimSpace(meta.StandardQuestion)
	}
	if title == "" {
		title = "FAQ 条目"
	}

	*output += fmt.Sprintf("  FAQ ID:       %s\n", chunk.ID)
	*output += fmt.Sprintf("  问题:         %s\n", title)
	if chunk.KnowledgeID != "" {
		*output += fmt.Sprintf("  容器 ID:      %s\n", chunk.KnowledgeID)
	}
	if meta != nil && len(meta.Answers) > 0 {
		*output += "  答案:\n"
		for _, ans := range meta.Answers {
			*output += fmt.Sprintf("    - %s\n", ans)
		}
	}
	if meta != nil && len(meta.SimilarQuestions) > 0 {
		display, omitted := truncateSimilarQuestionsForDisplay(meta.SimilarQuestions)
		*output += "  相似问题:\n"
		for _, sq := range display {
			*output += fmt.Sprintf("    - %s\n", sq)
		}
		if omitted > 0 {
			*output += fmt.Sprintf("    ... 另有 %d 条已省略\n", omitted)
		}
	}
	*output += "\n"

	entry := map[string]interface{}{
		"faq_id":       chunk.ID,
		"knowledge_id": chunk.KnowledgeID,
		"title":        title,
		"faq_question": title,
		"type":         "faq",
		"is_faq":       true,
		"chunk_count":  1,
	}
	if meta != nil {
		if len(meta.Answers) > 0 {
			entry["faq_answers"] = meta.Answers
		}
		appendSimilarQuestionsToChunkData(entry, meta.SimilarQuestions)
	}
	return entry
}

func formatSource(knowledgeType, source string) string {
	switch knowledgeType {
	case "file":
		return "文件上传"
	case "url":
		return fmt.Sprintf("URL: %s", source)
	case "passage":
		return "文本输入"
	default:
		return knowledgeType
	}
}

func formatFileSize(size int64) string {
	if size == 0 {
		return "Unknown"
	}
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

func formatParseStatus(status string) string {
	switch status {
	case "pending":
		return "待处理"
	case "processing":
		return "处理中"
	case "completed", "success":
		return "已完成"
	case "failed":
		return "失败"
	default:
		return status
	}
}
