package tools

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
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
