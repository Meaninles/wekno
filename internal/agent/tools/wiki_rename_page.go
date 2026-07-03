package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type wikiRenamePageTool struct {
	BaseTool
	wikiPageService interfaces.WikiPageService
	kbIDs           []string
}

// NewWikiRenamePageTool creates a new wiki_rename_page tool
func NewWikiRenamePageTool(wikiPageService interfaces.WikiPageService, kbIDs []string) types.Tool {
	return &wikiRenamePageTool{
		BaseTool: NewBaseTool(
			ToolWikiRenamePage,
			"重命名 Wiki 页面的 slug。会自动把新 slug 级联更新到所有链接旧 slug 的页面。",
			json.RawMessage(`{
				"type": "object",
				"properties": {
					"slug": {
						"type": "string",
						"description": "Wiki 页面的当前 slug"
					},
					"new_slug": {
						"type": "string",
						"description": "页面的新 slug"
					}
				},
				"required": ["slug", "new_slug"]
			}`),
		),
		wikiPageService: wikiPageService,
		kbIDs:           kbIDs,
	}
}

func (t *wikiRenamePageTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var params struct {
		Slug    string `json:"slug"`
		NewSlug string `json:"new_slug"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return &types.ToolResult{Success: false, Error: "解析参数失败: " + err.Error()}, nil
	}

	if len(t.kbIDs) == 0 {
		return &types.ToolResult{Success: false, Error: "没有可编辑的知识库"}, nil
	}
	kbID := t.kbIDs[0]

	if params.NewSlug == "" {
		return &types.ToolResult{Success: false, Error: "需要 new_slug"}, nil
	}
	if params.NewSlug == params.Slug {
		return &types.ToolResult{Success: false, Error: "new_slug 必须不同于旧 slug"}, nil
	}

	// Get existing page
	existingPage, err := t.wikiPageService.GetPageBySlug(ctx, kbID, params.Slug)
	if err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("未找到页面 %s，无法重命名不存在的页面。", params.Slug)}, nil
	}

	inLinks := make([]string, len(existingPage.InLinks))
	copy(inLinks, existingPage.InLinks)

	// Create new page with new slug but same content
	newPage := &types.WikiPage{
		KnowledgeBaseID: kbID,
		Slug:            params.NewSlug,
		Title:           existingPage.Title,
		Summary:         existingPage.Summary,
		Content:         existingPage.Content,
		PageType:        existingPage.PageType,
		Aliases:         existingPage.Aliases,
	}
	_, err = t.wikiPageService.CreatePage(ctx, newPage)
	if err != nil {
		return &types.ToolResult{Success: false, Error: "创建重命名页面失败: " + err.Error()}, nil
	}

	// Update incoming links in other pages
	updatedCount := 0
	var updatedSlugs []string
	for _, sourceSlug := range inLinks {
		sourcePage, err := t.wikiPageService.GetPageBySlug(ctx, kbID, sourceSlug)
		if err == nil {
			changed := false

			// Replace [[old-slug]] with [[new-slug]]
			link1 := "[[" + params.Slug + "]]"
			newLink1 := "[[" + params.NewSlug + "]]"
			if strings.Contains(sourcePage.Content, link1) {
				sourcePage.Content = strings.ReplaceAll(sourcePage.Content, link1, newLink1)
				changed = true
			}

			// Replace [[old-slug|text]] with [[new-slug|text]]
			link2 := "[[" + params.Slug + "|"
			newLink2 := "[[" + params.NewSlug + "|"
			if strings.Contains(sourcePage.Content, link2) {
				sourcePage.Content = strings.ReplaceAll(sourcePage.Content, link2, newLink2)
				changed = true
			}

			if changed {
				_, updateErr := t.wikiPageService.UpdatePage(ctx, sourcePage)
				if updateErr == nil {
					updatedCount++
					updatedSlugs = append(updatedSlugs, sourceSlug)
				}
			}
		}
	}

	// Delete old page
	err = t.wikiPageService.DeletePage(ctx, kbID, params.Slug)
	if err != nil {
		return &types.ToolResult{Success: false, Error: "已成功创建新页面并更新链接，但删除旧页面失败: " + err.Error()}, nil
	}

	// Inject cross-links so other pages know about this new slug
	t.wikiPageService.InjectCrossLinks(ctx, kbID, []string{params.NewSlug})

	// Rebuild the index page to reflect the new/updated summary
	_ = t.wikiPageService.RebuildIndexPage(ctx, kbID)

	outputMsg := fmt.Sprintf("已成功将页面 [[%s]] 重命名为 [[%s]]，并更新 %d 个入站链接。", params.Slug, params.NewSlug, updatedCount)
	if updatedCount > 0 {
		outputMsg += fmt.Sprintf("\n- 受影响页面: %s", strings.Join(updatedSlugs, ", "))
	}

	return &types.ToolResult{
		Success: true,
		Output:  outputMsg,
		Data: map[string]interface{}{
			"display_type":   "wiki_rename_page",
			"old_slug":       params.Slug,
			"new_slug":       params.NewSlug,
			"title":          existingPage.Title,
			"updated_count":  updatedCount,
			"affected_pages": updatedSlugs,
		},
	}, nil
}
