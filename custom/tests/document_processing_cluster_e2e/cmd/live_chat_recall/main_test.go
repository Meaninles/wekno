package main

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestValidateCaseTreatsGroundedRecallSeparatelyFromAnswerGeneration(t *testing.T) {
	testCase := recallCase{
		ExpectedSourceSubstrings: []string{"优秀班长评选规定"},
		RequiredEvidenceGroups: [][]string{
			{"3年"},
			{"5%"},
			{"30000元"},
		},
		RequiredAnswerGroups: [][]string{
			{"3年"},
			{"5%"},
			{"30000元"},
		},
	}
	rawReferences := types.References{
		&types.SearchResult{
			KnowledgeTitle: "优秀班长评选规定.doc",
			Content:        "连续任班长3年及以上；名额不超过5%；一次性奖金30000元。",
		},
	}
	result := caseResult{
		StreamCompleted: true,
		AnswerDone:      true,
		Answer:          "连续任班长3年；比例5%；奖金300元。",
		FailureReasons:  make([]string, 0),
	}

	validateCase(testCase, rawReferences, &result)

	if !result.Passed {
		t.Fatalf("recall should pass with complete source evidence: %v", result.FailureReasons)
	}
	if result.AnswerDiagnosticOK {
		t.Fatal("answer diagnostic should record the downstream numeric rewrite")
	}
	if len(result.AnswerDiagnosticNotes) != 1 {
		t.Fatalf("answer diagnostic notes = %v, want one mismatch", result.AnswerDiagnosticNotes)
	}
}
