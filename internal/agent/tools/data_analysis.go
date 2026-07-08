package tools

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	textencoding "github.com/Tencent/WeKnora/internal/custom/modules/textencoding"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/xuri/excelize/v2"
)

var dataAnalysisTool = BaseTool{
	name: ToolDataAnalysis,
	description: "Use this tool when the knowledge is CSV or Excel files. It loads the data into memory and executes SQL for data analysis. " +
		"For Excel files with multiple sheets, every sheet is loaded into the same table and the source sheet name is exposed as a '__sheet_name' column so you can filter/aggregate per sheet. " +
		"If the user's question requires data statistics, convert the question into SQL and execute it.",
	schema: utils.GenerateSchema[DataAnalysisInput](),
}

var tableAnalysisTool = BaseTool{
	name:        ToolTableAnalysis,
	description: "Use this tool for CSV/Excel table analysis. It exposes both a normal DuckDB table and a faithful original-file cell evidence table, executes SELECT-only DuckDB SQL, and can return structured chart data when chart_requested is true. You may use VALUES or UNION ALL to normalize irregular files, but chart results must include non-empty LLM-authored source_mapping JSON that maps result data back to original file evidence. The prompt template is weak guidance only and is not validated as a fixed schema.",
	schema:      utils.GenerateSchema[TableAnalysisInput](),
}

// excelSheetNameColumn is the name of the synthetic column that identifies
// which Excel sheet a row came from when multiple sheets are unioned together.
const excelSheetNameColumn = "__sheet_name"
const excelRawTableSuffix = "__raw"

const DisplayTypeStructuredAnalysis = "structured_analysis_result"
const tableAnalysisMaxRows = 1000

var reDuckDBTryCastForPostgresValidation = regexp.MustCompile(`(?i)\btry_cast\s*\(`)

// sqlSingleQuoteEscape escapes single quotes in a string so it can be safely
// embedded inside a single-quoted SQL literal.
func sqlSingleQuoteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func normalizeIdentifierForMatch(s string) string {
	normalized := strings.ToLower(strings.TrimSpace(s))
	normalized = strings.ReplaceAll(normalized, " ", "")
	normalized = strings.ReplaceAll(normalized, "\u3000", "")
	return normalized
}

func reconcileSQLColumnsWithSchema(sqlText string, schema *TableSchema) (string, []string) {
	if schema == nil || len(schema.Columns) == 0 {
		return sqlText, nil
	}

	normalizedToCanonical := make(map[string]string, len(schema.Columns))
	for _, col := range schema.Columns {
		key := normalizeIdentifierForMatch(col.Name)
		if key == "" {
			continue
		}
		if _, exists := normalizedToCanonical[key]; !exists {
			normalizedToCanonical[key] = col.Name
		}
	}

	quotedIdentifierPattern := regexp.MustCompile(`"([^"]+)"`)
	fixes := make([]string, 0)
	rewritten := quotedIdentifierPattern.ReplaceAllStringFunc(sqlText, func(token string) string {
		name := strings.Trim(token, "\"")
		canonical, ok := normalizedToCanonical[normalizeIdentifierForMatch(name)]
		if !ok || canonical == name {
			return token
		}
		fixes = append(fixes, fmt.Sprintf("%q -> %q", name, canonical))
		return fmt.Sprintf(`"%s"`, canonical)
	})

	return rewritten, fixes
}

func buildMissingColumnSuggestion(sqlErr error, schema *TableSchema) string {
	if sqlErr == nil || schema == nil {
		return ""
	}
	msg := sqlErr.Error()
	if !strings.Contains(msg, `Referenced column "`) || !strings.Contains(msg, `not found`) {
		return ""
	}

	matches := regexp.MustCompile(`Referenced column "([^"]+)" not found`).FindStringSubmatch(msg)
	if len(matches) < 2 {
		return ""
	}

	missing := matches[1]
	normalizedMissing := normalizeIdentifierForMatch(missing)
	if normalizedMissing == "" {
		return ""
	}

	for _, col := range schema.Columns {
		if normalizeIdentifierForMatch(col.Name) == normalizedMissing {
			return fmt.Sprintf("Column %q does not exist. Did you mean %q? Please use the exact column name from schema.", missing, col.Name)
		}
	}

	return ""
}

type DataAnalysisInput struct {
	KnowledgeID     string                 `json:"knowledge_id" jsonschema:"id of the knowledge-base CSV/Excel file or current-turn uploaded table attachment to query"`
	Sql             string                 `json:"sql" jsonschema:"DuckDB-compatible read-only SQL to run against the loaded CSV/Excel table; use the table name returned by table_schema"`
	ChartRequested  bool                   `json:"chart_requested,omitempty" jsonschema:"true only when the user explicitly asks for a chart, graph, plot, visualization, 图表, 可视化, or a named chart type"`
	PreferredChart  string                 `json:"preferred_chart,omitempty" jsonschema:"optional chart type requested by the user or selected after an explicit chart request: line,bar,stacked_bar,pie,scatter,histogram,heatmap,funnel,dual_axis_combo,area,radar,treemap,boxplot"`
	ChartIntent     string                 `json:"chart_intent,omitempty" jsonschema:"optional natural-language chart intent, e.g. compare subcategory counts by sequence; used only when chart_requested is true"`
	PrimaryMetric   string                 `json:"primary_metric,omitempty" jsonschema:"optional SQL result column name that should be the primary visual metric when chart_requested is true"`
	SecondaryMetric string                 `json:"secondary_metric,omitempty" jsonschema:"optional SQL result column name for a secondary metric, especially dual_axis_combo or relationship charts"`
	Dimension       string                 `json:"dimension,omitempty" jsonschema:"optional SQL result column name that should be the main category/time axis when chart_requested is true"`
	Series          string                 `json:"series,omitempty" jsonschema:"optional SQL result column name that should be the series/stack/group dimension when chart_requested is true"`
	ChartTitle      string                 `json:"chart_title,omitempty" jsonschema:"optional concise Chinese chart title; use when it helps align the rendered chart with the final explanation"`
	SourceMapping   map[string]interface{} `json:"source_mapping,omitempty" jsonschema:"required for table_analysis chart result queries: LLM-authored JSON mapping from SQL result fields/rows/values to original CSV/Excel fields, cells, ranges, and derivation rules; use the prompt template as weak guidance only; the runtime forwards it and may inspect referenced cells, but never auto-generates it or validates an exact template shape"`
}

type TableAnalysisInput = DataAnalysisInput

type DataAnalysisTool struct {
	BaseTool
	knowledgeBaseService interfaces.KnowledgeBaseService
	knowledgeService     interfaces.KnowledgeService
	fileService          interfaces.FileService
	tenantService        interfaces.TenantService
	db                   *sql.DB
	sessionID            string
	allowChart           bool
	displayIntent        *types.TableAnalysisDisplayIntent
	runtimeAttachments   types.MessageAttachments
	createdTables        []string // Track tables created in this session
	loadedSchemas        map[string]*TableSchema
	// localBaseDir is the LOCAL_STORAGE_BASE_DIR value captured at construction
	// time so resolveFileServiceForKnowledge uses the same base path that was
	// used when the local FileService was initialised by DI.  Re-reading the
	// env var at request time can produce a different (or empty) value if the
	// variable was not exported to the sub-process or was set programmatically
	// after startup, causing GetFile to look in the wrong directory (#1040).
	localBaseDir string
}

type TableAnalysisTool = DataAnalysisTool

func NewDataAnalysisTool(
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	tenantService interfaces.TenantService,
	fileService interfaces.FileService,
	db *sql.DB,
	sessionID string,
	agentTypes ...string,
) *DataAnalysisTool {
	allowChart := false
	for _, agentType := range agentTypes {
		if agentType == types.AgentTypeTableAnalysis {
			allowChart = true
			break
		}
	}

	return &DataAnalysisTool{
		BaseTool:             dataAnalysisTool,
		knowledgeBaseService: knowledgeBaseService,
		knowledgeService:     knowledgeService,
		fileService:          fileService,
		tenantService:        tenantService,
		db:                   db,
		sessionID:            sessionID,
		allowChart:           allowChart,
		loadedSchemas:        make(map[string]*TableSchema),
		// Capture LOCAL_STORAGE_BASE_DIR once at construction time so that every
		// call to resolveFileServiceForKnowledge uses the same base path.  The
		// env var is guaranteed to be set (or empty == "/data/files" fallback)
		// when the application starts and the DI container is assembled.
		localBaseDir: strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR")),
	}
}

func NewDataAnalysisToolWithConfig(
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	tenantService interfaces.TenantService,
	fileService interfaces.FileService,
	db *sql.DB,
	sessionID string,
	config *types.AgentConfig,
) *DataAnalysisTool {
	agentType := ""
	if config != nil {
		agentType = config.AgentType
	}
	tool := NewDataAnalysisTool(
		knowledgeBaseService,
		knowledgeService,
		tenantService,
		fileService,
		db,
		sessionID,
		agentType,
	)
	if config != nil {
		tool.displayIntent = config.TableAnalysisDisplayIntent
		tool.runtimeAttachments = append(types.MessageAttachments(nil), config.RuntimeAttachments...)
	}
	return tool
}

func NewTableAnalysisToolWithConfig(
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	tenantService interfaces.TenantService,
	fileService interfaces.FileService,
	db *sql.DB,
	sessionID string,
	config *types.AgentConfig,
) *TableAnalysisTool {
	tool := NewDataAnalysisToolWithConfig(
		knowledgeBaseService,
		knowledgeService,
		tenantService,
		fileService,
		db,
		sessionID,
		config,
	)
	tool.BaseTool = tableAnalysisTool
	return tool
}

// recordCreatedTable records a table name for cleanup, ensuring uniqueness
// Returns true if the table was newly recorded, false if it already existed
func (t *DataAnalysisTool) recordCreatedTable(tableName string) bool {
	for _, name := range t.createdTables {
		if name == tableName {
			return false
		}
	}
	t.createdTables = append(t.createdTables, tableName)
	return true
}

// Cleanup cleans up the session-specific schema
func (t *DataAnalysisTool) Cleanup(ctx context.Context) {
	if len(t.createdTables) == 0 {
		logger.Infof(ctx, "[Tool][LegacyDataAnalysis] No tables to clean up for session: %s", t.sessionID)
		t.loadedSchemas = nil
		return
	}

	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Cleaning up %d tables for session: %s", len(t.createdTables), t.sessionID)

	for _, tableName := range t.createdTables {
		dropSQL := fmt.Sprintf("DROP TABLE IF EXISTS \"%s\"", tableName)
		if _, err := t.db.ExecContext(ctx, dropSQL); err != nil {
			logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to drop table '%s': %v", tableName, err)
			// Continue to drop other tables even if one fails
			continue
		}
		logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Successfully dropped table '%s'", tableName)
	}

	// Clear the list after cleanup
	t.createdTables = nil
	t.loadedSchemas = nil
}

func (t *DataAnalysisTool) isTableAnalysisTool() bool {
	return t != nil && t.Name() == ToolTableAnalysis
}

func (t *DataAnalysisTool) tableAnalysisValidationOptions(allowedTables []string) []utils.SQLValidationOption {
	opts := []utils.SQLValidationOption{
		utils.WithInputValidation(6, 12000),
		utils.WithSelectOnly(),
		utils.WithAllowedTables(allowedTables...),
		utils.WithSingleStatement(),
		utils.WithNoDangerousFunctions(),
	}
	if t.isTableAnalysisTool() {
		opts = append(opts,
			utils.WithSafeUnionAll(64, 16),
			utils.WithSafeSystemTypeCasts(),
			utils.WithAllowTablelessSelect(),
		)
	} else {
		opts = append(opts, utils.WithInjectionRiskCheck())
	}
	return opts
}

func normalizeTableAnalysisSQL(sqlText string) string {
	sqlText = strings.TrimSpace(sqlText)
	for strings.HasSuffix(sqlText, ";") {
		sqlText = strings.TrimSpace(strings.TrimSuffix(sqlText, ";"))
	}
	return sqlText
}

func limitTableAnalysisSQL(sqlText string, maxRows int) string {
	if maxRows <= 0 {
		maxRows = tableAnalysisMaxRows
	}
	return fmt.Sprintf("SELECT * FROM (%s) AS __weknora_table_analysis_limited LIMIT %d", normalizeTableAnalysisSQL(sqlText), maxRows)
}

func tableAnalysisSQLForASTValidation(sqlText string) string {
	return reDuckDBTryCastForPostgresValidation.ReplaceAllString(sqlText, "CAST(")
}

func newTableAnalysisValidationError(errorType, message, details string) *utils.SQLValidationResult {
	return &utils.SQLValidationResult{
		Valid: false,
		Errors: []utils.SQLValidationError{
			{Type: errorType, Message: message, Details: details},
		},
	}
}

func normalizeTableAnalysisASTValidationErrors(validation *utils.SQLValidationResult) *utils.SQLValidationResult {
	if validation == nil || validation.Valid {
		return validation
	}
	for i := range validation.Errors {
		if validation.Errors[i].Type != "parse_error" {
			continue
		}
		validation.Errors[i].Type = "statement_validation_error"
		validation.Errors[i].Message = "DuckDB SQL passed syntax validation but could not be analyzed by table_analysis safety rules"
		validation.Errors[i].Details = validation.Errors[i].Details + "; simplify the SELECT shape or avoid DuckDB-only constructs that cannot be inspected by the safety validator"
	}
	return validation
}

func (t *DataAnalysisTool) validateTableAnalysisSQL(ctx context.Context, sqlText string, allowedTables []string) *utils.SQLValidationResult {
	sqlText = normalizeTableAnalysisSQL(sqlText)
	if t == nil || t.db == nil {
		return newTableAnalysisValidationError(
			"parse_error",
			"Failed to parse DuckDB SQL",
			"DuckDB connection is unavailable for table_analysis SQL validation",
		)
	}

	duckDBSQL := fmt.Sprintf("EXPLAIN SELECT * FROM (%s) AS __weknora_table_analysis_validate LIMIT 0", sqlText)
	rows, err := t.db.QueryContext(ctx, duckDBSQL)
	if err != nil {
		return newTableAnalysisValidationError(
			"parse_error",
			"Failed to parse DuckDB SQL",
			fmt.Sprintf("DuckDB SQL parse/bind error: %v", err),
		)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return newTableAnalysisValidationError(
			"parse_error",
			"Failed to parse DuckDB SQL",
			fmt.Sprintf("DuckDB SQL parse/bind error: %v", err),
		)
	}

	compatSQL := tableAnalysisSQLForASTValidation(sqlText)
	_, validation := utils.ValidateSQL(compatSQL, t.tableAnalysisValidationOptions(allowedTables)...)
	return normalizeTableAnalysisASTValidationErrors(validation)
}

// Execute executes the SQL query on DuckDB (only read-only queries are allowed)
func (t *DataAnalysisTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Execute started for session: %s", t.sessionID)
	var input DataAnalysisInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to parse input args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse input args: %v", err),
		}, err
	}
	if err := t.applyDisplayIntentToInput(&input); err != nil {
		logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Display intent rejected tool input for session %s: %v", t.sessionID, err)
		return &types.ToolResult{
			Success: false,
			Error:   err.Error(),
		}, err
	}

	schema, err := t.LoadFromKnowledgeID(ctx, input.KnowledgeID)
	if err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to load knowledge ID '%s': %v", input.KnowledgeID, err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to load knowledge ID '%s': %v", input.KnowledgeID, err),
		}, err
	}

	// Replace knowledge ID with table name
	input.Sql = strings.ReplaceAll(input.Sql, input.KnowledgeID, schema.TableName)
	if rewrittenSQL, fixes := reconcileSQLColumnsWithSchema(input.Sql, schema); len(fixes) > 0 {
		logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Auto-rewrote SQL identifiers for session %s: %v", t.sessionID, fixes)
		input.Sql = rewrittenSQL
	}
	input.Sql = normalizeTableAnalysisSQL(input.Sql)
	if t.isTableAnalysisTool() && input.ChartRequested && !hasUsableTableAnalysisSourceMapping(input.SourceMapping) {
		err := fmt.Errorf("table_analysis chart result queries must include non-empty LLM-authored source_mapping JSON that maps result data back to original file evidence; the template is weak guidance only and is not validated as a fixed schema")
		logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Missing source_mapping for session %s: %v", t.sessionID, err)
		return &types.ToolResult{
			Success: false,
			Error:   err.Error(),
		}, err
	}

	if !t.isTableAnalysisTool() {
		// Preserve legacy data_analysis behavior. The table_analysis tool uses
		// parser-based validation below so read-only CTEs are not misclassified.
		normalizedSQL := strings.TrimSpace(strings.ToLower(input.Sql))
		isReadOnly := strings.HasPrefix(normalizedSQL, "select") ||
			strings.HasPrefix(normalizedSQL, "show") ||
			strings.HasPrefix(normalizedSQL, "describe") ||
			strings.HasPrefix(normalizedSQL, "explain") ||
			strings.HasPrefix(normalizedSQL, "pragma")

		if !isReadOnly {
			// Reject modification queries
			logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Modification query rejected for session %s: %s", t.sessionID, input.Sql)
			return &types.ToolResult{
				Success: false,
				Error:   "DuckDB tool only supports read-only queries (SELECT, SHOW, DESCRIBE, EXPLAIN, PRAGMA). Modification operations (INSERT, UPDATE, DELETE, CREATE, DROP, etc.) are not allowed.",
			}, fmt.Errorf("modification queries are not allowed")
		}
	}

	// Validate SQL with comprehensive security checks
	// IMPORTANT: Must enable validateSelectStmt to block RangeFunction attacks
	allowedTables := allowedTableAnalysisTables(schema)
	var validation *utils.SQLValidationResult
	if t.isTableAnalysisTool() {
		validation = t.validateTableAnalysisSQL(ctx, input.Sql, allowedTables)
	} else {
		_, validation = utils.ValidateSQL(input.Sql, t.tableAnalysisValidationOptions(allowedTables)...)
	}
	if !validation.Valid {
		logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] SQL validation failed for session %s: %v", t.sessionID, validation.Errors)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("SQL validation failed: %v", validation.Errors),
		}, fmt.Errorf("SQL validation failed: %v", validation.Errors)
	}

	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Received SQL query for session %s: %s", t.sessionID, input.Sql)
	// Execute single query and get results
	columns, results, truncated, err := t.executeSingleQuery(ctx, input.Sql)
	if err != nil {
		if suggestion := buildMissingColumnSuggestion(err, schema); suggestion != "" {
			return &types.ToolResult{
				Success: false,
				Error:   fmt.Sprintf("Query execution failed: %v. %s", err, suggestion),
			}, err
		}
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Query execution failed: %v", err),
		}, err
	}

	queryOutput := t.formatQueryResults(results, input.Sql)
	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Completed execution query, total %d rows for session %s", len(results), t.sessionID)
	chartRequested := input.ChartRequested && t.allowChart
	chart := inferStructuredChartSpec(columns, results, input.PreferredChart, chartRequested, tableChartHintsFromInput(input))
	sourceMappingValidation := map[string]interface{}{"status": "not_applicable"}
	if t.isTableAnalysisTool() {
		sourceMappingValidation = t.validateTableAnalysisSourceMapping(ctx, schema, input.SourceMapping)
	}
	displayMode := "text_only"
	if eligible, _ := chart["eligible"].(bool); eligible {
		displayMode = "chart_only"
	}
	limits := map[string]interface{}{"truncated": truncated}
	if t.isTableAnalysisTool() {
		limits["max_rows"] = tableAnalysisMaxRows
	}
	return &types.ToolResult{
		Success: true,
		Output:  queryOutput,
		Data: map[string]interface{}{
			"display_type":              DisplayTypeStructuredAnalysis,
			"display_mode":              displayMode,
			"analysis_type":             "file",
			"source":                    map[string]interface{}{"type": "file", "knowledge_id": input.KnowledgeID, "table_name": schema.TableName, "allowed_tables": allowedTables},
			"columns":                   columns,
			"rows":                      results,
			"row_count":                 len(results),
			"query":                     input.Sql,
			"chart_requested":           chartRequested,
			"chart":                     chart,
			"source_mapping":            input.SourceMapping,
			"source_mapping_validation": sourceMappingValidation,
			"limits":                    limits,
			"session_id":                t.sessionID,
		},
	}, nil
}

// executeSingleQuery executes a single SQL query and returns columns and results
// Parameters:
//   - ctx: context for cancellation and timeout
//   - sqlQuery: the SQL query to execute
//   - existingColumns: existing column names to merge with (can be nil or empty)
//
// Returns:
//   - []map[string]interface{}: result column metadata
//   - []map[string]string: query results
//   - bool: whether the result was truncated by the table-analysis row budget
//   - error: any error that occurred during execution
func (t *DataAnalysisTool) executeSingleQuery(ctx context.Context, sqlQuery string) ([]map[string]interface{}, []map[string]string, bool, error) {
	querySQL := sqlQuery
	maxRows := 0
	if t.isTableAnalysisTool() {
		maxRows = tableAnalysisMaxRows
		querySQL = limitTableAnalysisSQL(sqlQuery, maxRows)
	}

	rows, err := t.db.QueryContext(ctx, querySQL)
	if err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Query execution failed: %v", err)
		return nil, nil, false, fmt.Errorf("query execution failed: %w", err)
	}
	defer rows.Close()

	// Get column names
	columnNames, err := rows.Columns()
	if err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to get columns: %v", err)
		return nil, nil, false, fmt.Errorf("failed to get columns: %w", err)
	}
	columnTypes, _ := rows.ColumnTypes()
	columns := make([]map[string]interface{}, 0, len(columnNames))
	for i, colName := range columnNames {
		dataType := "text"
		if i < len(columnTypes) && columnTypes[i].DatabaseTypeName() != "" {
			dataType = columnTypes[i].DatabaseTypeName()
		}
		columns = append(columns, map[string]interface{}{
			"name":          colName,
			"type":          dataType,
			"semantic_type": inferResultSemanticType(colName, dataType),
		})
	}

	// Process results
	results := make([]map[string]string, 0)
	for rows.Next() {
		columnValues := make([]interface{}, len(columnNames))
		columnPointers := make([]interface{}, len(columnNames))
		for i := range columnValues {
			columnPointers[i] = &columnValues[i]
		}

		if err := rows.Scan(columnPointers...); err != nil {
			logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to scan row: %v", err)
			return nil, nil, false, fmt.Errorf("failed to scan row: %w", err)
		}

		rowMap := make(map[string]string)
		for i, colName := range columnNames {
			val := columnValues[i]
			// Convert []byte to string for better readability
			if b, ok := val.([]byte); ok {
				rowMap[colName] = string(b)
			} else {
				rowMap[colName] = fmt.Sprintf("%v", val)
			}
		}
		results = append(results, rowMap)
	}

	if err := rows.Err(); err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Error iterating rows: %v", err)
		return nil, nil, false, fmt.Errorf("error iterating rows: %w", err)
	}

	truncated := maxRows > 0 && len(results) >= maxRows
	return columns, results, truncated, nil
}

// formatQueryResults formats query results into JSONL format (one JSON object per line)
func (t *DataAnalysisTool) formatQueryResults(results []map[string]string, query string) string {
	var output strings.Builder

	output.WriteString("=== DuckDB Query Results ===\n\n")
	output.WriteString(fmt.Sprintf("Executed SQL: %s\n\n", query))
	output.WriteString(fmt.Sprintf("Returned %d rows\n\n", len(results)))

	if len(results) == 0 {
		output.WriteString("No matching records found.\n")
		return output.String()
	}

	output.WriteString("=== Data Details ===\n\n")
	if len(results) > 10 {
		output.WriteString(fmt.Sprintf("Showing all %d records. Consider using a LIMIT clause to restrict the result count for better performance.\n\n", len(results)))
	}

	// Write each record as a separate JSON line
	for i, record := range results {
		recordBytes, _ := json.Marshal(record)

		// Remove the trailing newline added by Encode
		recordStr := strings.Trim(string(recordBytes), "\n")
		output.WriteString(fmt.Sprintf("record %d: %s\n", i+1, recordStr))
	}

	return output.String()
}

func (t *DataAnalysisTool) applyDisplayIntentToInput(input *DataAnalysisInput) error {
	if input == nil || !t.allowChart || t.displayIntent == nil {
		return nil
	}
	toolName := t.Name()
	if strings.TrimSpace(toolName) == "" {
		toolName = ToolTableAnalysis
	}
	if input.ChartRequested && !t.displayIntent.ChartRequested {
		return fmt.Errorf("table-analysis display intent is chart_requested=false, but %s was called with chart_requested=true; retry with chart_requested=false and answer with text/table evidence only", toolName)
	}
	if t.displayIntent.ChartRequested && !input.ChartRequested {
		if t.isTableAnalysisTool() {
			return nil
		}
		return fmt.Errorf("table-analysis display intent is chart_requested=true, but %s was called without chart_requested=true; retry the analytical result query with chart_requested=true", toolName)
	}
	if t.displayIntent.ChartRequested && strings.TrimSpace(input.PreferredChart) == "" && strings.TrimSpace(t.displayIntent.PreferredChart) != "" {
		input.PreferredChart = t.displayIntent.PreferredChart
	}
	return nil
}

func inferResultSemanticType(name, dataType string) string {
	lowerName := strings.ToLower(name)
	lowerType := strings.ToLower(dataType)
	if strings.Contains(lowerType, "date") || strings.Contains(lowerType, "time") ||
		strings.HasSuffix(lowerName, "_at") || strings.Contains(lowerName, "date") || strings.Contains(lowerName, "time") {
		return "time"
	}
	if strings.Contains(lowerType, "int") || strings.Contains(lowerType, "decimal") ||
		strings.Contains(lowerType, "numeric") || strings.Contains(lowerType, "double") ||
		strings.Contains(lowerType, "float") || strings.Contains(lowerType, "real") {
		if strings.HasSuffix(lowerName, "_id") || lowerName == "id" {
			return "dimension"
		}
		return "metric"
	}
	return "dimension"
}

type tableChartColumnGroups struct {
	dimensions []string
	metrics    []string
	times      []string
}

type tableChartIntentHints struct {
	intent          string
	primaryMetric   string
	secondaryMetric string
	dimension       string
	series          string
	title           string
}

func tableChartHintsFromInput(input DataAnalysisInput) tableChartIntentHints {
	return tableChartIntentHints{
		intent:          strings.TrimSpace(input.ChartIntent),
		primaryMetric:   strings.TrimSpace(input.PrimaryMetric),
		secondaryMetric: strings.TrimSpace(input.SecondaryMetric),
		dimension:       strings.TrimSpace(input.Dimension),
		series:          strings.TrimSpace(input.Series),
		title:           strings.TrimSpace(input.ChartTitle),
	}
}

func inferStructuredChartSpec(columns []map[string]interface{}, rows []map[string]string, preferred string, requested bool, hintsOpt ...tableChartIntentHints) map[string]interface{} {
	hints := tableChartIntentHints{}
	if len(hintsOpt) > 0 {
		hints = hintsOpt[0]
	}
	spec := map[string]interface{}{
		"eligible":       false,
		"id":             "",
		"type":           "",
		"default_type":   "",
		"x":              "",
		"y":              []string{},
		"group":          "",
		"secondary_y":    []string{},
		"value":          "",
		"reason":         "chart not requested",
		"language":       "zh-CN",
		"labels_locale":  "zh-CN",
		"table_visible":  false,
		"explicit_chart": requested,
		"chart_intent":   hints.intent,
		"chart_title":    hints.title,
		"contract":       map[string]interface{}{},
		"validation":     map[string]interface{}{"status": "not_requested", "issues": []string{"chart not requested"}},
	}
	if !requested {
		return spec
	}
	if len(rows) == 0 || len(columns) < 2 {
		spec["reason"] = "not enough result data for chart"
		spec["validation"] = map[string]interface{}{"status": "invalid", "issues": []string{"not enough result data for chart"}}
		return spec
	}
	groups := classifyTableChartColumns(columns, rows)
	if len(groups.metrics) == 0 {
		spec["reason"] = "no numeric metric column"
		spec["validation"] = map[string]interface{}{"status": "invalid", "issues": []string{"no numeric metric column"}}
		return spec
	}
	chartType := normalizeStructuredChartType(preferred)
	if chartType == "" {
		chartType = chooseDefaultTableChartType(groups)
	}
	if !supportedStructuredChartType(chartType) {
		spec["reason"] = "unsupported chart type"
		spec["validation"] = map[string]interface{}{"status": "invalid", "issues": []string{"unsupported chart type"}}
		return spec
	}
	if !populateTableChartSpec(spec, chartType, groups, rows, hints) {
		if spec["reason"] == "" {
			spec["reason"] = "result shape is not suitable for requested chart"
		}
		spec["validation"] = map[string]interface{}{"status": "invalid", "issues": []string{fmt.Sprint(spec["reason"])}}
		return spec
	}

	spec["eligible"] = true
	spec["default_type"] = chartType
	spec["type"] = chartType
	spec["id"] = structuredChartID(chartType, spec)
	spec["reason"] = ""
	contract := buildStructuredChartContract(spec, chartType, groups, columns, hints)
	spec["contract"] = contract
	spec["validation"] = validateStructuredChartContract(contract, columns)
	return spec
}

func classifyTableChartColumns(columns []map[string]interface{}, rows []map[string]string) tableChartColumnGroups {
	var groups tableChartColumnGroups
	for _, col := range columns {
		name := fmt.Sprint(col["name"])
		sem := fmt.Sprint(col["semantic_type"])
		if sem == "metric" && resultColumnLooksNumeric(rows, name) {
			groups.metrics = append(groups.metrics, name)
		} else if sem == "time" {
			groups.times = append(groups.times, name)
		} else {
			groups.dimensions = append(groups.dimensions, name)
		}
	}
	return groups
}

func normalizeStructuredChartType(chartType string) string {
	chartType = strings.ToLower(strings.TrimSpace(chartType))
	chartType = strings.ReplaceAll(chartType, "-", "_")
	chartType = strings.ReplaceAll(chartType, " ", "_")
	switch chartType {
	case "combo", "dual_axis", "dual_axis_chart", "dual_axis_bar_line", "bar_line", "bar_line_combo":
		return "dual_axis_combo"
	case "stacked", "stackedbar", "stacked_bar_chart":
		return "stacked_bar"
	case "tree_map":
		return "treemap"
	default:
		return chartType
	}
}

func supportedStructuredChartType(chartType string) bool {
	switch chartType {
	case "line", "bar", "stacked_bar", "pie", "scatter", "histogram", "heatmap", "funnel", "dual_axis_combo",
		"area", "radar", "treemap", "boxplot":
		return true
	default:
		return false
	}
}

func chooseDefaultTableChartType(groups tableChartColumnGroups) string {
	if len(groups.times) > 0 {
		if len(groups.metrics) >= 2 {
			return "dual_axis_combo"
		}
		return "line"
	}
	if len(groups.dimensions) >= 2 && len(groups.metrics) > 0 {
		return "stacked_bar"
	}
	if len(groups.metrics) >= 2 && len(groups.dimensions) == 0 {
		return "scatter"
	}
	return "bar"
}

func tableChartDimensionCandidates(groups tableChartColumnGroups, preferTime bool) []string {
	out := make([]string, 0, len(groups.dimensions)+len(groups.times))
	add := func(items []string) {
		for _, item := range items {
			if item != "" && !containsStringValue(out, item) {
				out = append(out, item)
			}
		}
	}
	if preferTime {
		add(groups.times)
		add(groups.dimensions)
	} else {
		add(groups.dimensions)
		add(groups.times)
	}
	return out
}

func preferredChartField(preferred string, candidates []string) string {
	preferred = strings.TrimSpace(preferred)
	if preferred == "" {
		return ""
	}
	for _, field := range candidates {
		if strings.TrimSpace(field) == preferred {
			return field
		}
	}
	for _, field := range candidates {
		if strings.EqualFold(strings.TrimSpace(field), preferred) {
			return field
		}
	}
	return ""
}

func chooseTableChartDimension(groups tableChartColumnGroups, preferred string, preferTime bool, exclude ...string) string {
	candidates := tableChartDimensionCandidates(groups, preferTime)
	filtered := make([]string, 0, len(candidates))
	for _, field := range candidates {
		if field == "" || containsStringValue(exclude, field) {
			continue
		}
		filtered = append(filtered, field)
	}
	if field := preferredChartField(preferred, filtered); field != "" {
		return field
	}
	return firstStringValue(filtered)
}

func chooseTableChartMetric(groups tableChartColumnGroups, preferred string, exclude ...string) string {
	candidates := make([]string, 0, len(groups.metrics))
	for _, field := range groups.metrics {
		if field == "" || containsStringValue(exclude, field) {
			continue
		}
		candidates = append(candidates, field)
	}
	if field := preferredChartField(preferred, candidates); field != "" {
		return field
	}
	return firstStringValue(candidates)
}

func orderedTableChartMetrics(groups tableChartColumnGroups, preferred ...string) []string {
	out := make([]string, 0, len(groups.metrics))
	for _, hint := range preferred {
		if field := preferredChartField(hint, groups.metrics); field != "" && !containsStringValue(out, field) {
			out = append(out, field)
		}
	}
	for _, field := range groups.metrics {
		if field != "" && !containsStringValue(out, field) {
			out = append(out, field)
		}
	}
	return out
}

func populateTableChartSpec(spec map[string]interface{}, chartType string, groups tableChartColumnGroups, rows []map[string]string, hints tableChartIntentHints) bool {
	_ = rows
	dimensionAxis := chooseTableChartDimension(groups, hints.dimension, true)
	categoryAxis := chooseTableChartDimension(groups, hints.dimension, false)
	metric := chooseTableChartMetric(groups, hints.primaryMetric)

	switch chartType {
	case "line", "area":
		if dimensionAxis == "" || metric == "" {
			spec["reason"] = "line/area chart requires one dimension/time column and one metric column"
			return false
		}
		spec["x"] = dimensionAxis
		if preferredChartField(hints.primaryMetric, groups.metrics) != "" {
			spec["y"] = []string{metric}
		} else {
			spec["y"] = groups.metrics
		}
	case "bar":
		if categoryAxis == "" || metric == "" {
			spec["reason"] = "bar chart requires one category column and one metric column"
			return false
		}
		spec["x"] = categoryAxis
		if preferredChartField(hints.primaryMetric, groups.metrics) != "" {
			spec["y"] = []string{metric}
		} else {
			spec["y"] = groups.metrics
		}
	case "stacked_bar":
		if categoryAxis == "" || len(groups.dimensions)+len(groups.times) < 2 || metric == "" {
			spec["reason"] = "stacked bar chart requires two dimensions and one metric column"
			return false
		}
		spec["x"] = categoryAxis
		spec["group"] = chooseTableChartDimension(groups, hints.series, false, categoryAxis)
		spec["y"] = []string{metric}
	case "pie", "funnel", "treemap":
		if categoryAxis == "" || metric == "" {
			spec["reason"] = chartType + " requires one category column and one metric column"
			return false
		}
		spec["x"] = categoryAxis
		spec["y"] = []string{metric}
		if chartType == "treemap" {
			spec["group"] = chooseTableChartDimension(groups, hints.series, false, categoryAxis)
		}
	case "scatter":
		if len(groups.metrics) < 2 {
			spec["reason"] = "scatter chart requires at least two metric columns"
			return false
		}
		xMetric := chooseTableChartMetric(groups, hints.primaryMetric)
		yMetric := chooseTableChartMetric(groups, hints.secondaryMetric, xMetric)
		if xMetric == "" || yMetric == "" {
			spec["reason"] = "scatter chart requires two distinct metric columns"
			return false
		}
		spec["x"] = xMetric
		spec["y"] = []string{yMetric}
	case "histogram":
		if metric == "" {
			spec["reason"] = "histogram requires one numeric metric column"
			return false
		}
		spec["x"] = metric
		if len(groups.metrics) > 1 {
			spec["y"] = []string{groups.metrics[1]}
		}
	case "heatmap":
		if categoryAxis == "" || len(groups.dimensions)+len(groups.times) < 2 || metric == "" {
			spec["reason"] = "heatmap requires two dimensions and one metric column"
			return false
		}
		spec["x"] = categoryAxis
		spec["group"] = chooseTableChartDimension(groups, hints.series, false, categoryAxis)
		spec["y"] = []string{metric}
	case "dual_axis_combo":
		if dimensionAxis == "" || len(groups.metrics) < 2 {
			spec["reason"] = "dual-axis combo chart requires one dimension and two metric columns"
			return false
		}
		primary := chooseTableChartMetric(groups, hints.primaryMetric)
		secondary := chooseTableChartMetric(groups, hints.secondaryMetric, primary)
		if primary == "" || secondary == "" {
			spec["reason"] = "dual-axis combo chart requires two distinct metric columns"
			return false
		}
		spec["x"] = dimensionAxis
		spec["y"] = []string{primary}
		spec["secondary_y"] = []string{secondary}
	case "radar":
		if len(groups.metrics) < 3 {
			spec["reason"] = "radar chart requires at least three metric columns"
			return false
		}
		spec["x"] = categoryAxis
		spec["y"] = orderedTableChartMetrics(groups, hints.primaryMetric, hints.secondaryMetric)
	case "boxplot":
		fiveNumber := inferFiveNumberFields(groups.metrics)
		if len(fiveNumber) == 5 {
			spec["x"] = categoryAxis
			spec["y"] = fiveNumber
			break
		}
		if metric == "" {
			spec["reason"] = "boxplot requires raw numeric values or min/q1/median/q3/max columns"
			return false
		}
		spec["x"] = categoryAxis
		spec["y"] = []string{metric}
	default:
		return false
	}
	return true
}

func buildStructuredChartContract(spec map[string]interface{}, chartType string, groups tableChartColumnGroups, columns []map[string]interface{}, hints tableChartIntentHints) map[string]interface{} {
	x := stringFromInterface(spec["x"])
	yFields := stringsFromInterface(spec["y"])
	metric := firstStringValue(yFields)
	group := stringFromInterface(spec["group"])
	secondary := firstStringValue(stringsFromInterface(spec["secondary_y"]))
	value := stringFromInterface(spec["value"])
	if value == "" {
		value = metric
	}
	spec["value"] = value

	encoding := map[string]interface{}{}
	setEncodingField(encoding, "x", x, tableChartFieldRole(groups, x), "")
	switch chartType {
	case "heatmap":
		setEncodingField(encoding, "y", group, tableChartFieldRole(groups, group), "")
		setEncodingField(encoding, "value", value, "metric", "sum")
	case "stacked_bar":
		setEncodingField(encoding, "series", group, tableChartFieldRole(groups, group), "")
		setEncodingField(encoding, "stack", group, tableChartFieldRole(groups, group), "")
		setEncodingField(encoding, "value", value, "metric", "sum")
	case "scatter":
		setEncodingField(encoding, "y", metric, "metric", "")
		if x != "" {
			setEncodingField(encoding, "x", x, "metric", "")
		}
	case "histogram":
		setEncodingField(encoding, "value", x, "metric", "count")
	case "dual_axis_combo":
		setEncodingField(encoding, "value", metric, "metric", "sum")
		setEncodingField(encoding, "secondary_value", secondary, "metric", "sum")
	case "treemap":
		hierarchy := []map[string]interface{}{}
		if x != "" {
			hierarchy = append(hierarchy, chartEncodingField(x, tableChartFieldRole(groups, x), ""))
		}
		if group != "" {
			hierarchy = append(hierarchy, chartEncodingField(group, tableChartFieldRole(groups, group), ""))
		}
		encoding["hierarchy"] = hierarchy
		setEncodingField(encoding, "value", value, "metric", "sum")
	case "radar", "boxplot":
		setEncodingField(encoding, "value", metric, "metric", chartAggregateForType(chartType))
		setEncodingFields(encoding, "values", yFields, "metric", chartAggregateForType(chartType))
	default:
		setEncodingField(encoding, "y", metric, "metric", "")
		setEncodingField(encoding, "value", value, "metric", "sum")
		if len(yFields) > 1 {
			setEncodingFields(encoding, "values", yFields, "metric", "sum")
		}
	}

	availableFields := columnNamesFromInterfaceMaps(columns)
	visualDimensions := chartVisualDimensions(chartType, x, group)
	visualMetrics := chartVisualMetrics(chartType, x, yFields, secondary, value)
	visualFields := uniqueNonEmptyStrings(append(visualDimensions, visualMetrics...)...)
	return map[string]interface{}{
		"id":       stringFromInterface(spec["id"]),
		"type":     chartType,
		"intent":   map[string]interface{}{"task": chartTask(chartType), "reason": hints.intent},
		"encoding": encoding,
		"transform": map[string]interface{}{
			"group_by":      chartGroupBy(chartType, x, group),
			"aggregate":     chartAggregateForType(chartType),
			"dedupe_policy": chartDedupePolicy(chartType),
			"sort":          []map[string]interface{}{},
			"limit":         nil,
		},
		"visual_scope": map[string]interface{}{
			"dimensions":  visualDimensions,
			"series":      group,
			"metrics":     visualMetrics,
			"fields":      visualFields,
			"description": "fields actually encoded by the rendered chart",
		},
		"evidence_scope": map[string]interface{}{
			"available_fields":  availableFields,
			"visualized_fields": visualFields,
			"non_visual_fields": differenceStrings(availableFields, visualFields),
			"description":       "query result fields may support textual insights; do not claim non_visual_fields are visually encoded by this chart",
		},
		"display": map[string]interface{}{
			"title":         chartDisplayTitle(chartType, x, group, value, hints.title),
			"x_label":       x,
			"y_label":       chartYLabel(chartType, group, value),
			"value_label":   value,
			"legend_title":  chartLegendTitle(chartType, group),
			"language":      "zh-CN",
			"table_visible": false,
		},
		"metadata": map[string]interface{}{
			"source":  "table_analysis",
			"columns": availableFields,
		},
	}
}

func setEncodingField(encoding map[string]interface{}, key string, field string, role string, aggregate string) {
	if field == "" {
		return
	}
	encoding[key] = chartEncodingField(field, role, aggregate)
}

func setEncodingFields(encoding map[string]interface{}, key string, fields []string, role string, aggregate string) {
	items := make([]map[string]interface{}, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		items = append(items, chartEncodingField(field, role, aggregate))
	}
	if len(items) > 0 {
		encoding[key] = items
	}
}

func chartEncodingField(field, role, aggregate string) map[string]interface{} {
	out := map[string]interface{}{"field": field}
	if role != "" {
		out["role"] = role
	}
	if aggregate != "" {
		out["aggregate"] = aggregate
	}
	return out
}

func tableChartFieldRole(groups tableChartColumnGroups, field string) string {
	if field == "" {
		return ""
	}
	for _, item := range groups.metrics {
		if item == field {
			return "metric"
		}
	}
	for _, item := range groups.times {
		if item == field {
			return "time"
		}
	}
	for _, item := range groups.dimensions {
		if item == field {
			return "dimension"
		}
	}
	return "dimension"
}

func chartTask(chartType string) string {
	switch chartType {
	case "line", "area", "dual_axis_combo":
		return "trend"
	case "pie", "stacked_bar", "treemap":
		return "composition"
	case "histogram", "boxplot":
		return "distribution"
	case "scatter":
		return "relationship"
	case "heatmap":
		return "comparison"
	case "funnel":
		return "conversion"
	default:
		return "comparison"
	}
}

func chartAggregateForType(chartType string) string {
	switch chartType {
	case "histogram":
		return "count"
	case "scatter", "boxplot":
		return "none"
	default:
		return "sum"
	}
}

func chartDedupePolicy(chartType string) string {
	switch chartType {
	case "scatter", "boxplot":
		return "keep"
	default:
		return "aggregate"
	}
}

func chartGroupBy(chartType, x, group string) []string {
	out := make([]string, 0, 3)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || containsStringValue(out, value) {
			return
		}
		out = append(out, value)
	}
	switch chartType {
	case "heatmap", "stacked_bar":
		add(x)
		add(group)
	default:
		add(x)
	}
	return out
}

func chartDisplayTitle(chartType, x, group, value, preferred string) string {
	preferred = strings.TrimSpace(preferred)
	if preferred != "" {
		return preferred
	}
	if x != "" && group != "" && value != "" {
		switch chartType {
		case "heatmap":
			return fmt.Sprintf("%s与%s%s热力图", x, group, value)
		case "stacked_bar":
			return fmt.Sprintf("%s-%s%s构成", x, group, value)
		case "treemap":
			return fmt.Sprintf("%s-%s%s树图", x, group, value)
		}
	}
	if x != "" && value != "" {
		switch chartType {
		case "line":
			return fmt.Sprintf("%s%s趋势", x, value)
		case "area":
			return fmt.Sprintf("%s%s面积趋势", x, value)
		case "bar":
			return fmt.Sprintf("%s%s对比", x, value)
		case "pie":
			return fmt.Sprintf("%s%s占比", x, value)
		case "funnel":
			return fmt.Sprintf("%s%s漏斗", x, value)
		case "dual_axis_combo":
			return fmt.Sprintf("%s多指标趋势", x)
		}
	}
	switch chartType {
	case "heatmap":
		return "交叉热力分析"
	case "stacked_bar":
		return "分组堆叠对比"
	case "line":
		return "趋势分析"
	case "area":
		return "面积趋势分析"
	case "pie":
		return "占比分析"
	case "scatter":
		return "散点关系分析"
	case "histogram":
		return "分布分析"
	case "funnel":
		return "漏斗分析"
	case "dual_axis_combo":
		return "双轴组合分析"
	case "radar":
		return "雷达对比分析"
	case "treemap":
		return "层级树图"
	case "boxplot":
		return "箱线分布分析"
	default:
		if x != "" && value != "" {
			return fmt.Sprintf("%s对比", x)
		}
		if group != "" {
			return fmt.Sprintf("%s分析", group)
		}
		return "数据图表"
	}
}

func chartYLabel(chartType, group, value string) string {
	switch chartType {
	case "heatmap":
		return group
	case "scatter", "boxplot":
		return value
	default:
		if value != "" {
			return value
		}
		return "数值"
	}
}

func chartLegendTitle(chartType, group string) string {
	switch chartType {
	case "stacked_bar", "heatmap":
		return group
	default:
		return ""
	}
}

func chartVisualDimensions(chartType, x, group string) []string {
	switch chartType {
	case "scatter", "histogram":
		return []string{}
	case "heatmap", "stacked_bar", "treemap":
		return uniqueNonEmptyStrings(x, group)
	default:
		return uniqueNonEmptyStrings(x)
	}
}

func chartVisualMetrics(chartType, x string, yFields []string, secondary, value string) []string {
	switch chartType {
	case "histogram":
		return uniqueNonEmptyStrings(x)
	case "dual_axis_combo":
		return uniqueNonEmptyStrings(value, secondary)
	case "scatter":
		return uniqueNonEmptyStrings(x, firstStringValue(yFields))
	default:
		return uniqueNonEmptyStrings(append(yFields, value)...)
	}
}

func validateStructuredChartContract(contract map[string]interface{}, columns []map[string]interface{}) map[string]interface{} {
	issues := make([]string, 0)
	chartType := stringFromInterface(contract["type"])
	encoding, _ := contract["encoding"].(map[string]interface{})
	columnSet := make(map[string]bool, len(columns))
	for _, col := range columns {
		name := strings.TrimSpace(fmt.Sprint(col["name"]))
		if name != "" {
			columnSet[name] = true
		}
	}
	requireField := func(key string) string {
		field := encodingFieldName(encoding, key)
		if field == "" {
			issues = append(issues, fmt.Sprintf("encoding.%s is required", key))
			return ""
		}
		if !columnSet[field] {
			issues = append(issues, fmt.Sprintf("encoding.%s field %q is not in query columns", key, field))
		}
		return field
	}
	requireAnyHierarchy := func() {
		raw, _ := encoding["hierarchy"].([]map[string]interface{})
		if len(raw) == 0 {
			if arr, ok := encoding["hierarchy"].([]interface{}); ok {
				if len(arr) == 0 {
					issues = append(issues, "encoding.hierarchy is required")
				}
				for _, item := range arr {
					if m, ok := item.(map[string]interface{}); ok {
						field := stringFromInterface(m["field"])
						if field == "" {
							issues = append(issues, "encoding.hierarchy contains an empty field")
						} else if !columnSet[field] {
							issues = append(issues, fmt.Sprintf("encoding.hierarchy field %q is not in query columns", field))
						}
					}
				}
				return
			}
			issues = append(issues, "encoding.hierarchy is required")
			return
		}
		for _, item := range raw {
			field := stringFromInterface(item["field"])
			if field == "" {
				issues = append(issues, "encoding.hierarchy contains an empty field")
			} else if !columnSet[field] {
				issues = append(issues, fmt.Sprintf("encoding.hierarchy field %q is not in query columns", field))
			}
		}
	}
	requireFields := func(key string, min int) []string {
		fields := encodingFieldNames(encoding, key)
		if len(fields) < min {
			issues = append(issues, fmt.Sprintf("encoding.%s requires at least %d field(s)", key, min))
			return fields
		}
		for _, field := range fields {
			if !columnSet[field] {
				issues = append(issues, fmt.Sprintf("encoding.%s field %q is not in query columns", key, field))
			}
		}
		return fields
	}

	switch chartType {
	case "heatmap":
		requireField("x")
		requireField("y")
		requireField("value")
	case "stacked_bar":
		requireField("x")
		requireField("series")
		requireField("value")
	case "bar", "line", "area", "pie", "funnel":
		requireField("x")
		requireField("value")
	case "dual_axis_combo":
		requireField("x")
		requireField("value")
		requireField("secondary_value")
	case "scatter":
		requireField("x")
		requireField("y")
	case "histogram":
		requireField("value")
	case "treemap":
		requireAnyHierarchy()
		requireField("value")
	case "radar":
		requireFields("values", 3)
	case "boxplot":
		if len(encodingFieldNames(encoding, "values")) > 0 {
			requireFields("values", 1)
		} else {
			requireField("value")
		}
	default:
		issues = append(issues, "unsupported chart type in contract")
	}

	status := "pass"
	if len(issues) > 0 {
		status = "invalid"
	}
	return map[string]interface{}{"status": status, "issues": issues}
}

func encodingFieldName(encoding map[string]interface{}, key string) string {
	raw, ok := encoding[key]
	if !ok {
		return ""
	}
	switch value := raw.(type) {
	case map[string]interface{}:
		return stringFromInterface(value["field"])
	case map[string]string:
		return value["field"]
	default:
		return ""
	}
}

func encodingFieldNames(encoding map[string]interface{}, key string) []string {
	raw, ok := encoding[key]
	if !ok {
		return nil
	}
	switch value := raw.(type) {
	case []map[string]interface{}:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if field := stringFromInterface(item["field"]); field != "" {
				out = append(out, field)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if m, ok := item.(map[string]interface{}); ok {
				if field := stringFromInterface(m["field"]); field != "" {
					out = append(out, field)
				}
			}
		}
		return out
	default:
		if field := encodingFieldName(encoding, key); field != "" {
			return []string{field}
		}
		return nil
	}
}

func columnNamesFromInterfaceMaps(columns []map[string]interface{}) []string {
	out := make([]string, 0, len(columns))
	for _, col := range columns {
		name := strings.TrimSpace(fmt.Sprint(col["name"]))
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func stringFromInterface(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func stringsFromInterface(value interface{}) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := stringFromInterface(item); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		if text := stringFromInterface(value); text != "" && text != "<nil>" {
			return []string{text}
		}
		return nil
	}
}

func firstStringValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func uniqueNonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || containsStringValue(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func differenceStrings(all []string, used []string) []string {
	out := make([]string, 0, len(all))
	for _, value := range all {
		value = strings.TrimSpace(value)
		if value == "" || containsStringValue(used, value) || containsStringValue(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func findFieldByName(fields []string, aliases ...string) string {
	for _, alias := range aliases {
		needle := strings.ToLower(alias)
		for _, field := range fields {
			lower := strings.ToLower(field)
			if lower == needle || strings.Contains(lower, needle) {
				return field
			}
		}
	}
	return ""
}

func inferFiveNumberFields(metrics []string) []string {
	minField := findFieldByName(metrics, "min", "minimum", "最小")
	q1Field := findFieldByName(metrics, "q1", "p25", "quantile_25", "lower_quartile", "第一四分位")
	medianField := findFieldByName(metrics, "median", "p50", "quantile_50", "中位")
	q3Field := findFieldByName(metrics, "q3", "p75", "quantile_75", "upper_quartile", "第三四分位")
	maxField := findFieldByName(metrics, "max", "maximum", "最大")
	if minField == "" || q1Field == "" || medianField == "" || q3Field == "" || maxField == "" {
		return nil
	}
	return []string{minField, q1Field, medianField, q3Field, maxField}
}

func structuredChartID(chartType string, spec map[string]interface{}) string {
	payload := fmt.Sprintf("%s|%v|%v|%v|%v",
		chartType,
		spec["x"],
		spec["y"],
		spec["group"],
		spec["secondary_y"],
	)
	sum := sha1.Sum([]byte(payload))
	return fmt.Sprintf("chart_%x", sum[:5])
}

func containsStringValue(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func resultColumnLooksNumeric(rows []map[string]string, name string) bool {
	for _, row := range rows {
		v := strings.TrimSpace(row[name])
		if v == "" || strings.EqualFold(v, "<nil>") {
			continue
		}
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return true
		}
		return false
	}
	return false
}

// TableSchema represents the schema information of a table
type TableSchema struct {
	TableName string                 `json:"table_name"`
	Columns   []ColumnInfo           `json:"columns"`
	RowCount  int64                  `json:"row_count"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// ColumnInfo represents information about a single column
type ColumnInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable string `json:"nullable"`
}

// LoadFromCSV loads data from a CSV file into a DuckDB table and returns the table schema
// Parameters:
//   - ctx: context for cancellation and timeout
//   - filename: path to the CSV file
//   - tableName: name of the table to create
//
// Returns:
//   - *TableSchema: schema information of the created table
//   - error: any error that occurred during the operation
func (t *DataAnalysisTool) LoadFromCSV(ctx context.Context, filename string, tableName string) (*TableSchema, error) {
	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Loading CSV file '%s' into table '%s' for session %s", filename, tableName, t.sessionID)
	cellTableName := cellEvidenceTableName(tableName)

	// Record the created table for cleanup. If already exists, skip creation
	if t.recordCreatedTable(tableName) {
		csvPath, cleanup, encodingName, err := normalizeCSVFileForDuckDB(ctx, filename)
		if err != nil {
			logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to prepare CSV for DuckDB: %v", err)
			return nil, fmt.Errorf("failed to prepare CSV for DuckDB: %w", err)
		}
		defer cleanup()

		// Create table from CSV using DuckDB's read_csv_auto function
		// with explicit header detection and VARCHAR coercion to align with
		// Excel loading behavior.
		// Table will be created in the session schema
		createTableSQL := fmt.Sprintf(
			"CREATE TABLE \"%s\" AS SELECT * FROM read_csv_auto('%s', header=true, all_varchar=true)",
			tableName, sqlSingleQuoteEscape(csvPath),
		)

		_, err = t.db.ExecContext(ctx, createTableSQL)
		if err != nil {
			logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to create table from CSV: %v", err)
			return nil, fmt.Errorf("failed to create table from CSV: %w", err)
		}

		logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Successfully created table '%s' from CSV file in session %s (encoding=%s)", tableName, t.sessionID, encodingName)

		if t.recordCreatedTable(cellTableName) {
			if rows, err := createCSVCellEvidenceTable(ctx, t.db, csvPath, cellTableName); err != nil {
				logger.Warnf(ctx,
					"[Tool][LegacyDataAnalysis] Failed to create CSV cell evidence table '%s' from '%s' (session=%s): %v",
					cellTableName, csvPath, t.sessionID, err,
				)
			} else {
				logger.Infof(ctx,
					"[Tool][LegacyDataAnalysis] Successfully created CSV cell evidence table '%s' with %d cells for session %s",
					cellTableName, rows, t.sessionID,
				)
			}
		}
	}

	// Get and return the table schema
	schema, err := t.LoadFromTable(ctx, tableName)
	if err != nil {
		return nil, err
	}
	t.attachCellEvidenceTableMetadata(ctx, schema, cellTableName, "CSV cell evidence table")
	return schema, nil
}

func normalizeCSVFileForDuckDB(ctx context.Context, filename string) (string, func(), string, error) {
	noop := func() {}

	data, err := os.ReadFile(filename)
	if err != nil {
		return "", noop, "", fmt.Errorf("failed to read CSV file: %w", err)
	}

	decoded, encodingName := textencoding.DecodeTextBytesWithEncoding(data)
	switch encodingName {
	case "empty", "utf-8", "legacy-raw":
		return filename, noop, encodingName, nil
	}

	tmp, err := os.CreateTemp("", "weknora-tabular-sql-utf8-*.csv")
	if err != nil {
		return "", noop, encodingName, fmt.Errorf("failed to create normalized CSV temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
			logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Failed to remove normalized CSV temp file %s: %v", tmpPath, err)
		}
	}

	if _, err := tmp.Write(decoded); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", noop, encodingName, fmt.Errorf("failed to write normalized CSV temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", noop, encodingName, fmt.Errorf("failed to finalize normalized CSV temp file: %w", err)
	}

	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Normalized CSV '%s' from %s to UTF-8 temp file %s",
		filename, encodingName, tmpPath)

	return tmpPath, cleanup, encodingName, nil
}

// LoadFromExcel loads data from an Excel file into a DuckDB table and returns the table schema.
//
// Multi-sheet workbooks are fully supported: every sheet in the workbook is
// loaded and the rows from all sheets are unioned (UNION ALL BY NAME) into a
// single table. A synthetic '__sheet_name' column is added so downstream SQL
// can filter / aggregate per sheet. If sheet enumeration fails for any
// reason, we fall back to reading just the first sheet (original behavior).
//
// Parameters:
//   - ctx: context for cancellation and timeout
//   - filename: path to the Excel file
//   - tableName: name of the table to create
//
// Returns:
//   - *TableSchema: schema information of the created table
//   - error: any error that occurred during the operation
//
// Note: requires the DuckDB 'excel' extension (for read_xlsx) and the
// 'spatial' extension (for st_read_meta used to enumerate sheets).
func (t *DataAnalysisTool) LoadFromExcel(ctx context.Context, filename string, tableName string) (*TableSchema, error) {
	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Loading Excel file '%s' into table '%s' for session %s", filename, tableName, t.sessionID)
	cellTableName := cellEvidenceTableName(tableName)

	// Record the created table for cleanup. If already exists, skip creation.
	if t.recordCreatedTable(tableName) {
		sheetNames, enumErr := t.listExcelSheets(ctx, filename)
		if enumErr != nil {
			logger.Warnf(ctx,
				"[Tool][LegacyDataAnalysis] Could not enumerate sheets for '%s' (session=%s): %v. Falling back to first sheet only.",
				filename, t.sessionID, enumErr,
			)
		}

		if t.recordCreatedTable(cellTableName) {
			if rows, err := createExcelCellEvidenceTable(ctx, t.db, filename, cellTableName, sheetNames); err != nil {
				logger.Warnf(ctx,
					"[Tool][LegacyDataAnalysis] Failed to create Excel cell evidence table '%s' from '%s' (sheets=%v, session=%s): %v",
					cellTableName, filename, sheetNames, t.sessionID, err,
				)
			} else {
				logger.Infof(ctx,
					"[Tool][LegacyDataAnalysis] Successfully created Excel cell evidence table '%s' with %d cells for session %s (sheets=%v)",
					cellTableName, rows, t.sessionID, sheetNames,
				)
			}
		}

		createTableSQL := buildExcelCreateTableSQL(tableName, filename, sheetNames)

		if _, err := t.db.ExecContext(ctx, createTableSQL); err != nil {
			logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Failed to create structured table from Excel (sheets=%v): %v; falling back to cell evidence table", sheetNames, err)
			fallbackSQL := fmt.Sprintf("CREATE TABLE %s AS SELECT * FROM %s", quoteDuckDBIdentifier(tableName), quoteDuckDBIdentifier(cellTableName))
			if _, fallbackErr := t.db.ExecContext(ctx, fallbackSQL); fallbackErr != nil {
				logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to create table from Excel and fallback cell table (sheets=%v): %v; fallback=%v", sheetNames, err, fallbackErr)
				return nil, fmt.Errorf("failed to create table from Excel file (sheets=%v): %w; fallback cell table failed: %v", sheetNames, err, fallbackErr)
			}
			logger.Infof(ctx,
				"[Tool][LegacyDataAnalysis] Created fallback table '%s' from cell evidence table '%s' for session %s",
				tableName, cellTableName, t.sessionID,
			)
		} else {
			logger.Infof(ctx,
				"[Tool][LegacyDataAnalysis] Successfully created table '%s' from Excel file in session %s (sheets=%v)",
				tableName, t.sessionID, sheetNames,
			)
		}
	}

	schema, err := t.LoadFromTable(ctx, tableName)
	if err != nil {
		return nil, err
	}
	t.attachCellEvidenceTableMetadata(ctx, schema, cellTableName, "Excel cell evidence table")
	return schema, nil
}

// listExcelSheets returns the names of every sheet (layer) inside the given
// Excel workbook by querying DuckDB's spatial st_read_meta table function.
// The returned slice preserves the on-disk order of sheets.
//
// st_read_meta returns a single row whose `layers` column is a LIST of
// STRUCTs (one per layer / sheet). We UNNEST that list and project the
// struct's `name` field to get a flat list of sheet names.
func (t *DataAnalysisTool) listExcelSheets(ctx context.Context, filename string) ([]string, error) {
	metaSQL := fmt.Sprintf(
		"SELECT UNNEST(layers).name FROM st_read_meta('%s')",
		sqlSingleQuoteEscape(filename),
	)

	rows, err := t.db.QueryContext(ctx, metaSQL)
	if err != nil {
		sheetNames, fallbackErr := listExcelSheetsWithExcelize(filename)
		if fallbackErr == nil && len(sheetNames) > 0 {
			return sheetNames, nil
		}
		return nil, fmt.Errorf("failed to query sheet metadata: %w; excelize fallback: %v", err, fallbackErr)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan sheet name: %w", err)
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating sheet metadata rows: %w", err)
	}
	if len(names) == 0 {
		sheetNames, fallbackErr := listExcelSheetsWithExcelize(filename)
		if fallbackErr == nil && len(sheetNames) > 0 {
			return sheetNames, nil
		}
	}
	return names, nil
}

func listExcelSheetsWithExcelize(filename string) ([]string, error) {
	workbook, err := excelize.OpenFile(filename)
	if err != nil {
		return nil, err
	}
	defer workbook.Close()

	rawNames := workbook.GetSheetList()
	names := make([]string, 0, len(rawNames))
	for _, name := range rawNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// buildExcelCreateTableSQL assembles the CREATE TABLE statement used by
// LoadFromExcel. Exposed at package level (lower-case) to make it trivially
// testable without a live DuckDB connection.
func buildExcelCreateTableSQL(tableName, filename string, sheetNames []string) string {
	escFile := sqlSingleQuoteEscape(filename)

	// No sheet info (enumeration failed or empty): read the first sheet only.
	if len(sheetNames) == 0 {
		return fmt.Sprintf(
			"CREATE TABLE \"%s\" AS SELECT * FROM read_xlsx('%s', header=true, all_varchar=true)",
			tableName, escFile,
		)
	}

	// Single sheet: keep it simple but still tag the source for consistency
	// with the multi-sheet path.
	if len(sheetNames) == 1 {
		escSheet := sqlSingleQuoteEscape(sheetNames[0])
		return fmt.Sprintf(
			"CREATE TABLE \"%s\" AS SELECT *, '%s' AS %s FROM read_xlsx('%s', sheet = '%s', header=true, all_varchar=true)",
			tableName, escSheet, excelSheetNameColumn, escFile, escSheet,
		)
	}

	// Multiple sheets: UNION ALL BY NAME tolerates schema differences
	// between sheets (missing columns become NULL, conflicting types are
	// widened).
	parts := make([]string, 0, len(sheetNames))
	for _, sheet := range sheetNames {
		escSheet := sqlSingleQuoteEscape(sheet)
		parts = append(parts, fmt.Sprintf(
			"SELECT *, '%s' AS %s FROM read_xlsx('%s', sheet = '%s', header=true, all_varchar=true)",
			escSheet, excelSheetNameColumn, escFile, escSheet,
		))
	}
	return fmt.Sprintf(
		"CREATE TABLE \"%s\" AS %s",
		tableName,
		strings.Join(parts, "\nUNION ALL BY NAME\n"),
	)
}

func buildExcelRawCreateTableSQL(tableName, filename string, sheetNames []string) string {
	escFile := sqlSingleQuoteEscape(filename)

	if len(sheetNames) == 0 {
		return fmt.Sprintf(
			"CREATE TABLE \"%s\" AS SELECT * FROM read_xlsx('%s', header=false, all_varchar=true)",
			tableName, escFile,
		)
	}

	if len(sheetNames) == 1 {
		escSheet := sqlSingleQuoteEscape(sheetNames[0])
		return fmt.Sprintf(
			"CREATE TABLE \"%s\" AS SELECT *, '%s' AS %s FROM read_xlsx('%s', sheet = '%s', header=false, all_varchar=true)",
			tableName, escSheet, excelSheetNameColumn, escFile, escSheet,
		)
	}

	parts := make([]string, 0, len(sheetNames))
	for _, sheet := range sheetNames {
		escSheet := sqlSingleQuoteEscape(sheet)
		parts = append(parts, fmt.Sprintf(
			"SELECT *, '%s' AS %s FROM read_xlsx('%s', sheet = '%s', header=false, all_varchar=true)",
			escSheet, excelSheetNameColumn, escFile, escSheet,
		))
	}
	return fmt.Sprintf(
		"CREATE TABLE \"%s\" AS %s",
		tableName,
		strings.Join(parts, "\nUNION ALL BY NAME\n"),
	)
}

func (t *DataAnalysisTool) attachCellEvidenceTableMetadata(ctx context.Context, schema *TableSchema, cellTableName string, label string) {
	if schema == nil || strings.TrimSpace(cellTableName) == "" {
		return
	}
	cellSchema, err := t.LoadFromTable(ctx, cellTableName)
	if err != nil {
		logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Cell evidence table '%s' is not available for session %s: %v", cellTableName, t.sessionID, err)
		return
	}
	if schema.Metadata == nil {
		schema.Metadata = map[string]interface{}{}
	}
	if strings.TrimSpace(label) == "" {
		label = "Original file cell evidence table"
	}
	schema.Metadata["cell_table_name"] = cellSchema.TableName
	schema.Metadata["cell_table_label"] = label
	schema.Metadata["cell_column_count"] = len(cellSchema.Columns)
	schema.Metadata["cell_row_count"] = cellSchema.RowCount
	cellColumns := make([]map[string]interface{}, 0, len(cellSchema.Columns))
	for _, col := range cellSchema.Columns {
		cellColumns = append(cellColumns, map[string]interface{}{"name": col.Name, "type": col.Type})
	}
	schema.Metadata["cell_columns"] = cellColumns
	schema.Metadata["cell_usage"] = "Use cell_table_name as the authoritative original-file evidence table for irregular layouts, merged cells, decorative headers, cross-tab data, section headings, or when the structured table appears incomplete. The table has one row per non-empty or merged cell with sheet_name, row_number, column_number, cell_ref, value, effective_value, and merged_range."
	// Keep the legacy metadata key as an alias so older references still point
	// at the faithful cell evidence table rather than DuckDB's inferred raw rows.
	schema.Metadata["raw_table_name"] = cellSchema.TableName
	schema.Metadata["raw_column_count"] = len(cellSchema.Columns)
	schema.Metadata["raw_row_count"] = cellSchema.RowCount
	schema.Metadata["raw_columns"] = cellColumns
	schema.Metadata["raw_usage"] = schema.Metadata["cell_usage"]
}

// LoadFromKnowledge loads data from a Knowledge entity into a DuckDB table and returns the table schema.
// It automatically determines the file type and calls the appropriate loading method.
//
// The source file is first materialized to a local temp file via FileService.GetFile
// so DuckDB's st_read / read_xlsx / read_csv_auto can open it directly. This
// side-steps provider-specific URL schemes (e.g. the local:// URL returned by
// the local file service) that DuckDB's extensions cannot resolve on their own.
//
// Parameters:
//   - ctx: context for cancellation and timeout
//   - knowledge: the Knowledge entity containing file information
//
// Returns:
//   - *TableSchema: schema information of the created table
//   - error: any error that occurred during the operation
func (t *DataAnalysisTool) LoadFromKnowledge(ctx context.Context, knowledge *types.Knowledge) (*TableSchema, error) {
	if knowledge == nil {
		return nil, fmt.Errorf("knowledge cannot be nil")
	}
	tableName := t.TableName(knowledge)

	// Normalize file type to lowercase for comparison
	fileType := strings.ToLower(knowledge.FileType)

	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Loading knowledge '%s' (type: %s) into table '%s' for session %s",
		knowledge.ID, fileType, tableName, t.sessionID)

	localPath, cleanup, err := t.materializeKnowledgeFile(ctx, knowledge)
	if err != nil {
		return nil, fmt.Errorf("failed to materialize knowledge '%s' for DuckDB: %w", knowledge.ID, err)
	}
	defer cleanup()

	switch fileType {
	case "csv":
		return t.LoadFromCSV(ctx, localPath, tableName)
	case "xlsx", "xls":
		return t.LoadFromExcel(ctx, localPath, tableName)
	default:
		logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Unsupported file type '%s' for knowledge '%s' in session %s",
			fileType, knowledge.ID, t.sessionID)
		return nil, fmt.Errorf("unsupported file type: %s (supported types: csv, xlsx, xls)", fileType)
	}
}

// materializeKnowledgeFile copies the knowledge's backing blob into a fresh
// temp file on the local filesystem so DuckDB can open it with ordinary path
// semantics. It returns the temp path and a cleanup closure that removes the
// temp file; the closure is always safe to call and is a no-op on failure.
//
// This hides storage-backend-specific URL schemes (local://, oss://, s3://,
// minio://, cos://, …) behind the FileService.GetFile abstraction, so the
// Data Analysis tool works identically across all deployments.
func (t *DataAnalysisTool) materializeKnowledgeFile(ctx context.Context, knowledge *types.Knowledge) (string, func(), error) {
	noop := func() {}

	reader, err := t.resolveFileServiceForKnowledge(ctx, knowledge).GetFile(ctx, knowledge.FilePath)
	if err != nil {
		return "", noop, fmt.Errorf("failed to open file for knowledge '%s': %w", knowledge.ID, err)
	}
	defer reader.Close()

	// Preserve the file extension so DuckDB's format auto-detection still
	// works (e.g. the CSV reader expects .csv, xlsx reader expects .xlsx).
	suffix := ""
	if ext := strings.ToLower(strings.TrimSpace(knowledge.FileType)); ext != "" {
		suffix = "." + ext
	}

	tmp, err := os.CreateTemp("", "weknora-tabular-sql-*"+suffix)
	if err != nil {
		return "", noop, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		// Best-effort cleanup; a missing file is fine, any other error is
		// only logged to avoid masking the original operation's result.
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
			logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Failed to remove temp file %s: %v", tmpPath, err)
		}
	}

	if _, err := io.Copy(tmp, reader); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", noop, fmt.Errorf("failed to copy knowledge '%s' to temp file: %w", knowledge.ID, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("failed to finalize temp file for knowledge '%s': %w", knowledge.ID, err)
	}

	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Materialized knowledge '%s' to temp file %s for session %s",
		knowledge.ID, tmpPath, t.sessionID)

	return tmpPath, cleanup, nil
}

func (t *DataAnalysisTool) runtimeAttachmentByID(attachmentID string) (types.MessageAttachment, bool) {
	index, ok := types.ParseRuntimeAttachmentID(attachmentID)
	if !ok || index < 0 || index >= len(t.runtimeAttachments) {
		return types.MessageAttachment{}, false
	}
	att := t.runtimeAttachments[index]
	if !types.IsTabularFileType(att.FileType) {
		return types.MessageAttachment{}, false
	}
	return att, true
}

func (t *DataAnalysisTool) materializeRuntimeAttachmentFile(ctx context.Context, attachmentID string, att types.MessageAttachment) (string, func(), error) {
	noop := func() {}
	if t.fileService == nil {
		return "", noop, fmt.Errorf("file service is not available")
	}
	if strings.TrimSpace(att.URL) == "" {
		return "", noop, fmt.Errorf("attachment '%s' has no stored file URL", attachmentID)
	}

	reader, err := t.fileService.GetFile(ctx, att.URL)
	if err != nil {
		return "", noop, fmt.Errorf("failed to open uploaded attachment '%s': %w", attachmentID, err)
	}
	defer reader.Close()

	suffix := ""
	if ext := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(att.FileType)), "."); ext != "" {
		suffix = "." + ext
	}
	tmp, err := os.CreateTemp("", "weknora-tabular-attachment-*"+suffix)
	if err != nil {
		return "", noop, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
			logger.Warnf(ctx, "[Tool][LegacyDataAnalysis] Failed to remove attachment temp file %s: %v", tmpPath, err)
		}
	}
	if _, err := io.Copy(tmp, reader); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", noop, fmt.Errorf("failed to copy uploaded attachment '%s' to temp file: %w", attachmentID, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("failed to finalize temp file for uploaded attachment '%s': %w", attachmentID, err)
	}
	return tmpPath, cleanup, nil
}

func (t *DataAnalysisTool) LoadFromRuntimeAttachment(ctx context.Context, attachmentID string, att types.MessageAttachment) (*TableSchema, error) {
	tableName := runtimeAttachmentTableName(attachmentID)
	fileType := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(att.FileType)), ".")
	localPath, cleanup, err := t.materializeRuntimeAttachmentFile(ctx, attachmentID, att)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	switch fileType {
	case "csv":
		return t.LoadFromCSV(ctx, localPath, tableName)
	case "xlsx", "xls":
		return t.LoadFromExcel(ctx, localPath, tableName)
	default:
		return nil, fmt.Errorf("unsupported uploaded attachment file type: %s (supported types: csv, xlsx, xls)", fileType)
	}
}

func cloneTableSchema(schema *TableSchema) *TableSchema {
	if schema == nil {
		return nil
	}
	cloned := *schema
	cloned.Columns = append([]ColumnInfo(nil), schema.Columns...)
	if schema.Metadata != nil {
		cloned.Metadata = make(map[string]interface{}, len(schema.Metadata))
		for key, value := range schema.Metadata {
			cloned.Metadata[key] = value
		}
	}
	return &cloned
}

// LoadFromKnowledgeID loads data from a Knowledge ID into a DuckDB table and returns the table schema
// Parameters:
//   - ctx: context for cancellation and timeout
//   - knowledgeID: the ID of the Knowledge entity
//
// Returns:
//   - string: the name of the created table
//   - *TableSchema: schema information of the created table
//   - error: any error that occurred during the operation
func (t *DataAnalysisTool) LoadFromKnowledgeID(ctx context.Context, knowledgeID string) (*TableSchema, error) {
	if t.isTableAnalysisTool() {
		if t.loadedSchemas == nil {
			t.loadedSchemas = make(map[string]*TableSchema)
		}
		if cached, ok := t.loadedSchemas[knowledgeID]; ok {
			logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Reusing cached table schema for knowledge ID '%s' in session %s", knowledgeID, t.sessionID)
			return cloneTableSchema(cached), nil
		}
	}

	var schema *TableSchema
	var err error
	if att, ok := t.runtimeAttachmentByID(knowledgeID); ok {
		schema, err = t.LoadFromRuntimeAttachment(ctx, knowledgeID, att)
		if err != nil {
			return nil, err
		}
		if t.isTableAnalysisTool() {
			t.loadedSchemas[knowledgeID] = cloneTableSchema(schema)
		}
		return schema, nil
	}

	// Use GetKnowledgeByIDOnly to support cross-tenant shared KB
	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to get knowledge by ID '%s': %v", knowledgeID, err)
		return nil, fmt.Errorf("failed to get knowledge by ID: %w", err)
	}

	schema, err = t.LoadFromKnowledge(ctx, knowledge)
	if err != nil {
		return nil, err
	}
	if t.isTableAnalysisTool() {
		t.loadedSchemas[knowledgeID] = cloneTableSchema(schema)
	}
	return schema, nil
}

// LoadFromTable retrieves the schema information of an existing table
// Parameters:
//   - ctx: context for cancellation and timeout
//   - tableName: name of the table to query
//
// Returns:
//   - *TableSchema: schema information of the table
//   - error: any error that occurred during the operation
//
// Note: This function does NOT create the table, it only retrieves schema information
func (t *DataAnalysisTool) LoadFromTable(ctx context.Context, tableName string) (*TableSchema, error) {
	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Getting schema for table '%s' in session %s", tableName, t.sessionID)

	// Query to get column information using PRAGMA table_info or DESCRIBE
	schemaSQL := fmt.Sprintf("DESCRIBE \"%s\"", tableName)

	rows, err := t.db.QueryContext(ctx, schemaSQL)
	if err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to get table schema: %v", err)
		return nil, fmt.Errorf("failed to get table schema: %w", err)
	}
	defer rows.Close()

	// Parse column information
	columns := make([]ColumnInfo, 0)
	for rows.Next() {
		var colName, colType, nullable string
		var extra1, extra2, extra3 interface{} // DuckDB DESCRIBE may return additional columns

		// Try to scan with different column counts
		err := rows.Scan(&colName, &colType, &nullable, &extra1, &extra2, &extra3)
		if err != nil {
			// Try with fewer columns
			err = rows.Scan(&colName, &colType, &nullable)
			if err != nil {
				logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to scan column info: %v", err)
				return nil, fmt.Errorf("failed to scan column info: %w", err)
			}
		}

		columns = append(columns, ColumnInfo{
			Name:     colName,
			Type:     colType,
			Nullable: nullable,
		})
	}

	if err := rows.Err(); err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Error iterating schema rows: %v", err)
		return nil, fmt.Errorf("error iterating schema rows: %w", err)
	}

	// Get row count
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM \"%s\"", tableName)
	var rowCount int64
	if err := t.db.QueryRowContext(ctx, countSQL).Scan(&rowCount); err != nil {
		logger.Errorf(ctx, "[Tool][LegacyDataAnalysis] Failed to get row count: %v", err)
		return nil, fmt.Errorf("failed to get row count: %w", err)
	}

	schema := &TableSchema{
		TableName: tableName,
		Columns:   columns,
		RowCount:  rowCount,
		Metadata: map[string]interface{}{
			"column_count": len(columns),
			"session_id":   t.sessionID,
		},
	}

	logger.Infof(ctx, "[Tool][LegacyDataAnalysis] Retrieved schema for table '%s' in session %s: %d columns, %d rows",
		tableName, t.sessionID, len(columns), rowCount)

	return schema, nil
}

func (t *DataAnalysisTool) TableName(knowledge *types.Knowledge) string {
	return "k_" + strings.ReplaceAll(knowledge.ID, "-", "_")
}

func allowedTableAnalysisTables(schema *TableSchema) []string {
	if schema == nil {
		return nil
	}
	out := []string{schema.TableName}
	if cellName := metadataString(schema.Metadata, "cell_table_name"); cellName != "" && cellName != schema.TableName {
		out = append(out, cellName)
	}
	if rawName := metadataString(schema.Metadata, "raw_table_name"); rawName != "" && rawName != schema.TableName {
		if !containsStringValue(out, rawName) {
			out = append(out, rawName)
		}
	}
	return out
}

func runtimeAttachmentTableName(attachmentID string) string {
	index, ok := types.ParseRuntimeAttachmentID(attachmentID)
	if !ok {
		return "attachment_unknown"
	}
	return fmt.Sprintf("attachment_%d", index+1)
}

func rawExcelTableName(tableName string) string {
	return tableName + excelRawTableSuffix
}

func metadataString(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if strings.EqualFold(text, "<nil>") {
		return ""
	}
	return text
}

// buildSchemaDescription builds a formatted schema description
func (t *TableSchema) Description() string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Table name: %s\n", t.TableName))
	builder.WriteString(fmt.Sprintf("Columns: %d\n", len(t.Columns)))
	builder.WriteString(fmt.Sprintf("Rows: %d\n\n", t.RowCount))
	builder.WriteString("Column info:\n")

	for _, col := range t.Columns {
		builder.WriteString(fmt.Sprintf("- %s (%s)\n", col.Name, col.Type))
	}

	if cellName := metadataString(t.Metadata, "cell_table_name"); cellName != "" {
		builder.WriteString("\nOriginal file cell evidence table:\n")
		builder.WriteString(fmt.Sprintf("- Table name: %s\n", cellName))
		builder.WriteString(fmt.Sprintf("- Columns: %v\n", t.Metadata["cell_column_count"]))
		builder.WriteString(fmt.Sprintf("- Rows: %v\n", t.Metadata["cell_row_count"]))
		builder.WriteString("- This is the authoritative source for irregular CSV/Excel understanding. It contains source_kind, sheet_name, row_number, column_number, column_letter, cell_ref, value, effective_value, merged_range, is_merged, and is_blank.\n")
		builder.WriteString("- Use this table to inspect original cells, section headings, merged cells, and non-rectangular layouts before constructing a normalized analysis result.\n")
		builder.WriteString("- When using table_analysis for chart output, include source_mapping JSON that maps result fields and rows back to this evidence table or the original file fields.\n")
		if cellColumns, ok := t.Metadata["cell_columns"].([]map[string]interface{}); ok && len(cellColumns) > 0 {
			builder.WriteString("- Cell evidence column info:\n")
			for _, col := range cellColumns {
				builder.WriteString(fmt.Sprintf("  - %s (%s)\n", col["name"], col["type"]))
			}
		}
	}

	return builder.String()
}

// resolveFileServiceForKnowledge resolves a provider-specific FileService based on the knowledge file path.
// It falls back to the injected default service when provider/config cannot be resolved.
func (t *DataAnalysisTool) resolveFileServiceForKnowledge(ctx context.Context, knowledge *types.Knowledge) interfaces.FileService {
	if knowledge == nil {
		logger.Warnf(ctx, "[Tool][LegacyDataAnalysis][storage] fallback default: session_id=%s reason=knowledge_nil", t.sessionID)
		return t.fileService
	}

	kbID := strings.TrimSpace(knowledge.KnowledgeBaseID)
	var kb *types.KnowledgeBase
	if t.knowledgeBaseService != nil && kbID != "" {
		var err error
		kb, err = t.knowledgeBaseService.GetKnowledgeBaseByID(ctx, kbID)
		if err != nil {
			logger.Warnf(ctx, "[Tool][LegacyDataAnalysis][storage] get kb failed, fallback default: session_id=%s knowledge_id=%s kb_id=%s err=%v",
				t.sessionID, knowledge.ID, kbID, err)
			return t.fileService
		}
	}
	if kb == nil && kbID != "" {
		logger.Infof(ctx, "[Tool][LegacyDataAnalysis][storage] kb not found, fallback default: session_id=%s knowledge_id=%s kb_id=%s",
			t.sessionID, knowledge.ID, kbID)
		return t.fileService
	}

	provider := ""
	if kb != nil {
		provider = kb.GetStorageProvider()
	}
	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	if tenant == nil {
		tenantID := uint64(0)
		if tid, ok := ctx.Value(types.TenantIDContextKey).(uint64); ok {
			tenantID = tid
		}
		if tenantID == 0 && kb != nil {
			tenantID = knowledge.TenantID
		}
		if tenantID > 0 && t.tenantService != nil {
			resolvedTenant, err := t.tenantService.GetTenantByID(ctx, tenantID)
			if err != nil {
				logger.Warnf(ctx, "[Tool][LegacyDataAnalysis][storage] get tenant failed: session_id=%s knowledge_id=%s kb_id=%s tenant_id=%d err=%v",
					t.sessionID, knowledge.ID, kbID, tenantID, err)
			} else if resolvedTenant != nil {
				tenant = resolvedTenant
				logger.Infof(ctx, "[Tool][LegacyDataAnalysis][storage] resolved tenant from service: session_id=%s knowledge_id=%s kb_id=%s tenant_id=%d",
					t.sessionID, knowledge.ID, kbID, tenantID)
			}
		}
	}
	if provider == "" && tenant != nil && tenant.StorageEngineConfig != nil {
		provider = strings.ToLower(strings.TrimSpace(tenant.StorageEngineConfig.DefaultProvider))
	}

	if provider == "" || tenant == nil || tenant.StorageEngineConfig == nil {
		hasTenantStorageConfig := tenant != nil && tenant.StorageEngineConfig != nil
		logger.Infof(ctx, "[Tool][LegacyDataAnalysis][storage] fallback default: session_id=%s knowledge_id=%s kb_id=%s provider=%q tenant_cfg=%t",
			t.sessionID, knowledge.ID, kbID, provider, hasTenantStorageConfig)
		return t.fileService
	}

	storageConfig := tenant.StorageEngineConfig
	// Use the localBaseDir captured at construction time rather than re-reading
	// LOCAL_STORAGE_BASE_DIR from os.Getenv here.  Reading the env var at
	// request-handling time can produce an empty string (or the wrong value)
	// when the variable was set programmatically before startup or is absent
	// from the process environment of the DI-constructed sub-component, causing
	// the newly created local FileService to use the /data/files fallback
	// instead of the configured path and therefore fail to locate files (#1040).
	baseDir := t.localBaseDir

	resolvedSvc, resolvedProvider, err := filesvc.NewFileServiceFromStorageConfig(provider, storageConfig, baseDir)
	if err != nil {
		logger.Warnf(ctx, "[Tool][LegacyDataAnalysis][storage] create file service failed, fallback default: session_id=%s knowledge_id=%s kb_id=%s provider=%s err=%v",
			t.sessionID, knowledge.ID, kbID, provider, err)
		return t.fileService
	}

	logger.Infof(ctx, "[Tool][LegacyDataAnalysis][storage] resolved file service: session_id=%s knowledge_id=%s kb_id=%s provider=%s",
		t.sessionID, knowledge.ID, kbID, resolvedProvider)
	return resolvedSvc
}
