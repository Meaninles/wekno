package dbanalytics

import (
	"reflect"
	"strings"
	"testing"

	wktypes "github.com/Tencent/WeKnora/internal/types"
)

func TestMaskSensitiveString(t *testing.T) {
	cases := map[string]string{
		"13800138000":        "138＊＊＊＊8000",
		"user@example.com":   "u＊＊r@example.com",
		"张三":                 "张＊",
		"王小明":                "王＊明",
		"customer-token-001": "cus＊＊＊＊-001",
	}

	for input, want := range cases {
		if got := maskSensitiveString(input); got != want {
			t.Fatalf("maskSensitiveString(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMaskedColumnSampleValuesAreMaskedForOutput(t *testing.T) {
	col := SourceColumn{
		ColumnName:     "phone",
		SensitiveLevel: "masked",
		SampleValues:   wktypes.JSON(`["13800138000","13800138001"]`),
	}

	got := maskedColumnSampleValues(col)
	want := []string{"138＊＊＊＊8000", "138＊＊＊＊8001"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("maskedColumnSampleValues = %#v, want %#v", got, want)
	}
}

func TestMaskColumnForResponseRewritesSampleValues(t *testing.T) {
	col := SourceColumn{
		ColumnName:     "email",
		SensitiveLevel: "masked",
		SampleValues:   wktypes.JSON(`["alice@example.com"]`),
	}

	maskColumnForResponse(&col)

	got := parseStringArray(col.SampleValues)
	want := []string{"al＊＊＊ce@example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response sample values = %#v, want %#v", got, want)
	}
}

func TestMaterializedMaskedColumnUsesTextAndMaskedValue(t *testing.T) {
	col := SourceColumn{
		ColumnName:     "phone",
		DataType:       "bigint",
		SensitiveLevel: "masked",
	}

	if got := duckTypeForMaterializedColumn(col); got != "TEXT" {
		t.Fatalf("duck type for masked column = %q, want TEXT", got)
	}
	if got := materializedColumnValue(col, int64(13800138000)); got != "138＊＊＊＊8000" {
		t.Fatalf("materialized value = %#v, want masked phone", got)
	}
}

func TestMaskSensitiveStringAvoidsMarkdownAsteriskRuns(t *testing.T) {
	got := maskSensitiveString("Ava Chen")
	if strings.Contains(got, "*") {
		t.Fatalf("masked value %q contains ASCII asterisks that can be parsed as Markdown", got)
	}
	if got != "Av＊＊＊en" {
		t.Fatalf("masked value = %q, want Markdown-safe fullwidth stars", got)
	}
}
