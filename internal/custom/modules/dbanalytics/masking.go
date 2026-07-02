package dbanalytics

import (
	"encoding/json"
	"fmt"
	"strings"

	wktypes "github.com/Tencent/WeKnora/internal/types"
)

const (
	sensitiveLevelMasked = "masked"
	maskChar             = "＊"
)

func isMaskedColumn(col SourceColumn) bool {
	return isMaskedSensitiveLevel(col.SensitiveLevel)
}

func isMaskedSensitiveLevel(level string) bool {
	return strings.EqualFold(strings.TrimSpace(level), sensitiveLevelMasked)
}

func duckTypeForMaterializedColumn(col SourceColumn) string {
	if isMaskedColumn(col) {
		return "TEXT"
	}
	return duckType(col.DataType)
}

func materializedColumnValue(col SourceColumn, value any) any {
	if !isMaskedColumn(col) {
		return value
	}
	return maskSensitiveValue(value)
}

func maskedColumnSampleValues(col SourceColumn) []string {
	values := parseStringArray(col.SampleValues)
	if !isMaskedColumn(col) {
		return values
	}
	return maskStringSamples(values)
}

func maskColumnForResponse(col *SourceColumn) {
	if col == nil || !isMaskedColumn(*col) {
		return
	}
	sampleJSON, _ := json.Marshal(maskStringSamples(parseStringArray(col.SampleValues)))
	col.SampleValues = wktypes.JSON(sampleJSON)
}

func maskSourceColumnsForResponse(src *Source) {
	if src == nil {
		return
	}
	for i := range src.Tables {
		for j := range src.Tables[i].Columns {
			maskColumnForResponse(&src.Tables[i].Columns[j])
		}
	}
}

func maskStringSamples(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		masked := maskSensitiveString(value)
		if masked == "" || seen[masked] {
			continue
		}
		seen[masked] = true
		out = append(out, masked)
	}
	return out
}

func maskSensitiveValue(value any) any {
	if value == nil {
		return nil
	}
	return maskSensitiveString(fmt.Sprint(value))
}

func maskSensitiveString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if local, domain, ok := strings.Cut(value, "@"); ok && local != "" && domain != "" {
		return maskRunes(local) + "@" + domain
	}
	runes := []rune(value)
	if len(runes) >= 7 && allDigits(runes) {
		return string(runes[:3]) + strings.Repeat(maskChar, 4) + string(runes[len(runes)-4:])
	}
	return maskRunes(value)
}

func maskRunes(value string) string {
	runes := []rune(value)
	switch n := len(runes); {
	case n <= 0:
		return ""
	case n == 1:
		return maskChar
	case n == 2:
		return string(runes[:1]) + maskChar
	case n <= 4:
		return string(runes[:1]) + strings.Repeat(maskChar, n-2) + string(runes[n-1:])
	case n <= 8:
		return string(runes[:2]) + strings.Repeat(maskChar, 3) + string(runes[n-2:])
	default:
		return string(runes[:3]) + strings.Repeat(maskChar, 4) + string(runes[n-4:])
	}
}

func allDigits(runes []rune) bool {
	for _, r := range runes {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
