package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/xuri/excelize/v2"
)

func TestBuildExcelCreateTableSQL_NoSheets(t *testing.T) {
	got := buildExcelCreateTableSQL("tbl", "/tmp/data.xlsx", nil)
	want := `CREATE TABLE "tbl" AS SELECT * FROM read_xlsx('/tmp/data.xlsx', header=true, all_varchar=true)`
	if got != want {
		t.Fatalf("mismatch.\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildExcelCreateTableSQL_SingleSheetTagsSource(t *testing.T) {
	got := buildExcelCreateTableSQL("tbl", "/tmp/data.xlsx", []string{"Sheet1"})

	// Must use read_xlsx (excel extension) with explicit sheet param.
	if !strings.Contains(got, "FROM read_xlsx('/tmp/data.xlsx', sheet = 'Sheet1', header=true, all_varchar=true)") {
		t.Fatalf("expected read_xlsx with sheet param, got: %s", got)
	}
	// Must tag the source sheet name via the synthetic column so downstream
	// SQL behaves consistently between single- and multi-sheet workbooks.
	if !strings.Contains(got, "'Sheet1' AS "+excelSheetNameColumn) {
		t.Fatalf("expected sheet-name column, got: %s", got)
	}
}

func TestBuildExcelCreateTableSQL_MultiSheetUsesUnionAllByName(t *testing.T) {
	got := buildExcelCreateTableSQL("tbl", "/tmp/data.xlsx", []string{"Sheet1", "Sheet2", "报表"})

	// Each sheet must appear as a SELECT reading that specific sheet, and
	// the __sheet_name column must carry its name for per-sheet filtering.
	for _, sheet := range []string{"Sheet1", "Sheet2", "报表"} {
		needleRead := "FROM read_xlsx('/tmp/data.xlsx', sheet = '" + sheet + "', header=true, all_varchar=true)"
		needleTag := "'" + sheet + "' AS " + excelSheetNameColumn
		if !strings.Contains(got, needleRead) {
			t.Fatalf("missing read_xlsx for sheet %q in:\n%s", sheet, got)
		}
		if !strings.Contains(got, needleTag) {
			t.Fatalf("missing __sheet_name tag for sheet %q in:\n%s", sheet, got)
		}
	}

	// Must combine with UNION ALL BY NAME so schema drift between sheets is
	// tolerated.
	if !strings.Contains(got, "UNION ALL BY NAME") {
		t.Fatalf("expected UNION ALL BY NAME in multi-sheet SQL, got:\n%s", got)
	}

	// Exactly N-1 UNIONs for N sheets.
	if strings.Count(got, "UNION ALL BY NAME") != 2 {
		t.Fatalf("expected 2 UNION ALL BY NAME separators, got %d in:\n%s",
			strings.Count(got, "UNION ALL BY NAME"), got)
	}
}

func TestBuildExcelCreateTableSQL_EscapesSingleQuotes(t *testing.T) {
	// Sheet name and file path both contain single quotes, which must be
	// doubled to produce a valid SQL literal.
	sheets := []string{"Jo's data"}
	got := buildExcelCreateTableSQL("tbl", "/tmp/O'Brien/data.xlsx", sheets)

	if !strings.Contains(got, "sheet = 'Jo''s data'") {
		t.Fatalf("sheet name was not escaped, got:\n%s", got)
	}
	if !strings.Contains(got, "read_xlsx('/tmp/O''Brien/data.xlsx'") {
		t.Fatalf("file path was not escaped, got:\n%s", got)
	}
	if !strings.Contains(got, "'Jo''s data' AS "+excelSheetNameColumn) {
		t.Fatalf("sheet-name literal was not escaped, got:\n%s", got)
	}
}

func TestBuildExcelRawCreateTableSQLUsesHeaderFalse(t *testing.T) {
	got := buildExcelRawCreateTableSQL("tbl_raw", "/tmp/data.xlsx", []string{"Sheet1", "报表"})

	for _, sheet := range []string{"Sheet1", "报表"} {
		needleRead := "FROM read_xlsx('/tmp/data.xlsx', sheet = '" + sheet + "', header=false, all_varchar=true)"
		needleTag := "'" + sheet + "' AS " + excelSheetNameColumn
		if !strings.Contains(got, needleRead) {
			t.Fatalf("missing raw read_xlsx for sheet %q in:\n%s", sheet, got)
		}
		if !strings.Contains(got, needleTag) {
			t.Fatalf("missing raw __sheet_name tag for sheet %q in:\n%s", sheet, got)
		}
	}
	if !strings.Contains(got, "UNION ALL BY NAME") {
		t.Fatalf("expected UNION ALL BY NAME in raw multi-sheet SQL, got:\n%s", got)
	}
}

func TestListExcelSheetsWithExcelize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi-sheet.xlsx")
	workbook := excelize.NewFile()
	if err := workbook.SetSheetName("Sheet1", "收入"); err != nil {
		t.Fatal(err)
	}
	if _, err := workbook.NewSheet("成本"); err != nil {
		t.Fatal(err)
	}
	if err := workbook.SaveAs(path); err != nil {
		t.Fatal(err)
	}
	if err := workbook.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := listExcelSheetsWithExcelize(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "收入,成本" {
		t.Fatalf("unexpected sheet names: %#v", got)
	}
}

func TestSqlSingleQuoteEscape(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"no_quote":       "no_quote",
		"a'b":            "a''b",
		"''":             "''''",
		"mix'ed'quote":   "mix''ed''quote",
		"中文 with 'quote": "中文 with ''quote",
	}
	for in, want := range cases {
		if got := sqlSingleQuoteEscape(in); got != want {
			t.Errorf("sqlSingleQuoteEscape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTableSchemaDescriptionAndAllowedTablesIncludeRawExcelTable(t *testing.T) {
	schema := &TableSchema{
		TableName: "attachment_1",
		Columns: []ColumnInfo{
			{Name: "序列", Type: "VARCHAR"},
		},
		RowCount: 4,
		Metadata: map[string]interface{}{
			"raw_table_name":   "attachment_1__raw",
			"raw_column_count": 3,
			"raw_row_count":    int64(40),
			"raw_columns": []map[string]interface{}{
				{"name": "column0", "type": "VARCHAR"},
				{"name": "column1", "type": "VARCHAR"},
			},
		},
	}

	desc := schema.Description()
	if !strings.Contains(desc, "Raw Excel cell table") || !strings.Contains(desc, "attachment_1__raw") {
		t.Fatalf("raw table guidance missing from description:\n%s", desc)
	}
	allowed := allowedTableAnalysisTables(schema)
	if len(allowed) != 2 || allowed[0] != "attachment_1" || allowed[1] != "attachment_1__raw" {
		t.Fatalf("unexpected allowed tables: %#v", allowed)
	}
}

func TestTableAnalysisToolAllowsChartsOnlyForTableAnalysis(t *testing.T) {
	defaultTool := NewDataAnalysisTool(nil, nil, nil, nil, nil, "session-default")
	if defaultTool.allowChart {
		t.Fatal("default data_analysis tool should not allow chart output")
	}

	tableTool := NewTableAnalysisToolWithConfig(nil, nil, nil, nil, nil, "session-table", &types.AgentConfig{AgentType: types.AgentTypeTableAnalysis})
	if !tableTool.allowChart {
		t.Fatal("table_analysis tool should allow chart output")
	}
	if tableTool.Name() != ToolTableAnalysis {
		t.Fatalf("expected table tool name %q, got %q", ToolTableAnalysis, tableTool.Name())
	}

	dbAnalysisTool := NewDataAnalysisTool(nil, nil, nil, nil, nil, "session-db", types.AgentTypeDataAnalysis)
	if dbAnalysisTool.allowChart {
		t.Fatal("database data-analysis agent should keep using its own chart pipeline")
	}
}

func TestLimitTableAnalysisSQLWrapsQuery(t *testing.T) {
	got := limitTableAnalysisSQL(" SELECT id FROM attachment_1 ORDER BY id; ", 25)
	want := "SELECT * FROM (SELECT id FROM attachment_1 ORDER BY id) AS __weknora_table_analysis_limited LIMIT 25"
	if got != want {
		t.Fatalf("unexpected limited SQL.\n got: %s\nwant: %s", got, want)
	}
}

func TestTableAnalysisValidationOptionsAreTableOnly(t *testing.T) {
	defaultTool := NewDataAnalysisTool(nil, nil, nil, nil, nil, "session-default")
	_, defaultValidation := utils.ValidateSQL(
		"SELECT id FROM attachment_1 UNION ALL SELECT id FROM attachment_1__raw",
		defaultTool.tableAnalysisValidationOptions([]string{"attachment_1", "attachment_1__raw"})...,
	)
	if defaultValidation.Valid {
		t.Fatal("legacy data_analysis validation should not allow UNION ALL")
	}

	tableTool := NewTableAnalysisToolWithConfig(nil, nil, nil, nil, nil, "session-table", &types.AgentConfig{AgentType: types.AgentTypeTableAnalysis})
	_, tableValidation := utils.ValidateSQL(
		"SELECT id FROM attachment_1 UNION ALL SELECT id FROM attachment_1__raw",
		tableTool.tableAnalysisValidationOptions([]string{"attachment_1", "attachment_1__raw"})...,
	)
	if !tableValidation.Valid {
		t.Fatalf("table_analysis validation should allow bounded UNION ALL, got %#v", tableValidation.Errors)
	}

	_, hardcodedValidation := utils.ValidateSQL(
		"(SELECT '管理序列' AS seq, 14 AS child_count FROM attachment_1 WHERE \"__sheet_name\" = '岗位' LIMIT 1) UNION ALL (SELECT '职能序列', 23 FROM attachment_1 WHERE \"__sheet_name\" = '岗位' LIMIT 1)",
		tableTool.tableAnalysisValidationOptions([]string{"attachment_1", "attachment_1__raw"})...,
	)
	if hardcodedValidation.Valid {
		t.Fatal("table_analysis validation should reject hard-coded constant chart datasets even when each branch references a table")
	}
	if len(hardcodedValidation.Errors) == 0 || !strings.Contains(hardcodedValidation.Errors[0].Details, "constant-only") {
		t.Fatalf("expected constant-only grounding error, got %#v", hardcodedValidation.Errors)
	}
}

func TestInferStructuredChartSpecIncludesStableContract(t *testing.T) {
	columns := []map[string]interface{}{
		{"name": "month", "semantic_type": "time"},
		{"name": "sales", "semantic_type": "metric"},
	}
	rows := []map[string]string{
		{"month": "2026-01", "sales": "12.5"},
		{"month": "2026-02", "sales": "18.0"},
	}

	spec := inferStructuredChartSpec(columns, rows, "line", true)
	if spec["eligible"] != true {
		t.Fatalf("expected chart to be eligible, got %#v", spec)
	}
	if spec["type"] != "line" || spec["default_type"] != "line" {
		t.Fatalf("expected line chart type, got %#v", spec)
	}
	if spec["id"] == "" {
		t.Fatalf("expected stable chart id, got %#v", spec)
	}

	contract, ok := spec["contract"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected chart contract, got %#v", spec["contract"])
	}
	if contract["id"] != spec["id"] || contract["type"] != "line" {
		t.Fatalf("unexpected contract identity, got %#v", contract)
	}
	encoding, ok := contract["encoding"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected contract encoding, got %#v", contract["encoding"])
	}
	x, _ := encoding["x"].(map[string]interface{})
	value, _ := encoding["value"].(map[string]interface{})
	if x["field"] != "month" || value["field"] != "sales" {
		t.Fatalf("unexpected encoding, got %#v", encoding)
	}
}

func TestInferStructuredChartSpecUsesChartHintsAndEvidenceScope(t *testing.T) {
	columns := []map[string]interface{}{
		{"name": "序列名称", "semantic_type": "dimension"},
		{"name": "类型", "semantic_type": "dimension"},
		{"name": "数量", "semantic_type": "metric"},
		{"name": "备注", "semantic_type": "dimension"},
	}
	rows := []map[string]string{
		{"序列名称": "职能序列", "类型": "子序列", "数量": "14", "备注": "运营职能"},
		{"序列名称": "职能序列", "类型": "子类", "数量": "27", "备注": "运营职能"},
		{"序列名称": "技能序列", "类型": "子序列", "数量": "9", "备注": "生产流程"},
	}
	hints := tableChartIntentHints{
		intent:        "比较各序列子序列和子类数量",
		dimension:     "序列名称",
		series:        "类型",
		primaryMetric: "数量",
		title:         "各序列子序列与子类数量对比",
	}

	spec := inferStructuredChartSpec(columns, rows, "stacked_bar", true, hints)
	if spec["eligible"] != true {
		t.Fatalf("expected chart to be eligible, got %#v", spec)
	}
	if spec["type"] != "stacked_bar" {
		t.Fatalf("expected stacked_bar chart, got %#v", spec)
	}

	contract, ok := spec["contract"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected chart contract, got %#v", spec["contract"])
	}
	encoding, _ := contract["encoding"].(map[string]interface{})
	x, _ := encoding["x"].(map[string]interface{})
	series, _ := encoding["series"].(map[string]interface{})
	value, _ := encoding["value"].(map[string]interface{})
	if x["field"] != "序列名称" || series["field"] != "类型" || value["field"] != "数量" {
		t.Fatalf("unexpected encoding, got %#v", encoding)
	}

	visualScope, _ := contract["visual_scope"].(map[string]interface{})
	fields, _ := visualScope["fields"].([]string)
	if !containsStringValue(fields, "序列名称") || !containsStringValue(fields, "类型") || !containsStringValue(fields, "数量") {
		t.Fatalf("visual_scope fields missing chart columns: %#v", visualScope)
	}
	evidenceScope, _ := contract["evidence_scope"].(map[string]interface{})
	nonVisual, _ := evidenceScope["non_visual_fields"].([]string)
	if !containsStringValue(nonVisual, "备注") {
		t.Fatalf("expected non-visual evidence field to be preserved, got %#v", evidenceScope)
	}
	display, _ := contract["display"].(map[string]interface{})
	if display["title"] != "各序列子序列与子类数量对比" {
		t.Fatalf("chart title did not use hint, got %#v", display)
	}
	validation, _ := spec["validation"].(map[string]interface{})
	if validation["status"] != "pass" {
		t.Fatalf("expected validation pass, got %#v", validation)
	}
}
