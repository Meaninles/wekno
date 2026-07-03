package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type wikiReplaceTextTool struct {
	BaseTool
	wikiPageService  interfaces.WikiPageService
	knowledgeService interfaces.KnowledgeService
	kbIDs            []string
}

// NewWikiReplaceTextTool creates a new wiki_replace_text tool
func NewWikiReplaceTextTool(wikiPageService interfaces.WikiPageService, kbIDs []string, knowledgeService interfaces.KnowledgeService) types.Tool {
	return &wikiReplaceTextTool{
		BaseTool: NewBaseTool(
			ToolWikiReplaceText,
			"替换 Wiki 页面中的特定精确文本。适合小范围修正。",
			json.RawMessage(`{
				"type": "object",
				"properties": {
					"slug": {
						"type": "string",
						"description": "Wiki 页面的 slug"
					},
					"old_text": {
						"type": "string",
						"description": "要查找并替换的精确文本"
					},
					"new_text": {
						"type": "string",
						"description": "要插入的新文本"
					},
					"source_refs": {
						"type": "array",
						"items": {"type": "string"},
						"description": "可选来源知识 ID 列表（仅 UUID），用于说明此变更依据。如果提供，将完全替换页面现有的 source_refs。"
					}
				},
				"required": ["slug", "old_text", "new_text"]
			}`),
		),
		wikiPageService:  wikiPageService,
		knowledgeService: knowledgeService,
		kbIDs:            kbIDs,
	}
}

func (t *wikiReplaceTextTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var params struct {
		Slug       string   `json:"slug"`
		OldText    string   `json:"old_text"`
		NewText    string   `json:"new_text"`
		SourceRefs []string `json:"source_refs"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return &types.ToolResult{Success: false, Error: "解析参数失败: " + err.Error()}, nil
	}

	if len(t.kbIDs) == 0 {
		return &types.ToolResult{Success: false, Error: "没有可编辑的知识库"}, nil
	}
	kbID := t.kbIDs[0]

	if params.OldText == "" {
		return &types.ToolResult{Success: false, Error: "需要 old_text"}, nil
	}

	// Get the existing page
	existingPage, err := t.wikiPageService.GetPageBySlug(ctx, kbID, params.Slug)
	if err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("获取页面 %s 失败: %v", params.Slug, err)}, nil
	}

	if !strings.Contains(existingPage.Content, params.OldText) {
		return &types.ToolResult{Success: false, Error: "当前页面内容中未找到 old_text。请确保按原文精确复制。"}, nil
	}

	existingPage.Content = strings.Replace(existingPage.Content, params.OldText, params.NewText, 1)

	if len(params.SourceRefs) > 0 {
		existingPage.SourceRefs = resolveSourceRefs(ctx, t.knowledgeService, params.SourceRefs)
	}

	_, err = t.wikiPageService.UpdatePage(ctx, existingPage)
	if err != nil {
		return &types.ToolResult{Success: false, Error: "更新页面失败: " + err.Error()}, nil
	}

	oldPreview := truncateRunes(params.OldText, 80)
	newPreview := truncateRunes(params.NewText, 80)

	output := fmt.Sprintf("已成功替换页面 [[%s]] 上的文本。\n- 旧文本: %s\n- 新文本: %s", params.Slug, oldPreview, newPreview)

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"display_type": "wiki_replace_text",
			"slug":         params.Slug,
			"title":        existingPage.Title,
			"old_text":     oldPreview,
			"new_text":     newPreview,
		},
	}, nil
}
