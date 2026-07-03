package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type wikiReadIssueTool struct {
	BaseTool
	wikiService interfaces.WikiPageService
	kbIDs       []string
}

func NewWikiReadIssueTool(wikiService interfaces.WikiPageService, kbIDs []string) types.Tool {
	return &wikiReadIssueTool{
		BaseTool: NewBaseTool(
			ToolWikiReadIssue,
			"读取特定 wiki 页面问题的详情，或列出某个 wiki 页面的待处理问题。",
			json.RawMessage(`{
  "type": "object",
  "properties": {
    "issue_id": {
      "type": "string",
      "description": "可选：要读取的特定问题 ID。"
    },
    "slug": {
      "type": "string",
      "description": "可选：要列出待处理问题的 wiki 页面 slug。"
    }
  },
  "description": "提供 issue_id 或 slug 以读取问题。"
}`),
		),
		wikiService: wikiService,
		kbIDs:       kbIDs,
	}
}

func (t *wikiReadIssueTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var params struct {
		IssueID string `json:"issue_id"`
		Slug    string `json:"slug"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &types.ToolResult{Success: false, Error: "参数无效: " + err.Error()}, nil
	}

	issueID := strings.TrimSpace(params.IssueID)
	slug := strings.TrimSpace(params.Slug)

	if issueID == "" && slug == "" {
		return &types.ToolResult{Success: false, Error: "需要 issue_id 或 slug"}, nil
	}

	if len(t.kbIDs) == 0 {
		return &types.ToolResult{Success: false, Error: "没有可用知识库"}, nil
	}

	kbID := t.kbIDs[0]

	if issueID != "" {
		// Just reuse ListIssues since there's no GetIssueByID yet
		issues, err := t.wikiService.ListIssues(ctx, kbID, "", "")
		if err != nil {
			return &types.ToolResult{Success: false, Error: "列出问题失败: " + err.Error()}, nil
		}

		for _, issue := range issues {
			if issue.ID == issueID {
				out, _ := json.MarshalIndent(issue, "", "  ")
				return &types.ToolResult{Success: true, Output: string(out)}, nil
			}
		}
		return &types.ToolResult{Success: false, Error: "未找到问题"}, nil
	}

	issues, err := t.wikiService.ListIssues(ctx, kbID, slug, "pending")
	if err != nil {
		return &types.ToolResult{Success: false, Error: "列出问题失败: " + err.Error()}, nil
	}

	if len(issues) == 0 {
		return &types.ToolResult{Success: true, Output: "未找到该 slug 的待处理问题: " + slug}, nil
	}

	out, _ := json.MarshalIndent(issues, "", "  ")
	return &types.ToolResult{Success: true, Output: string(out)}, nil
}
