package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type wikiWritePageTool struct {
	BaseTool
	wikiPageService  interfaces.WikiPageService
	knowledgeService interfaces.KnowledgeService
	kbIDs            []string
}

// NewWikiWritePageTool creates a new wiki_write_page tool
func NewWikiWritePageTool(wikiPageService interfaces.WikiPageService, kbIDs []string, knowledgeService interfaces.KnowledgeService) types.Tool {
	return &wikiWritePageTool{
		BaseTool: NewBaseTool(
			ToolWikiWritePage,
			"创建新的 Wiki 页面，或完整覆盖已有页面。会自动处理出站链接。",
			json.RawMessage(`{
				"type": "object",
				"properties": {
					"slug": {
						"type": "string",
						"description": "Wiki 页面的 slug（例如 'entity/hunyuan-damoxing'）"
					},
					"title": {
						"type": "string",
						"description": "页面标题"
					},
					"summary": {
						"type": "string",
						"description": "用于索引列表的一句话摘要"
					},
					"content": {
						"type": "string",
						"description": "页面完整的 Markdown 内容。不要使用占位符。"
					},
					"page_type": {
						"type": "string",
						"description": "页面类型，例如 'summary'、'entity'、'concept'、'synthesis'、'comparison'"
					},
					"aliases": {
						"type": "array",
						"items": {"type": "string"},
						"description": "页面别名列表（可选）"
					},
					"source_refs": {
						"type": "array",
						"items": {"type": "string"},
						"description": "对此页面有贡献的来源知识 ID 列表（仅 UUID）。如果提供，将完全替换页面现有的 source_refs。"
					}
				},
				"required": ["slug", "title", "summary", "content", "page_type"]
			}`),
		),
		wikiPageService:  wikiPageService,
		knowledgeService: knowledgeService,
		kbIDs:            kbIDs,
	}
}

func (t *wikiWritePageTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var params struct {
		Slug       string   `json:"slug"`
		Title      string   `json:"title"`
		Summary    string   `json:"summary"`
		Content    string   `json:"content"`
		PageType   string   `json:"page_type"`
		Aliases    []string `json:"aliases"`
		SourceRefs []string `json:"source_refs"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return &types.ToolResult{Success: false, Error: "解析参数失败: " + err.Error()}, nil
	}

	if len(t.kbIDs) == 0 {
		return &types.ToolResult{Success: false, Error: "没有可编辑的知识库"}, nil
	}
	kbID := t.kbIDs[0]

	if params.Title == "" || params.PageType == "" || params.Content == "" || params.Summary == "" {
		return &types.ToolResult{Success: false, Error: "写入操作需要 title、summary、content 和 page_type"}, nil
	}

	// Try to get the existing page
	existingPage, err := t.wikiPageService.GetPageBySlug(ctx, kbID, params.Slug)
	if err != nil && !errors.Is(err, repository.ErrWikiPageNotFound) {
		return &types.ToolResult{Success: false, Error: "检查已有页面失败: " + err.Error()}, nil
	}

	resolvedRefs := resolveSourceRefs(ctx, t.knowledgeService, params.SourceRefs)

	var action string
	if existingPage != nil {
		// Update
		existingPage.Title = params.Title
		existingPage.Summary = params.Summary
		existingPage.Content = params.Content
		existingPage.PageType = params.PageType
		existingPage.Aliases = params.Aliases

		if len(resolvedRefs) > 0 {
			existingPage.SourceRefs = resolvedRefs
		}

		_, err = t.wikiPageService.UpdatePage(ctx, existingPage)
		if err != nil {
			return &types.ToolResult{Success: false, Error: "更新页面失败: " + err.Error()}, nil
		}
		action = "updated"
	} else {
		// Create
		newPage := &types.WikiPage{
			KnowledgeBaseID: kbID,
			Slug:            params.Slug,
			Title:           params.Title,
			Summary:         params.Summary,
			Content:         params.Content,
			PageType:        params.PageType,
			Aliases:         params.Aliases,
			SourceRefs:      resolvedRefs,
		}
		_, err = t.wikiPageService.CreatePage(ctx, newPage)
		if err != nil {
			return &types.ToolResult{Success: false, Error: "创建页面失败: " + err.Error()}, nil
		}
		action = "created"
	}

	// Inject cross-links so other pages know about this new/updated entity
	t.wikiPageService.InjectCrossLinks(ctx, kbID, []string{params.Slug})

	// Rebuild the index page to reflect the new/updated summary
	_ = t.wikiPageService.RebuildIndexPage(ctx, kbID)

	actionText := "创建"
	if action == "updated" {
		actionText = "更新"
	}
	output := fmt.Sprintf("已成功%s页面 [[%s]]。\n- 标题: %s\n- 类型: %s\n- 摘要: %s\n- 内容长度: %d 字符", actionText, params.Slug, params.Title, params.PageType, params.Summary, len(params.Content))
	if len(params.Aliases) > 0 {
		output += fmt.Sprintf("\n- 别名: %s", strings.Join(params.Aliases, ", "))
	}
	if len(resolvedRefs) > 0 {
		output += fmt.Sprintf("\n- 来源引用: %d 个文档", len(resolvedRefs))
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"display_type": "wiki_write_page",
			"action":       action,
			"slug":         params.Slug,
			"title":        params.Title,
			"page_type":    params.PageType,
			"summary":      params.Summary,
		},
	}, nil
}
