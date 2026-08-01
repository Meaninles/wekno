package ocrstructure

import (
	"strings"
	"testing"
)

func TestDetectAndTrimEmptyMarkdownTableTail(t *testing.T) {
	input := "| 姓名 | 部门 |\n|---|---|\n| 张三 | 财务部 |\n" + strings.Repeat("| | |\n", 12)
	if !HasRunawayEmptyTableTail(input) {
		t.Fatal("empty Markdown tail was not detected")
	}
	trimmed, removed := TrimEmptyTableTail(input)
	if removed != 12 {
		t.Fatalf("removed=%d, want 12", removed)
	}
	if !strings.Contains(trimmed, "张三") || strings.Contains(trimmed, "| | |") {
		t.Fatalf("unexpected trim result: %q", trimmed)
	}
}

func TestNeverTrimsRowsContainingInformation(t *testing.T) {
	input := "| 字段 | 值 |\n|---|---|\n| A | 1 |\n| B | 2 |\n| C | 3 |"
	trimmed, removed := TrimEmptyTableTail(input)
	if removed != 0 || trimmed != input {
		t.Fatalf("information-bearing rows changed: removed=%d text=%q", removed, trimmed)
	}
}
