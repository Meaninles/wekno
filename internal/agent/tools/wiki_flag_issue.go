package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type wikiFlagIssueTool struct {
	BaseTool
	wikiService interfaces.WikiPageService
	kbIDs       []string
}

func NewWikiFlagIssueTool(wikiService interfaces.WikiPageService, kbIDs []string) types.Tool {
	return &wikiFlagIssueTool{
		BaseTool: NewBaseTool(
			ToolWikiFlagIssue,
			`标记包含错误、混合实体或过期信息的 wiki 页面。
当你或用户发现某个 wiki 页面事实错误或错误合并时使用此工具（例如一个页面包含两个不同产品的信息）。
此操作会记录一个问题，供人工审核或自动维护。`,
			json.RawMessage(`{
  "type": "object",
  "properties": {
    "slug": {
      "type": "string",
      "description": "存在问题的 wiki 页面 slug（例如 'entity/hunyuan-damoxing'）"
    },
    "issue_type": {
      "type": "string",
      "enum": ["mixed_entities", "contradictory_facts", "out_of_date", "other"],
      "description": "问题类别"
    },
    "description": {
      "type": "string",
      "description": "详细说明页面哪里有问题以及应如何修复。"
    },
    "suspected_knowledge_ids": {
      "type": "array",
      "items": { "type": "string" },
      "description": "可选 knowledge_id 列表（来自 <sources> 块），表示你怀疑导致污染或错误的来源。"
    }
  },
  "required": ["slug", "issue_type", "description"]
}`),
		),
		wikiService: wikiService,
		kbIDs:       kbIDs,
	}
}

func (t *wikiFlagIssueTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var params struct {
		Slug                  string   `json:"slug"`
		IssueType             string   `json:"issue_type"`
		Description           string   `json:"description"`
		SuspectedKnowledgeIDs []string `json:"suspected_knowledge_ids"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &types.ToolResult{Success: false, Error: "参数无效: " + err.Error()}, nil
	}

	slug := strings.TrimSpace(params.Slug)
	if slug == "" {
		return &types.ToolResult{Success: false, Error: "需要 slug"}, nil
	}

	if len(t.kbIDs) == 0 {
		return &types.ToolResult{Success: false, Error: "没有可用于问题跟踪的知识库"}, nil
	}

	// Default to first KB ID if multiple (normally there's only one in this context)
	kbID := t.kbIDs[0]

	// Verify the page exists
	page, err := t.wikiService.GetPageBySlug(ctx, kbID, slug)
	if err != nil || page == nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("未找到 slug 为 '%s' 的 Wiki 页面", slug)}, nil
	}

	issue := &types.WikiPageIssue{
		TenantID:              page.TenantID,
		KnowledgeBaseID:       kbID,
		Slug:                  slug,
		IssueType:             params.IssueType,
		Description:           params.Description,
		SuspectedKnowledgeIDs: params.SuspectedKnowledgeIDs,
		ReportedBy:            "wiki-researcher-agent",
		Status:                "pending",
	}

	_, err = t.wikiService.CreateIssue(ctx, issue)
	if err != nil {
		return &types.ToolResult{Success: false, Error: "创建问题失败: " + err.Error()}, nil
	}

	return &types.ToolResult{
		Success: true,
		Output:  fmt.Sprintf("已成功标记 %s 的问题，并创建维护工单供审核。", slug),
	}, nil
}
