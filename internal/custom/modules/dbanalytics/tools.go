package dbanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/custom/modules/toolcontract"
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
Returns authorized table identifiers and metadata. Use it when the required table or field is not yet known.`,
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
Use this when the fields, types or business meaning needed for the query are not already known.
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
The query is executed by DuckDB over complete source tables. The output row limit never truncates input to aggregates. Write DuckDB-compatible SQL.
Use only SQL table names returned by db_catalog/db_schema. Use SELECT only, aggregate before returning raw rows, and add filters/limits.
Do not use compound queries: UNION, UNION ALL, INTERSECT, or EXCEPT are rejected by SQL validation.
source_id is optional; omit it unless you intentionally want to restrict the query to one source_id returned by db_catalog/db_schema.
Set chart_requested=true only when the user explicitly asks for chart/graph/plot/visualization/图表/可视化 or a named chart type.
When a chart is returned, use its ChartContract visual_scope as the source of truth for what the chart actually renders; query rows and evidence_scope may still support separate textual insights.`,
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
	output, err := toolcontract.QueryOutput(data)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, err
	}
	return &types.ToolResult{Success: true, Output: output, Data: data}, nil
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
	var b strings.Builder
	b.WriteString("=== Database Schema ===\n\n")
	for _, table := range rawTables {
		b.WriteString(fmt.Sprintf("Table %s (%s.%s)\n", table["sql_table_name"], table["schema_name"], table["table_name"]))
		if desc := strings.TrimSpace(fmt.Sprint(table["description"])); desc != "" {
			b.WriteString("Description: " + desc + "\n")
		}
		if cols, ok := table["columns"].([]map[string]any); ok {
			for _, col := range cols {
				b.WriteString(fmt.Sprintf("- %s %s; nullable=%v; configured_role=%s; description=%s; samples=%v\n",
					col["name"], col["type"], col["nullable"], col["semantic_type"], col["description"], col["sample_values"]))
			}
		}
		b.WriteString("\n")
	}

	return b.String()
}
