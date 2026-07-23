package documentsplit

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChunkLocatorRefinerNarrowsOriginalSpreadsheetRows(t *testing.T) {
	content := "A: 180000,B: first\nA: 180001,B: second\nA: 180002,B: third"
	refiner, err := NewChunkLocatorRefiner(map[string]any{
		"kind":      "sheet_range",
		"sheet":     "数据源表",
		"row_start": float64(180001),
		"row_end":   float64(180003),
	}, content, 10)
	require.NoError(t, err)

	start := len([]rune("A: 180000,B: first\n"))
	end := start + len([]rune("A: 180001,B: second"))
	locator := refiner.Locator(start, end, "A: 180001,B: second")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(locator, &decoded))
	require.Equal(t, float64(180002), decoded["row_start"])
	require.Equal(t, float64(180002), decoded["row_end"])
	require.Equal(t, float64(180001), decoded["part_row_start"])
	require.Equal(t, float64(180003), decoded["part_row_end"])
	require.Equal(t, float64(10), decoded["physical_part_index"])
}

func TestChunkLocatorRefinerSupportsOverlappingLineChunks(t *testing.T) {
	content := "第一行\n第二行\n第三行\n第四行"
	refiner, err := NewChunkLocatorRefiner(map[string]any{
		"kind":       "line_range",
		"line_start": 500,
		"line_end":   503,
	}, content, 2)
	require.NoError(t, err)

	body := "第二行\n第三行"
	start := len([]rune("第一行\n"))
	locator := refiner.Locator(start, start+len([]rune(body)), body)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(locator, &decoded))
	require.Equal(t, float64(501), decoded["line_start"])
	require.Equal(t, float64(502), decoded["line_end"])
}

func TestChunkLocatorRefinerKeepsBroadRangeWhenMappingIsAmbiguous(t *testing.T) {
	content := "# synthetic sheet title\nfirst row\nsecond row"
	refiner, err := NewChunkLocatorRefiner(map[string]any{
		"kind":      "record_range",
		"row_start": 100,
		"row_end":   101,
	}, content, 4)
	require.NoError(t, err)

	locator := refiner.Locator(0, len([]rune(content)), content)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(locator, &decoded))
	require.Equal(t, float64(100), decoded["row_start"])
	require.Equal(t, float64(101), decoded["row_end"])
	require.NotContains(t, decoded, "part_row_start")
}

func TestChunkLocatorRefinerKeepsNonLinearCoordinates(t *testing.T) {
	content := "page text"
	refiner, err := NewChunkLocatorRefiner(map[string]any{
		"kind":       "pages",
		"page_start": 4,
		"page_end":   9,
	}, content, 3)
	require.NoError(t, err)

	locator := refiner.Locator(0, len([]rune(content)), content)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(locator, &decoded))
	require.Equal(t, float64(4), decoded["page_start"])
	require.Equal(t, float64(9), decoded["page_end"])
	require.Equal(t, float64(3), decoded["physical_part_index"])
}
