package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikicontract"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type wikiDeletePageTool struct {
	BaseTool
	wikiPageService interfaces.WikiPageService
	kbIDs           []string
}

// NewWikiDeletePageTool creates a new wiki_delete_page tool
func NewWikiDeletePageTool(wikiPageService interfaces.WikiPageService, kbIDs []string) types.Tool {
	return &wikiDeletePageTool{
		BaseTool: NewBaseTool(
			ToolWikiDeletePage,
			"Delete a Wiki page. Automatically cleans up incoming links on other pages to prevent dead links.",
			wikicontract.TargetSchema(json.RawMessage(`{
				"type": "object",
				"properties": {
					"slug": {
						"type": "string",
						"description": "The slug of the Wiki page to delete"
					}
				},
				"required": ["slug"]
			}`), true),
		),
		wikiPageService: wikiPageService,
		kbIDs:           kbIDs,
	}
}

func (t *wikiDeletePageTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var params struct {
		wikicontract.Target
		Slug string `json:"slug"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return &types.ToolResult{Success: false, Error: "Failed to parse arguments: " + err.Error()}, nil
	}

	if len(t.kbIDs) == 0 {
		return &types.ToolResult{Success: false, Error: "No knowledge bases available for editing"}, nil
	}
	kbID, targetErr := params.Target.KnowledgeBase(t.kbIDs)
	if targetErr != nil {
		return &types.ToolResult{Success: false, Error: targetErr.Error()}, nil
	}

	if params.Slug == "" {
		return &types.ToolResult{Success: false, Error: "slug is required"}, nil
	}

	// Fetch page to get its incoming links before deleting
	existingPage, err := t.wikiPageService.GetPageBySlug(ctx, kbID, params.Slug)
	if err != nil {
		return &types.ToolResult{Success: false, Error: "Failed to fetch page to delete: " + err.Error()}, nil
	}
	if existingPage != nil {
		if err := params.Target.ValidatePage(existingPage, true); err != nil {
			return &types.ToolResult{Success: false, Error: err.Error()}, nil
		}
	}
	if err := t.wikiPageService.DeletePageVersion(ctx, existingPage); err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, nil
	}
	outputMsg := fmt.Sprintf("Deleted page %s in knowledge base %s; links and issues updated atomically.", params.Slug, kbID)

	return &types.ToolResult{
		Success: true,
		Output:  outputMsg,
		Data: map[string]interface{}{
			"display_type": "wiki_delete_page",
			"slug":         params.Slug,
			"title":        existingPage.Title,
		},
	}, nil
}
