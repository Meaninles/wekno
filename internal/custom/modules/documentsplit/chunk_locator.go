package documentsplit

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

// ChunkLocatorRefiner turns a physical-part coordinate into an original-source
// coordinate for an individual text chunk. It only narrows linear coordinates
// when the parser output has a provable one-output-line-to-one-source-line
// mapping. When that invariant does not hold, Locator returns the complete
// physical-part range rather than inventing a misleading coordinate.
//
// The line index is built once per part so locator refinement stays O(log n)
// per chunk even for very large worksheets and delimited files.
type ChunkLocatorRefiner struct {
	base             map[string]any
	broad            types.JSON
	contentRunes     int
	lineStarts       []int
	contentFirstLine int
	coordinateStart  int
	coordinateEnd    int
	startKey         string
	endKey           string
	canRefine        bool
}

// NewChunkLocatorRefiner creates a reusable locator refiner for one physical
// part. physical_part_index remains in persisted metadata for diagnostics and
// recovery, but model-facing citation rendering removes it.
func NewChunkLocatorRefiner(
	base map[string]any,
	content string,
	physicalPartIndex int,
) (*ChunkLocatorRefiner, error) {
	locator := cloneLocator(base)
	locator["physical_part_index"] = physicalPartIndex
	broad, err := json.Marshal(locator)
	if err != nil {
		return nil, err
	}
	refiner := &ChunkLocatorRefiner{
		base:  locator,
		broad: types.JSON(broad),
	}

	switch kind, _ := locator["kind"].(string); kind {
	case "sheet_range", "record_range":
		refiner.startKey, refiner.endKey = "row_start", "row_end"
	case "line_range":
		refiner.startKey, refiner.endKey = "line_start", "line_end"
	default:
		return refiner, nil
	}
	refiner.coordinateStart = locatorInteger(locator[refiner.startKey])
	refiner.coordinateEnd = locatorInteger(locator[refiner.endKey])
	if refiner.coordinateStart <= 0 ||
		refiner.coordinateEnd < refiner.coordinateStart ||
		content == "" {
		return refiner, nil
	}

	refiner.lineStarts = []int{0}
	firstContentRune, lastContentRune := -1, -1
	runeOffset := 0
	for _, current := range content {
		if !unicode.IsSpace(current) {
			if firstContentRune < 0 {
				firstContentRune = runeOffset
			}
			lastContentRune = runeOffset
		}
		runeOffset++
		if current == '\n' {
			refiner.lineStarts = append(refiner.lineStarts, runeOffset)
		}
	}
	refiner.contentRunes = runeOffset
	if firstContentRune < 0 {
		return refiner, nil
	}
	refiner.contentFirstLine = refiner.lineAt(firstContentRune)
	contentLastLine := refiner.lineAt(lastContentRune)
	sourceLineCount := refiner.coordinateEnd - refiner.coordinateStart + 1
	outputLineCount := contentLastLine - refiner.contentFirstLine + 1
	refiner.canRefine = sourceLineCount == outputLineCount
	return refiner, nil
}

// Locator returns a copy safe for direct persistence on a chunk. start and end
// are rune offsets within the normalized physical-part content, and body must
// be the exact chunk slice represented by that half-open range.
func (r *ChunkLocatorRefiner) Locator(start, end int, body string) types.JSON {
	if r == nil || !r.canRefine || start < 0 || end <= start ||
		end > r.contentRunes || utf8.RuneCountInString(body) != end-start {
		return cloneJSON(r.broad)
	}
	trimmedLeft := strings.TrimLeftFunc(body, unicode.IsSpace)
	if trimmedLeft == "" {
		return cloneJSON(r.broad)
	}
	leadingRunes := utf8.RuneCountInString(body) -
		utf8.RuneCountInString(trimmedLeft)
	trimmed := strings.TrimRightFunc(trimmedLeft, unicode.IsSpace)
	firstRune := start + leadingRunes
	lastRune := firstRune + utf8.RuneCountInString(trimmed) - 1
	if firstRune < 0 || lastRune < firstRune || lastRune >= r.contentRunes {
		return cloneJSON(r.broad)
	}

	firstLine := r.lineAt(firstRune) - r.contentFirstLine
	lastLine := r.lineAt(lastRune) - r.contentFirstLine
	if firstLine < 0 || lastLine < firstLine {
		return cloneJSON(r.broad)
	}
	refinedStart := r.coordinateStart + firstLine
	refinedEnd := r.coordinateStart + lastLine
	if refinedStart < r.coordinateStart || refinedEnd > r.coordinateEnd {
		return cloneJSON(r.broad)
	}

	locator := cloneLocator(r.base)
	locator["part_"+r.startKey] = r.coordinateStart
	locator["part_"+r.endKey] = r.coordinateEnd
	locator[r.startKey] = refinedStart
	locator[r.endKey] = refinedEnd
	encoded, err := json.Marshal(locator)
	if err != nil {
		return cloneJSON(r.broad)
	}
	return types.JSON(encoded)
}

func (r *ChunkLocatorRefiner) lineAt(runeOffset int) int {
	index := sort.Search(len(r.lineStarts), func(index int) bool {
		return r.lineStarts[index] > runeOffset
	})
	if index == 0 {
		return 0
	}
	return index - 1
}

func cloneLocator(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source)+3)
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func cloneJSON(source types.JSON) types.JSON {
	if len(source) == 0 {
		return nil
	}
	return types.JSON(append([]byte(nil), source...))
}

func locatorInteger(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float32:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		integer, _ := typed.Int64()
		return int(integer)
	default:
		return 0
	}
}
