package questioncontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseIgnoresUnknownMergesDuplicatesAndReportsMissing(t *testing.T) {
	inputs := []Input{
		{RecordID: "chunk-a", Content: "a"},
		{RecordID: "chunk-b", Content: "b"},
	}
	report, err := Parse(`{"results":[
        {"record_id":"chunk-a","questions":["谁负责审批该事项？"]},
        {"record_id":"one-past-end","questions":["不应污染任何块？"]},
        {"record_id":"chunk-a","questions":["办理期限是多久？","谁负责审批该事项？"]}
    ]}`, inputs, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Results["chunk-a"]; len(got) != 2 {
		t.Fatalf("merged questions = %#v, want 2 unique", got)
	}
	if len(report.UnknownRecordIDs) != 1 || report.UnknownRecordIDs[0] != "one-past-end" {
		t.Fatalf("unknown IDs = %#v", report.UnknownRecordIDs)
	}
	if report.DuplicateRecordCount != 1 {
		t.Fatalf("duplicate count = %d, want 1", report.DuplicateRecordCount)
	}
	if len(report.MissingRecordIDs) != 1 || report.MissingRecordIDs[0] != "chunk-b" {
		t.Fatalf("missing IDs = %#v", report.MissingRecordIDs)
	}
}

func TestParseSkipsMalformedQuestionObjectWithoutDiscardingRecord(t *testing.T) {
	report, err := Parse(`{"results":[{"record_id":"chunk-a","questions":[
        {"question":"谁负责审批该事项？","text":"冲突文本"},
        {"answer":"业务部门"},
        {"question":"办理期限是多久？"}
    ]}]}`, []Input{{RecordID: "chunk-a"}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if report.InvalidQuestionCount != 2 {
		t.Fatalf("invalid questions = %d, want 2", report.InvalidQuestionCount)
	}
	if got := report.Results["chunk-a"]; len(got) != 1 || got[0] != "办理期限是多久？" {
		t.Fatalf("questions = %#v", got)
	}
	if len(report.MissingRecordIDs) != 0 {
		t.Fatalf("known record with invalid questions became missing: %#v", report.MissingRecordIDs)
	}
}

func TestParseAcceptsBareArrayInsideMarkdownFence(t *testing.T) {
	report, err := Parse("```json\n"+`[
        {"record_id":"r_a19f3c20","questions":["谁负责审批该事项？"]},
        {"record_id":"r_84b1e902","questions":[]}
    ]`+"\n```", []Input{
		{RecordID: "r_a19f3c20"},
		{RecordID: "r_84b1e902"},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Results["r_a19f3c20"]; len(got) != 1 || got[0] != "谁负责审批该事项？" {
		t.Fatalf("questions = %#v", got)
	}
	if _, ok := report.Results["r_84b1e902"]; !ok {
		t.Fatal("explicit empty bare-array record became missing")
	}
}

func TestSchemaEnumeratesOnlyOpaqueRecordIDs(t *testing.T) {
	raw, err := Schema([]Input{{RecordID: "chunk-a"}, {RecordID: "chunk-b"}})
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if text == "" || !containsAll(text, "record_id", "chunk-a", "chunk-b") {
		t.Fatalf("schema does not bind record IDs: %s", text)
	}
	if containsAll(text, "chunk_index") {
		t.Fatalf("schema still exposes positional chunk_index: %s", text)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
