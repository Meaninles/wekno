package documentsplit

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	defaultTableSummarySampleBudget = 24_000
	tableSummarySampleRunes         = 640
)

// TableSummaryCorpus is the bounded, coordinate-aware representation used by
// downstream table metadata generation for a physically split workbook/CSV.
// It intentionally samples the immutable logical chunk sequence instead of
// reopening and materialising the entire original workbook in DuckDB.
type TableSummaryCorpus struct {
	TableName         string
	SchemaDescription string
	SampleDescription string
}

type tableCoordinate struct {
	Sheet         string
	Kind          string
	RowStart      int64
	RowEnd        int64
	ColumnStart   int64
	ColumnEnd     int64
	HeaderContext string
}

type sheetCoverage struct {
	Name          string
	MinRow        int64
	MaxRow        int64
	MinColumn     int64
	MaxColumn     int64
	HeaderContext []string
}

// BuildTableSummaryCorpus turns evenly distributed text chunks into a prompt
// corpus that preserves worksheet, row and column coordinates. The result is
// deterministic and strictly bounded so very large workbooks do not create
// unbounded LLM prompts.
func BuildTableSummaryCorpus(
	plan *Plan,
	knowledge *types.Knowledge,
	samples []*types.Chunk,
	logicalChunkCount int64,
	sampleBudget int,
	physicalParts ...*Part,
) (TableSummaryCorpus, error) {
	if plan == nil || knowledge == nil {
		return TableSummaryCorpus{}, fmt.Errorf("document split: table summary requires plan and knowledge")
	}
	if len(samples) == 0 {
		return TableSummaryCorpus{}, fmt.Errorf("document split: table summary has no logical text samples")
	}
	if sampleBudget <= 0 {
		sampleBudget = defaultTableSummarySampleBudget
	}

	tableName := strings.TrimSpace(knowledge.FileName)
	if tableName == "" {
		tableName = strings.TrimSpace(knowledge.Title)
	}
	if tableName == "" {
		tableName = plan.SourceName
	}

	coverageBySheet := make(map[string]*sheetCoverage)
	coverageOrder := make([]string, 0)
	coordinates := make([]tableCoordinate, len(samples))
	for index, chunk := range samples {
		coordinates[index] = parseTableCoordinate(chunk)
	}
	coverageCoordinates := coordinates
	coverageIsComplete := false
	if len(physicalParts) > 0 {
		coverageCoordinates = make([]tableCoordinate, 0, len(physicalParts))
		for _, part := range physicalParts {
			if part == nil {
				continue
			}
			coverageCoordinates = append(coverageCoordinates, parseTableLocator(part.Locator))
		}
		coverageIsComplete = len(coverageCoordinates) > 0
	}
	for _, coordinate := range coverageCoordinates {
		sheet := coordinate.Sheet
		if sheet == "" {
			sheet = "(default)"
		}
		coverage := coverageBySheet[sheet]
		if coverage == nil {
			coverage = &sheetCoverage{Name: sheet}
			coverageBySheet[sheet] = coverage
			coverageOrder = append(coverageOrder, sheet)
		}
		coverage.MinRow = positiveMinimum(coverage.MinRow, coordinate.RowStart)
		coverage.MaxRow = max(coverage.MaxRow, coordinate.RowEnd)
		coverage.MinColumn = positiveMinimum(coverage.MinColumn, coordinate.ColumnStart)
		coverage.MaxColumn = max(coverage.MaxColumn, coordinate.ColumnEnd)
		if header := strings.TrimSpace(coordinate.HeaderContext); header != "" &&
			!containsString(coverage.HeaderContext, header) &&
			len(coverage.HeaderContext) < 16 {
			coverage.HeaderContext = append(coverage.HeaderContext, header)
		}
	}
	// Header/schema context can be carried by logical chunks even when the
	// physical manifest only records extents. Merge it without changing the
	// exact part-derived row/column coverage.
	for _, coordinate := range coordinates {
		header := strings.TrimSpace(coordinate.HeaderContext)
		if header == "" {
			continue
		}
		sheet := coordinate.Sheet
		if sheet == "" {
			sheet = "(default)"
		}
		coverage := coverageBySheet[sheet]
		if coverage == nil {
			coverage = &sheetCoverage{Name: sheet}
			coverageBySheet[sheet] = coverage
			coverageOrder = append(coverageOrder, sheet)
		}
		if !containsString(coverage.HeaderContext, header) && len(coverage.HeaderContext) < 16 {
			coverage.HeaderContext = append(coverage.HeaderContext, header)
		}
	}

	var schema strings.Builder
	fmt.Fprintf(&schema, "Logical table document: %s\n", tableName)
	fmt.Fprintf(&schema, "Source type: %s\n", plan.SourceType)
	fmt.Fprintf(&schema, "Original source bytes: %d\n", plan.SourceSize)
	fmt.Fprintf(&schema, "Physical parser parts: %d\n", plan.PartCount)
	fmt.Fprintf(&schema, "Logical text chunks: %d\n", logicalChunkCount)
	fmt.Fprintf(&schema, "Evenly distributed samples: %d\n", len(samples))
	if coverageIsComplete {
		schema.WriteString("Complete logical source coverage from the split manifest (physical part boundaries are not semantic boundaries):\n")
	} else {
		schema.WriteString("Sampled logical source coverage (physical part boundaries are not semantic boundaries):\n")
	}
	for _, sheet := range coverageOrder {
		item := coverageBySheet[sheet]
		fmt.Fprintf(&schema, "- Sheet %q", item.Name)
		if item.MinRow > 0 || item.MaxRow > 0 {
			fmt.Fprintf(&schema, ", rows %d-%d", item.MinRow, item.MaxRow)
		}
		if item.MinColumn > 0 || item.MaxColumn > 0 {
			fmt.Fprintf(&schema, ", columns %d-%d", item.MinColumn, item.MaxColumn)
		}
		schema.WriteString("\n")
		for _, header := range item.HeaderContext {
			fmt.Fprintf(&schema, "  - Header/schema: %s\n", header)
		}
	}

	// Keep the corpus in logical order even if a caller handed us a set.
	type indexedSample struct {
		chunk      *types.Chunk
		coordinate tableCoordinate
	}
	indexed := make([]indexedSample, 0, len(samples))
	for index, chunk := range samples {
		if chunk == nil || strings.TrimSpace(chunk.Content) == "" {
			continue
		}
		indexed = append(indexed, indexedSample{chunk: chunk, coordinate: coordinates[index]})
	}
	sort.SliceStable(indexed, func(i, j int) bool {
		return indexed[i].chunk.ChunkIndex < indexed[j].chunk.ChunkIndex
	})

	perSample := sampleBudget / max(1, len(indexed))
	perSample = min(tableSummarySampleRunes, max(160, perSample))
	var sampleText strings.Builder
	sampleText.WriteString("Stratified samples across the complete logical table (not only its beginning):\n")
	for _, sample := range indexed {
		header := formatTableCoordinate(sample.coordinate)
		body := boundedHeadTail(strings.TrimSpace(sample.chunk.Content), perSample)
		if body == "" {
			continue
		}
		fmt.Fprintf(&sampleText, "\n[Logical chunk %d", sample.chunk.ChunkIndex)
		if header != "" {
			fmt.Fprintf(&sampleText, "; %s", header)
		}
		sampleText.WriteString("]\n")
		sampleText.WriteString(body)
		sampleText.WriteString("\n")
		if sampleText.Len() >= sampleBudget {
			break
		}
	}
	sampleRunes := []rune(sampleText.String())
	if len(sampleRunes) > sampleBudget {
		sampleRunes = sampleRunes[:sampleBudget]
	}

	return TableSummaryCorpus{
		TableName:         tableName,
		SchemaDescription: schema.String(),
		SampleDescription: string(sampleRunes),
	}, nil
}

func parseTableCoordinate(chunk *types.Chunk) tableCoordinate {
	var coordinate tableCoordinate
	if chunk == nil || len(chunk.SourceLocator) == 0 {
		return coordinate
	}
	return parseTableLocator(chunk.SourceLocator)
}

func parseTableLocator(raw []byte) tableCoordinate {
	var coordinate tableCoordinate
	if len(raw) == 0 {
		return coordinate
	}
	var locator map[string]any
	if err := json.Unmarshal(raw, &locator); err != nil {
		return coordinate
	}
	coordinate.Kind = stringValue(locator, "kind")
	coordinate.Sheet = firstStringValue(locator, "sheet", "sheet_name")
	coordinate.RowStart = firstIntegerValue(locator, "row_start", "line_start")
	coordinate.RowEnd = firstIntegerValue(locator, "row_end", "line_end")
	coordinate.ColumnStart = firstIntegerValue(locator, "column_start", "col_start")
	coordinate.ColumnEnd = firstIntegerValue(locator, "column_end", "col_end")
	coordinate.HeaderContext = stringValue(locator, "header_context")
	return coordinate
}

func formatTableCoordinate(coordinate tableCoordinate) string {
	values := make([]string, 0, 4)
	if coordinate.Sheet != "" {
		values = append(values, "sheet "+strconv.Quote(coordinate.Sheet))
	}
	if coordinate.RowStart > 0 || coordinate.RowEnd > 0 {
		values = append(values, fmt.Sprintf("rows %d-%d", coordinate.RowStart, coordinate.RowEnd))
	}
	if coordinate.ColumnStart > 0 || coordinate.ColumnEnd > 0 {
		values = append(values, fmt.Sprintf("columns %d-%d", coordinate.ColumnStart, coordinate.ColumnEnd))
	}
	if coordinate.HeaderContext != "" {
		values = append(values, "schema "+coordinate.HeaderContext)
	}
	return strings.Join(values, ", ")
}

func boundedHeadTail(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	head := limit * 3 / 4
	tail := limit - head
	return string(runes[:head]) + "\n[…]\n" + string(runes[len(runes)-tail:])
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(values, key); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func firstIntegerValue(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return int64(typed)
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return parsed
			}
		case string:
			if parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil {
				return parsed
			}
		}
	}
	return 0
}

func positiveMinimum(current, candidate int64) int64 {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
