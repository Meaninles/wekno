package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

var webSearchTool = BaseTool{
	name: ToolWebSearch,
	description: `搜索 Web 获取当前信息和新闻。此工具会联网查找知识库中可能没有的最新信息。

## 关键 - 知识库优先规则
**绝对规则**：使用此工具前，必须先完成知识库检索（grep_chunks 和 knowledge_search）。
- 不要在未尝试 grep_chunks 和 knowledge_search 的情况下使用 web_search
- 只有当 grep_chunks 和 knowledge_search 都返回不足或无结果时，才使用 web_search
- 知识库检索是强制步骤，不可跳过

## 特性
- 实时 Web 搜索：联网搜索当前信息
- RAG 压缩：自动压缩并抽取搜索结果中的相关内容
- 会话级缓存：为会话维护临时知识库，避免重复索引

## 用法

**使用场景**：
- **仅在**完成 grep_chunks 和 knowledge_search 之后
- 知识库检索结果不足或无结果
- 需要当前或实时信息（新闻、事件、近期更新）
- 信息不在知识库中
- 需要验证或补充知识库信息
- 搜索近期进展或趋势

**参数**：
- query（必填）：搜索查询字符串

**返回**：包含标题、URL、摘要片段和内容的 Web 搜索结果（最多 %d 条）

## 示例

` + "`" + `
# 搜索当前信息
{
  "query": "latest developments in AI"
}

# 搜索近期新闻
{
  "query": "Python 3.12 release notes"
}
` + "`" + `

## 提示

- 结果会自动使用 RAG 压缩以抽取相关内容
- 搜索结果会存入会话临时知识库
- 当知识库没有所需信息时使用此工具
- 结果包含 URL、标题、摘要片段和内容片段（可能被截断）
- **关键**：如果内容被截断或需要完整细节，请使用 **web_fetch** 获取完整页面内容
- 每次搜索最多返回 %d 条结果`,
	schema: utils.GenerateSchema[WebSearchInput](),
}

// WebSearchInput defines the input parameters for web search tool
type WebSearchInput struct {
	Query string `json:"query" jsonschema:"搜索查询字符串"`
}

// WebSearchTool performs web searches and returns results
type WebSearchTool struct {
	BaseTool
	webSearchService      interfaces.WebSearchService
	knowledgeBaseService  interfaces.KnowledgeBaseService
	knowledgeService      interfaces.KnowledgeService
	webSearchStateService interfaces.WebSearchStateService
	sessionID             string
	maxResults            int
	providerID            string // WebSearchProviderEntity ID (resolved from agent config or tenant default)
}

// NewWebSearchTool creates a new web search tool
func NewWebSearchTool(
	webSearchService interfaces.WebSearchService,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	webSearchStateService interfaces.WebSearchStateService,
	sessionID string,
	maxResults int,
	providerID string,
) *WebSearchTool {
	tool := webSearchTool
	tool.description = fmt.Sprintf(tool.description, maxResults, maxResults)

	return &WebSearchTool{
		BaseTool:              tool,
		webSearchService:      webSearchService,
		knowledgeBaseService:  knowledgeBaseService,
		knowledgeService:      knowledgeService,
		webSearchStateService: webSearchStateService,
		sessionID:             sessionID,
		maxResults:            maxResults,
		providerID:            providerID,
	}
}

// Execute executes the web search tool
func (t *WebSearchTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][WebSearch] Execute started")

	// Parse args from json.RawMessage
	var input WebSearchInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][WebSearch] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("解析参数失败: %v", err),
		}, err
	}

	// Parse query
	query := input.Query
	ok := query != ""
	if !ok || query == "" {
		logger.Errorf(ctx, "[Tool][WebSearch] Query is required")
		return &types.ToolResult{
			Success: false,
			Error:   "需要提供 query 参数",
		}, fmt.Errorf("query parameter is required")
	}

	logger.Infof(ctx, "[Tool][WebSearch] Searching with query: %s, max_results: %d", query, t.maxResults)

	// Get tenant ID from context
	tenantID := uint64(0)
	if tid, ok := ctx.Value(types.TenantIDContextKey).(uint64); ok {
		tenantID = tid
	}

	if tenantID == 0 {
		logger.Errorf(ctx, "[Tool][WebSearch] Tenant ID not found in context")
		return &types.ToolResult{
			Success: false,
			Error:   "上下文中未找到租户 ID",
		}, fmt.Errorf("tenant ID not found in context")
	}

	// Get tenant info from context (same approach as search.go)
	var tenant *types.Tenant
	if tenantValue := ctx.Value(types.TenantInfoContextKey); tenantValue != nil {
		tenant, _ = tenantValue.(*types.Tenant)
	}

	// Resolve provider ID: tool-level (set from agent config, which already resolved default)
	resolvedProviderID := t.providerID

	// Create a copy of the effective web search config with maxResults from agent config.
	searchConfig := types.EffectiveWebSearchConfig(nil)
	if tenant != nil {
		searchConfig = types.EffectiveWebSearchConfig(tenant.WebSearchConfig)
	}
	searchConfig.MaxResults = t.maxResults

	// Perform web search
	logger.Infof(
		ctx,
		"[Tool][WebSearch] Performing web search with providerID: %s, maxResults: %d",
		resolvedProviderID,
		searchConfig.MaxResults,
	)
	webResults, err := t.webSearchService.Search(ctx, resolvedProviderID, searchConfig, query)
	if err != nil {
		logger.Errorf(ctx, "[Tool][WebSearch] Web search failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("网页搜索失败: %v", err),
		}, fmt.Errorf("web search failed: %w", err)
	}

	logger.Infof(ctx, "[Tool][WebSearch] Web search returned %d results", len(webResults))

	// Apply RAG compression if configured
	if len(webResults) > 0 && searchConfig.CompressionMethod != "none" &&
		searchConfig.CompressionMethod != "" {
		// Load session-scoped temp KB state from Redis using WebSearchStateRepository
		tempKBID, seen, ids := t.webSearchStateService.GetWebSearchTempKBState(ctx, t.sessionID)

		// Build questions for RAG compression
		questions := []string{strings.TrimSpace(query)}

		logger.Infof(ctx, "[Tool][WebSearch] Applying RAG compression")
		compressed, kbID, newSeen, newIDs, err := t.webSearchService.CompressWithRAG(
			ctx, t.sessionID, tempKBID, questions, webResults, searchConfig,
			t.knowledgeBaseService, t.knowledgeService, seen, ids,
		)
		if err != nil {
			logger.Warnf(ctx, "[Tool][WebSearch] RAG compression failed, using raw results: %v", err)
		} else {
			webResults = compressed
			// Persist temp KB state back into Redis using WebSearchStateRepository
			t.webSearchStateService.SaveWebSearchTempKBState(ctx, t.sessionID, kbID, newSeen, newIDs)
			logger.Infof(ctx, "[Tool][WebSearch] RAG compression completed, %d results", len(webResults))
		}
	}

	// Format output
	if len(webResults) == 0 {
		return &types.ToolResult{
			Success: true,
			Output:  fmt.Sprintf("未找到 Web 搜索结果，查询：%s", query),
			Data: map[string]interface{}{
				"query":   query,
				"results": []interface{}{},
				"count":   0,
			},
		}, nil
	}

	// Build output text
	output := "=== Web 搜索结果 ===\n"
	output += fmt.Sprintf("查询：%s\n", query)
	output += fmt.Sprintf("找到 %d 条结果\n\n", len(webResults))

	// Format results
	formattedResults := make([]map[string]interface{}, 0, len(webResults))
	for i, result := range webResults {
		output += fmt.Sprintf("结果 #%d:\n", i+1)
		output += fmt.Sprintf("  标题: %s\n", result.Title)
		output += fmt.Sprintf("  URL: %s\n", result.URL)
		if result.Snippet != "" {
			output += fmt.Sprintf("  摘要: %s\n", result.Snippet)
		}
		if result.Content != "" {
			// Truncate content if too long
			content := result.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			output += fmt.Sprintf("  内容: %s\n", content)
		}
		if result.PublishedAt != nil {
			output += fmt.Sprintf("  发布时间: %s\n", result.PublishedAt.Format(time.RFC3339))
		}
		output += "\n"

		resultData := map[string]interface{}{
			"result_index": i + 1,
			"title":        result.Title,
			"url":          result.URL,
			"snippet":      result.Snippet,
			"content":      result.Content,
			"source":       result.Source,
		}
		if result.PublishedAt != nil {
			resultData["published_at"] = result.PublishedAt.Format(time.RFC3339)
		}
		formattedResults = append(formattedResults, resultData)
	}

	// Add guidance for next steps
	output += "\n=== 下一步 ===\n"
	if len(webResults) > 0 {
		output += "- ⚠️ 内容可能已截断（仅显示前 500 个字符）。请使用 web_fetch 获取完整页面内容。\n"
		output += "- 从上方结果提取 URL，并使用合适的提示词调用 web_fetch 获取详细信息。\n"
		output += "- 综合多个来源的信息，生成全面回答。\n"
	} else {
		output += "- 未找到 Web 搜索结果。请考虑：\n"
		output += "  - 尝试不同搜索查询或关键词\n"
		output += "  - 检查是否可改由知识库回答\n"
		output += "  - 确认该主题是否确实需要实时信息\n"
	}

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"query":        query,
			"results":      formattedResults,
			"count":        len(webResults),
			"display_type": "web_search_results",
		},
	}, nil
}
