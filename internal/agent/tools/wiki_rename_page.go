package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikicontract"
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
			"Rename a Wiki page's slug. Automatically cascades the new slug to all pages that linked to the old one.",
			wikicontract.TargetSchema(json.RawMessage(`{
				"type": "object",
				"properties": {
					"slug": {
						"type": "string",
						"description": "The current slug of the Wiki page"
					},
					"new_slug": {
						"type": "string",
						"description": "The new slug for the page"
					}
				},
				"required": ["slug", "new_slug"]
			}`), true),
		),
		wikiPageService: wikiPageService,
		kbIDs:           kbIDs,
	}
}

func (t *wikiRenamePageTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var params struct {
		wikicontract.Target
		Slug    string `json:"slug"`
		NewSlug string `json:"new_slug"`
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

	if params.NewSlug == "" {
		return &types.ToolResult{Success: false, Error: "new_slug is required"}, nil
	}
	if params.NewSlug == params.Slug {
		return &types.ToolResult{Success: false, Error: "new_slug must be different from old slug"}, nil
	}

	// Get existing page
	existingPage, err := t.wikiPageService.GetPageBySlug(ctx, kbID, params.Slug)
	if err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("Page %s not found. Cannot rename a non-existent page.", params.Slug)}, nil
	}
	if existingPage != nil {
		if err := params.Target.ValidatePage(existingPage, true); err != nil {
			return &types.ToolResult{Success: false, Error: err.Error()}, nil
		}
	}

	updated, err := t.wikiPageService.RenamePage(ctx, existingPage, params.NewSlug)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, nil
	}
	outputMsg := fmt.Sprintf("Renamed page %s to %s; page_id=%s version=%d. Links and issues updated atomically.", params.Slug, params.NewSlug, updated.ID, updated.Version)

	return &types.ToolResult{
		Success: true,
		Output:  outputMsg,
		Data: map[string]interface{}{
			"display_type":      "wiki_rename_page",
			"old_slug":          params.Slug,
			"new_slug":          params.NewSlug,
			"title":             existingPage.Title,
			"page_id":           updated.ID,
			"version":           updated.Version,
			"knowledge_base_id": kbID,
		},
	}, nil
}
