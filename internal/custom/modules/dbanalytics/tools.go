package dbanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

type CatalogTool struct {
	agenttools.BaseTool
	service *Service
	scope   ToolScope
}

type SchemaTool struct {
	agenttools.BaseTool
	service *Service
	scope   ToolScope
}

type QueryTool struct {
	agenttools.BaseTool
	service    *Service
	scope      ToolScope
	allowChart bool
}

func NewCatalogTool(service *Service, scope ToolScope) *CatalogTool {
	return &CatalogTool{
		BaseTool: agenttools.NewBaseTool(
			ToolDBCatalog,
			`按业务术语查找相关 MySQL/PostgreSQL 分析表和列。
写 SQL 前先使用此工具。请将用户问题与表名、列名、字段描述和样例值匹配。
此工具属于隐藏推理工作流：查询前用它推断表/字段业务含义。`,
			utils.GenerateSchema[CatalogInput](),
		),
		service: service,
		scope:   scope,
	}
}

func NewSchemaTool(service *Service, scope ToolScope) *SchemaTool {
	return &SchemaTool{
		BaseTool: agenttools.NewBaseTool(
			ToolDBSchema,
			`获取已绑定 MySQL/PostgreSQL 数据源的完整 schema、字段描述、语义类型、样例值和 SQL 表名。
db_query 前必须调用此工具。先根据描述和样例推断表与字段的业务含义，再编写 SQL。
table_names 请传入 db_catalog 返回的 sql_table_name 值。source_id 是可选项，通常应省略。
除非用户要求，不要在最终回答中暴露这段中间语义推断。`,
			utils.GenerateSchema[SchemaInput](),
		),
		service: service,
		scope:   scope,
	}
}

func NewQueryTool(service *Service, scope ToolScope, allowChart bool) *QueryTool {
	return &QueryTool{
		BaseTool: agenttools.NewBaseTool(
			ToolDBQuery,
			`对已授权的 MySQL/PostgreSQL 数据源表执行只读 SQL 分析查询。
源表物化后由 DuckDB 执行查询，因此请编写 DuckDB 兼容 SQL，而不是源数据库专属 SQL。
只使用 db_catalog/db_schema 返回的 SQL 表名。仅使用 SELECT；返回原始行前先聚合，并添加过滤/限制。
source_id 是可选项；除非你有意限制到 db_catalog/db_schema 返回的某个 source_id，否则省略它。
只有当用户明确要求 chart/graph/plot/visualization/图表/可视化或某个具名图表类型时，才设置 chart_requested=true。
返回图表时，将其 ChartContract visual_scope 作为图表实际渲染内容的事实来源；查询行和 evidence_scope 仍可支持独立文本洞察。
只有当用户明确要求 table/detail/raw/list 输出时，才设置 table_requested=true；否则查询行仅作为答案证据，UI 不会渲染表格。`,
			utils.GenerateSchema[QueryInput](),
		),
		service:    service,
		scope:      scope,
		allowChart: allowChart,
	}
}

func (t *CatalogTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input CatalogInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, err
	}
	data, err := t.service.Catalog(ctx, t.scopeFromContext(ctx), input)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, err
	}
	return &types.ToolResult{Success: true, Output: formatCatalogOutput(data), Data: data}, nil
}

func (t *SchemaTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input SchemaInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, err
	}
	data, err := t.service.Schema(ctx, t.scopeFromContext(ctx), input)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, err
	}
	return &types.ToolResult{Success: true, Output: formatSchemaOutput(data), Data: data}, nil
}

func (t *QueryTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input QueryInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, err
	}
	data, err := t.service.ExecuteQuery(ctx, t.scopeFromContext(ctx), input, t.allowChart)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, err
	}
	return &types.ToolResult{Success: true, Output: formatAnalysisOutput(data), Data: data}, nil
}

func (t *CatalogTool) scopeFromContext(ctx context.Context) ToolScope {
	scope := t.scope
	fillScopeFromContext(ctx, &scope)
	return scope
}

func (t *SchemaTool) scopeFromContext(ctx context.Context) ToolScope {
	scope := t.scope
	fillScopeFromContext(ctx, &scope)
	return scope
}

func (t *QueryTool) scopeFromContext(ctx context.Context) ToolScope {
	scope := t.scope
	fillScopeFromContext(ctx, &scope)
	return scope
}

func fillScopeFromContext(ctx context.Context, scope *ToolScope) {
	if scope.TenantID == 0 {
		if tid, ok := ctx.Value(types.TenantIDContextKey).(uint64); ok {
			scope.TenantID = tid
		}
	}
	if scope.SourceTenantID == 0 {
		scope.SourceTenantID = scope.TenantID
	}
	if scope.UserID == "" {
		if uid, ok := ctx.Value(types.UserIDContextKey).(string); ok {
			scope.UserID = uid
		}
	}
	if scope.TenantRole == "" {
		scope.TenantRole = types.TenantRoleFromContext(ctx)
	}
}

func formatCatalogOutput(data map[string]any) string {
	tables, _ := data["tables"].([]map[string]any)
	var b strings.Builder
	b.WriteString("=== 数据库目录 ===\n")
	b.WriteString(fmt.Sprintf("匹配表数量：%d\n\n", len(tables)))
	b.WriteString("在 db_schema table_names 和 db_query SQL 中精确使用 sql_table_name。source_id 是可选项；除非要过滤到单个来源，否则省略。\n\n")
	for _, table := range tables {
		b.WriteString(fmt.Sprintf("- sql_table_name=%s; source_id=%s; source=%s/%s; physical=%s.%s; description=%s\n",
			table["sql_table_name"], table["source_id"], table["source_name"], table["source_type"],
			table["schema_name"], table["table_name"], table["description"]))
	}
	return b.String()
}

func formatSchemaOutput(data map[string]any) string {
	rawTables, _ := data["tables"].([]map[string]any)
	semanticContext, _ := data["semantic_context"].([]map[string]any)
	var b strings.Builder
	b.WriteString("=== 数据库 Schema ===\n\n")
	for _, table := range rawTables {
		b.WriteString(fmt.Sprintf("表 %s (%s.%s)\n", table["sql_table_name"], table["schema_name"], table["table_name"]))
		if desc := strings.TrimSpace(fmt.Sprint(table["description"])); desc != "" {
			b.WriteString("描述：" + desc + "\n")
		}
		if cols, ok := table["columns"].([]map[string]any); ok {
			for _, col := range cols {
				b.WriteString(fmt.Sprintf("- %s %s [%s]: %s; samples=%v\n",
					col["name"], col["type"], col["semantic_type"], col["description"], col["sample_values"]))
			}
		}
		b.WriteString("\n")
	}
	if len(semanticContext) > 0 {
		b.WriteString("=== 用于 SQL 推理的业务含义推断 ===\n")
		b.WriteString("使用此推断上下文选择 join、指标、维度和时间过滤。除非用户要求，不要在最终回答中重复此推断。\n")
		for _, item := range semanticContext {
			b.WriteString(fmt.Sprintf("- %s: %s; grain=%s\n",
				item["sql_table_name"], item["business_meaning"], item["grain_hint"]))
			if rels, ok := item["likely_relationships"].([]map[string]string); ok && len(rels) > 0 {
				for _, rel := range rels {
					b.WriteString(fmt.Sprintf("  join 提示：%s -> %s\n", rel["column"], rel["likely_references"]))
				}
			}
			b.WriteString(fmt.Sprintf("  metrics=%v; dimensions=%v; time=%v\n",
				item["metric_columns"], item["dimension_columns"], item["time_columns"]))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func formatAnalysisOutput(data map[string]any) string {
	rows, _ := data["rows"].([]map[string]any)
	chart, _ := data["chart"].(map[string]any)
	chartEligible, _ := chart["eligible"].(bool)
	tableRequested, _ := data["table_requested"].(bool)
	var b strings.Builder
	b.WriteString("=== 结构化分析结果 ===\n")
	b.WriteString(fmt.Sprintf("SQL: %s\n", data["query"]))
	b.WriteString(fmt.Sprintf("返回行数：%d\n", len(rows)))
	b.WriteString(fmt.Sprintf("显示模式：%s\n", data["display_mode"]))
	b.WriteString(fmt.Sprintf("是否请求图表：%v\n", data["chart_requested"]))
	b.WriteString(fmt.Sprintf("是否请求表格：%v\n", tableRequested))
	if chartEligible {
		chartID := fmt.Sprint(chart["id"])
		contractJSON, _ := json.Marshal(chart["contract"])
		b.WriteString(fmt.Sprintf("结构化图表：id=%s; type=%s; x=%s; y=%v; group=%s; secondary_y=%v; value=%s\n",
			chartID,
			chart["default_type"],
			chart["x"],
			chart["y"],
			chart["group"],
			chart["secondary_y"],
			chart["value"],
		))
		if len(contractJSON) > 0 && string(contractJSON) != "null" {
			b.WriteString(fmt.Sprintf("ChartContract: %s\n", string(contractJSON)))
		}
		b.WriteString(fmt.Sprintf("最终回答中，请将 {{chart:%s}} 立即放在解释该图表的段落之后。除非用户明确要求文件导出，不要创建图表图片或产物文件。\n", chartID))
		b.WriteString("最终图表解释规则：ChartContract visual_scope 描述图表实际渲染的内容。额外结论可以使用查询行或 evidence_scope，但应写成文本洞察，不要声称图表本身可视化了 non_visual_fields。\n")
	} else if data["chart_requested"] == true {
		b.WriteString(fmt.Sprintf("结构化图表不可用：%s\n", chart["reason"]))
	}
	if !tableRequested {
		b.WriteString("最终回答规则：除非用户明确要求表格/明细/原始/list 输出，否则不要把这些行渲染为 Markdown 表格。\n")
	}
	b.WriteString("\n")
	for i, row := range rows {
		if i >= 20 {
			b.WriteString("...\n")
			break
		}
		encoded, _ := json.Marshal(row)
		b.WriteString(fmt.Sprintf("行 %d: %s\n", i+1, string(encoded)))
	}
	return b.String()
}
