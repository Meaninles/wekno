package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type wikiUpdateIssueTool struct {
	BaseTool
	wikiService interfaces.WikiPageService
	kbIDs       []string
}

func NewWikiUpdateIssueTool(wikiService interfaces.WikiPageService, kbIDs []string) types.Tool {
	return &wikiUpdateIssueTool{
		BaseTool: NewBaseTool(
			ToolWikiUpdateIssue,
			"更新特定 wiki 页面问题的状态（例如设为 'resolved' 或 'ignored'）。",
			json.RawMessage(`{
  "type": "object",
  "properties": {
    "issue_id": {
      "type": "string",
      "description": "要更新的问题 ID。"
    },
    "status": {
      "type": "string",
      "enum": ["resolved", "ignored", "pending"],
      "description": "问题的新状态。"
    }
  },
  "required": ["issue_id", "status"]
}`),
		),
		wikiService: wikiService,
		kbIDs:       kbIDs,
	}
}

func (t *wikiUpdateIssueTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var params struct {
		IssueID string `json:"issue_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &types.ToolResult{Success: false, Error: "参数无效: " + err.Error()}, nil
	}

	if params.IssueID == "" {
		return &types.ToolResult{Success: false, Error: "需要 issue_id"}, nil
	}
	if params.Status == "" {
		return &types.ToolResult{Success: false, Error: "需要 status"}, nil
	}

	if len(t.kbIDs) == 0 {
		return &types.ToolResult{Success: false, Error: "没有可用知识库"}, nil
	}

	// Update issue status
	err := t.wikiService.UpdateIssueStatus(ctx, params.IssueID, params.Status)
	if err != nil {
		return &types.ToolResult{Success: false, Error: "更新问题状态失败: " + err.Error()}, nil
	}

	return &types.ToolResult{
		Success: true,
		Output:  fmt.Sprintf("已成功将问题 %s 更新为状态 '%s'", params.IssueID, params.Status),
	}, nil
}
