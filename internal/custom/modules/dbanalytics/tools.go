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
			`Find relevant MySQL/PostgreSQL analysis tables and columns by business terms.
Use this before writing SQL. Match the user's question against table names, column names, field descriptions and sample values.
This tool is part of the hidden reasoning workflow: use it to infer table/field business meaning before querying.`,
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
			`Get full schema, field descriptions, semantic types, sample values and SQL table names for bound MySQL/PostgreSQL data sources.
Always call this before db_query. First infer the business meaning of tables and fields from descriptions and samples, then write SQL.
Pass table_names as the sql_table_name values returned by db_catalog. source_id is optional and should usually be omitted.
Do not expose this intermediate semantic inference in the final answer unless the user asks.`,
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
			`Execute a read-only SQL analysis query over authorized MySQL/PostgreSQL data source tables.
The query is executed by DuckDB after source tables are materialized, so write DuckDB-compatible SQL rather than source-database-specific SQL.
Use only SQL table names returned by db_catalog/db_schema. Use SELECT only, aggregate before returning raw rows, and add filters/limits.
source_id is optional; omit it unless you intentionally want to restrict the query to one source_id returned by db_catalog/db_schema.
Set chart_requested=true only when the user explicitly asks for chart/graph/plot/visualization/图表/可视化 or a named chart type.
When a chart is returned, use its ChartContract visual_scope as the source of truth for what the chart actually renders; query rows and evidence_scope may still support separate textual insights.
Set table_requested=true only when the user explicitly asks for table/detail/raw/list output; otherwise query rows are evidence for the answer and the UI will not render a table.`,
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
	b.WriteString("=== Database Catalog ===\n")
	b.WriteString(fmt.Sprintf("Matched tables: %d\n\n", len(tables)))
	b.WriteString("Use sql_table_name exactly in db_schema table_names and db_query SQL. source_id is optional; omit it unless filtering to one source.\n\n")
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
	b.WriteString("=== Database Schema ===\n\n")
	for _, table := range rawTables {
		b.WriteString(fmt.Sprintf("Table %s (%s.%s)\n", table["sql_table_name"], table["schema_name"], table["table_name"]))
		if desc := strings.TrimSpace(fmt.Sprint(table["description"])); desc != "" {
			b.WriteString("Description: " + desc + "\n")
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
		b.WriteString("=== Business Meaning Inference For SQL Reasoning ===\n")
		b.WriteString("Use this inferred context to choose joins, metrics, dimensions and time filters. Do not repeat this inference in the final answer unless the user asks.\n")
		for _, item := range semanticContext {
			b.WriteString(fmt.Sprintf("- %s: %s; grain=%s\n",
				item["sql_table_name"], item["business_meaning"], item["grain_hint"]))
			if rels, ok := item["likely_relationships"].([]map[string]string); ok && len(rels) > 0 {
				for _, rel := range rels {
					b.WriteString(fmt.Sprintf("  join hint: %s -> %s\n", rel["column"], rel["likely_references"]))
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
	b.WriteString("=== Structured Analysis Result ===\n")
	b.WriteString(fmt.Sprintf("SQL: %s\n", data["query"]))
	b.WriteString(fmt.Sprintf("Returned rows: %d\n", len(rows)))
	b.WriteString(fmt.Sprintf("Display mode: %s\n", data["display_mode"]))
	b.WriteString(fmt.Sprintf("Chart requested: %v\n", data["chart_requested"]))
	b.WriteString(fmt.Sprintf("Table requested: %v\n", tableRequested))
	if chartEligible {
		chartID := fmt.Sprint(chart["id"])
		contractJSON, _ := json.Marshal(chart["contract"])
		b.WriteString(fmt.Sprintf("Structured chart: id=%s; type=%s; x=%s; y=%v; group=%s; secondary_y=%v; value=%s\n",
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
		b.WriteString(fmt.Sprintf("In the final answer, place {{chart:%s}} immediately after the paragraph that explains this chart. Do not create chart images or artifact files unless the user explicitly requested a file export.\n", chartID))
		b.WriteString("Final chart explanation rule: ChartContract visual_scope describes what the chart actually renders. Additional conclusions may use query rows or evidence_scope, but write them as textual insights and do not claim the chart itself visualizes non_visual_fields.\n")
	} else if data["chart_requested"] == true {
		b.WriteString(fmt.Sprintf("Structured chart unavailable: %s\n", chart["reason"]))
	}
	if !tableRequested {
		b.WriteString("Final answer rule: do not render these rows as a Markdown table unless the user explicitly asked for a table/detail/raw/list output.\n")
	}
	b.WriteString("\n")
	for i, row := range rows {
		if i >= 20 {
			b.WriteString("...\n")
			break
		}
		encoded, _ := json.Marshal(row)
		b.WriteString(fmt.Sprintf("row %d: %s\n", i+1, string(encoded)))
	}
	return b.String()
}
