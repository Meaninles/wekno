package tools

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

const tabularCellTableSuffix = "__cells"
const tabularCellSheetCSV = "CSV"

type tabularCellRecord struct {
	SourceKind     string
	SheetName      string
	RowNumber      int
	ColumnNumber   int
	ColumnLetter   string
	CellRef        string
	Value          string
	EffectiveValue string
	MergedRange    string
	IsMerged       bool
	IsBlank        bool
}

type tabularCellKey struct {
	row int
	col int
}

type TableAnalysisSourceRef struct {
	Sheet        string   `json:"sheet,omitempty" jsonschema:"source sheet name for Excel; use CSV for CSV files when applicable"`
	Cell         string   `json:"cell,omitempty" jsonschema:"A1 cell reference or A1 range such as C56 or C56:C57"`
	Cells        []string `json:"cells,omitempty" jsonschema:"A1 cell references or ranges when multiple original cells support the mapped value"`
	RowNumber    int      `json:"row_number,omitempty" jsonschema:"1-based source row number when an A1 cell is not convenient"`
	ColumnNumber int      `json:"column_number,omitempty" jsonschema:"1-based source column number when an A1 cell is not convenient"`
	Text         string   `json:"text,omitempty" jsonschema:"original text expected at the source cell(s), if known"`
	ExpectedText string   `json:"expected_text,omitempty" jsonschema:"text that should be present in the referenced source cell(s), if known"`
	Description  string   `json:"description,omitempty" jsonschema:"short natural-language description of the source reference"`
	Note         string   `json:"note,omitempty" jsonschema:"optional note about why this source reference supports the result"`
}

type TableAnalysisFieldMapping struct {
	ResultField  string                   `json:"result_field" jsonschema:"field name in the SQL result table"`
	Meaning      string                   `json:"meaning,omitempty" jsonschema:"business meaning of this result field"`
	SourceFields []string                 `json:"source_fields,omitempty" jsonschema:"original file fields, columns, cell groups, or headings used to derive this result field"`
	SourceRefs   []TableAnalysisSourceRef `json:"source_refs,omitempty" jsonschema:"specific source cells or ranges supporting this result field"`
	Derivation   string                   `json:"derivation,omitempty" jsonschema:"how this result field is derived from the original file"`
}

type TableAnalysisRowMapping struct {
	ResultRow    string                   `json:"result_row" jsonschema:"stable row label or row identity in the SQL result table, e.g. 营销序列"`
	ResultValues map[string]string        `json:"result_values,omitempty" jsonschema:"visible SQL result values for this row, stringified when needed"`
	SourceRefs   []TableAnalysisSourceRef `json:"source_refs,omitempty" jsonschema:"specific source cells or ranges supporting this result row"`
	Derivation   string                   `json:"derivation,omitempty" jsonschema:"how this row's values were derived from the referenced source evidence"`
}

type TableAnalysisSourceMapping struct {
	Purpose         string                      `json:"purpose,omitempty" jsonschema:"why this mapping is provided, e.g. chart_result, analysis_result, evidence_inspection"`
	ResultTable     string                      `json:"result_table,omitempty" jsonschema:"short description of the SQL result table"`
	ResultFields    []TableAnalysisFieldMapping `json:"result_fields,omitempty" jsonschema:"example mappings from result table fields to original file fields/cells"`
	RowMappings     []TableAnalysisRowMapping   `json:"row_mappings,omitempty" jsonschema:"example mappings from result rows/values to original file cells/ranges"`
	SourceRefs      []TableAnalysisSourceRef    `json:"source_refs,omitempty" jsonschema:"general source references supporting the overall result"`
	DerivationRules []string                    `json:"derivation_rules,omitempty" jsonschema:"calculation or normalization rules applied to derive the result table"`
	Assumptions     []string                    `json:"assumptions,omitempty" jsonschema:"explicit assumptions made while normalizing irregular file layout"`
	Confidence      string                      `json:"confidence,omitempty" jsonschema:"high, medium, or low confidence in this mapping"`
}

func hasUsableTableAnalysisSourceMapping(mapping map[string]interface{}) bool {
	if len(mapping) == 0 {
		return false
	}
	for key, value := range mapping {
		if strings.TrimSpace(key) == "" || value == nil {
			continue
		}
		if tableAnalysisMappingValueHasContent(value) {
			return true
		}
	}
	return false
}

func tableAnalysisMappingValueHasContent(value interface{}) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []interface{}:
		for _, item := range typed {
			if tableAnalysisMappingValueHasContent(item) {
				return true
			}
		}
		return false
	case []string:
		for _, item := range typed {
			if strings.TrimSpace(item) != "" {
				return true
			}
		}
		return false
	case map[string]interface{}:
		for key, item := range typed {
			if strings.TrimSpace(key) != "" && tableAnalysisMappingValueHasContent(item) {
				return true
			}
		}
		return false
	default:
		return fmt.Sprint(value) != "" && fmt.Sprint(value) != "<nil>"
	}
}

func quoteDuckDBIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func cellEvidenceTableName(tableName string) string {
	return tableName + tabularCellTableSuffix
}

func createTabularCellEvidenceTable(ctx context.Context, db *sql.DB, tableName string, records []tabularCellRecord) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("duckdb connection is unavailable")
	}
	createSQL := fmt.Sprintf(`CREATE TABLE %s (
source_kind VARCHAR,
sheet_name VARCHAR,
row_number INTEGER,
column_number INTEGER,
column_letter VARCHAR,
cell_ref VARCHAR,
value VARCHAR,
effective_value VARCHAR,
merged_range VARCHAR,
is_merged BOOLEAN,
is_blank BOOLEAN
)`, quoteDuckDBIdentifier(tableName))
	if _, err := db.ExecContext(ctx, createSQL); err != nil {
		return 0, fmt.Errorf("failed to create cell evidence table: %w", err)
	}
	if len(records) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin cell evidence insert transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (source_kind, sheet_name, row_number, column_number, column_letter, cell_ref, value, effective_value, merged_range, is_merged, is_blank) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		quoteDuckDBIdentifier(tableName),
	))
	if err != nil {
		return 0, fmt.Errorf("failed to prepare cell evidence insert: %w", err)
	}
	defer stmt.Close()

	for _, record := range records {
		if _, err := stmt.ExecContext(ctx,
			record.SourceKind,
			record.SheetName,
			record.RowNumber,
			record.ColumnNumber,
			record.ColumnLetter,
			record.CellRef,
			record.Value,
			record.EffectiveValue,
			record.MergedRange,
			record.IsMerged,
			record.IsBlank,
		); err != nil {
			return 0, fmt.Errorf("failed to insert cell evidence row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit cell evidence insert transaction: %w", err)
	}
	committed = true
	return int64(len(records)), nil
}

func createCSVCellEvidenceTable(ctx context.Context, db *sql.DB, filename string, tableName string) (int64, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, fmt.Errorf("failed to open CSV for cell evidence: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	rows, err := reader.ReadAll()
	if err != nil {
		return 0, fmt.Errorf("failed to parse CSV for cell evidence: %w", err)
	}

	records := make([]tabularCellRecord, 0)
	for r, row := range rows {
		for c, value := range row {
			colNumber := c + 1
			rowNumber := r + 1
			colName, _ := excelize.ColumnNumberToName(colNumber)
			cellRef, _ := excelize.CoordinatesToCellName(colNumber, rowNumber)
			value = strings.TrimSpace(value)
			records = append(records, tabularCellRecord{
				SourceKind:     "csv",
				SheetName:      tabularCellSheetCSV,
				RowNumber:      rowNumber,
				ColumnNumber:   colNumber,
				ColumnLetter:   colName,
				CellRef:        cellRef,
				Value:          value,
				EffectiveValue: value,
				IsBlank:        value == "",
			})
		}
	}
	return createTabularCellEvidenceTable(ctx, db, tableName, records)
}

func createExcelCellEvidenceTable(ctx context.Context, db *sql.DB, filename string, tableName string, sheetNames []string) (int64, error) {
	workbook, err := excelize.OpenFile(filename)
	if err != nil {
		return 0, fmt.Errorf("failed to open Excel for cell evidence: %w", err)
	}
	defer workbook.Close()

	if len(sheetNames) == 0 {
		sheetNames = workbook.GetSheetList()
	}

	records := make([]tabularCellRecord, 0)
	for _, sheet := range sheetNames {
		if strings.TrimSpace(sheet) == "" {
			continue
		}
		sheetRecords, err := excelSheetCellRecords(workbook, sheet)
		if err != nil {
			return 0, err
		}
		records = append(records, sheetRecords...)
	}
	return createTabularCellEvidenceTable(ctx, db, tableName, records)
}

func excelSheetCellRecords(workbook *excelize.File, sheet string) ([]tabularCellRecord, error) {
	rows, err := workbook.GetRows(sheet)
	if err != nil {
		return nil, fmt.Errorf("failed to read rows from sheet %q: %w", sheet, err)
	}
	mergedIndex, err := excelMergedCellIndex(workbook, sheet)
	if err != nil {
		return nil, err
	}

	cellValues := make(map[tabularCellKey]string)
	maxRow := len(rows)
	maxCol := 0
	for r, row := range rows {
		if len(row) > maxCol {
			maxCol = len(row)
		}
		rowNumber := r + 1
		for c, value := range row {
			value = strings.TrimSpace(value)
			colNumber := c + 1
			if value == "" {
				continue
			}
			cellValues[tabularCellKey{row: rowNumber, col: colNumber}] = value
		}
	}
	for key := range mergedIndex {
		if key.row > maxRow {
			maxRow = key.row
		}
		if key.col > maxCol {
			maxCol = key.col
		}
	}

	records := make([]tabularCellRecord, 0, len(cellValues)+len(mergedIndex))
	seen := make(map[tabularCellKey]bool)
	appendRecord := func(rowNumber, colNumber int, value string) {
		key := tabularCellKey{row: rowNumber, col: colNumber}
		if seen[key] {
			return
		}
		seen[key] = true
		mergeInfo := mergedIndex[key]
		effectiveValue := strings.TrimSpace(value)
		if effectiveValue == "" && mergeInfo.effectiveValue != "" {
			effectiveValue = mergeInfo.effectiveValue
		}
		if effectiveValue == "" && value == "" {
			return
		}
		colName, _ := excelize.ColumnNumberToName(colNumber)
		cellRef, _ := excelize.CoordinatesToCellName(colNumber, rowNumber)
		records = append(records, tabularCellRecord{
			SourceKind:     "excel",
			SheetName:      sheet,
			RowNumber:      rowNumber,
			ColumnNumber:   colNumber,
			ColumnLetter:   colName,
			CellRef:        cellRef,
			Value:          value,
			EffectiveValue: effectiveValue,
			MergedRange:    mergeInfo.rangeRef,
			IsMerged:       mergeInfo.rangeRef != "",
			IsBlank:        strings.TrimSpace(value) == "",
		})
	}

	for key, value := range cellValues {
		appendRecord(key.row, key.col, value)
	}
	for key := range mergedIndex {
		appendRecord(key.row, key.col, cellValues[key])
	}
	_ = maxRow
	_ = maxCol
	return records, nil
}

type excelMergedCellInfo struct {
	rangeRef       string
	effectiveValue string
}

func excelMergedCellIndex(workbook *excelize.File, sheet string) (map[tabularCellKey]excelMergedCellInfo, error) {
	mergedCells, err := workbook.GetMergeCells(sheet)
	if err != nil {
		return nil, fmt.Errorf("failed to read merged cells from sheet %q: %w", sheet, err)
	}
	out := make(map[tabularCellKey]excelMergedCellInfo)
	for _, merged := range mergedCells {
		start := merged.GetStartAxis()
		end := merged.GetEndAxis()
		startCol, startRow, err := excelize.CellNameToCoordinates(start)
		if err != nil {
			continue
		}
		endCol, endRow, err := excelize.CellNameToCoordinates(end)
		if err != nil {
			continue
		}
		if startCol > endCol {
			startCol, endCol = endCol, startCol
		}
		if startRow > endRow {
			startRow, endRow = endRow, startRow
		}
		rangeRef := start + ":" + end
		value := strings.TrimSpace(merged.GetCellValue())
		for row := startRow; row <= endRow; row++ {
			for col := startCol; col <= endCol; col++ {
				out[tabularCellKey{row: row, col: col}] = excelMergedCellInfo{rangeRef: rangeRef, effectiveValue: value}
			}
		}
	}
	return out, nil
}

func expandSourceCellRefs(ref TableAnalysisSourceRef, limit int) []string {
	if limit <= 0 {
		limit = 80
	}
	items := make([]string, 0)
	add := func(value string) {
		value = strings.TrimSpace(strings.ToUpper(value))
		if value == "" {
			return
		}
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(strings.ToUpper(part))
			if part == "" {
				continue
			}
			expanded := expandA1Range(part, limit-len(items))
			for _, cell := range expanded {
				if !containsStringValue(items, cell) {
					items = append(items, cell)
				}
				if len(items) >= limit {
					return
				}
			}
		}
	}
	add(ref.Cell)
	for _, cell := range ref.Cells {
		add(cell)
		if len(items) >= limit {
			break
		}
	}
	if len(items) == 0 && ref.RowNumber > 0 && ref.ColumnNumber > 0 {
		if cell, err := excelize.CoordinatesToCellName(ref.ColumnNumber, ref.RowNumber); err == nil {
			add(cell)
		}
	}
	return items
}

func expandA1Range(value string, limit int) []string {
	value = strings.TrimSpace(strings.ToUpper(value))
	if value == "" || limit <= 0 {
		return nil
	}
	if !strings.Contains(value, ":") {
		return []string{value}
	}
	parts := strings.SplitN(value, ":", 2)
	startCol, startRow, err := excelize.CellNameToCoordinates(strings.TrimSpace(parts[0]))
	if err != nil {
		return []string{value}
	}
	endCol, endRow, err := excelize.CellNameToCoordinates(strings.TrimSpace(parts[1]))
	if err != nil {
		return []string{value}
	}
	if startCol > endCol {
		startCol, endCol = endCol, startCol
	}
	if startRow > endRow {
		startRow, endRow = endRow, startRow
	}
	out := make([]string, 0)
	for row := startRow; row <= endRow; row++ {
		for col := startCol; col <= endCol; col++ {
			cell, err := excelize.CoordinatesToCellName(col, row)
			if err != nil {
				continue
			}
			out = append(out, cell)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func normalizeSourceRefSheet(sheet string) string {
	sheet = strings.TrimSpace(sheet)
	if sheet == "" {
		return ""
	}
	return strings.ToLower(sheet)
}

func sourceRefExpectedText(ref TableAnalysisSourceRef) string {
	for _, value := range []string{ref.Text, ref.ExpectedText, ref.Description} {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func refLabel(ref TableAnalysisSourceRef, cell string) string {
	sheet := strings.TrimSpace(ref.Sheet)
	if sheet == "" {
		sheet = "*"
	}
	if cell == "" && ref.RowNumber > 0 && ref.ColumnNumber > 0 {
		cell = "R" + strconv.Itoa(ref.RowNumber) + "C" + strconv.Itoa(ref.ColumnNumber)
	}
	if cell == "" {
		cell = strings.TrimSpace(ref.Note)
	}
	return sheet + "!" + cell
}

func collectSourceMappingRefs(mapping map[string]interface{}) []TableAnalysisSourceRef {
	if len(mapping) == 0 {
		return nil
	}
	refs := make([]TableAnalysisSourceRef, 0)
	collectSourceMappingRefsFromValue(mapping, "", &refs)
	return refs
}

func collectSourceMappingRefsFromValue(value interface{}, inheritedSheet string, refs *[]TableAnalysisSourceRef) {
	switch typed := value.(type) {
	case map[string]interface{}:
		sheet := firstStringField(typed, inheritedSheet, "sheet", "sheet_name", "worksheet", "工作表")
		cells := stringListField(typed, "cells", "cell_refs", "source_cells", "ranges")
		if cell := firstStringField(typed, "", "cell", "cell_ref", "source_cell", "range", "a1"); cell != "" {
			cells = append([]string{cell}, cells...)
		}
		rowNumber := mappingIntField(typed, "row", "row_number")
		columnNumber := mappingIntField(typed, "column", "column_number", "col")
		if len(cells) > 0 || (rowNumber > 0 && columnNumber > 0) {
			*refs = append(*refs, TableAnalysisSourceRef{
				Sheet:        sheet,
				Cells:        cells,
				RowNumber:    rowNumber,
				ColumnNumber: columnNumber,
				Text:         firstStringField(typed, "", "text", "expected_text", "source_text", "value", "label"),
				Description:  firstStringField(typed, "", "description", "derivation", "rule", "meaning"),
				Note:         firstStringField(typed, "", "note", "reason"),
			})
		}
		for _, child := range typed {
			collectSourceMappingRefsFromValue(child, sheet, refs)
		}
	case []interface{}:
		for _, child := range typed {
			collectSourceMappingRefsFromValue(child, inheritedSheet, refs)
		}
	case []map[string]interface{}:
		for _, child := range typed {
			collectSourceMappingRefsFromValue(child, inheritedSheet, refs)
		}
	}
}

func firstStringField(m map[string]interface{}, fallback string, names ...string) string {
	for _, name := range names {
		value, ok := m[name]
		if !ok {
			continue
		}
		text := strings.TrimSpace(fmt.Sprint(value))
		if text != "" && text != "<nil>" {
			return text
		}
	}
	return strings.TrimSpace(fallback)
}

func stringListField(m map[string]interface{}, names ...string) []string {
	out := make([]string, 0)
	for _, name := range names {
		value, ok := m[name]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case []string:
			for _, item := range typed {
				if text := strings.TrimSpace(item); text != "" {
					out = append(out, text)
				}
			}
		case []interface{}:
			for _, item := range typed {
				if text := strings.TrimSpace(fmt.Sprint(item)); text != "" && text != "<nil>" {
					out = append(out, text)
				}
			}
		default:
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
				out = append(out, text)
			}
		}
	}
	return out
}

func mappingIntField(m map[string]interface{}, names ...string) int {
	for _, name := range names {
		value, ok := m[name]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case int:
			return typed
		case int64:
			return int(typed)
		case float64:
			return int(typed)
		case json.Number:
			if n, err := typed.Int64(); err == nil {
				return int(n)
			}
		default:
			if n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value))); err == nil {
				return n
			}
		}
	}
	return 0
}

func (t *DataAnalysisTool) validateTableAnalysisSourceMapping(ctx context.Context, schema *TableSchema, mapping map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{
		"status":           "not_provided",
		"checked_refs":     0,
		"missing_refs":     []string{},
		"text_mismatches":  []map[string]string{},
		"referenced_cells": []map[string]interface{}{},
		"issues":           []string{},
	}
	if len(mapping) == 0 {
		result["issues"] = []string{"source_mapping is not provided"}
		return result
	}

	refs := collectSourceMappingRefs(mapping)
	if len(refs) == 0 {
		result["status"] = "unchecked"
		result["issues"] = []string{"source_mapping was provided, but no checkable cell references were found; template shape is not enforced"}
		return result
	}

	cellTable := metadataString(schema.Metadata, "cell_table_name")
	if cellTable == "" {
		cellTable = metadataString(schema.Metadata, "raw_table_name")
	}
	if cellTable == "" {
		result["status"] = "unchecked"
		result["issues"] = []string{"cell evidence table is unavailable"}
		return result
	}

	checked := 0
	missing := make([]string, 0)
	mismatches := make([]map[string]string, 0)
	cells := make([]map[string]interface{}, 0)
	for _, ref := range refs {
		cellRefs := expandSourceCellRefs(ref, 80)
		if len(cellRefs) == 0 {
			continue
		}
		for _, cell := range cellRefs {
			if checked >= 120 {
				break
			}
			checked++
			found, err := t.lookupCellEvidence(ctx, cellTable, ref.Sheet, cell)
			if err != nil {
				result["status"] = "unchecked"
				result["issues"] = []string{err.Error()}
				return result
			}
			if len(found) == 0 {
				missing = append(missing, refLabel(ref, cell))
				continue
			}
			expected := sourceRefExpectedText(ref)
			for _, item := range found {
				if len(cells) < 120 {
					cells = append(cells, item)
				}
				if expected == "" {
					continue
				}
				value := strings.TrimSpace(fmt.Sprint(item["value"]))
				effective := strings.TrimSpace(fmt.Sprint(item["effective_value"]))
				if !strings.Contains(value, expected) && !strings.Contains(effective, expected) {
					mismatches = append(mismatches, map[string]string{
						"ref":       refLabel(ref, cell),
						"expected":  expected,
						"value":     value,
						"effective": effective,
					})
				}
			}
		}
	}

	status := "pass"
	issues := make([]string, 0)
	if checked == 0 {
		status = "warning"
		issues = append(issues, "source_mapping has source_refs but no checkable cell references")
	}
	if len(missing) > 0 {
		status = "warning"
		issues = append(issues, "some source_mapping cell references were not found in the original file evidence table")
	}
	if len(mismatches) > 0 {
		status = "warning"
		issues = append(issues, "some source_mapping expected_text values do not match the referenced cells")
	}

	result["status"] = status
	result["checked_refs"] = checked
	result["missing_refs"] = missing
	result["text_mismatches"] = mismatches
	result["referenced_cells"] = cells
	result["issues"] = issues
	return result
}

func (t *DataAnalysisTool) lookupCellEvidence(ctx context.Context, tableName string, sheet string, cell string) ([]map[string]interface{}, error) {
	if t == nil || t.db == nil {
		return nil, fmt.Errorf("duckdb connection is unavailable for source_mapping validation")
	}
	cell = strings.TrimSpace(strings.ToUpper(cell))
	if cell == "" {
		return nil, nil
	}
	query := fmt.Sprintf(`SELECT source_kind, sheet_name, row_number, column_number, column_letter, cell_ref, value, effective_value, merged_range, is_merged, is_blank FROM %s WHERE UPPER(cell_ref) = ?`, quoteDuckDBIdentifier(tableName))
	args := []interface{}{cell}
	if strings.TrimSpace(sheet) != "" {
		query += ` AND LOWER(sheet_name) = ?`
		args = append(args, normalizeSourceRefSheet(sheet))
	}
	query += ` ORDER BY sheet_name, row_number, column_number LIMIT 8`

	rows, err := t.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to validate source_mapping against cell evidence table: %w", err)
	}
	defer rows.Close()

	out := make([]map[string]interface{}, 0)
	for rows.Next() {
		var sourceKind, sheetName, columnLetter, cellRef, value, effectiveValue, mergedRange string
		var rowNumber, columnNumber int
		var isMerged, isBlank bool
		if err := rows.Scan(&sourceKind, &sheetName, &rowNumber, &columnNumber, &columnLetter, &cellRef, &value, &effectiveValue, &mergedRange, &isMerged, &isBlank); err != nil {
			return nil, fmt.Errorf("failed to scan cell evidence row: %w", err)
		}
		out = append(out, map[string]interface{}{
			"source_kind":     sourceKind,
			"sheet":           sheetName,
			"row_number":      rowNumber,
			"column_number":   columnNumber,
			"column_letter":   columnLetter,
			"cell":            cellRef,
			"value":           value,
			"effective_value": effectiveValue,
			"merged_range":    mergedRange,
			"is_merged":       isMerged,
			"is_blank":        isBlank,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate cell evidence rows: %w", err)
	}
	return out, nil
}
